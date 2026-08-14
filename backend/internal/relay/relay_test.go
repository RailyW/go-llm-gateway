package relay

import (
	"encoding/json"
	"testing"

	"github.com/rin/go-llm-gateway/backend/internal/relay/selector"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

func TestJoinURL(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com":       "https://api.openai.com/v1/chat/completions",
		"https://api.openai.com/":      "https://api.openai.com/v1/chat/completions",
		"https://api.openai.com/v1":    "https://api.openai.com/v1/chat/completions",
		"https://x.com/proxy/v1/":      "https://x.com/proxy/v1/chat/completions",
		"http://127.0.0.1:9911":        "http://127.0.0.1:9911/v1/chat/completions",
	}
	for base, want := range cases {
		if got := JoinURL(base, "/v1/chat/completions"); got != want {
			t.Errorf("JoinURL(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestReplaceModel(t *testing.T) {
	in := []byte(`{"model":"public-name","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := replaceModel(in, "upstream-real")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "upstream-real" {
		t.Errorf("model = %v", got["model"])
	}
	if got["stream"] != true {
		t.Errorf("stream 字段丢失: %v", got)
	}
	if _, ok := got["messages"]; !ok {
		t.Error("messages 字段丢失")
	}
}

func TestSSEPayload(t *testing.T) {
	if p, ok := ssePayload([]byte(`data: {"a":1}`)); !ok || string(p) != `{"a":1}` {
		t.Errorf("payload = %q ok=%v", p, ok)
	}
	if _, ok := ssePayload([]byte("data: [DONE]")); ok {
		t.Error("[DONE] 不应被当作载荷")
	}
	if _, ok := ssePayload([]byte("event: ping")); ok {
		t.Error("非 data 行不应被当作载荷")
	}
}

func TestExtractUsage(t *testing.T) {
	a := &OpenAIAdapter{}
	u := a.ExtractUsage([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	if u.TotalTokens != 7 || u.PromptTokens != 3 || u.CompletionTokens != 4 {
		t.Errorf("usage = %+v", u)
	}
	if got := a.ExtractUsage([]byte(`{"choices":[]}`)); got.TotalTokens != 0 {
		t.Errorf("无 usage 时应为零值: %+v", got)
	}
}

func TestSelectors(t *testing.T) {
	bindings := []store.Binding{{ID: 1, Weight: 1}, {ID: 2, Weight: 5}}
	for _, name := range []string{"random", "weighted", "unknown-fallback"} {
		b, err := selector.Get(name).Select(bindings)
		if err != nil || b == nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := selector.Get("random").Select(nil); err == nil {
		t.Error("空候选应报错")
	}
}
