package store

import (
	"path/filepath"
	"testing"
)

// GORM 对「带 default 标签且值为 Go 零值」的字段会跳过写入，让数据库默认值生效。
// 这曾导致一个真实 bug：在 WebUI 创建上游/模型/key 时取消勾选「启用」，
// 存进库仍然是 enabled=true。因此所有 Enabled 字段都不能带 default:true，
// 这个测试就是钉住这个约定。
func TestCreateWithEnabledFalse(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"), "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}

	g := Group{Name: "off-group", Enabled: false}
	ch := Channel{Name: "off-ch", Protocols: ",openai-chat,", BaseURL: "http://x", Enabled: false}
	m := Model{Name: "off-model", Enabled: false}
	for _, v := range []any{&g, &ch, &m} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	ck := ChannelKey{ChannelID: ch.ID, GroupID: g.ID, Name: "off-key", Key: "k", Weight: 1, Enabled: false}
	b := Binding{ModelID: m.ID, ChannelID: ch.ID, UpstreamModel: "u", Weight: 1, Enabled: false}
	ak := APIKey{UserID: 1, Name: "off-ak", Key: "sk-off", Enabled: false}
	u := User{Username: "off-user", PasswordHash: "x", Role: RoleUser, GroupID: g.ID, Enabled: false}
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
		{"Group", &Group{}, g.ID, func(v any) bool { return v.(*Group).Enabled }},
		{"Channel", &Channel{}, ch.ID, func(v any) bool { return v.(*Channel).Enabled }},
		{"Model", &Model{}, m.ID, func(v any) bool { return v.(*Model).Enabled }},
		{"ChannelKey", &ChannelKey{}, ck.ID, func(v any) bool { return v.(*ChannelKey).Enabled }},
		{"Binding", &Binding{}, b.ID, func(v any) bool { return v.(*Binding).Enabled }},
		{"APIKey", &APIKey{}, ak.ID, func(v any) bool { return v.(*APIKey).Enabled }},
		{"User", &User{}, u.ID, func(v any) bool { return v.(*User).Enabled }},
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
