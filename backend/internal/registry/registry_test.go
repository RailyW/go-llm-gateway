package registry

import (
	"path/filepath"
	"testing"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

// seed 造一份典型配置：
//
//	渠道1 支持 openai-chat/responses，研发部(2) 两把 key、客服组(3) 一把
//	渠道2 只支持 anthropic-messages，只有研发部一把 key
//	渠道3 已禁用
func seed(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"), "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Create(&store.Group{Name: "rd", Enabled: true}).Error)   // id=2
	must(db.Create(&store.Group{Name: "cs", Enabled: true}).Error)   // id=3
	must(db.Create(&store.Group{Name: "off", Enabled: false}).Error) // id=4 禁用
	must(db.Create(&store.Channel{Name: "oai", Protocols: ",openai-chat,openai-responses,", BaseURL: "http://a", Enabled: true}).Error)
	must(db.Create(&store.Channel{Name: "ant", Protocols: ",anthropic-messages,", BaseURL: "http://b", Enabled: true}).Error)
	must(db.Create(&store.Channel{Name: "dead", Protocols: ",openai-chat,", BaseURL: "http://c", Enabled: false}).Error)

	keys := []store.ChannelKey{
		{ChannelID: 1, GroupID: 2, Name: "rd-a", Key: "k1", Weight: 1, Enabled: true},
		{ChannelID: 1, GroupID: 2, Name: "rd-b", Key: "k2", Weight: 1, Enabled: true},
		{ChannelID: 1, GroupID: 2, Name: "rd-off", Key: "k3", Weight: 1, Enabled: false}, // 禁用的不进快照
		{ChannelID: 1, GroupID: 3, Name: "cs-a", Key: "k4", Weight: 1, Enabled: true},
		{ChannelID: 1, GroupID: 4, Name: "off-a", Key: "k5", Weight: 1, Enabled: true}, // 归属禁用
		{ChannelID: 2, GroupID: 2, Name: "rd-ant", Key: "k6", Weight: 1, Enabled: true},
		{ChannelID: 3, GroupID: 2, Name: "dead-a", Key: "k7", Weight: 1, Enabled: true}, // 渠道禁用
	}
	for i := range keys {
		must(db.Create(&keys[i]).Error)
	}
	must(db.Create(&store.Model{Name: "m1", Enabled: true}).Error)
	must(db.Create(&store.Model{Name: "off", Enabled: false}).Error)
	must(db.Create(&store.Binding{ModelID: 1, ChannelID: 1, UpstreamModel: "gpt", Weight: 1, Enabled: true}).Error)
	must(db.Create(&store.Binding{ModelID: 1, ChannelID: 2, UpstreamModel: "claude", Weight: 1, Enabled: true}).Error)
	must(db.Create(&store.Binding{ModelID: 1, ChannelID: 3, UpstreamModel: "x", Weight: 1, Enabled: true}).Error)  // 渠道禁用
	must(db.Create(&store.Binding{ModelID: 1, ChannelID: 1, UpstreamModel: "y", Weight: 1, Enabled: false}).Error) // 绑定禁用

	must(db.Create(&store.User{Username: "dev", PasswordHash: "x", Role: store.RoleUser, GroupID: 2, Enabled: true}).Error) // id=2
	must(db.Create(&store.User{Username: "ban", PasswordHash: "x", Role: store.RoleUser, GroupID: 2, Enabled: false}).Error)
	must(db.Create(&store.APIKey{UserID: 2, Name: "dev-key", Key: "sk-dev", Enabled: true}).Error)
	must(db.Create(&store.APIKey{UserID: 2, Name: "dev-off", Key: "sk-off", Enabled: false}).Error)
	must(db.Create(&store.APIKey{UserID: 3, Name: "ban-key", Key: "sk-ban", Enabled: true}).Error)
	return db
}

func TestSnapshotCaller(t *testing.T) {
	r, err := New(seed(t))
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Get()

	c, ok := snap.Caller("sk-dev")
	if !ok {
		t.Fatal("网关 key 应能查到")
	}
	if c.Username != "dev" || c.GroupID != 2 || c.GroupName != "rd" || !c.UserEnabled || !c.APIKeyEnabled {
		t.Errorf("caller = %+v", c)
	}
	if c2, _ := snap.Caller("sk-off"); c2 == nil || c2.APIKeyEnabled {
		t.Error("禁用的 key 也要在快照里（由中间件判断并给出明确错误）")
	}
	if c3, _ := snap.Caller("sk-ban"); c3 == nil || c3.UserEnabled {
		t.Error("禁用用户的 key 同理")
	}
	if _, ok := snap.Caller("sk-nope"); ok {
		t.Error("不存在的 key 不该命中")
	}
}

func TestSnapshotKeysFilter(t *testing.T) {
	r, _ := New(seed(t))
	snap := r.Get()

	if got := len(snap.ChannelKeys(1, 2)); got != 2 {
		t.Errorf("渠道1/研发部 可用 key = %d, want 2（禁用那把要排除）", got)
	}
	if got := len(snap.ChannelKeys(1, 3)); got != 1 {
		t.Errorf("渠道1/客服组 = %d, want 1", got)
	}
	if snap.HasKeyFor(1, 4) {
		t.Error("归属被禁用时不应有可用 key")
	}
	if snap.HasKeyFor(3, 2) {
		t.Error("渠道被禁用时不应有可用 key")
	}
	if snap.HasKeyFor(2, 3) {
		t.Error("客服组在 anthropic 渠道下没有 key")
	}
	if !snap.HasKeyFor(2, 2) {
		t.Error("研发部在 anthropic 渠道下应有 key")
	}
}

func TestSnapshotModelBindings(t *testing.T) {
	r, _ := New(seed(t))
	snap := r.Get()

	m, ok := snap.Model("m1")
	if !ok || !m.Enabled {
		t.Fatal("m1 应存在且启用")
	}
	// 禁用的绑定、禁用渠道的绑定都要排除，剩 2 条
	if len(m.Bindings) != 2 {
		t.Fatalf("可用绑定 = %d, want 2: %+v", len(m.Bindings), m.Bindings)
	}
	for _, b := range m.Bindings {
		if b.Channel == nil {
			t.Fatal("Channel 必须已填充（热路径不再查库）")
		}
		if len(b.Channel.ProtocolList) == 0 {
			t.Error("ProtocolList 必须已展开")
		}
	}
	if off, _ := snap.Model("off"); off == nil || off.Enabled {
		t.Error("禁用模型仍在快照里，由 relay 给出明确错误")
	}
	if _, ok := snap.Model("nope"); ok {
		t.Error("不存在的模型不该命中")
	}
}

// 改库后 Invalidate 必须能立刻反映出来
func TestInvalidate(t *testing.T) {
	db := seed(t)
	r, _ := New(db)
	if !r.Get().HasKeyFor(1, 2) {
		t.Fatal("初始应有 key")
	}

	db.Model(&store.ChannelKey{}).Where("channel_id = ? AND group_id = ?", 1, 2).Update("enabled", false)
	if !r.Get().HasKeyFor(1, 2) {
		t.Error("未失效前应仍读到旧快照")
	}
	if err := r.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if r.Get().HasKeyFor(1, 2) {
		t.Error("失效后应读到新配置")
	}
	if st := r.Stats(); st["reloads"].(uint64) < 2 {
		t.Errorf("reloads = %v", st["reloads"])
	}
}
