package sink

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/archive"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/RailyW/go-llm-gateway/backend/internal/storetest"
	"gorm.io/gorm"
)

func setup(t *testing.T) (*gorm.DB, *archive.Archiver, string) {
	t.Helper()
	db := storetest.New(t)
	root := filepath.Join(t.TempDir(), "archive")
	return db, archive.NewArchiver(root), root
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
		Files: []archive.File{
			{DateDir: "2026-01-01", Name: archive.RequestFileName(id), Data: []byte(`{"a":1}`)},
			{DateDir: "2026-01-01", Name: archive.ResponseFileName(id), Data: []byte(`ok`)},
		},
		TouchGatewayKeyID: 1,
		TouchChannelKeyID: 1,
	}
}

// 攒批落库：投递 N 条 -> Close 后应全部入库，且归档文件都写了
func TestBatchPersistsAllOnClose(t *testing.T) {
	db, arch, root := setup(t)
	_ = store.SetSettings(map[string]string{store.KeyLogFlushIntervalMs: "20", store.KeyLogFlushBatch: "10"})

	s := NewBatch(db, arch, 1024)
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
	// 归档文件
	for i := 0; i < n; i++ {
		p := filepath.Join(root, "2026-01-01", archive.RequestFileName(idOf(i)))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("归档文件缺失: %v", err)
		}
	}
}

// 队列满时必须丢弃并计数，绝不阻塞请求
func TestBatchDropsWhenFull(t *testing.T) {
	db, arch, _ := setup(t)
	s := NewBatch(db, arch, 2) // 故意开很小，且不 Start()，让队列填满

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
	db, arch, _ := setup(t)
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

	s := NewBatch(db, arch, 1024)
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
	db, arch, _ := setup(t)
	s := NewBatch(db, arch, 8)
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
