// Package sink 把**日志落库**从请求热路径上摘下来。
//
// 为什么需要它：原先每个请求同步做 1 条日志 INSERT + 2 条 last_used_at UPDATE，
// 也就是 3 个写事务；每个事务都要等一次 WAL 落盘确认 + 一次网络往返。
// 这把网关吞吐压在几百 RPS、p99 拉到秒级（当时是 sqlite，全局单写者，更惨：~220 RPS / p99 2.5s）。
//
// 现在的做法：请求协程只把 Entry 丢进有界队列（非阻塞），后台协程攒批，
// 在**单个事务**里批量插入日志 + 合并写 last_used_at（last-write-wins，天生可合并），
// 于是「每请求 3 个事务」变成「每批 1 个事务」。
//
// 换到 PostgreSQL 后攒批依然是净收益，只是收益来源变了：
// 不再是「绕开单写者」，而是更少的事务提交、更少的 WAL flush、更少的网络往返。
//
// 队列里**只有日志行**（~400 字节/条，所以 8192 深的队列就是 ~3MB，与请求大小无关）。
// 请求/响应原文由 archive 包在请求协程里当场写盘——它们动辄几十 KB，
// 放进队列会让内存占用变成「队列深度 × 请求体大小」，而且攒批对写文件没任何好处。
//
// Sink 是接口：将来要多实例部署、或要把日志交给独立消费者写 ClickHouse，
// 换一个 Redis Stream 实现即可，请求侧代码不用动。
package sink

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

// Entry 一次请求产生的待落库内容。它必须保持**小且定长**（~400 字节）：
// 排队中的 Entry 占的是堆内存，任何跟请求体积相关的东西（原文！）都不能放进来。
type Entry struct {
	Log *store.RequestLog

	// 需要刷新 last_used_at 的 key（合并成每批 1 条 UPDATE）
	TouchGatewayKeyID uint
	TouchChannelKeyID uint
}

// Stats 供 WebUI 观察异步管道的健康度。丢弃必须可见，否则就是隐性数据丢失。
type Stats struct {
	Enqueued     uint64 `json:"enqueued"`
	Dropped      uint64 `json:"dropped"`
	Persisted    uint64 `json:"persisted"`
	Batches      uint64 `json:"batches"`
	QueueLen     int    `json:"queue_len"`
	QueueCap     int    `json:"queue_cap"`
	LastFlushAt  string `json:"last_flush_at"`
	LastFlushMs  int64  `json:"last_flush_ms"`
	LastBatchLen int    `json:"last_batch_len"`
	LastError    string `json:"last_error"`
	// UsingCopy 是否在用 COPY 快路径（false = 已退化到逐行 INSERT，吞吐约 1/5）
	UsingCopy bool `json:"using_copy"`
	// Active 本实例是否真的承担落库职责。gateway 角色为 false，
	// 此时上面那些指标都没有意义，前端不该拿它们报警。
	Active bool `json:"active"`
}

// Sink 落库管道。
type Sink interface {
	// Submit 非阻塞投递。返回 false 表示队列已满、该条被丢弃（宁可丢观测数据，也不阻塞转发）。
	Submit(e Entry) bool
	Stats() Stats
	// Close 停止接收并把队列里剩下的刷完（优雅退出时调用）。
	Close(ctx context.Context) error
}

// Batch 进程内实现：有界 channel + 单写协程 + 攒批单事务。
type Batch struct {
	db *gorm.DB

	ch     chan Entry
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once

	enqueued, dropped, persisted, batches atomic.Uint64

	// useCopy 是否走 COPY 快路径；Start 时校验列清单，不一致就退化回 GORM
	useCopy atomic.Bool

	mu           sync.RWMutex
	lastFlushAt  time.Time
	lastFlushMs  int64
	lastBatchLen int
	lastErr      string
}

func NewBatch(db *gorm.DB, queueSize int) *Batch {
	if queueSize <= 0 {
		queueSize = 4096
	}
	return &Batch{
		db:   db,
		ch:   make(chan Entry, queueSize),
		done: make(chan struct{}),
	}
}

func (b *Batch) Start() {
	// COPY 的列清单是手写的，跟实际表结构校一下；不一致就退化到 GORM（慢但正确），
	// 而不是默默丢字段。
	if b.db != nil {
		if err := VerifyLogColumns(b.db); err != nil {
			log.Printf("[sink] COPY 不可用，退化为逐行 INSERT（吞吐约降为 1/5）: %v", err)
		} else {
			b.useCopy.Store(true)
		}
	}
	go b.loop()
}

// UsingCopy 当前是否走 COPY 快路径（暴露到 WebUI，退化了要能看见）。
func (b *Batch) UsingCopy() bool { return b.useCopy.Load() }

func (b *Batch) Submit(e Entry) bool {
	if b.closed.Load() {
		return false
	}
	select {
	case b.ch <- e:
		b.enqueued.Add(1)
		return true
	default:
		// 队列满：丢弃并计数，绝不阻塞请求
		b.dropped.Add(1)
		return false
	}
}

func (b *Batch) loop() {
	defer close(b.done)

	interval := b.flushInterval()
	timer := time.NewTimer(interval)
	defer timer.Stop()

	// 守望定时器：只用来发现「设置页把攒批间隔改小了」。
	// 没有它的话，把间隔从 60s 改回 200ms 得等那 60s 走完才生效（实测到的 bug）。
	watch := time.NewTicker(time.Second)
	defer watch.Stop()

	batch := make([]Entry, 0, 256)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.persist(batch)
		batch = batch[:0]
	}
	// resetTimer 按当前配置重置定时器
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		interval = b.flushInterval()
		timer.Reset(interval)
	}

	for {
		select {
		case e, ok := <-b.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= b.flushBatch() {
				flush()
				resetTimer()
			}
		case <-timer.C:
			flush()
			interval = b.flushInterval()
			timer.Reset(interval)
		case <-watch.C:
			// 间隔被改小了（比如 60s -> 200ms），立即刷一批并重置，
			// 别让已经排在队列里的日志继续按旧间隔干等。
			if b.flushInterval() < interval {
				flush()
				resetTimer()
			}
		}
	}
}

// insertChunk 单条 INSERT 里放多少行（仅 GORM 回退路径用）。
//
// PostgreSQL 的协议限制：一条语句最多 65535 个绑定参数。RequestLog 有 ~30 列，
// 所以每条 INSERT 的行数必须 < 65535/30 ≈ 2184，取 500 留足余量。
// COPY 走的是流式二进制协议，没有这个限制。
const insertChunk = 500

// persistViaGORM COPY 不可用时的回退路径（比如列清单校验不通过）。慢，但正确。
func (b *Batch) persistViaGORM(ctx context.Context, logs []store.RequestLog, gwKeys, chKeys map[uint]struct{}, now time.Time) error {
	return b.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL synchronous_commit = off").Error; err != nil {
			return err
		}
		if len(logs) > 0 {
			if err := tx.CreateInBatches(logs, insertChunk).Error; err != nil {
				return err
			}
		}
		if len(gwKeys) > 0 {
			if err := tx.Model(&store.APIKey{}).Where("id IN ?", ids(gwKeys)).
				Update("last_used_at", now).Error; err != nil {
				return err
			}
		}
		if len(chKeys) > 0 {
			if err := tx.Model(&store.ChannelKey{}).Where("id IN ?", ids(chKeys)).
				Update("last_used_at", now).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// persist 一批日志：单事务 COPY 插入 + 合并 last_used_at UPDATE。
func (b *Batch) persist(batch []Entry) {
	start := time.Now()
	var errMsg string

	logs := make([]store.RequestLog, 0, len(batch))
	gwKeys := make(map[uint]struct{}, len(batch))
	chKeys := make(map[uint]struct{}, len(batch))
	for i := range batch {
		if batch[i].Log != nil {
			logs = append(logs, *batch[i].Log)
		}
		if id := batch[i].TouchGatewayKeyID; id > 0 {
			gwKeys[id] = struct{}{}
		}
		if id := batch[i].TouchChannelKeyID; id > 0 {
			chKeys[id] = struct{}{}
		}
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var err error
	if b.useCopy.Load() {
		err = copyLogs(ctx, b.db, logs, ids(gwKeys), ids(chKeys), now)
	} else {
		err = b.persistViaGORM(ctx, logs, gwKeys, chKeys, now)
	}
	if err != nil {
		errMsg = "落库失败: " + err.Error()
		log.Printf("[sink] %s（本批 %d 条）", errMsg, len(logs))
	} else {
		b.persisted.Add(uint64(len(logs)))
	}
	b.batches.Add(1)

	b.mu.Lock()
	b.lastFlushAt = time.Now()
	b.lastFlushMs = time.Since(start).Milliseconds()
	b.lastBatchLen = len(batch)
	b.lastErr = errMsg
	b.mu.Unlock()
}

func (b *Batch) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s := Stats{
		Enqueued:     b.enqueued.Load(),
		Dropped:      b.dropped.Load(),
		Persisted:    b.persisted.Load(),
		Batches:      b.batches.Load(),
		QueueLen:     len(b.ch),
		QueueCap:     cap(b.ch),
		UsingCopy:    b.useCopy.Load(),
		Active:       true,
		LastFlushMs:  b.lastFlushMs,
		LastBatchLen: b.lastBatchLen,
		LastError:    b.lastErr,
	}
	if !b.lastFlushAt.IsZero() {
		s.LastFlushAt = b.lastFlushAt.Format(time.RFC3339)
	}
	return s
}

// Close 停止接收，等后台把剩余队列刷完（受 ctx 超时约束）。
func (b *Batch) Close(ctx context.Context) error {
	b.once.Do(func() {
		b.closed.Store(true)
		close(b.ch)
	})
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Batch) flushInterval() time.Duration {
	return store.GetSettingDuration(store.KeyLogFlushIntervalMs, time.Millisecond, 200*time.Millisecond)
}

func (b *Batch) flushBatch() int {
	n := store.GetSettingInt(store.KeyLogFlushBatch, 200)
	if n < 1 {
		n = 1
	}
	return n
}

func ids(m map[uint]struct{}) []uint {
	out := make([]uint, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}

// Discard 不落库的实现，给 gateway 角色用。
//
// 注意这是**临时形态**：拆角色的第一步先让 gateway 不直连 PG 写日志，
// 但日志确实被丢掉了。下一步会换成 Redis Streams 实现（XADD 进队列，worker 消费），
// 那时 gateway 既不写 PG 也不丢日志。留一个显式的 Discard 而不是 nil，
// 是为了让「日志去哪了」这件事在代码里有名字、并且能在 Console 上看见。
type Discard struct{ dropped atomic.Uint64 }

func (d *Discard) Submit(Entry) bool {
	d.dropped.Add(1)
	return false
}

func (d *Discard) Stats() Stats {
	n := d.dropped.Load()
	// Active=false：前端据此不展示队列/COPY 之类的指标，那些在这里没有意义
	return Stats{Enqueued: n, Dropped: n, Active: false}
}

func (d *Discard) Close(context.Context) error { return nil }
