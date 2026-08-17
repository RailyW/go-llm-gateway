package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 全部通过环境变量注入，DEMO 级别，给了合理默认值。
// 启动时会先加载当前目录的 .env（已存在的真实环境变量优先，不被覆盖）。
type Config struct {
	Port          string // 监听端口
	DataDir       string // 数据目录（原文归档）
	DSN           string // PostgreSQL 连接串
	DBMaxOpen     int    // 连接池上限（别超过 PG 的 max_connections）
	DBMaxIdle     int    // 空闲连接数
	ArchiveDir    string // 请求/响应原文归档目录
	JWTSecret     string // JWT 签名密钥
	AdminUser     string // 初始管理员用户名
	AdminPass     string // 初始管理员密码
	AllowRegister bool   // 是否允许自助注册
	LogQueueSize  int    // 异步落库队列容量（满了丢日志行，不阻塞转发）
}

func Load() *Config {
	LoadDotEnv(".env")

	dataDir := env("GATEWAY_DATA_DIR", "./data")
	c := &Config{
		Port:          env("GATEWAY_PORT", "8080"),
		DataDir:       dataDir,
		DSN:           dsn(),
		DBMaxOpen:     envInt("GATEWAY_DB_MAX_OPEN", 32),
		DBMaxIdle:     envInt("GATEWAY_DB_MAX_IDLE", 8),
		ArchiveDir:    env("GATEWAY_ARCHIVE_DIR", filepath.Join(dataDir, "archive")),
		JWTSecret:     env("GATEWAY_JWT_SECRET", "dev-insecure-secret-change-me"),
		AdminUser:     env("GATEWAY_ADMIN_USER", "admin"),
		AdminPass:     env("GATEWAY_ADMIN_PASS", "admin"),
		AllowRegister: envBool("GATEWAY_ALLOW_REGISTER", true),
		// 队列里只放日志行（~350 字节/条），所以 32768 条也只有 ~11MB。
		// 定这个数是为了吞下**突发**：压测里 20000 个请求在 0.9 秒内砸进来，
		// 8192 的队列装不下（那是当初按「每条 Entry 很大」拍的，摘掉原文后明显偏小）。
		LogQueueSize: envInt("GATEWAY_LOG_QUEUE_SIZE", 32768),
	}
	return c
}

// dsn 优先用整条 GATEWAY_DB_DSN；没给就按分量拼。
// 分量方式的好处是 .env 里改个密码不用重写整条 URL。
func dsn() string {
	if v := os.Getenv("GATEWAY_DB_DSN"); v != "" {
		return v
	}
	host := env("GATEWAY_DB_HOST", "127.0.0.1")
	port := env("GATEWAY_DB_PORT", "5432")
	user := env("GATEWAY_DB_USER", "gateway")
	pass := env("GATEWAY_DB_PASSWORD", "gateway")
	name := env("GATEWAY_DB_NAME", "gateway")

	// 注意：这里故意**不用** url.Values.Encode()。
	// gorm.io/driver/postgres 是拿正则在**原始 DSN 字符串**上抠 TimeZone 的（postgres.go），
	// 不会做 percent-decode，于是 Encode() 把 Asia/Shanghai 转义成 Asia%2FShanghai 后，
	// 它会拿着 "Asia%2FShanghai" 去调 time.LoadLocation 并报错。
	// 而 '/' 在 query 里本来就不需要转义（RFC 3986），所以手拼。
	params := []string{"sslmode=" + env("GATEWAY_DB_SSLMODE", "disable")}
	if tz := env("GATEWAY_DB_TIMEZONE", "Asia/Shanghai"); tz != "" {
		params = append(params, "TimeZone="+tz)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     host + ":" + port,
		Path:     "/" + name,
		RawQuery: strings.Join(params, "&"),
	}
	return u.String()
}

// SafeDSN 去掉密码，用于日志输出。
func SafeDSN(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "redacted") // &, / 之类会被转义，用纯字母占位
	}
	return u.String()
}

// LoadDotEnv 读一个极简 .env：KEY=VALUE，# 开头是注释，支持 export 前缀与引号。
// 已存在的环境变量优先（真实环境 > 文件），所以容器里注入的变量不会被文件盖掉。
// 自己实现是为了不引第三方依赖——这点规模不值得。
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i]) // 行尾注释
		}
		if k == "" {
			continue
		}
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "[config] 设置 %s 失败: %v\n", k, err)
		}
	}
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
