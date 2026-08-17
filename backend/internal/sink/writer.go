package sink

// Writer：把一批 Entry 落进 PostgreSQL。
//
// 抽出来是因为有两个完全不同的调用方需要同一段落库逻辑：
//
//	Batch    进程内攒批（all 角色，也是无 Redis 时的路径）
//	Consumer 从 Redis Stream 读出来的批（worker 角色）
//
// 两边都要 COPY 快路径、都要 last_used_at 合并、都要同样的列漂移保护。
// 复制一份的话，将来给日志加字段就会漏掉一边——那种 bug 是静默丢数据。

import (
	"context"
	"log"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

// Writer 落库执行器。它本身无状态、无缓冲，只负责「把手上这批写进去」。
type Writer struct {
	db *gorm.DB
	// useCopy 是否走 COPY 快路径。启动时校验列清单，不一致就退化回 GORM
	// （慢 5 倍但正确），而不是默默丢字段。
	useCopy bool
}

// NewWriter 建立落库执行器并做一次列清单校验。
func NewWriter(db *gorm.DB) *Writer {
	w := &Writer{db: db}
	if db != nil {
		if err := VerifyLogColumns(db); err != nil {
			log.Printf("[sink] COPY 不可用，退化为逐行 INSERT（吞吐约降为 1/5）: %v", err)
		} else {
			w.useCopy = true
		}
	}
	return w
}

// UsingCopy 是否走 COPY 快路径（要暴露到 WebUI，退化了必须能看见）。
func (w *Writer) UsingCopy() bool { return w != nil && w.useCopy }

// Result 一次落库的结果，供调用方更新自己的统计。
type Result struct {
	Logs     int   // 实际写入的日志行数
	Duration int64 // 毫秒
	Err      error
}

// Write 落一批。返回的 error 决定调用方的行为：
//
//	Batch    失败就只能记账（数据已经从队列里出来了，丢了）
//	Consumer 失败**绝不能 XACK**，让消息留在 pending 里由下一轮重试
//
// 这个区别是 Redis Streams 相对进程内队列的全部价值所在。
func (w *Writer) Write(ctx context.Context, batch []Entry) Result {
	start := time.Now()

	logs := make([]store.RequestLog, 0, len(batch))
	gwKeys := make(map[uint]struct{}, len(batch))
	chKeys := make(map[uint]struct{}, len(batch))
	for i := range batch {
		if batch[i].Log != nil {
			logs = append(logs, *batch[i].Log)
		}
		// last_used_at 是 last-write-wins 的，天生可合并：
		// 一批里同一把 key 出现 100 次也只写一条 UPDATE
		if id := batch[i].TouchGatewayKeyID; id > 0 {
			gwKeys[id] = struct{}{}
		}
		if id := batch[i].TouchChannelKeyID; id > 0 {
			chKeys[id] = struct{}{}
		}
	}

	now := time.Now()
	var err error
	if w.useCopy {
		err = copyLogs(ctx, w.db, logs, ids(gwKeys), ids(chKeys), now)
	} else {
		err = w.writeViaGORM(ctx, logs, gwKeys, chKeys, now)
	}

	res := Result{Duration: time.Since(start).Milliseconds(), Err: err}
	if err == nil {
		res.Logs = len(logs)
	}
	return res
}

// insertChunk 单条 INSERT 里放多少行（仅 GORM 回退路径用）。
//
// PostgreSQL 的协议限制：一条语句最多 65535 个绑定参数。RequestLog 有 ~26 列，
// 所以每条 INSERT 的行数必须 < 65535/26 ≈ 2500，取 500 留足余量。
// COPY 走的是流式二进制协议，没有这个限制。
const insertChunk = 500

// writeViaGORM COPY 不可用时的回退路径（比如列清单校验不通过）。慢，但正确。
func (w *Writer) writeViaGORM(ctx context.Context, logs []store.RequestLog, gwKeys, chKeys map[uint]struct{}, now time.Time) error {
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 只对日志事务关闭同步提交：日志是可容忍丢最后几毫秒的观测数据。
		// 配置写入仍然是安全的 synchronous_commit=on —— 这种按事务区分的能力
		// 是选 PG 而不是 SQLite 的一个净胜项（SQLite 的 synchronous 是全库开关）。
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

func ids(m map[uint]struct{}) []uint {
	out := make([]uint, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}
