package coord

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
)

// 测试用 Redis 连接信息；未设置地址则跳过需要 Redis 的测试。
const (
	EnvRedis     = "GATEWAY_TEST_REDIS_ADDR"
	EnvRedisPass = "GATEWAY_TEST_REDIS_PASSWORD"
)

func testClient(t *testing.T) *rds.Client {
	t.Helper()
	addr := os.Getenv(EnvRedis)
	if addr == "" {
		t.Skipf("需要 Redis，请设置 %s，例如 %s=127.0.0.1:6379", EnvRedis, EnvRedis)
	}
	c := rds.New(rds.Options{
		Addr:      addr,
		Password:  os.Getenv(EnvRedisPass),
		KeyPrefix: "gwtest_" + t.Name(),
		Timeout:   500 * time.Millisecond, // 测试环境放宽，避免偶发超时导致假失败
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Skipf("Redis %s 连不上: %v", addr, err)
	}
	t.Cleanup(func() {
		// 清掉本测试造的 key
		keys, _ := c.Raw().Keys(context.Background(), "gwtest_"+t.Name()+"*").Result()
		if len(keys) > 0 {
			c.Raw().Del(context.Background(), keys...)
		}
		c.Close()
	})
	return c
}

// 只有一个实例能当 leader —— 这是清理任务不重复执行的根本保证
func TestElectorSingleLeader(t *testing.T) {
	rc := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := NewElector(rc, "cleanup", "inst-a", 3*time.Second, false)
	b := NewElector(rc, "cleanup", "inst-b", 3*time.Second, false)
	a.Run(ctx)
	b.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.IsLeader() || b.IsLeader() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if a.IsLeader() == b.IsLeader() {
		t.Fatalf("必须恰好一个实例是 leader，实际 a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
}

// leader 退出后，锁过期，另一个实例应该接管
func TestElectorFailover(t *testing.T) {
	rc := testClient(t)
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	ttl := 1500 * time.Millisecond
	a := NewElector(rc, "failover", "inst-a", ttl, false)
	a.Run(ctxA)
	// 等 a 拿到锁
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !a.IsLeader() {
		time.Sleep(30 * time.Millisecond)
	}
	if !a.IsLeader() {
		t.Fatal("a 应先拿到锁")
	}

	b := NewElector(rc, "failover", "inst-b", ttl, false)
	b.Run(ctxB)
	time.Sleep(300 * time.Millisecond)
	if b.IsLeader() {
		t.Fatal("a 还持有锁时 b 不该是 leader")
	}

	cancelA() // a 退出会主动释放锁
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !b.IsLeader() {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.IsLeader() {
		t.Error("a 退出后 b 应接管（清理任务不能因为一个实例挂了就永久停摆）")
	}
}

// 没有 Redis 时（solo）必须直接视为 leader，否则单实例部署会永远不清理
func TestElectorSoloMode(t *testing.T) {
	e := NewElector(nil, "cleanup", "solo", time.Second, true)
	if !e.IsLeader() {
		t.Error("solo 模式必须直接是 leader")
	}
	e.Run(context.Background()) // 应该直接返回，不 panic
	if st := e.Stats(); st["solo"] != true {
		t.Errorf("stats 要标明 solo: %v", st)
	}
}

// 这是 fail-closed 的核心：Redis 不可用且非 solo 时，绝不能自认 leader。
// 否则多个实例会同时删同一批数据。
func TestElectorFailClosedWithoutRedis(t *testing.T) {
	e := NewElector(nil, "cleanup", "inst", time.Second, false)
	if e.IsLeader() {
		t.Fatal("Redis 不可用且非 solo 时必须放弃 leader（宁可不清理，不可重复删）")
	}

	// 指向一个不存在的 Redis，同样不能变成 leader
	bad := rds.New(rds.Options{Addr: "127.0.0.1:1", Timeout: 50 * time.Millisecond})
	defer bad.Close()
	e2 := NewElector(bad, "cleanup", "inst", time.Second, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e2.Run(ctx)
	time.Sleep(400 * time.Millisecond)
	if e2.IsLeader() {
		t.Error("连不上 Redis 时不该成为 leader")
	}
}

// 配置失效广播：B 实例必须收到 A 实例的通知并重建快照
func TestInvalidateBroadcast(t *testing.T) {
	rc := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan struct{}, 4)
	b := NewInvalidator(rc, "inst-b", func() { got <- struct{}{} })
	b.Subscribe(ctx)

	// 等订阅建立（Subscribe 是异步的）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := b.Stats(); st["connected"] == true {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	a := NewInvalidator(rc, "inst-a", nil)
	a.Publish(ctx)

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("B 实例没收到配置失效广播（多实例下配置变更就只能等 30 秒兜底轮询了）")
	}
}

// 自己发的广播自己要忽略：写操作那一侧已经同步重建过了，重复重建是浪费
func TestInvalidateIgnoresSelf(t *testing.T) {
	rc := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	done := make(chan struct{}, 4)
	self := NewInvalidator(rc, "inst-self", func() { calls++; done <- struct{}{} })
	self.Subscribe(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := self.Stats(); st["connected"] == true {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	self.Publish(ctx)
	select {
	case <-done:
		t.Error("不该处理自己发出的广播")
	case <-time.After(700 * time.Millisecond):
	}
	if st := self.Stats(); st["ignored"].(uint64) == 0 {
		t.Errorf("应记录忽略计数: %v", st)
	}
}

// Redis 未配置时，广播相关调用必须安全无操作（fail-open）
func TestInvalidateDisabled(t *testing.T) {
	i := NewInvalidator(nil, "inst", func() { t.Error("不该被调用") })
	i.Subscribe(context.Background())
	i.Publish(context.Background())
	if st := i.Stats(); st["enabled"] != false {
		t.Errorf("未启用时 stats 应标明: %v", st)
	}
}
