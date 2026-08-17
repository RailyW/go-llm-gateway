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
	// Via 日志的去处："postgres"（直接落库）/ "redis-stream"（投给 worker）/ ""（丢弃）。
	// 多实例下这是回答「我的日志去哪了」最直接的一个字段。
	Via string `json:"via,omitempty"`
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
	w  *Writer

	ch     chan Entry
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once

	enqueued, dropped, persisted, batches atomic.Uint64

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
	b.w = NewWriter(b.db)
	go b.loop()
}

// UsingCopy 当前是否走 COPY 快路径（暴露到 WebUI，退化了要能看见）。
func (b *Batch) UsingCopy() bool { return b.w.UsingCopy() }

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
	collect(b.ch, b.persist)
}

// collect 攒批循环：从 ch 收 Entry，按「条数达标」或「间隔到了」触发 flush。
//
// 抽出来给两个生产端共用：Batch（flush = 落 PG）和 Stream（flush = XADD 一批）。
// 攒批的收益在两边是同一个道理——把 N 次往返压成 1 次。
func collect(ch <-chan Entry, flushFn func([]Entry)) {
	interval := currentFlushInterval()
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
		flushFn(batch)
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
		interval = currentFlushInterval()
		timer.Reset(interval)
	}

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= currentFlushBatch() {
				flush()
				resetTimer()
			}
		case <-timer.C:
			flush()
			interval = currentFlushInterval()
			timer.Reset(interval)
		case <-watch.C:
			// 间隔被改小了（比如 60s -> 200ms），立即刷一批并重置，
			// 别让已经排在队列里的日志继续按旧间隔干等。
			if currentFlushInterval() < interval {
				flush()
				resetTimer()
			}
		}
	}
}

// persist 一批日志：单事务 COPY 插入 + 合并 last_used_at UPDATE。
//
// 注意这里失败就是**真丢了**：Entry 已经从队列里取出来，没有别的副本。
// 这是进程内队列的固有性质，也正是 worker + Redis Streams 那条路存在的理由
// （那边失败可以不 ACK，让消息留在 pending 里重试）。
func (b *Batch) persist(batch []Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := b.w.Write(ctx, batch)
	var errMsg string
	if res.Err != nil {
		errMsg = "落库失败: " + res.Err.Error()
		log.Printf("[sink] %s（本批 %d 条已丢失）", errMsg, len(batch))
	} else {
		b.persisted.Add(uint64(res.Logs))
	}
	b.batches.Add(1)

	b.mu.Lock()
	b.lastFlushAt = time.Now()
	b.lastFlushMs = res.Duration
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
		UsingCopy:    b.w.UsingCopy(),
		Active:       true,
		Via:          "postgres",
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

// 攒批参数每次都从设置里现读：WebUI 改完立刻生效，不用重启。
func currentFlushInterval() time.Duration {
	return store.GetSettingDuration(store.KeyLogFlushIntervalMs, time.Millisecond, 200*time.Millisecond)
}

func currentFlushBatch() int {
	n := store.GetSettingInt(store.KeyLogFlushBatch, 200)
	if n < 1 {
		n = 1
	}
	return n
}

// Discard 不落库也不入队的实现。
//
// 现在只有一种情况会用到它：**配成 gateway 角色却没配 Redis**。
// 这是个配置错误（gateway 角色的意义就是横向扩展，而横向扩展必须有 Redis），
// 但不该因此拒绝启动——转发本身是好的，只是日志没地方去。
// 留一个有名字的 Discard 而不是 nil，是为了让「日志去哪了」这件事
// 在代码里有交代、并且能在 Console 上看见丢了多少。
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
