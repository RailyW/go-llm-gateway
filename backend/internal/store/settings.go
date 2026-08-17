package store

import (
	"strconv"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// 可在 WebUI 修改的配置项。
const (
	KeyArchiveRetentionDays  = "archive_retention_days"   // 原文归档保留天数
	KeyLogRetentionDays      = "log_retention_days"       // logs 表保留天数，0 = 永久
	KeyCleanupIntervalMin    = "cleanup_interval_minutes" // 清理服务运行间隔
	KeyRouteStrategy         = "route_strategy"           // 多绑定路由策略
	KeyUpstreamKeyStrategy   = "upstream_key_strategy"    // 归属下多把上游 key 的选择策略
	KeyDefaultGroupID        = "default_group_id"         // 新注册用户的默认归属
	KeyAllowRegister         = "allow_register"           // 是否允许注册
	KeyUpstreamTimeoutSecond = "upstream_timeout_seconds" // 上游超时（流式不受限）
	KeyLogFlushIntervalMs    = "log_flush_interval_ms"    // 异步落库：攒批时间窗
	KeyLogFlushBatch         = "log_flush_batch"          // 异步落库：单批最大条数
	KeyArchiveEnabled        = "archive_enabled"          // 是否归档请求/响应原文（目前已停用，见 ArchiveFeatureEnabled）
)

// ArchiveFeatureEnabled 原文归档功能的**总开关**，当前硬编码关闭。
//
// 为什么停用：归档写的是**本地磁盘**，而转发已经要多实例水平扩展了。
// 请求落在实例 3 上，归档文件就只在实例 3 的盘上；而日志页在 console 实例上，
// 根本读不到——只会给用户一个迷惑的「原文不存在」。
//
// 要真正恢复这个功能，得先把存储换成共享的（S3/MinIO 或共享卷），
// 那是独立一轮的事。代码（internal/archive）全部保留，只是不再被调用。
const ArchiveFeatureEnabled = false

var defaults = map[string]string{
	KeyArchiveRetentionDays:  "7",
	KeyLogRetentionDays:      "30",
	KeyCleanupIntervalMin:    "60",
	KeyRouteStrategy:         "random",
	KeyUpstreamKeyStrategy:   "random",
	KeyDefaultGroupID:        "1",
	KeyAllowRegister:         "true",
	KeyUpstreamTimeoutSecond: "300",
	KeyLogFlushIntervalMs:    "200",
	KeyLogFlushBatch:         "200",
	// 默认关：原文是增长最快的部分，只在排查问题时才需要，
	// 不该默认就把每个请求的完整 body 写上磁盘。
	KeyArchiveEnabled: "false",
}

// ArchiveEnabled 归档是否开启。总开关关闭时永远返回 false，
// 不管数据库里的设置是什么（旧库里可能已经被打开过）。
func ArchiveEnabled() bool {
	if !ArchiveFeatureEnabled {
		return false
	}
	return GetSettingBool(KeyArchiveEnabled, false)
}

var (
	cacheMu sync.RWMutex
	cache   = map[string]string{}
)

func seedSettings(gdb *gorm.DB) error {
	db := gdb.Session(&gorm.Session{Logger: logger.Discard})
	for k, v := range defaults {
		var s Setting
		err := db.Where("key = ?", k).First(&s).Error
		if err == gorm.ErrRecordNotFound {
			if err := db.Create(&Setting{Key: k, Value: v}).Error; err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return reloadSettings(gdb)
}

func reloadSettings(db *gorm.DB) error {
	var list []Setting
	if err := db.Find(&list).Error; err != nil {
		return err
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]string{}
	for k, v := range defaults {
		cache[k] = v
	}
	for _, s := range list {
		cache[s.Key] = s.Value
	}
	return nil
}

// AllSettings 返回全部配置（含默认值）。
func AllSettings() map[string]string {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	out := make(map[string]string, len(cache))
	for k, v := range cache {
		out[k] = v
	}
	return out
}

func GetSetting(key string) string {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	if v, ok := cache[key]; ok {
		return v
	}
	return defaults[key]
}

func GetSettingInt(key string, def int) int {
	if n, err := strconv.Atoi(GetSetting(key)); err == nil {
		return n
	}
	return def
}

func GetSettingUint(key string, def uint) uint {
	if n, err := strconv.Atoi(GetSetting(key)); err == nil && n > 0 {
		return uint(n)
	}
	return def
}

func GetSettingBool(key string, def bool) bool {
	if b, err := strconv.ParseBool(GetSetting(key)); err == nil {
		return b
	}
	return def
}

func GetSettingDuration(key string, unit time.Duration, def time.Duration) time.Duration {
	if n, err := strconv.Atoi(GetSetting(key)); err == nil && n > 0 {
		return time.Duration(n) * unit
	}
	return def
}

// SetSettings 批量写入并刷新缓存。只接受已知 key。
func SetSettings(kv map[string]string) error {
	rows := make([]Setting, 0, len(kv))
	for k, v := range kv {
		if _, ok := defaults[k]; !ok {
			continue
		}
		rows = append(rows, Setting{Key: k, Value: v, UpdatedAt: time.Now()})
	}
	if len(rows) == 0 {
		return nil
	}
	err := DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&rows).Error
	if err != nil {
		return err
	}
	return reloadSettings(DB)
}
