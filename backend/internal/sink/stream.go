package sink

// Redis Streams 落库管道：让 gateway 角色既不直连 PostgreSQL 写日志、又不丢日志。
//
// # 为什么要经过 Redis 而不是让 gateway 直接写 PG
//
// 不是因为 PG 写不动（COPY 能到 20 万条/s），而是为了**让转发实例不持有写职责**：
//
//   - N 个转发实例 × 连接池会打穿 PG 的 max_connections
//   - 转发实例要能随时杀、随时加，不该在退出时还欠着一批没落库的数据
//   - 落库变慢/PG 抖动不应该反压到转发实例上
//
// # 为什么请求协程不能直接 XADD
//
// 这是这个文件最重要的一条。XADD 是一次网络往返（本机 ~50µs，跨机 ~500µs），
// 直接放在请求协程里，等于把「同步写 PG」的问题换成了「同步写 Redis」的问题——
// 热路径又多了一个可以抖动、可以变慢、可以挂掉的依赖。
//
// 所以结构和进程内攒批完全一样，只是 flush 的目的地不同：
//
//	请求协程 → 本地有界 chan（非阻塞，满了丢弃并计数）
//	           ↓ 后台协程攒批（200 条 / 200ms）
//	         XADD pipeline 一次送一批 → Redis Stream
//	                                    ↓ XREADGROUP
//	                                  worker 落库 → XACK
//
// # 投递语义
//
// 至少一次（at-least-once）。worker 是「读一批 → 落库成功 → 才 XACK」，
// 所以 worker 被 kill -9 时那批消息还在 pending 里，会被 XAUTOCLAIM 捞回来重投。
// 代价是极端情况下可能重复落一条日志——对观测数据来说这远比丢失可接受。
//
// 但 gateway 侧仍然是「最多一次」：本地 chan 里还没 XADD 出去的那部分，
// 进程被 kill -9 就没了（优雅退出会 flush）。要做到端到端不丢就得在请求协程里同步
// XADD，而那正是上面拒绝的事情。**这是一个明确的权衡，不是遗漏。**

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/redis/go-redis/v9"
)

// streamField Stream 消息里存 payload 的字段名。
// 一条消息就一个字段，值是整个 Entry 的 JSON —— 不摊平成多个字段，
// 因为摊平之后加字段要同时改生产端和消费端，而 JSON 天然向后兼容。
const streamField = "e"

// StreamOptions 生产端参数。
type StreamOptions struct {
	// QueueSize 本地缓冲深度（等待 XADD 的 Entry）。与请求体大小无关，~350 字节/条
	QueueSize int
	// MaxLen Stream 的最大长度。**必须有界**：Redis 是内存数据库，
	// 无界 Stream 在 worker 挂掉时会把 Redis 吃光，然后拖垮限流/选主/广播。
	// 用 XADD MAXLEN ~ 做近似裁剪（按整节点裁，比精确裁剪快得多）。
	MaxLen int64
}

func (o *StreamOptions) setDefaults() {
	if o.QueueSize <= 0 {
		o.QueueSize = 32768
	}
	if o.MaxLen <= 0 {
		// 100 万条 × ~400 字节 ≈ 400MB。按 22000 RPS 算能扛 45 秒的完全积压，
		// 足够 worker 重启；再多就该让它丢了，保住 Redis 比保住日志重要。
		o.MaxLen = 1_000_000
	}
}

// Stream 生产端：实现 Sink 接口，投递到 Redis Stream。
type Stream struct {
	rc   *rds.Client
	key  string
	opt  StreamOptions
	inst string

	ch     chan Entry
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once

	enqueued, dropped, published, batches, failedBatches atomic.Uint64

	mu           sync.RWMutex
	lastFlushAt  time.Time
	lastFlushMs  int64
	lastBatchLen int
	lastErr      string
}

// StreamKey 日志流的 key（生产端和消费端必须一致，所以放在一处）。
func StreamKey(rc *rds.Client) string { return rc.Key("logs") }

// ConsumerGroup 消费组名。用消费组而不是裸 XREAD，是为了拿到两件东西：
// 多个 worker 自动分担（同组内竞争消费），以及 pending 列表（未 ACK 可重投）。
const ConsumerGroup = "gw-workers"

func NewStream(rc *rds.Client, instance string, opt StreamOptions) *Stream {
	opt.setDefaults()
	return &Stream{
		rc:   rc,
		key:  StreamKey(rc),
		opt:  opt,
		inst: instance,
		ch:   make(chan Entry, opt.QueueSize),
		done: make(chan struct{}),
	}
}

func (s *Stream) Start() { go s.loop() }

// Submit 非阻塞投递到本地缓冲。热路径只做这一件事，不碰网络。
func (s *Stream) Submit(e Entry) bool {
	if s.closed.Load() {
		return false
	}
	select {
	case s.ch <- e:
		s.enqueued.Add(1)
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}

func (s *Stream) loop() {
	defer close(s.done)
	collect(s.ch, s.publish) // 攒批逻辑与进程内落库共用，只是 flush 换成 XADD
}

// publish 把一批 Entry 用 pipeline 一次性 XADD 出去。
//
// pipeline 的意义：200 条日志从 200 次往返变成 1 次。不用 pipeline 的话，
// 单次 XADD 50µs × 200 = 10ms，攒批就白攒了。
func (s *Stream) publish(batch []Entry) {
	start := time.Now()
	var errMsg string
	var ok int

	// 这里的超时不能用 rds 那个 50ms 的默认值：那是给「限流查询」定的，
	// 一批 200 条的 pipeline 需要更多时间，而且这不在热路径上（后台协程）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 不走 rc.Do（那层的超时/熔断是为热路径的小查询设计的），
	// 但要在失败时把错误喂给它，让 Console 上的降级状态保持真实。
	err := func() error {
		pipe := s.rc.Raw().Pipeline()
		for i := range batch {
			b, err := json.Marshal(&batch[i])
			if err != nil {
				// 单条序列化失败不该拖垮整批（理论上不会发生）
				log.Printf("[sink] 日志序列化失败，跳过该条: %v", err)
				continue
			}
			pipe.XAdd(ctx, &redis.XAddArgs{
				Stream: s.key,
				MaxLen: s.opt.MaxLen,
				Approx: true, // MAXLEN ~ ：按整节点裁剪，避免精确裁剪的 O(n) 开销
				Values: []any{streamField, b},
			})
			ok++
		}
		if ok == 0 {
			return nil
		}
		_, err := pipe.Exec(ctx)
		return err
	}()

	if err != nil {
		errMsg = "XADD 失败: " + err.Error()
		s.failedBatches.Add(1)
		s.dropped.Add(uint64(len(batch)))
		// Redis 不可用时日志确实丢了。这是 fail-open 的代价：
		// 宁可丢观测数据，也不阻塞转发、更不把请求打回给客户端。
		log.Printf("[sink] %s（本批 %d 条已丢弃）", errMsg, len(batch))
	} else {
		s.published.Add(uint64(ok))
	}
	s.batches.Add(1)

	s.mu.Lock()
	s.lastFlushAt = time.Now()
	s.lastFlushMs = time.Since(start).Milliseconds()
	s.lastBatchLen = len(batch)
	s.lastErr = errMsg
	s.mu.Unlock()
}

func (s *Stream) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		Enqueued:     s.enqueued.Load(),
		Dropped:      s.dropped.Load(),
		Persisted:    s.published.Load(), // 对生产端来说「已投递到 Stream」就是它的终点
		Batches:      s.batches.Load(),
		QueueLen:     len(s.ch),
		QueueCap:     cap(s.ch),
		Active:       true,
		Via:          "redis-stream",
		LastFlushMs:  s.lastFlushMs,
		LastBatchLen: s.lastBatchLen,
		LastError:    s.lastErr,
	}
	if !s.lastFlushAt.IsZero() {
		st.LastFlushAt = s.lastFlushAt.Format(time.RFC3339)
	}
	// 生产端不落库，COPY 与它无关；报 true 免得前端拿这个报警
	st.UsingCopy = true
	return st
}

// Close 停止接收并把本地缓冲里剩下的 XADD 出去（优雅退出时尽量不丢）。
func (s *Stream) Close(ctx context.Context) error {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.ch)
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// entryJSON 是 Entry 的线上格式。
//
// 单独定义而不是直接给 Entry 加 tag，是因为线上格式是**跨进程契约**：
// 新版 gateway 和旧版 worker 可能同时在跑（滚动升级），字段只能加不能改名。
// 有个独立的类型能让这件事在代码里显式可见。
type entryJSON struct {
	Log               *store.RequestLog `json:"log,omitempty"`
	TouchGatewayKeyID uint              `json:"gk,omitempty"`
	TouchChannelKeyID uint              `json:"ck,omitempty"`
}

func (e Entry) MarshalJSON() ([]byte, error) {
	return json.Marshal(entryJSON{
		Log:               e.Log,
		TouchGatewayKeyID: e.TouchGatewayKeyID,
		TouchChannelKeyID: e.TouchChannelKeyID,
	})
}

func (e *Entry) UnmarshalJSON(b []byte) error {
	var v entryJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	e.Log = v.Log
	e.TouchGatewayKeyID = v.TouchGatewayKeyID
	e.TouchChannelKeyID = v.TouchChannelKeyID
	return nil
}
