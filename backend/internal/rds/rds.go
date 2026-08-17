// Package rds 是 Redis 的最小封装：连接、健康探测、超时与熔断、降级可见。
//
// 设计前提（这几条决定了整个包的形状）：
//
//  1. **fail-open**：Redis 不可用时网关继续转发。Redis 承担的是限流/配额/协调，
//     这些东西挂了应该「放过」而不是「拒服务」——宁可超额，不可拒服务。
//     唯一例外是 cleaner 选主锁：那个必须 fail-closed（宁可不删，不可重复删）。
//
//  2. **慢比挂危险**。Redis 彻底挂掉会立刻返回连接错误，反而安全；真正会拖垮网关的是
//     Redis 变慢（比如 fork 做 RDB、大 key 阻塞）。所以每次调用都有独立超时（默认 50ms），
//     并且连续失败会打开熔断器，直接短路掉后续调用，不再等超时。
//
//  3. **降级必须可见**。fail-open 最大的风险是「静默裸奔」：限流失效了但没人知道，
//     等发现时上游账单已经爆了。所以这里记录降级次数、降级持续时间、最后一次错误，
//     全部暴露到 Console 的概览页。
package rds

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrDisabled 未配置 Redis 时返回，调用方据此走本地降级路径。
var ErrDisabled = errors.New("redis 未启用")

// ErrOpen 熔断器打开时返回（不去打 Redis，直接短路）。
var ErrOpen = errors.New("redis 熔断中")

// Options 连接与容错参数。
type Options struct {
	Addr     string
	Password string
	DB       int
	// KeyPrefix 所有 key 的前缀，便于多个环境共用一个 Redis 实例
	KeyPrefix string
	// Timeout 单次命令超时。宁可放过一次限流，也不能让转发等 Redis
	Timeout time.Duration
	// FailThreshold 连续失败多少次后打开熔断
	FailThreshold int
	// BreakerCooldown 熔断打开后多久试探一次
	BreakerCooldown time.Duration
	PoolSize        int
}

func (o *Options) setDefaults() {
	if o.Timeout <= 0 {
		o.Timeout = 50 * time.Millisecond
	}
	if o.FailThreshold <= 0 {
		o.FailThreshold = 5
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 3 * time.Second
	}
	if o.PoolSize <= 0 {
		o.PoolSize = 32
	}
	if o.KeyPrefix == "" {
		o.KeyPrefix = "gw"
	}
}

// Stats 供 Console 展示：降级了多少次、现在是否降级中、上次错误是什么。
type Stats struct {
	Enabled       bool   `json:"enabled"`
	Healthy       bool   `json:"healthy"`
	BreakerOpen   bool   `json:"breaker_open"`
	Calls         uint64 `json:"calls"`
	Failures      uint64 `json:"failures"`
	Degradations  uint64 `json:"degradations"` // 熔断打开的次数
	DegradedSince string `json:"degraded_since,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Addr          string `json:"addr,omitempty"`
}

// Client 带熔断的 Redis 客户端。所有方法在 Redis 不可用时返回错误，
// 由调用方决定 fail-open 还是 fail-closed —— 这个包不替调用方做那个决定。
type Client struct {
	rdb  *redis.Client
	opt  Options
	tags string // 展示用的地址

	calls, failures, degradations atomic.Uint64

	mu            sync.RWMutex
	consecutive   int
	openUntil     time.Time
	degradedSince time.Time
	lastErr       string
}

// New 建立客户端。addr 为空表示不启用 Redis（返回 nil，调用方全部走降级路径）。
//
// 注意这里**不阻塞等待连通**：Redis 起得比网关晚是常见情况，
// fail-open 的语义下没连上也应该能启动，之后自动恢复。
func New(opt Options) *Client {
	if opt.Addr == "" {
		return nil
	}
	opt.setDefaults()
	return &Client{
		rdb: redis.NewClient(&redis.Options{
			Addr:         opt.Addr,
			Password:     opt.Password,
			DB:           opt.DB,
			PoolSize:     opt.PoolSize,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  opt.Timeout,
			WriteTimeout: opt.Timeout,
		}),
		opt:  opt,
		tags: opt.Addr,
	}
}

// Enabled 是否配置了 Redis。nil 接收者安全，调用方不用到处判空。
func (c *Client) Enabled() bool { return c != nil }

// Key 加上前缀，避免多环境共用 Redis 时撞车。
func (c *Client) Key(parts ...string) string {
	if c == nil {
		return ""
	}
	k := c.opt.KeyPrefix
	for _, p := range parts {
		k += ":" + p
	}
	return k
}

// Raw 暴露底层客户端，供需要 Pub/Sub、Stream 等长连接语义的调用方使用。
// 这类调用不走熔断（它们本身是长期阻塞的），需要自己处理重连。
func (c *Client) Raw() *redis.Client {
	if c == nil {
		return nil
	}
	return c.rdb
}

// Do 执行一次带超时和熔断的 Redis 操作。
//
// fn 里应该只做一次（或一小批）命令；ctx 已经带上超时。
func (c *Client) Do(ctx context.Context, fn func(context.Context, redis.Cmdable) error) error {
	if c == nil {
		return ErrDisabled
	}
	if c.breakerOpen() {
		return ErrOpen
	}
	c.calls.Add(1)

	callCtx, cancel := context.WithTimeout(ctx, c.opt.Timeout)
	defer cancel()

	err := fn(callCtx, c.rdb)
	// redis.Nil 是「key 不存在」，属于正常结果，不能算故障
	if err != nil && !errors.Is(err, redis.Nil) {
		c.recordFailure(err)
		return err
	}
	c.recordSuccess()
	return err
}

func (c *Client) breakerOpen() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Now().Before(c.openUntil)
}

func (c *Client) recordFailure(err error) {
	c.failures.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err.Error()
	c.consecutive++
	if c.consecutive >= c.opt.FailThreshold && time.Now().After(c.openUntil) {
		c.openUntil = time.Now().Add(c.opt.BreakerCooldown)
		c.degradations.Add(1)
		if c.degradedSince.IsZero() {
			c.degradedSince = time.Now()
		}
	}
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive = 0
	c.degradedSince = time.Time{}
}

// Ping 主动探测，供健康检查与启动日志使用。
func (c *Client) Ping(ctx context.Context) error {
	return c.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		return r.Ping(ctx).Err()
	})
}

func (c *Client) Stats() Stats {
	if c == nil {
		return Stats{Enabled: false, Healthy: true} // 没启用不算不健康
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := Stats{
		Enabled:      true,
		BreakerOpen:  time.Now().Before(c.openUntil),
		Calls:        c.calls.Load(),
		Failures:     c.failures.Load(),
		Degradations: c.degradations.Load(),
		LastError:    c.lastErr,
		Addr:         c.tags,
	}
	st.Healthy = !st.BreakerOpen
	if !c.degradedSince.IsZero() {
		st.DegradedSince = c.degradedSince.Format(time.RFC3339)
	}
	return st
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	return c.rdb.Close()
}
