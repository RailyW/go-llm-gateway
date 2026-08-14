// Package cleaner 后台清理服务：按配置的保留天数删除归档原文与历史日志。
package cleaner

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

type Status struct {
	LastRunAt      *time.Time `json:"last_run_at"`
	NextRunAt      *time.Time `json:"next_run_at"`
	RemovedDirs    int        `json:"last_removed_archive_dirs"`
	RemovedLogRows int64      `json:"last_removed_log_rows"`
	LastError      string     `json:"last_error"`
	Running        bool       `json:"running"`
}

type Cleaner struct {
	db       *gorm.DB
	archiver *relay.Archiver

	mu     sync.RWMutex
	status Status
	kick   chan struct{}
}

func New(db *gorm.DB, archiver *relay.Archiver) *Cleaner {
	return &Cleaner{db: db, archiver: archiver, kick: make(chan struct{}, 1)}
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
	return c.status
}
