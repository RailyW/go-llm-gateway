package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponseFileIncremental(t *testing.T) {
	root := t.TempDir()
	a := NewArchiver(root)
	dir := a.DateDir(time.Now())

	rf, err := a.OpenResponse(dir, "req-1", 200)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟流式：逐行追加，内存里不留全量
	for i := 0; i < 1000; i++ {
		if _, err := rf.Write([]byte("data: {\"i\":1}\n\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(root, dir, ResponseFileName("req-1")))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "# request_id: req-1") || !strings.Contains(s, "# status: 200") {
		t.Errorf("缺少归档头: %q", s[:80])
	}
	if got := strings.Count(s, "data:"); got != 1000 {
		t.Errorf("落盘行数 = %d, want 1000", got)
	}
}

// 超过上限只写截断标记，避免超长响应打满磁盘
func TestResponseFileCap(t *testing.T) {
	old := MaxResponseBytes
	MaxResponseBytes = 100
	defer func() { MaxResponseBytes = old }()

	root := t.TempDir()
	a := NewArchiver(root)
	dir := a.DateDir(time.Now())
	rf, _ := a.OpenResponse(dir, "req-2", 200)
	for i := 0; i < 50; i++ {
		rf.Write([]byte(strings.Repeat("x", 20)))
	}
	rf.Close()

	b, _ := os.ReadFile(filepath.Join(root, dir, ResponseFileName("req-2")))
	if !strings.Contains(string(b), "归档已截断") {
		t.Error("超过上限应写入截断标记")
	}
	if len(b) > 100+len(truncatedMark)+200 { // 200 给头部留余量
		t.Errorf("截断后文件仍然过大: %d", len(b))
	}
}

// 请求协程当场写盘（不再经过异步队列）
func TestWriteRequestAndResponse(t *testing.T) {
	root := t.TempDir()
	a := NewArchiver(root)
	dir := a.DateDir(time.Now())

	meta := &RequestMeta{RequestID: "req-3", Time: time.Now(), Protocol: "openai-chat", Username: "u", GroupName: "g"}
	meta.Body = []byte(`{"model":"m"}`)
	if err := a.WriteRequest(dir, "req-3", meta); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteResponse(dir, "req-3", 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	req, resp, err := a.Read(dir, "req-3")
	if err != nil {
		t.Fatal(err)
	}
	var got RequestMeta
	if err := json.Unmarshal([]byte(req), &got); err != nil {
		t.Fatalf("请求归档不是合法 JSON: %v", err)
	}
	if got.RequestID != "req-3" || got.GroupName != "g" {
		t.Errorf("meta = %+v", got)
	}
	if !strings.Contains(resp, `{"ok":true}`) {
		t.Errorf("响应归档内容不对: %q", resp)
	}
}

// 非 JSON 的请求体也要能安全归档（包成字符串，不能写出坏 JSON）
func TestMarshalRequestInvalidBody(t *testing.T) {
	meta := &RequestMeta{RequestID: "req-4", Body: []byte("not-json{")}
	f := MarshalRequest("d", "req-4", meta)
	if !json.Valid(f.Data) {
		t.Errorf("归档文件必须是合法 JSON: %s", f.Data)
	}
}

func TestCleanup(t *testing.T) {
	root := t.TempDir()
	a := NewArchiver(root)
	old := time.Now().AddDate(0, 0, -10).Format(dateLayout)
	today := a.DateDir(time.Now())
	for _, d := range []string{old, today, "not-a-date"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	removed, err := a.Cleanup(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed = %v, want [%s]", removed, old)
	}
	for _, d := range []string{today, "not-a-date"} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Errorf("%s 不该被删除", d)
		}
	}
	if r, _ := a.Cleanup(0); r != nil {
		t.Error("保留天数 0 表示不清理")
	}
}

// 关闭归档时 ResponseFile 为 nil，写入必须安全丢弃（relay 里靠这个省掉分支）
func TestNilResponseFile(t *testing.T) {
	var rf *ResponseFile
	n, err := rf.Write([]byte("data: hello\n"))
	if err != nil || n != 12 {
		t.Errorf("nil 写入应静默成功: n=%d err=%v", n, err)
	}
	if err := rf.Close(); err != nil {
		t.Errorf("nil Close 应返回 nil: %v", err)
	}
}
