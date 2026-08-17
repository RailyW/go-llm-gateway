package sink

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/RailyW/go-llm-gateway/backend/internal/storetest"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	envRedis     = "GATEWAY_TEST_REDIS_ADDR"
	envRedisPass = "GATEWAY_TEST_REDIS_PASSWORD"
)

func testRedis(t *testing.T) *rds.Client {
	t.Helper()
	addr := os.Getenv(envRedis)
	if addr == "" {
		t.Skipf("需要 Redis，请设置 %s", envRedis)
	}
	c := rds.New(rds.Options{
		Addr:      addr,
		Password:  os.Getenv(envRedisPass),
		KeyPrefix: "gwtest_" + t.Name(),
		Timeout:   2 * time.Second,
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Skipf("Redis %s 连不上: %v", addr, err)
	}
	t.Cleanup(func() {
		keys, _ := c.Raw().Keys(context.Background(), "gwtest_"+t.Name()+"*").Result()
		if len(keys) > 0 {
			c.Raw().Del(context.Background(), keys...)
		}
		c.Close()
	})
	return c
}

// 造一批可落库的日志（外键是开着的，所以要真建关联记录）
func seedFixtures(t *testing.T, db *gorm.DB) (userID, keyID uint) {
	t.Helper()
	g := store.Group{Name: "g", Enabled: true}
	if err := db.Create(&g).Error; err != nil {
		t.Fatal(err)
	}
	u := store.User{Username: "u", PasswordHash: "x", GroupID: g.ID, Enabled: true}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	k := store.APIKey{Name: "k", Key: "sk-test", UserID: u.ID, Enabled: true}
	if err := db.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	return u.ID, k.ID
}

func mkEntry(id string, userID, keyID uint) Entry {
	return Entry{
		Log: &store.RequestLog{
			ID: id, Protocol: "openai-chat", Endpoint: "/v1/chat/completions",
			UserID: userID, Username: "u", APIKeyID: keyID, ModelName: "m",
			StatusCode: 200, TotalTokens: 3, CreatedAt: time.Now(),
			Usage: store.JSONB(`{"total_tokens":3}`),
		},
		TouchGatewayKeyID: keyID,
	}
}

// Entry 的线上格式必须能完整往返 —— 这是跨进程契约，字段丢了就是静默丢数据
func TestEntryJSONRoundTrip(t *testing.T) {
	in := mkEntry("id-1", 7, 9)
	in.TouchChannelKeyID = 11

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Entry
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Log == nil {
		t.Fatal("Log 丢了")
	}
	if out.Log.ID != in.Log.ID || out.Log.Protocol != in.Log.Protocol ||
		out.Log.UserID != in.Log.UserID || out.Log.TotalTokens != in.Log.TotalTokens {
		t.Errorf("日志字段不一致: %+v", out.Log)
	}
	if string(out.Log.Usage) != string(in.Log.Usage) {
		t.Errorf("usage jsonb 不一致: %s vs %s", out.Log.Usage, in.Log.Usage)
	}
	if out.TouchGatewayKeyID != 9 || out.TouchChannelKeyID != 11 {
		t.Errorf("touch key 丢了: %+v", out)
	}
}

// 端到端：Stream 生产 → Consumer 消费 → 真的落进 PG
func TestStreamRoundTripToPostgres(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	userID, keyID := seedFixtures(t, db)

	st := NewStream(rc, "gw-1", StreamOptions{QueueSize: 128})
	st.Start()

	const n = 25
	for i := 0; i < n; i++ {
		if !st.Submit(mkEntry("s-"+string(rune('a'+i%26))+string(rune('0'+i/26)), userID, keyID)) {
			t.Fatalf("第 %d 条投递失败", i)
		}
	}
	// 关掉生产端，确保本地缓冲全部 XADD 出去
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Close(ctx); err != nil {
		t.Fatalf("生产端关闭超时: %v", err)
	}
	if got := st.Stats().Persisted; got != n {
		t.Fatalf("应投递 %d 条到 Stream，实际 %d（%s）", n, got, st.Stats().LastError)
	}

	// 消费端接管
	cons := NewConsumer(rc, db, ConsumerOptions{Instance: "worker-1", Count: 100, Block: 200 * time.Millisecond})
	cctx, ccancel := context.WithCancel(context.Background())
	cons.Start(cctx)

	deadline := time.Now().Add(15 * time.Second)
	var count int64
	for time.Now().Before(deadline) {
		db.Model(&store.RequestLog{}).Count(&count)
		if count >= n {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	ccancel()
	_ = cons.Wait(context.Background())

	if count != n {
		t.Fatalf("PG 里应有 %d 条日志，实际 %d（消费端: %+v）", n, count, cons.Stats())
	}
	if p := cons.Stats().Persisted; p != n {
		t.Errorf("消费端统计应为 %d，实际 %d", n, p)
	}
	// last_used_at 也该被合并写进去
	var k store.APIKey
	db.First(&k, keyID)
	if k.LastUsedAt == nil {
		t.Error("last_used_at 没被更新（合并 UPDATE 丢了）")
	}
}

// 落库失败**绝不能** XACK —— 这是整个 Streams 方案不丢日志的唯一依据。
// 用一个已关闭的 DB 制造失败，然后确认消息还留在 pending 里。
func TestConsumerDoesNotAckOnFailure(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	userID, keyID := seedFixtures(t, db)

	st := NewStream(rc, "gw-1", StreamOptions{QueueSize: 16})
	st.Start()
	st.Submit(mkEntry("fail-1", userID, keyID))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// 把底层连接关掉，让落库必然失败
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()

	cons := NewConsumer(rc, db, ConsumerOptions{Instance: "worker-broken", Count: 10, Block: 200 * time.Millisecond})
	cctx, ccancel := context.WithCancel(context.Background())
	cons.Start(cctx)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && cons.Stats().Failed == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	ccancel()
	_ = cons.Wait(context.Background())

	if cons.Stats().Failed == 0 {
		t.Fatal("落库应该失败（DB 已关闭）")
	}
	if cons.Stats().Persisted != 0 {
		t.Error("落库失败却报告已落库")
	}
	// 关键断言：消息必须还在 pending 里，等着被重试
	pend, err := rc.Raw().XPending(context.Background(), StreamKey(rc), ConsumerGroup).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pend.Count == 0 {
		t.Error("落库失败后消息被 ACK 了 —— Streams 的至少一次投递就此失效")
	}
}

// worker 崩溃遗留的 pending 消息必须能被另一个 worker 接管，否则每次异常退出都永久漏一批
func TestConsumerClaimsStaleMessages(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	userID, keyID := seedFixtures(t, db)
	key := StreamKey(rc)
	ctx := context.Background()

	// 手工造出「已投递给 worker-dead 但没 ACK」的状态
	if err := rc.Raw().XGroupCreateMkStream(ctx, key, ConsumerGroup, "0").Err(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mkEntry("stale-1", userID, keyID))
	if err := rc.Raw().XAdd(ctx, &redis.XAddArgs{Stream: key, Values: []any{streamField, b}}).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Raw().XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: ConsumerGroup, Consumer: "worker-dead",
		Streams: []string{key, ">"}, Count: 10,
	}).Result(); err != nil {
		t.Fatal(err)
	}
	// 此时消息 pending 在一个再也不会回来的 consumer 名下
	pend, _ := rc.Raw().XPending(ctx, key, ConsumerGroup).Result()
	if pend.Count != 1 {
		t.Fatalf("准备阶段失败，pending=%d", pend.Count)
	}

	// MinIdle=0：立刻可接管（真实环境是 1 分钟，测试里不等）
	cons := NewConsumer(rc, db, ConsumerOptions{
		Instance: "worker-new", Count: 10, Block: 100 * time.Millisecond, ClaimMinIdle: time.Nanosecond,
	})
	cons.claimStale(ctx)

	var count int64
	db.Model(&store.RequestLog{}).Count(&count)
	if count != 1 {
		t.Fatalf("遗留日志应被接管并落库，实际 PG 里有 %d 条（%+v）", count, cons.Stats())
	}
	if cons.Stats().Retried != 1 {
		t.Errorf("应记录 1 条重试，实际 %d", cons.Stats().Retried)
	}
}

// 解不出来的消息不能永久卡在 pending 里挡住后面的日志
func TestConsumerSkipsUndecodableMessage(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	key := StreamKey(rc)
	ctx := context.Background()

	if err := rc.Raw().XGroupCreateMkStream(ctx, key, ConsumerGroup, "0").Err(); err != nil {
		t.Fatal(err)
	}
	// 垃圾 payload
	rc.Raw().XAdd(ctx, &redis.XAddArgs{Stream: key, Values: []any{streamField, "{not json"}})
	rc.Raw().XAdd(ctx, &redis.XAddArgs{Stream: key, Values: []any{"wrong-field", "x"}})

	cons := NewConsumer(rc, db, ConsumerOptions{Instance: "w", Count: 10, Block: 100 * time.Millisecond})
	if err := cons.ensureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cons.readAndPersist(ctx); err != nil {
		t.Fatal(err)
	}

	if cons.Stats().Poisoned != 2 {
		t.Errorf("两条坏消息都应计入 poisoned，实际 %d", cons.Stats().Poisoned)
	}
	pend, _ := rc.Raw().XPending(ctx, key, ConsumerGroup).Result()
	if pend.Count != 0 {
		t.Errorf("坏消息应被 ACK 掉，否则会永久堵塞；pending=%d", pend.Count)
	}
}

// Stream 必须有长度上限：worker 挂掉时不能把 Redis 内存吃光
func TestStreamMaxLenBounded(t *testing.T) {
	rc := testRedis(t)
	// MaxLen 很小，且 Approx=true 是按整节点裁剪，所以实际长度会略大于 MaxLen
	st := NewStream(rc, "gw", StreamOptions{QueueSize: 4096, MaxLen: 50})
	st.Start()
	for i := 0; i < 1500; i++ {
		st.Submit(mkEntry("m", 1, 1))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Close(ctx); err != nil {
		t.Fatal(err)
	}

	n, err := rc.Raw().XLen(context.Background(), StreamKey(rc)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n >= 1500 {
		t.Errorf("Stream 没有被裁剪，长度 %d —— 无界 Stream 会吃光 Redis 内存", n)
	}
	t.Logf("投递 1500 条后 Stream 长度 %d（MAXLEN ~50，近似裁剪按整节点）", n)
}

// Redis 不可用时生产端必须 fail-open：转发不受影响，日志计入丢弃并可见
func TestStreamFailsOpenWithoutRedis(t *testing.T) {
	bad := rds.New(rds.Options{Addr: "127.0.0.1:1", Timeout: 50 * time.Millisecond, KeyPrefix: "gwtest_dead"})
	defer bad.Close()

	st := NewStream(bad, "gw", StreamOptions{QueueSize: 32})
	st.Start()
	for i := 0; i < 5; i++ {
		if !st.Submit(mkEntry("x", 1, 1)) {
			t.Fatal("Submit 不该失败：本地缓冲还没满")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = st.Close(ctx)

	s := st.Stats()
	if s.Dropped == 0 {
		t.Error("Redis 不可用时日志确实丢了，必须计数（否则就是静默丢失）")
	}
	if s.LastError == "" {
		t.Error("必须记录错误原因供 Console 展示")
	}
	if s.Via != "redis-stream" {
		t.Errorf("Via 应说明去处，实际 %q", s.Via)
	}
}

// 本地缓冲满了要丢弃并计数，绝不阻塞转发
func TestStreamDropsWhenLocalBufferFull(t *testing.T) {
	bad := rds.New(rds.Options{Addr: "127.0.0.1:1", Timeout: 10 * time.Millisecond, KeyPrefix: "gwtest_full"})
	defer bad.Close()
	// 不 Start：没有后台协程消费，缓冲必然填满
	st := NewStream(bad, "gw", StreamOptions{QueueSize: 4})

	var ok, dropped int
	for i := 0; i < 20; i++ {
		if st.Submit(mkEntry("x", 1, 1)) {
			ok++
		} else {
			dropped++
		}
	}
	if ok != 4 || dropped != 16 {
		t.Errorf("应接收 4 条丢弃 16 条，实际 %d/%d", ok, dropped)
	}
	if st.Stats().Dropped != 16 {
		t.Errorf("丢弃计数不对: %d", st.Stats().Dropped)
	}
}

// 积压指标必须用消费组的 lag，不能用 XLEN。
//
// XACK 不删除消息，所以 XLEN 是「历史总条数」。用它当积压会显示一个
// 永远不归零的数字，运维就再也不会相信这个指标了。
func TestConsumerBacklogUsesLagNotXLen(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	userID, keyID := seedFixtures(t, db)

	st := NewStream(rc, "gw", StreamOptions{QueueSize: 64})
	st.Start()
	for i := 0; i < 10; i++ {
		st.Submit(mkEntry("lag-"+string(rune('a'+i)), userID, keyID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.Close(ctx); err != nil {
		t.Fatal(err)
	}

	cons := NewConsumer(rc, db, ConsumerOptions{Instance: "w", Count: 100, Block: 100 * time.Millisecond})
	if err := cons.ensureGroup(ctx); err != nil {
		t.Fatal(err)
	}

	// 消费前：应该看到 10 条积压
	cons.refreshLag(ctx)
	if got := cons.Stats().Backlog; got != 10 {
		t.Errorf("消费前积压应为 10，实际 %d", got)
	}

	// 全部消费掉
	if _, err := cons.readAndPersist(ctx); err != nil {
		t.Fatal(err)
	}
	cons.refreshLag(ctx)
	st2 := cons.Stats()
	if st2.Backlog != 0 {
		t.Errorf("消费完积压必须归 0，实际 %d（很可能误用了 XLEN）", st2.Backlog)
	}
	if st2.Length == 0 {
		t.Error("Length 应反映 Stream 物理长度（XACK 不删消息），用于观察内存占用")
	}
	if st2.Pending != 0 {
		t.Errorf("落库成功后 pending 应归 0，实际 %d", st2.Pending)
	}
}

// 已消费的消息要回收，否则 Redis 里会常驻几百 MB 早已落库的日志
func TestConsumerTrimsConsumedMessages(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	userID, keyID := seedFixtures(t, db)
	ctx := context.Background()

	st := NewStream(rc, "gw", StreamOptions{QueueSize: 64})
	st.Start()
	for i := 0; i < 10; i++ {
		st.Submit(mkEntry("trim-"+string(rune('a'+i)), userID, keyID))
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := st.Close(cctx); err != nil {
		t.Fatal(err)
	}

	cons := NewConsumer(rc, db, ConsumerOptions{Instance: "w", Count: 100, Block: 100 * time.Millisecond})
	if err := cons.ensureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cons.readAndPersist(ctx); err != nil {
		t.Fatal(err)
	}

	before, _ := rc.Raw().XLen(ctx, StreamKey(rc)).Result()
	if before != 10 {
		t.Fatalf("准备阶段：Stream 应有 10 条，实际 %d", before)
	}
	cons.trim(ctx)
	after, _ := rc.Raw().XLen(ctx, StreamKey(rc)).Result()
	if after >= before {
		t.Errorf("已消费的消息应被回收，裁剪前 %d 裁剪后 %d", before, after)
	}
	if cons.Stats().Trimmed == 0 {
		t.Error("回收数量要计入统计")
	}
	// 数据必须还在 PG 里（回收的是 Redis 副本，不是数据本身）
	var n int64
	db.Model(&store.RequestLog{}).Count(&n)
	if n != 10 {
		t.Errorf("PG 里应有 10 条日志，实际 %d", n)
	}
}

// 有未 ACK 消息时绝不能裁剪：那些正等着重试
func TestConsumerDoesNotTrimPending(t *testing.T) {
	rc := testRedis(t)
	db := storetest.New(t)
	userID, keyID := seedFixtures(t, db)
	ctx := context.Background()
	key := StreamKey(rc)

	if err := rc.Raw().XGroupCreateMkStream(ctx, key, ConsumerGroup, "0").Err(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mkEntry("pend-1", userID, keyID))
	rc.Raw().XAdd(ctx, &redis.XAddArgs{Stream: key, Values: []any{streamField, b}})
	// 读走但不 ACK
	if _, err := rc.Raw().XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: ConsumerGroup, Consumer: "w-dead", Streams: []string{key, ">"}, Count: 10,
	}).Result(); err != nil {
		t.Fatal(err)
	}

	cons := NewConsumer(rc, db, ConsumerOptions{Instance: "w", Count: 10})
	cons.trim(ctx)

	n, _ := rc.Raw().XLen(ctx, key).Result()
	if n != 1 {
		t.Errorf("未 ACK 的消息被裁掉了（那批数据就永久丢了），剩余 %d", n)
	}
}
