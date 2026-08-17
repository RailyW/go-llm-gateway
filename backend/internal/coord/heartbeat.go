package coord

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/redis/go-redis/v9"
)

// Heartbeat 实例注册与指标心跳。
//
// 为什么需要：拆成多实例后，Console 上 /api/stats 里的 sink/registry 状态
// 只是**它自己进程**的（console 不转发、不落库，全是 0），运维等于瞎了。
// 所以每个实例定期把自己的状态写进 Redis（带 TTL 自动过期），Console 聚合展示。
//
// 用 Hash + TTL 而不是 Set：实例挂了不需要谁去清理，key 自己过期消失。
type Heartbeat struct {
	rc       *rds.Client
	key      string
	instance string
	ttl      time.Duration
	// snapshot 由调用方提供，每次心跳时取一份当前状态
	snapshot func() map[string]any
}

func NewHeartbeat(rc *rds.Client, instance string, ttl time.Duration, snapshot func() map[string]any) *Heartbeat {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Heartbeat{
		rc:       rc,
		key:      rc.Key("instances"),
		instance: instance,
		ttl:      ttl,
		snapshot: snapshot,
	}
}

// Start 定期上报，间隔为 ttl/3（留出两次失败余量）。
func (h *Heartbeat) Start(ctx context.Context) {
	if h == nil || !h.rc.Enabled() {
		return
	}
	go func() {
		t := time.NewTicker(h.ttl / 3)
		defer t.Stop()
		h.beat(ctx)
		for {
			select {
			case <-ctx.Done():
				h.remove()
				return
			case <-t.C:
				h.beat(ctx)
			}
		}
	}()
}

func (h *Heartbeat) beat(ctx context.Context) {
	payload := map[string]any{"instance": h.instance, "at": time.Now().Format(time.RFC3339)}
	if h.snapshot != nil {
		for k, v := range h.snapshot() {
			payload[k] = v
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	err = h.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		// 每个实例一个独立 key（带 TTL 自动过期），再用一个 set 做索引会引入清理问题，
		// 所以直接靠 SCAN 前缀枚举——实例数量是个位数，不值得优化。
		return r.Set(ctx, h.instanceKey(), b, h.ttl).Err()
	})
	if err != nil {
		log.Printf("[coord] 上报实例心跳失败: %v", err)
	}
}

func (h *Heartbeat) instanceKey() string { return h.key + ":" + h.instance }

func (h *Heartbeat) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = h.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		return r.Del(ctx, h.instanceKey()).Err()
	})
}

// Peers 列出集群里所有存活实例的状态（供 Console 聚合展示）。
// Redis 不可用时返回 nil —— 调用方据此显示「集群视图不可用」。
func (h *Heartbeat) Peers(ctx context.Context) []map[string]any {
	if h == nil || !h.rc.Enabled() {
		return nil
	}
	var out []map[string]any
	err := h.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		var cursor uint64
		for {
			keys, next, err := r.Scan(ctx, cursor, h.key+":*", 100).Result()
			if err != nil {
				return err
			}
			for _, k := range keys {
				b, err := r.Get(ctx, k).Bytes()
				if err != nil {
					continue
				}
				var m map[string]any
				if json.Unmarshal(b, &m) == nil {
					out = append(out, m)
				}
			}
			if next == 0 {
				return nil
			}
			cursor = next
		}
	})
	if err != nil {
		return nil
	}
	return out
}
