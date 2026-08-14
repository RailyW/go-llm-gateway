package relay

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/RailyW/go-llm-gateway/backend/internal/relay/keyselector"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay/selector"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
)

func TestJoinURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://api.openai.com", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/", "/v1/responses", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.anthropic.com", "/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"http://127.0.0.1:9911/v1/", "/v1/messages", "http://127.0.0.1:9911/v1/messages"},
	}
	for _, c := range cases {
		if got := JoinURL(c.base, c.path); got != c.want {
			t.Errorf("JoinURL(%q,%q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

// 每个协议的入口路径必须唯一，且注册表能反查
func TestProtocolRegistry(t *testing.T) {
	seen := map[string]string{}
	for _, p := range Protocols() {
		if prev, dup := seen[p.InboundPath()]; dup {
			t.Errorf("路径冲突 %s: %s 与 %s", p.InboundPath(), prev, p.Name())
		}
		seen[p.InboundPath()] = p.Name()
		if _, err := GetProtocol(p.Name()); err != nil {
			t.Errorf("GetProtocol(%s): %v", p.Name(), err)
		}
	}
	for _, want := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		if seen[want] == "" {
			t.Errorf("缺少端点 %s", want)
		}
	}
}

func TestReplaceModelKeepsBody(t *testing.T) {
	in := []byte(`{"model":"public-name","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	for _, p := range Protocols() {
		out, err := p.ReplaceModel(in, "upstream-real")
		if err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got["model"] != "upstream-real" {
			t.Errorf("%s: model = %v", p.Name(), got["model"])
		}
		if got["stream"] != true || got["max_tokens"] == nil || got["messages"] == nil {
			t.Errorf("%s: 其他字段被改动了: %v", p.Name(), got)
		}
		model, stream, err := p.ParseRequest(out)
		if err != nil || model != "upstream-real" || !stream {
			t.Errorf("%s: ParseRequest = %q %v %v", p.Name(), model, stream, err)
		}
	}
}

// 上游鉴权头：openai 用 Bearer，anthropic 用 x-api-key + anthropic-version
func TestBuildRequestAuth(t *testing.T) {
	ch := &store.Channel{BaseURL: "http://up.local"}
	req := &ProtoRequest{
		UpstreamModel: "m",
		APIKey:        "up-key", // 由归属挑出来的上游 key
		Stream:        true,
		Body:          []byte(`{}`),
		ClientHeader:  httptest.NewRequest("POST", "/", nil).Header,
	}

	chat, _ := GetProtocol(ProtocolOpenAIChat)
	r, err := chat.BuildRequest(t.Context(), ch, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.URL.String() != "http://up.local/v1/chat/completions" {
		t.Errorf("url = %s", r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer up-key" {
		t.Errorf("authorization = %q", r.Header.Get("Authorization"))
	}

	resp, _ := GetProtocol(ProtocolOpenAIResponses)
	r2, _ := resp.BuildRequest(t.Context(), ch, req)
	if r2.URL.String() != "http://up.local/v1/responses" {
		t.Errorf("url = %s", r2.URL)
	}

	ant, _ := GetProtocol(ProtocolAnthropicMessages)
	r3, err := ant.BuildRequest(t.Context(), ch, req)
	if err != nil {
		t.Fatal(err)
	}
	if r3.URL.String() != "http://up.local/v1/messages" {
		t.Errorf("url = %s", r3.URL)
	}
	if r3.Header.Get("x-api-key") != "up-key" || r3.Header.Get("Authorization") != "" {
		t.Errorf("anthropic 鉴权头不对: %v", r3.Header)
	}
	if r3.Header.Get("anthropic-version") != defaultAnthropicVersion {
		t.Errorf("anthropic-version = %q", r3.Header.Get("anthropic-version"))
	}
}

func TestUsagePerProtocol(t *testing.T) {
	// tokens 只比三个归一化后的数字；Raw 单独校验（它是 []byte，结构体不可比较）
	tokens := func(u Usage) [3]int { return [3]int{u.PromptTokens, u.CompletionTokens, u.TotalTokens} }

	chat, _ := GetProtocol(ProtocolOpenAIChat)
	var u Usage
	chat.MergeUsage([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), &u)
	if tokens(u) != [3]int{3, 4, 7} {
		t.Errorf("chat usage = %+v", u)
	}

	// responses：input/output 命名，流式事件里包在 response 下
	resp, _ := GetProtocol(ProtocolOpenAIResponses)
	u = Usage{}
	resp.MergeUsage([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":5}}}`), &u)
	if tokens(u) != [3]int{11, 5, 16} {
		t.Errorf("responses usage = %+v", u)
	}

	// anthropic：input 在 message_start 的 message.usage，output 在 message_delta 顶层 usage
	ant, _ := GetProtocol(ProtocolAnthropicMessages)
	u = Usage{}
	ant.MergeUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":1}}}`), &u)
	ant.MergeUsage([]byte(`{"type":"content_block_delta","delta":{"text":"hi"}}`), &u)
	ant.MergeUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":42}}`), &u)
	if tokens(u) != [3]int{9, 42, 51} {
		t.Errorf("anthropic usage = %+v", u)
	}
}

// 原始 usage 对象要原样留下来（落进 logs.usage jsonb），
// 归一化只取三个数字，各家自己的扩展字段不能丢。
func TestUsageRawPreserved(t *testing.T) {
	ant, _ := GetProtocol(ProtocolAnthropicMessages)
	var u Usage
	ant.MergeUsage([]byte(`{"usage":{"input_tokens":9,"output_tokens":2,`+
		`"cache_creation_input_tokens":100,"cache_read_input_tokens":7}}`), &u)

	var got map[string]int
	if err := json.Unmarshal(u.Raw, &got); err != nil {
		t.Fatalf("Raw 不是合法 JSON: %v (%s)", err, u.Raw)
	}
	if got["cache_creation_input_tokens"] != 100 || got["cache_read_input_tokens"] != 7 {
		t.Errorf("扩展字段丢了: %v", got)
	}

	// 流式要逐帧**合并**而不是取最后一帧：anthropic 的 input_tokens 只在
	// message_start 出现，output_tokens 在 message_delta 里更新，取最后一帧会丢 input。
	u = Usage{}
	ant.MergeUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":9,"cache_read_input_tokens":3}}}`), &u)
	ant.MergeUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":42}}`), &u)
	if err := json.Unmarshal(u.Raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["input_tokens"] != 9 || got["output_tokens"] != 42 || got["cache_read_input_tokens"] != 3 {
		t.Errorf("流式 Raw 应该是各帧合并的结果: %v", got)
	}
}

// 网关自身的错误体要符合各协议的格式
func TestErrorBodyShape(t *testing.T) {
	chat, _ := GetProtocol(ProtocolOpenAIChat)
	var oe struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal(chat.ErrorBody(404, "boom", "rid"), &oe); err != nil || oe.Error.Message != "boom" {
		t.Errorf("openai error body: %v %+v", err, oe)
	}

	ant, _ := GetProtocol(ProtocolAnthropicMessages)
	var ae struct {
		Type  string                         `json:"type"`
		Error struct{ Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal(ant.ErrorBody(404, "boom", "rid"), &ae); err != nil || ae.Type != "error" || ae.Error.Message != "boom" {
		t.Errorf("anthropic error body: %v %+v", err, ae)
	}
}

func TestProtocolStorageFormat(t *testing.T) {
	s, err := relayNormalize(t, []string{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIChat})
	if err != nil {
		t.Fatal(err)
	}
	if s != ",anthropic-messages,openai-chat," {
		t.Errorf("storage = %q", s)
	}
	if got := SplitProtocols(s); len(got) != 2 {
		t.Errorf("split = %v", got)
	}
	if _, err := NormalizeProtocols([]string{"nope"}); err == nil {
		t.Error("未知协议应报错")
	}
	if _, err := NormalizeProtocols(nil); err == nil {
		t.Error("空列表应报错")
	}
}

func relayNormalize(t *testing.T, in []string) (string, error) {
	t.Helper()
	return NormalizeProtocols(in)
}

func TestSSEPayload(t *testing.T) {
	if p, ok := ssePayload([]byte(`data: {"a":1}`)); !ok || string(p) != `{"a":1}` {
		t.Errorf("payload = %q ok=%v", p, ok)
	}
	if _, ok := ssePayload([]byte("data: [DONE]")); ok {
		t.Error("[DONE] 不应被当作载荷")
	}
	if _, ok := ssePayload([]byte("event: message_start")); ok {
		t.Error("非 data 行不应被当作载荷")
	}
}

// 上游 key 选择：随机/加权都要选出来；亲和性策略要稳定
func TestKeySelectors(t *testing.T) {
	keys := []store.ChannelKey{{ID: 7, Weight: 1}, {ID: 3, Weight: 4}, {ID: 11, Weight: 1}}
	ctx := keyselector.Context{ChannelID: 1, GroupID: 2, UserID: 3, GatewayKeyID: 9, AffinityKey: "gwkey:9"}

	for _, name := range []string{"random", "weighted", "affinity-hash", "unknown-fallback"} {
		k, err := keyselector.Get(name).Select(ctx, keys)
		if err != nil || k == nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := keyselector.Get("random").Select(ctx, nil); err == nil {
		t.Error("空 key 列表应报错")
	}

	// 亲和性：同一 AffinityKey 多次选择必须落在同一把 key 上
	aff := keyselector.Get("affinity-hash")
	first, _ := aff.Select(ctx, keys)
	for i := 0; i < 20; i++ {
		got, _ := aff.Select(ctx, keys)
		if got.ID != first.ID {
			t.Fatalf("亲和性不稳定: %d vs %d", got.ID, first.ID)
		}
	}
	// AffinityKey 为空时退化为随机（不报错即可）
	if _, err := aff.Select(keyselector.Context{}, keys); err != nil {
		t.Error(err)
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
