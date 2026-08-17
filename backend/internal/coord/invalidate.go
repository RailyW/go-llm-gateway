// Package coord 是多实例之间的协调层：配置失效广播、任务选主、实例心跳。
//
// 为什么需要它：转发实例把配置全量缓存在本地内存（registry 快照，热路径零查询），
// 这在**单实例**下靠「写操作后同步重建」就够了。多实例下 A 实例改了配置，
// B 实例根本不知道，只能等 30 秒兜底刷新 —— 「禁用一把 key」等 30 秒生效是不能接受的。
//
// 所以这里用 Redis Pub/Sub 广播一条「配置变了」的消息，各实例收到后立刻重建本地快照。
// 注意快照本身仍在**本地内存**：Redis 只负责通知，不负责存配置。
// 本地内存是纳秒级访问，Redis 是微秒级 + 网络故障面，热路径不该依赖它。
//
// 全部 fail-open：Redis 挂了退回 30 秒兜底轮询（config 变更最多迟 30 秒生效），
// 唯一例外是选主锁 —— 那个必须 fail-closed，见 Elector。
package coord

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/redis/go-redis/v9"
)

// Invalidator 配置失效广播。
type Invalidator struct {
	rc         *rds.Client
	channel    string
	instanceID string
	// onInvalid 收到广播后的回调（重建本地快照）
	onInvalid func()

	published atomic.Uint64
	received  atomic.Uint64
	ignored   atomic.Uint64 // 自己发的自己收到，跳过（本地已同步重建过）
	connected atomic.Bool
}

func NewInvalidator(rc *rds.Client, instanceID string, onInvalid func()) *Invalidator {
	return &Invalidator{
		rc:         rc,
		channel:    rc.Key("config", "invalidate"),
		instanceID: instanceID,
		onInvalid:  onInvalid,
	}
}

// Publish 广播「配置已变更」。由管理 API 的写操作调用。
//
// 失败不返回错误给调用方：配置**已经写进 PG 了**，广播只是加速其他实例感知。
// 广播失败最坏的结果是其他实例晚 30 秒（兜底轮询）生效，不该因此让管理操作失败。
func (i *Invalidator) Publish(ctx context.Context) {
	if i == nil || !i.rc.Enabled() {
		return
	}
	err := i.rc.Do(ctx, func(ctx context.Context, r redis.Cmdable) error {
		return r.Publish(ctx, i.channel, i.instanceID).Err()
	})
	if err != nil {
		log.Printf("[coord] 广播配置失效失败（其他实例将在兜底轮询时生效）: %v", err)
		return
	}
	i.published.Add(1)
}

// Subscribe 订阅失效广播，阻塞直到 ctx 取消。应该在自己的 goroutine 里跑。
//
// 这里不走 rds.Client.Do 的熔断包装：Pub/Sub 是长期阻塞的连接，
// 语义上跟「一次短命令」不同，需要自己处理断线重连。
func (i *Invalidator) Subscribe(ctx context.Context) {
	if i == nil || !i.rc.Enabled() {
		return
	}
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			sub := i.rc.Raw().Subscribe(ctx, i.channel)
			// 确认订阅成功后再进入接收循环，否则会静默丢消息
			if _, err := sub.Receive(ctx); err != nil {
				i.connected.Store(false)
				sub.Close()
				if ctx.Err() != nil {
					return
				}
				log.Printf("[coord] 订阅配置失效失败，%v 后重试: %v", backoff, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			i.connected.Store(true)
			backoff = time.Second
			log.Printf("[coord] 已订阅配置失效广播 %s", i.channel)

			ch := sub.Channel()
		recv:
			for {
				select {
				case <-ctx.Done():
					sub.Close()
					return
				case m, ok := <-ch:
					if !ok {
						break recv // 连接断了，外层重连
					}
					i.received.Add(1)
					// 自己发的跳过：写操作那一侧已经同步重建过本地快照了
					if m.Payload == i.instanceID {
						i.ignored.Add(1)
						continue
					}
					if i.onInvalid != nil {
						i.onInvalid()
					}
				}
			}
			i.connected.Store(false)
			sub.Close()
		}
	}()
}

// Stats 供 Console 展示广播是否在工作。
func (i *Invalidator) Stats() map[string]any {
	if i == nil || !i.rc.Enabled() {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":   true,
		"channel":   i.channel,
		"connected": i.connected.Load(),
		"published": i.published.Load(),
		"received":  i.received.Load(),
		"ignored":   i.ignored.Load(),
	}
}
