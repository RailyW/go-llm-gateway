package sink

// COPY 落库：走 pgx 的二进制 COPY 协议，绕开 GORM 的逐行反射与 65535 绑定参数上限。
//
// 为什么要它：丢弃率的根因是**消费端太慢**，不是缓冲太小。加大队列只是把问题藏起来，
// 抬高落库天花板才是真解。实测同一张表、同样单事务 + synchronous_commit=off：
//
//	  200 条   GORM  19,869 条/s  |  COPY  102,794 条/s   5.2x
//	 1000 条   GORM  30,505 条/s  |  COPY  188,090 条/s   6.2x
//	 5000 条   GORM  52,208 条/s  |  COPY  203,303 条/s   3.9x
//
// 天花板抬到 20 万条/s 后，到达率远低于消费率，队列自然不再积压。

import (
	"context"
	"fmt"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/gorm"
)

// logColumns request_logs 的列顺序，必须与 logRow 一一对应。
//
// 这里是**手写的列清单**，而 AutoMigrate 是按 store.RequestLog 结构体建表的，
// 两边有漂移风险（加了字段忘了加进来就会静默丢数据）。所以启动时会用
// VerifyLogColumns 跟数据库实际列做一次校验，不一致直接退化回 GORM 并告警。
var logColumns = []string{
	"id", "protocol", "endpoint", "user_id", "username", "group_id", "group_name",
	"api_key_id", "api_key_name", "model_name", "channel_id", "channel_name", "upstream_model",
	"channel_key_id", "channel_key_name", "stream", "status_code", "prompt_tokens",
	"completion_tokens", "total_tokens", "duration_ms", "client_ip", "error_message",
	"archive_path", "created_at", "usage",
}

// logRow 把一条日志摊成 COPY 的一行值，顺序与 logColumns 严格一致。
func logRow(l *store.RequestLog) []any {
	// jsonb 列空值必须给 nil 而不是空串（空串不是合法 JSON）
	var usage any
	if len(l.Usage) > 0 {
		usage = string(l.Usage)
	}
	return []any{
		l.ID, l.Protocol, l.Endpoint, int64(l.UserID), l.Username, int64(l.GroupID), l.GroupName,
		int64(l.APIKeyID), l.APIKeyName, l.ModelName, int64(l.ChannelID), l.ChannelName, l.UpstreamModel,
		int64(l.ChannelKeyID), l.ChannelKeyName, l.Stream, int64(l.StatusCode), int64(l.PromptTokens),
		int64(l.CompletionTokens), int64(l.TotalTokens), l.DurationMs, l.ClientIP, l.ErrorMessage,
		l.ArchivePath, l.CreatedAt, usage,
	}
}

// copyLogs 用 COPY 插入一批日志，并在同一事务里合并写 last_used_at。
//
// 整个批次一个事务：COPY 一次网络往返送完所有行，两条 UPDATE 合并 key，
// 然后一次提交（且 synchronous_commit=off，不等 WAL 落盘）。
func copyLogs(ctx context.Context, db *gorm.DB, logs []store.RequestLog, gwKeys, chKeys []uint, now time.Time) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	return conn.Raw(func(driverConn any) error {
		pc, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("底层连接不是 pgx（%T），无法使用 COPY", driverConn)
		}
		c := pc.Conn()

		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck // 提交成功后 Rollback 是 no-op

		// 日志是观测数据：丢最近几个事务可接受，换来不等 WAL 落盘确认。
		// SET LOCAL 随事务结束失效，配置类写入仍是默认的 synchronous_commit=on。
		if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
			return err
		}
		if len(logs) > 0 {
			src := pgx.CopyFromSlice(len(logs), func(i int) ([]any, error) { return logRow(&logs[i]), nil })
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"request_logs"}, logColumns, src); err != nil {
				return err
			}
		}
		// last_used_at 是 last-write-wins，天生可合并：每批一条 UPDATE。
		if len(gwKeys) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE api_keys SET last_used_at = $1 WHERE id = ANY($2)`, now, gwKeys); err != nil {
				return err
			}
		}
		if len(chKeys) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE channel_keys SET last_used_at = $1 WHERE id = ANY($2)`, now, chKeys); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
}

// VerifyLogColumns 校验手写的 logColumns 与数据库里 request_logs 的实际列完全一致。
//
// 目的是防止「给 RequestLog 加了字段但忘了更新 logColumns」导致新字段静默不落库。
// 不一致时返回错误，调用方应退化到 GORM 路径（慢但正确）。
func VerifyLogColumns(db *gorm.DB) error {
	var actual []string
	err := db.Raw(`SELECT column_name FROM information_schema.columns
	               WHERE table_name = 'request_logs' AND table_schema = current_schema()`).
		Scan(&actual).Error
	if err != nil {
		return err
	}
	if len(actual) == 0 {
		return fmt.Errorf("找不到 request_logs 表")
	}
	want := make(map[string]bool, len(logColumns))
	for _, c := range logColumns {
		want[c] = true
	}
	have := make(map[string]bool, len(actual))
	for _, c := range actual {
		have[c] = true
	}
	for _, c := range actual {
		if !want[c] {
			return fmt.Errorf("request_logs 多出列 %q，未包含在 COPY 列清单里（新增字段后请同步 logColumns）", c)
		}
	}
	for _, c := range logColumns {
		if !have[c] {
			return fmt.Errorf("COPY 列清单里的 %q 在表中不存在", c)
		}
	}
	return nil
}
