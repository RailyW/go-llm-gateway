// Package sink 把「日志落库 + 原文归档」从请求热路径上摘下来。
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
// Sink 是接口：将来要多实例部署、或要把日志交给独立消费者写 ClickHouse，
// 换一个 Redis Stream 实现即可，请求侧代码不用动。
package sink

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/archive"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

// Entry 一次请求产生的全部待持久化内容。
type Entry struct {
	Log   *store.RequestLog
	Files []archive.File // 待写的归档文件（流式响应文件已在请求协程增量写完，不在这里）

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
	db       *gorm.DB
	archiver *archive.Archiver

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

func NewBatch(db *gorm.DB, archiver *archive.Archiver, queueSize int) *Batch {
	if queueSize <= 0 {
		queueSize = 4096
	}
	return &Batch{
		db:       db,
		archiver: archiver,
		ch:       make(chan Entry, queueSize),
		done:     make(chan struct{}),
	}
}

func (b *Batch) Start() {
	go b.loop()
}

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

	batch := make([]Entry, 0, 256)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.persist(batch)
		batch = batch[:0]
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
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(b.flushInterval())
			}
		case <-timer.C:
			flush()
			if n := b.flushInterval(); n != interval {
				interval = n // 设置页改了刷新间隔，下一轮生效
			}
			timer.Reset(interval)
		}
	}
}

// insertChunk 单条 INSERT 里放多少行。
//
// PostgreSQL 的协议限制：一条语句最多 65535 个绑定参数。RequestLog 有 ~30 列，
// 所以每条 INSERT 的行数必须 < 65535/30 ≈ 2184。这跟用户可调的「攒批条数」
// 是两件事——攒批决定事务多大，这里决定事务内切成几条语句，取 500 留足余量。
const insertChunk = 500

// persist 一批数据：先写归档文件，再在单事务里批量插入日志 + 合并 UPDATE。
//
// 顺序很重要：文件先落盘、日志行后入库，保证「日志里能看到的请求，原文一定已存在」。
func (b *Batch) persist(batch []Entry) {
	start := time.Now()
	var errMsg string

	files := make([]archive.File, 0, len(batch)*2)
	logs := make([]store.RequestLog, 0, len(batch))
	gwKeys := make(map[uint]struct{}, len(batch))
	chKeys := make(map[uint]struct{}, len(batch))
	for i := range batch {
		files = append(files, batch[i].Files...)
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

	if len(files) > 0 {
		if err := b.archiver.WriteFiles(files); err != nil {
			errMsg = "写归档失败: " + err.Error()
			log.Printf("[sink] %s", errMsg)
		}
	}

	now := time.Now()
	err := b.db.Transaction(func(tx *gorm.DB) error {
		// 只对这个事务关掉同步提交：日志是观测数据，丢最近几个事务可以接受，
		// 换来不必等 WAL 落盘确认。SET LOCAL 随事务结束自动失效，
		// 所以配置/用户这些写入仍然是默认的 synchronous_commit=on。
		// （sqlite 的 synchronous=NORMAL 是全库开关，做不到这种区分）
		if err := tx.Exec("SET LOCAL synchronous_commit = off").Error; err != nil {
			return err
		}
		if len(logs) > 0 {
			if err := tx.CreateInBatches(logs, insertChunk).Error; err != nil {
				return err
			}
		}
		// last_used_at 是 last-write-wins，合并成每批一条 UPDATE；
		// 时间戳取本批刷新时刻（误差 <= 刷新间隔，够用了）。
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
