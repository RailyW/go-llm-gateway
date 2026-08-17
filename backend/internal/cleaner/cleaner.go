// Package cleaner 后台清理服务：按配置的保留天数删除归档原文与历史日志。
package cleaner

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

type Status struct {
	LastRunAt      *time.Time `json:"last_run_at"`
	NextRunAt      *time.Time `json:"next_run_at"`
	RemovedDirs    int        `json:"last_removed_archive_dirs"`
	RemovedLogRows int64      `json:"last_removed_log_rows"`
	LastError      string     `json:"last_error"`
	Running        bool       `json:"running"`
	// Skipped 因为没抢到选主锁而跳过的次数（多实例下只有一个实例真正执行）
	Skipped uint64 `json:"skipped_not_leader"`
	Leader  bool   `json:"leader"`
}

// Leader 选主接口（由 coord.Elector 实现）。
//
// 为什么清理需要选主：它会 DELETE 历史日志、RemoveAll 归档目录。
// N 个实例同时跑不只是浪费，而是并发删除 + 各自算保留窗口，行为不可预测。
type Leader interface {
	IsLeader() bool
}

type Cleaner struct {
	db       *gorm.DB
	archiver *archive.Archiver
	// leader 为 nil 时表示无需选主（单实例）
	leader Leader

	mu      sync.RWMutex
	status  Status
	skipped atomic.Uint64
	kick    chan struct{}
}

func New(db *gorm.DB, archiver *archive.Archiver) *Cleaner {
	return &Cleaner{db: db, archiver: archiver, kick: make(chan struct{}, 1)}
}

// WithLeader 挂上选主守卫。没抢到锁就不执行。
func (c *Cleaner) WithLeader(l Leader) *Cleaner {
	c.leader = l
	return c
}

// Start 起后台 goroutine，启动时先跑一次，之后按 cleanup_interval_minutes 周期跑。
func (c *Cleaner) Start(ctx context.Context) {
	go func() {
		c.RunOnce()
		for {
			interval := store.GetSettingDuration(store.KeyCleanupIntervalMin, time.Minute, time.Hour)
			next := time.Now().Add(interval)
			c.mu.Lock()
			c.status.NextRunAt = &next
			c.mu.Unlock()

			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				log.Println("[cleaner] 退出")
				return
			case <-timer.C:
			case <-c.kick:
				timer.Stop()
			}
			c.RunOnce()
		}
	}()
}

// Trigger 手动触发一次（WebUI 用）。
func (c *Cleaner) Trigger() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Cleaner) RunOnce() {
	// 选主守卫：这里是 fail-closed——抢不到锁（包括 Redis 不可用）就不删。
	// 数据多留几天没有坏处，多个实例并发删则不可控。
	if c.leader != nil && !c.leader.IsLeader() {
		c.skipped.Add(1)
		return
	}

	c.mu.Lock()
	c.status.Running = true
	c.mu.Unlock()

	now := time.Now()
	var errMsg string
	archiveDays := store.GetSettingInt(store.KeyArchiveRetentionDays, 7)
	logDays := store.GetSettingInt(store.KeyLogRetentionDays, 30)

	removed, err := c.archiver.Cleanup(archiveDays)
	if err != nil {
		errMsg = err.Error()
		log.Printf("[cleaner] 清理归档失败: %v", err)
	} else if len(removed) > 0 {
		log.Printf("[cleaner] 已删除 %d 个过期归档目录(保留 %d 天): %v", len(removed), archiveDays, removed)
	}

	var rows int64
	if logDays > 0 {
		cutoff := now.AddDate(0, 0, -logDays)
		res := c.db.Where("created_at < ?", cutoff).Delete(&store.RequestLog{})
		if res.Error != nil {
			errMsg = res.Error.Error()
			log.Printf("[cleaner] 清理日志失败: %v", res.Error)
		} else {
			rows = res.RowsAffected
			if rows > 0 {
				log.Printf("[cleaner] 已删除 %d 条过期日志(保留 %d 天)", rows, logDays)
			}
		}
	}

	c.mu.Lock()
	c.status.Running = false
	c.status.LastRunAt = &now
	c.status.RemovedDirs = len(removed)
	c.status.RemovedLogRows = rows
	c.status.LastError = errMsg
	c.mu.Unlock()
}

func (c *Cleaner) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.status
	s.Skipped = c.skipped.Load()
	s.Leader = c.leader == nil || c.leader.IsLeader()
	return s
}
