package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config 全部通过环境变量注入，DEMO 级别，给了合理默认值。
type Config struct {
	Port          string // 监听端口
	DataDir       string // 数据目录（sqlite 文件 + 原文归档）
	DBPath        string // sqlite 文件路径
	ArchiveDir    string // 请求/响应原文归档目录
	JWTSecret     string // JWT 签名密钥
	AdminUser     string // 初始管理员用户名
	AdminPass     string // 初始管理员密码
	AllowRegister bool   // 是否允许自助注册
	LogQueueSize  int    // 异步落库队列容量（满了丢日志，不阻塞转发）
}

func Load() *Config {
	dataDir := env("GATEWAY_DATA_DIR", "./data")
	c := &Config{
		Port:          env("GATEWAY_PORT", "8080"),
		DataDir:       dataDir,
		DBPath:        env("GATEWAY_DB_PATH", filepath.Join(dataDir, "gateway.db")),
		ArchiveDir:    env("GATEWAY_ARCHIVE_DIR", filepath.Join(dataDir, "archive")),
		JWTSecret:     env("GATEWAY_JWT_SECRET", "dev-insecure-secret-change-me"),
		AdminUser:     env("GATEWAY_ADMIN_USER", "admin"),
		AdminPass:     env("GATEWAY_ADMIN_PASS", "admin"),
		AllowRegister: envBool("GATEWAY_ALLOW_REGISTER", true),
		LogQueueSize:  envInt("GATEWAY_LOG_QUEUE_SIZE", 8192),
	}
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}
