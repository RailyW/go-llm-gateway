package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/RailyW/go-llm-gateway/backend/internal/storetest"
)

// GORM 对「带 default 标签且值为 Go 零值」的字段会跳过写入，让数据库默认值生效。
// 这曾导致一个真实 bug：在 WebUI 创建上游/模型/key 时取消勾选「启用」，
// 存进库仍然是 enabled=true。因此所有 Enabled 字段都不能带 default:true，
// 这个测试就是钉住这个约定。
func TestCreateWithEnabledFalse(t *testing.T) {
	db := storetest.New(t)

	g := store.Group{Name: "off-group", Enabled: false}
	ch := store.Channel{Name: "off-ch", Protocols: ",openai-chat,", BaseURL: "http://x", Enabled: false}
	m := store.Model{Name: "off-model", Enabled: false}
	for _, v := range []any{&g, &ch, &m} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	ck := store.ChannelKey{ChannelID: ch.ID, GroupID: g.ID, Name: "off-key", Key: "k", Weight: 1, Enabled: false}
	b := store.Binding{ModelID: m.ID, ChannelID: ch.ID, UpstreamModel: "u", Weight: 1, Enabled: false}
	ak := store.APIKey{UserID: 1, Name: "off-ak", Key: "sk-off", Enabled: false}
	u := store.User{Username: "off-user", PasswordHash: "x", Role: store.RoleUser, GroupID: g.ID, Enabled: false}
	for _, v := range []any{&ck, &b, &ak, &u} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		out  any
		id   uint
		get  func(any) bool
	}{
		{"store.Group", &store.Group{}, g.ID, func(v any) bool { return v.(*store.Group).Enabled }},
		{"store.Channel", &store.Channel{}, ch.ID, func(v any) bool { return v.(*store.Channel).Enabled }},
		{"store.Model", &store.Model{}, m.ID, func(v any) bool { return v.(*store.Model).Enabled }},
		{"store.ChannelKey", &store.ChannelKey{}, ck.ID, func(v any) bool { return v.(*store.ChannelKey).Enabled }},
		{"store.Binding", &store.Binding{}, b.ID, func(v any) bool { return v.(*store.Binding).Enabled }},
		{"store.APIKey", &store.APIKey{}, ak.ID, func(v any) bool { return v.(*store.APIKey).Enabled }},
		{"store.User", &store.User{}, u.ID, func(v any) bool { return v.(*store.User).Enabled }},
	}
	for _, c := range cases {
		if err := db.First(c.out, c.id).Error; err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.get(c.out) {
			t.Errorf("%s: 创建时 Enabled=false，读回来却是 true（检查是否又加了 gorm default:true）", c.name)
		}
	}
}

// jsonb 列要能原样存取，并且 API 输出里是 JSON 而不是 base64。
func TestJSONBRoundTrip(t *testing.T) {
	db := storetest.New(t)

	raw := `{"input_tokens":9,"cache_read_input_tokens":3,"nested":{"a":[1,2]}}`
	rec := store.RequestLog{
		ID: "log-jsonb", Protocol: "anthropic-messages", StatusCode: 200,
		CreatedAt: time.Now(), Usage: store.JSONB(raw),
	}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}

	var got store.RequestLog
	if err := db.First(&got, "id = ?", "log-jsonb").Error; err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Usage, &m); err != nil {
		t.Fatalf("读回来的不是合法 JSON: %v (%s)", err, got.Usage)
	}
	if m["cache_read_input_tokens"] != float64(3) {
		t.Errorf("字段丢了: %v", m)
	}

	// 能用 jsonb 运算符直接查（这才是选 jsonb 而不是 text 的意义）
	var n int64
	if err := db.Model(&store.RequestLog{}).
		Where("usage->>'input_tokens' = ?", "9").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("jsonb 运算符查询命中 %d 行, want 1", n)
	}

	// 序列化进 API 响应时应该是对象，不是 base64 字符串
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	// 注意 jsonb 存的是解析后的结构，读回来 key 顺序会变（不是原文回放），所以只查子串
	if !strings.Contains(string(out), `"usage":{`) || !strings.Contains(string(out), `"input_tokens":9`) {
		t.Errorf("API 输出里 usage 应是 JSON 对象而不是 base64 字符串: %s", out)
	}

	// 空值写 NULL，不能往 jsonb 列里塞空串
	if err := db.Create(&store.RequestLog{ID: "log-null", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("Usage 为空时应能正常插入: %v", err)
	}
	// 非法 JSON 要在写库前就被拒绝，而不是等 PG 报错
	if err := db.Create(&store.RequestLog{ID: "log-bad", CreatedAt: time.Now(), Usage: store.JSONB("not-json{")}).Error; err == nil {
		t.Error("非法 JSON 应该写入失败")
	}
}
