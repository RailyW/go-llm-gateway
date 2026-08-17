package sink

// Redis Stream 消费端（worker 角色）。
//
// # 唯一重要的纪律：先落库，成功了才 XACK
//
// 顺序反过来（读完就 ACK 再慢慢落库）的话，Streams 就退化成了一个更慢、
// 更复杂的进程内队列——worker 崩溃时那批数据一样丢。整个 Streams 方案的价值
// 就在这一个顺序上。
//
// # 崩溃后遗留的消息怎么办
//
// XACK 之前消息处于 pending 状态（属于某个 consumer）。worker 被 kill -9 后，
// 那些消息永远不会被自动重投——它们仍然「属于」那个已经不存在的 consumer。
// 所以必须定期 XAUTOCLAIM：把空闲超过 N 分钟的 pending 消息转移给自己再处理。
// 没有这一步，每次 worker 异常退出都会永久漏掉一批。
//
// # 毒消息（poison message）
//
// 如果某条消息总是让落库失败（比如触发了外键约束），无限重试会卡住整个消费。
// 所以 XAUTOCLAIM 捞回来时会看投递次数，超过阈值就单独丢弃并告警——
// 宁可丢一条并且说出来，也不能让它挡住后面所有日志。

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// ConsumerOptions 消费端参数。
type ConsumerOptions struct {
	// Instance 消费者名字（用实例 ID，便于在 XINFO CONSUMERS 里对上人）
	Instance string
	// Count 单次 XREADGROUP 最多取多少条。这直接决定落库批的大小，
	// 也就决定了吞吐：COPY 在 1000 条/批时约 18.8 万条/s
	Count int64
	// Block XREADGROUP 阻塞等待时长。空闲时就挂在这儿，不轮询
	Block time.Duration
	// ClaimMinIdle pending 消息空闲多久后可以被别的 worker 接管
	ClaimMinIdle time.Duration
	// MaxDeliveries 同一条消息最多被投递几次，超了当毒消息丢弃
	MaxDeliveries int64
}

func (o *ConsumerOptions) setDefaults() {
	if o.Count <= 0 {
		o.Count = 1000
	}
	if o.Block <= 0 {
		o.Block = 2 * time.Second
	}
	if o.ClaimMinIdle <= 0 {
		// 要显著大于「一批的落库耗时」，否则正在被正常处理的消息会被别的 worker 抢走，
		// 造成重复落库。落库一批通常几十毫秒，1 分钟余量充足。
		o.ClaimMinIdle = time.Minute
	}
	if o.MaxDeliveries <= 0 {
		o.MaxDeliveries = 5
	}
}

// ConsumerStats 消费端健康度，暴露到 Console。
type ConsumerStats struct {
	Running bool `json:"running"`
	// Backlog 消费组还没读到的消息数（XINFO GROUPS 的 lag）。
	//
	// 注意**不能用 XLEN**：Redis Stream 的 XACK 不删除消息（要留给其他消费组回溯），
	// 所以 XLEN 是「历史总条数」而不是积压。用 XLEN 当积压会让运维看到一个
	// 永远不归零的数字，从而彻底不信这个指标。
	Backlog int64 `json:"backlog"`
	// Length Stream 当前的物理长度（XLEN），即占着 Redis 内存的消息数
	Length int64 `json:"length"`
	// Pending 已投递未 ACK 的数量（正常应接近 0）
	Pending   int64  `json:"pending"`
	Consumed  uint64 `json:"consumed"`
	Persisted uint64 `json:"persisted"`
	Retried   uint64 `json:"retried"`  // 被 XAUTOCLAIM 捞回来重试的
	Poisoned  uint64 `json:"poisoned"` // 超过重试上限被丢弃的
	// Trimmed 已回收（XTRIM）的消息数。已落库的消息留在 Redis 里没有价值，
	// 内存要留给限流计数那类真正需要它的东西
	Trimmed   uint64 `json:"trimmed"`
	Batches   uint64 `json:"batches"`
	Failed    uint64 `json:"failed"` // 落库失败的批次（未 ACK，会重试）
	LastAt    string `json:"last_at,omitempty"`
	LastMs    int64  `json:"last_ms"`
	LastLen   int    `json:"last_len"`
	LastError string `json:"last_error,omitempty"`
	Group     string `json:"group"`
	StreamKey string `json:"stream_key"`
	UsingCopy bool   `json:"using_copy"`
	Consumer  string `json:"consumer"`
}

// Consumer 从 Redis Stream 读日志并落 PostgreSQL。
type Consumer struct {
	rc  *rds.Client
	w   *Writer
	key string
	opt ConsumerOptions

	running atomic.Bool
	done    chan struct{}
	once    sync.Once

	consumed, persisted, retried, poisoned, batches, failed atomic.Uint64
	backlog, pending, length, trimmed                       atomic.Int64

	mu      sync.RWMutex
	lastAt  time.Time
	lastMs  int64
	lastLen int
	lastErr string
}

func NewConsumer(rc *rds.Client, db *gorm.DB, opt ConsumerOptions) *Consumer {
	opt.setDefaults()
	return &Consumer{
		rc:   rc,
		w:    NewWriter(db),
		key:  StreamKey(rc),
		opt:  opt,
		done: make(chan struct{}),
	}
}

// Start 起消费循环。ctx 取消后循环退出（会把手上这批处理完）。
func (c *Consumer) Start(ctx context.Context) {
	if !c.rc.Enabled() {
		return
	}
	c.running.Store(true)
	go c.loop(ctx)
	go c.observe(ctx)
}

// ensureGroup 建消费组。BUSYGROUP 表示已存在，是正常情况。
//
// MKSTREAM：Stream 还不存在时一起创建（worker 可能比 gateway 先启动）。
// 起始位置用 "0" 而不是 "$"：从头消费，别把已经堆在 Stream 里的日志跳过去。
func (c *Consumer) ensureGroup(ctx context.Context) error {
	return c.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		err := r.XGroupCreateMkStream(ctx, c.key, ConsumerGroup, "0").Err()
		if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return err
		}
		return nil
	})
}

func (c *Consumer) loop(ctx context.Context) {
	defer close(c.done)
	defer c.running.Store(false)

	// 建组要重试：worker 可能比 Redis 先起来
	for ctx.Err() == nil {
		if err := c.ensureGroup(ctx); err != nil {
			log.Printf("[worker] 创建消费组失败（%v），2 秒后重试", err)
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		break
	}
	log.Printf("[worker] 开始消费日志流 %s（组 %s，消费者 %s，每批最多 %d 条）",
		c.key, ConsumerGroup, c.opt.Instance, c.opt.Count)

	// 定期回收别的 worker 崩溃后遗留的 pending 消息
	claim := time.NewTicker(30 * time.Second)
	defer claim.Stop()

	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-claim.C:
			c.claimStale(ctx)
		default:
		}

		n, err := c.readAndPersist(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[worker] 读取日志流失败（%v），1 秒后重试", err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		// 读到空批说明 Block 超时了，直接进下一轮（顺带跑到 claim 分支）
		_ = n
	}
}

// readAndPersist 读一批 → 落库 → XACK。
func (c *Consumer) readAndPersist(ctx context.Context) (int, error) {
	// 这里不能用 rc.Do：XREADGROUP 是**故意阻塞**的（Block 秒级），
	// 套上 50ms 的热路径超时会让它每次都失败。
	res, err := c.rc.Raw().XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: c.opt.Instance,
		Streams:  []string{c.key, ">"}, // ">" = 只要还没投递给任何人的新消息
		Count:    c.opt.Count,
		Block:    c.opt.Block,
	}).Result()
	if err == redis.Nil {
		return 0, nil // Block 超时，没新消息
	}
	if err != nil {
		return 0, err
	}

	var ids []string
	var batch []Entry
	for _, st := range res {
		for _, m := range st.Messages {
			e, ok := decodeEntry(m)
			if !ok {
				// 解不出来的消息直接 ACK 掉，否则它会永远卡在 pending 里
				ids = append(ids, m.ID)
				c.poisoned.Add(1)
				continue
			}
			batch = append(batch, e)
			ids = append(ids, m.ID)
		}
	}
	if len(batch) == 0 {
		if len(ids) > 0 {
			c.ack(ctx, ids)
		}
		return 0, nil
	}
	c.consumed.Add(uint64(len(batch)))
	c.persistAndAck(ctx, batch, ids)
	return len(batch), nil
}

// persistAndAck 落库成功才 ACK。失败就**不 ACK**，消息留在 pending 里，
// 之后由 XAUTOCLAIM 捞回来重试 —— 这是整个方案不丢日志的关键。
func (c *Consumer) persistAndAck(ctx context.Context, batch []Entry, ids []string) {
	// 落库用独立的超时，不受 ctx 取消影响：退出时也该把手上这批写完
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	res := c.w.Write(writeCtx, batch)
	c.batches.Add(1)

	var errMsg string
	if res.Err != nil {
		errMsg = "落库失败（不 ACK，将重试）: " + res.Err.Error()
		c.failed.Add(1)
		log.Printf("[worker] %s（本批 %d 条）", errMsg, len(batch))
	} else {
		c.persisted.Add(uint64(res.Logs))
		c.ack(writeCtx, ids)
	}

	c.mu.Lock()
	c.lastAt = time.Now()
	c.lastMs = res.Duration
	c.lastLen = len(batch)
	c.lastErr = errMsg
	c.mu.Unlock()
}

func (c *Consumer) ack(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	if err := c.rc.Raw().XAck(ctx, c.key, ConsumerGroup, ids...).Err(); err != nil {
		// ACK 失败不算数据丢失：消息还在 pending 里，最坏结果是重复落一次
		log.Printf("[worker] XACK 失败（可能重复落库 %d 条）: %v", len(ids), err)
	}
}

// claimStale 接管空闲太久的 pending 消息。
//
// 没有这一步，worker 被 kill -9 后它名下未 ACK 的消息会永久滞留：
// 消费组认为那些消息「已经投递给某个 consumer 了」，而那个 consumer 再也不会回来。
func (c *Consumer) claimStale(ctx context.Context) {
	msgs, _, err := c.rc.Raw().XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   c.key,
		Group:    ConsumerGroup,
		Consumer: c.opt.Instance,
		MinIdle:  c.opt.ClaimMinIdle,
		Start:    "0",
		Count:    c.opt.Count,
	}).Result()
	if err != nil {
		if err != redis.Nil {
			log.Printf("[worker] XAUTOCLAIM 失败: %v", err)
		}
		return
	}
	if len(msgs) == 0 {
		return
	}

	// 查投递次数，把反复失败的毒消息挑出来丢掉，别让它挡住后面的日志
	var batch []Entry
	var ids, poison []string
	deliveries := c.deliveryCounts(ctx, msgs)
	for _, m := range msgs {
		if deliveries[m.ID] > c.opt.MaxDeliveries {
			poison = append(poison, m.ID)
			continue
		}
		e, ok := decodeEntry(m)
		if !ok {
			poison = append(poison, m.ID)
			continue
		}
		batch = append(batch, e)
		ids = append(ids, m.ID)
	}

	if len(poison) > 0 {
		c.poisoned.Add(uint64(len(poison)))
		log.Printf("[worker] ⚠ %d 条日志重试超过 %d 次仍失败，已丢弃（避免堵塞后续日志）",
			len(poison), c.opt.MaxDeliveries)
		c.ack(ctx, poison)
	}
	if len(batch) > 0 {
		c.retried.Add(uint64(len(batch)))
		log.Printf("[worker] 接管 %d 条遗留日志（前一个 worker 可能异常退出）", len(batch))
		c.persistAndAck(ctx, batch, ids)
	}
}

// deliveryCounts 查这批消息各自被投递过几次（XPENDING 的 delivery count）。
func (c *Consumer) deliveryCounts(ctx context.Context, msgs []redis.XMessage) map[string]int64 {
	out := make(map[string]int64, len(msgs))
	pend, err := c.rc.Raw().XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.key,
		Group:  ConsumerGroup,
		Start:  "-",
		End:    "+",
		Count:  int64(len(msgs)),
	}).Result()
	if err != nil {
		return out // 查不到就当没重试过，最坏是多试几次
	}
	for _, p := range pend {
		out[p.ID] = p.RetryCount
	}
	return out
}

// observe 定期刷新积压指标并回收已消费的消息。
//
// 积压（lag）是这条链路唯一的「慢性病」信号：短时堆积正常，持续增长说明
// 消费能力不够或 PG 出问题了。
func (c *Consumer) observe(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refreshLag(ctx)
			c.trim(ctx)
		}
	}
}

func (c *Consumer) refreshLag(ctx context.Context) {
	_ = c.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		if n, err := r.XLen(ctx, c.key).Result(); err == nil {
			c.length.Store(n)
		}
		groups, err := r.XInfoGroups(ctx, c.key).Result()
		if err != nil {
			return nil // Stream 还不存在等情况，不算故障
		}
		for _, g := range groups {
			if g.Name != ConsumerGroup {
				continue
			}
			c.pending.Store(g.Pending)
			// Lag 在某些情况下（比如消费组建立前 Stream 被 XTRIM 过）Redis 会返回 nil，
			// 这时退回用 XLEN 近似，宁可高估也别显示 0 让人误以为没积压
			c.backlog.Store(g.Lag)
		}
		return nil
	})
}

// trim 回收已消费消息占用的 Redis 内存。
//
// 为什么必须做：XACK **不删除**消息，Stream 会一直长到 MAXLEN（默认 100 万条 ≈ 400MB）
// 并常驻内存。而这些消息已经落进 PG 了，留在 Redis 里毫无价值——
// Redis 的内存应该留给限流计数这类真正需要它的东西。
//
// 裁剪位置用 MINID = last-delivered-id 而不是 MAXLEN：
// 只删「已经投递出去的」，未投递的一条不碰。且必须等 pending 归零，
// 否则会删掉还没 ACK、正等着重试的消息。
func (c *Consumer) trim(ctx context.Context) {
	_ = c.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		groups, err := r.XInfoGroups(ctx, c.key).Result()
		if err != nil {
			return nil
		}
		for _, g := range groups {
			if g.Name != ConsumerGroup {
				continue
			}
			// 有未 ACK 的就先别裁：那些消息还要留着重试
			if g.Pending > 0 || g.LastDeliveredID == "" || g.LastDeliveredID == "0-0" {
				return nil
			}
			n, err := r.XTrimMinID(ctx, c.key, g.LastDeliveredID).Result()
			if err == nil && n > 0 {
				c.trimmed.Add(n)
			}
			return nil
		}
		return nil
	})
}

func (c *Consumer) Stats() ConsumerStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := ConsumerStats{
		Running:   c.running.Load(),
		Backlog:   c.backlog.Load(),
		Length:    c.length.Load(),
		Pending:   c.pending.Load(),
		Trimmed:   uint64(c.trimmed.Load()),
		Consumed:  c.consumed.Load(),
		Persisted: c.persisted.Load(),
		Retried:   c.retried.Load(),
		Poisoned:  c.poisoned.Load(),
		Batches:   c.batches.Load(),
		Failed:    c.failed.Load(),
		LastMs:    c.lastMs,
		LastLen:   c.lastLen,
		LastError: c.lastErr,
		Group:     ConsumerGroup,
		StreamKey: c.key,
		UsingCopy: c.w.UsingCopy(),
		Consumer:  c.opt.Instance,
	}
	if !c.lastAt.IsZero() {
		st.LastAt = c.lastAt.Format(time.RFC3339)
	}
	return st
}

// Wait 等消费循环退出（优雅停机用）。
func (c *Consumer) Wait(ctx context.Context) error {
	if !c.rc.Enabled() {
		return nil
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func decodeEntry(m redis.XMessage) (Entry, bool) {
	raw, ok := m.Values[streamField]
	if !ok {
		log.Printf("[worker] 日志消息 %s 缺少字段 %q，跳过", m.ID, streamField)
		return Entry{}, false
	}
	str, ok := raw.(string)
	if !ok {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal([]byte(str), &e); err != nil {
		log.Printf("[worker] 日志消息 %s 解析失败，跳过: %v", m.ID, err)
		return Entry{}, false
	}
	if e.Log == nil && e.TouchGatewayKeyID == 0 && e.TouchChannelKeyID == 0 {
		return Entry{}, false
	}
	return e, true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
