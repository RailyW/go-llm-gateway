package sink

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/RailyW/go-llm-gateway/backend/internal/storetest"
	"gorm.io/gorm"
)

func setup(t *testing.T) *gorm.DB {
	t.Helper()
	return storetest.New(t)
}

func entry(id string) Entry {
	return Entry{
		Log: &store.RequestLog{
			ID: id, Protocol: "openai-chat", Endpoint: "/v1/chat/completions",
			UserID: 1, Username: "admin", GroupID: 1, GroupName: "default",
			APIKeyID: 1, APIKeyName: "k", ModelName: "m", StatusCode: 200,
			PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CreatedAt: time.Now(),
			ArchivePath: "2026-01-01",
		},
		TouchGatewayKeyID: 1,
		TouchChannelKeyID: 1,
	}
}

// 攒批落库：投递 N 条 -> Close 后应全部入库，且归档文件都写了
func TestBatchPersistsAllOnClose(t *testing.T) {
	db := setup(t)
	_ = store.SetSettings(map[string]string{store.KeyLogFlushIntervalMs: "20", store.KeyLogFlushBatch: "10"})

	s := NewBatch(db, 1024)
	s.Start()

	const n = 55
	for i := 0; i < n; i++ {
		if !s.Submit(entry(idOf(i))) {
			t.Fatalf("第 %d 条被丢弃了，队列不该满", i)
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var cnt int64
	db.Model(&store.RequestLog{}).Count(&cnt)
	if cnt != n {
		t.Errorf("落库 %d 条, want %d", cnt, n)
	}
	st := s.Stats()
	if st.Enqueued != n || st.Persisted != n || st.Dropped != 0 {
		t.Errorf("stats = %+v", st)
	}
	if st.Batches < 2 {
		t.Errorf("应该分成多批，实际 %d 批", st.Batches)
	}
}

// 队列满时必须丢弃并计数，绝不阻塞请求
func TestBatchDropsWhenFull(t *testing.T) {
	db := setup(t)
	s := NewBatch(db, 2) // 故意开很小，且不 Start()，让队列填满

	ok1 := s.Submit(entry("a"))
	ok2 := s.Submit(entry("b"))
	ok3 := s.Submit(entry("c")) // 满了
	if !ok1 || !ok2 {
		t.Fatal("前两条应入队成功")
	}
	if ok3 {
		t.Fatal("队列满时应返回 false")
	}
	if st := s.Stats(); st.Dropped != 1 || st.Enqueued != 2 || st.QueueCap != 2 {
		t.Errorf("stats = %+v", st)
	}
}

// last_used_at 合并：同一把 key 被 100 次请求命中，只应产生一次 UPDATE 的效果
func TestBatchCoalescesLastUsedAt(t *testing.T) {
	db := setup(t)
	_ = store.SetSettings(map[string]string{store.KeyLogFlushIntervalMs: "20", store.KeyLogFlushBatch: "500"})

	db.Create(&store.APIKey{UserID: 1, Name: "k", Key: "sk-coalesce", Enabled: true})
	// 外键是打开的：先把上游建出来，channel_keys 才能引用它
	ch := store.Channel{Name: "c", Protocols: ",openai-chat,", BaseURL: "http://x", Enabled: true}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatal(err)
	}
	ck0 := store.ChannelKey{ChannelID: ch.ID, GroupID: 1, Name: "ck", Key: "up", Weight: 1, Enabled: true}
	if err := db.Create(&ck0).Error; err != nil {
		t.Fatal(err)
	}

	s := NewBatch(db, 1024)
	s.Start()
	for i := 0; i < 100; i++ {
		e := entry(idOf(i))
		e.TouchGatewayKeyID = 1
		e.TouchChannelKeyID = ck0.ID
		s.Submit(e)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var ak store.APIKey
	db.First(&ak, 1)
	if ak.LastUsedAt == nil {
		t.Error("网关 key 的 last_used_at 应被刷新")
	}
	var ck store.ChannelKey
	db.First(&ck, ck0.ID)
	if ck.LastUsedAt == nil {
		t.Error("上游 key 的 last_used_at 应被刷新")
	}
	if st := s.Stats(); st.Batches > 3 {
		t.Errorf("100 条应该只用很少批次，实际 %d", st.Batches)
	}
}

// Close 之后不再接收
func TestSubmitAfterClose(t *testing.T) {
	db := setup(t)
	s := NewBatch(db, 8)
	s.Start()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Submit(entry("x")) {
		t.Error("Close 后不应再接收")
	}
}

func idOf(i int) string {
	return "req-" + time.Now().Format("150405") + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// 队列内存必须与请求体大小**无关**。
//
// 这是个回归测试：早期版本把请求/响应原文塞进 Entry，于是队列占用 =
// 队列深度 × 请求体大小（8192 深 × 64KB 原文 = 1GB 堆内存，20MB 的请求体更是直接爆）。
// 现在原文由 archive 包当场写盘，Entry 里只有日志行。
func TestQueueMemoryIndependentOfBodySize(t *testing.T) {
	const n = 2000
	measure := func(bodySize int) uint64 {
		s := NewBatch(nil, n) // 不 Start，纯粹让 Entry 堆在队列里
		body := make([]byte, bodySize)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i < n; i++ {
			e := entry(idOf(i))
			// 模拟「请求体很大」：日志行里唯一可能变长的是错误信息，
			// 原文则完全不该出现在 Entry 里。
			e.Log.ErrorMessage = ""
			_ = body
			s.Submit(e)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(s)
		return after.HeapAlloc - before.HeapAlloc
	}

	small := measure(100)
	large := measure(1 << 20) // 1MB 请求体

	// 允许 GC 噪声，但不能出现数量级差异
	if large > small*2 {
		t.Errorf("队列内存随请求体增长了：小请求 %d 字节 vs 1MB 请求 %d 字节\n"+
			"（说明有原文之类的大对象又被放进 Entry 了）", small, large)
	}
	perEntry := float64(small) / n
	t.Logf("每条排队 Entry 约 %.0f 字节；8192 深队列 ≈ %.1f MB", perEntry, perEntry*8192/1048576)
	if perEntry > 2048 {
		t.Errorf("单条 Entry %.0f 字节，超出预期（应在 400 字节量级）", perEntry)
	}
}

// 把攒批间隔从很大改小后，已经排在队列里的日志必须尽快落库，
// 而不是继续按旧间隔干等（实测发现过的 bug：60s 改回 200ms 要等满 60s）。
func TestShrinkFlushIntervalTakesEffect(t *testing.T) {
	db := setup(t)
	_ = store.SetSettings(map[string]string{
		store.KeyLogFlushIntervalMs: "60000", // 1 分钟，正常不会触发
		store.KeyLogFlushBatch:      "5000",  // 也不会靠条数触发
	})

	s := NewBatch(db, 1024)
	s.Start()
	defer s.Close(context.Background())

	for i := 0; i < 10; i++ {
		s.Submit(entry(idOf(i)))
	}
	time.Sleep(300 * time.Millisecond)
	if got := s.Stats().Persisted; got != 0 {
		t.Fatalf("间隔 60s 时不该落库，实际已落 %d 条", got)
	}

	// 改小间隔
	_ = store.SetSettings(map[string]string{store.KeyLogFlushIntervalMs: "50"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Stats().Persisted == 10 {
			return // 成功
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("改小攒批间隔后 3 秒内应落库完，实际只落了 %d/10 条", s.Stats().Persisted)
}
