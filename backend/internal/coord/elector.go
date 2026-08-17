package coord

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/redis/go-redis/v9"
)

// Elector 单例任务的选主锁（当前用于后台清理服务）。
//
// **这是整个系统里唯一 fail-closed 的地方**，跟限流/配额相反：
//
//	限流挂了 -> 放过（宁可超额，不可拒服务）
//	选主挂了 -> 不执行（宁可不删，不可多个实例重复删同一批数据）
//
// 清理服务会 DELETE 历史日志、RemoveAll 归档目录。多个实例同时跑不只是浪费，
// 而是并发删除 + 各自算保留窗口，行为不可预测。所以 Redis 不可用时宁可不清理
// （数据多留几天没有坏处，等 Redis 恢复自然继续）。
//
// 单实例（role=all 且未配置 Redis）例外：此时没有竞争者，直接视为持有锁，
// 否则本地开发和零依赖部署会永远不清理。
type Elector struct {
	rc       *rds.Client
	key      string
	owner    string
	ttl      time.Duration
	soloMode bool // 未配置 Redis：单实例假设，直接视为 leader

	held      atomic.Bool
	acquires  atomic.Uint64
	renewFail atomic.Uint64
	lastErr   atomic.Value // string
}

// NewElector name 是任务名（一个任务一把锁），ttl 是锁的租约时长。
//
// soloMode 由调用方根据「是否配置了 Redis」决定：
// 没有 Redis 就意味着不可能有第二个实例共享状态，此时坚持 fail-closed 只会让功能永久失效。
func NewElector(rc *rds.Client, name, owner string, ttl time.Duration, soloMode bool) *Elector {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	e := &Elector{
		rc:       rc,
		key:      rc.Key("leader", name),
		owner:    owner,
		ttl:      ttl,
		soloMode: soloMode,
	}
	if soloMode {
		e.held.Store(true)
	}
	return e
}

// IsLeader 当前是否持有锁。调用方在每轮任务开始前问一次。
func (e *Elector) IsLeader() bool {
	if e == nil {
		return false
	}
	return e.held.Load()
}

// Run 后台维持锁，阻塞到 ctx 取消。soloMode 下直接返回（永远是 leader）。
//
// 续租频率是 ttl/3：留出两次失败的余量，避免网络抖动就丢锁导致任务在实例间跳。
func (e *Elector) Run(ctx context.Context) {
	if e == nil || e.soloMode || !e.rc.Enabled() {
		return
	}
	go func() {
		tick := time.NewTicker(e.ttl / 3)
		defer tick.Stop()
		for {
			e.tryAcquireOrRenew(ctx)
			select {
			case <-ctx.Done():
				e.release()
				return
			case <-tick.C:
			}
		}
	}()
}

func (e *Elector) tryAcquireOrRenew(ctx context.Context) {
	// 已持有：续租（只有 owner 匹配才续，防止续了别人的锁）
	if e.held.Load() {
		var ok bool
		err := e.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
			res, err := renewScript.Run(ctx, r, []string{e.key}, e.owner, int(e.ttl/time.Millisecond)).Int64()
			ok = res == 1
			return err
		})
		if err != nil {
			// 续租失败：**立刻放弃 leader 身份**（fail-closed）。
			// 不能乐观地继续跑——万一是网络分区，别的实例可能已经拿到锁了。
			e.renewFail.Add(1)
			e.lastErr.Store(err.Error())
			e.held.Store(false)
			log.Printf("[coord] 续租 %s 失败，暂停单例任务: %v", e.key, err)
			return
		}
		if !ok {
			// 锁已经不是我们的了（过期后被别人抢走）
			e.held.Store(false)
			log.Printf("[coord] 锁 %s 已被其他实例持有，暂停单例任务", e.key)
		}
		return
	}

	// 未持有：尝试抢锁
	var won bool
	err := e.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		var err error
		won, err = r.SetNX(ctx, e.key, e.owner, e.ttl).Result()
		return err
	})
	if err != nil {
		e.lastErr.Store(err.Error())
		return
	}
	if won {
		e.held.Store(true)
		e.acquires.Add(1)
		log.Printf("[coord] 已获得单例任务锁 %s（owner=%s）", e.key, e.owner)
	}
}

func (e *Elector) release() {
	if !e.held.Load() {
		return
	}
	e.held.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// 只删自己的锁，避免误删别人刚抢到的
	_ = e.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		return releaseScript.Run(ctx, r, []string{e.key}, e.owner).Err()
	})
}

func (e *Elector) Stats() map[string]any {
	if e == nil {
		return map[string]any{"enabled": false}
	}
	out := map[string]any{
		"leader":     e.held.Load(),
		"solo":       e.soloMode,
		"key":        e.key,
		"owner":      e.owner,
		"acquires":   e.acquires.Load(),
		"renew_fail": e.renewFail.Load(),
	}
	if v, ok := e.lastErr.Load().(string); ok && v != "" {
		out["last_error"] = v
	}
	return out
}

// renewScript 只在 owner 匹配时续租（PEXPIRE），返回 1 表示续租成功。
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  return 1
end
return 0
`)

// releaseScript 只删自己持有的锁。
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
