// Package storetest 给需要真实数据库的测试提供一次性 PostgreSQL 环境。
//
// 做法：在同一个测试库里为每个测试建一个**独立 schema**，通过 DSN 的 search_path
// 指过去，AutoMigrate 于是把表都建在这个 schema 里；测试结束 DROP SCHEMA ... CASCADE。
// 比「每个测试建一个 database」快得多，而且天然支持并行。
//
// 没有设置 GATEWAY_TEST_DSN 就跳过（并打印怎么设），这样别人 clone 下来直接
// go test ./... 不会红一片。
package storetest

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

// EnvDSN 测试库连接串。注意是**测试专用库**，里面的 schema 会被反复创建/删除。
const EnvDSN = "GATEWAY_TEST_DSN"

var seq atomic.Uint64

// New 打开一个独立 schema 的数据库，已迁移完表结构并写好默认归属/管理员（admin/admin）。
func New(t *testing.T) *gorm.DB {
	t.Helper()
	base := os.Getenv(EnvDSN)
	if base == "" {
		t.Skipf("需要 PostgreSQL 测试库，请设置 %s，例如：\n"+
			"  export %s='postgres://gateway:gateway@127.0.0.1:5432/gateway_test?sslmode=disable'\n"+
			"或直接 make test", EnvDSN, EnvDSN)
	}

	schema := fmt.Sprintf("t_%d_%d", time.Now().UnixNano()%1e9, seq.Add(1))
	admin, err := store.Open(base, "admin", "admin", store.Options{MaxOpen: 4, MaxIdle: 1})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	// 用默认 schema 的连接创建目标 schema，再换一条指向它的连接
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("创建 schema 失败: %v", err)
	}

	db, err := store.Open(withSearchPath(base, schema), "admin", "admin", store.Options{MaxOpen: 4, MaxIdle: 1})
	if err != nil {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		t.Fatalf("在 schema %s 上初始化失败: %v", schema, err)
	}

	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Logf("清理 schema %s 失败: %v", schema, err)
		}
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// withSearchPath 往 DSN 里塞 search_path，兼容 URL 与 key=value 两种写法。
//
// 直接拼字符串而不是用 url.Values.Encode()：gorm 的 postgres 驱动会拿正则在原始
// DSN 上抠 TimeZone 且不做 percent-decode，Encode() 会把 Asia/Shanghai 转义成
// Asia%2FShanghai 从而让驱动报 "unknown time zone"。
func withSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}
