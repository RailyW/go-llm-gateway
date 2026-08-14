// Package relay 负责把客户端请求**同协议直转**到上游：
//
//	客户端 POST /v1/chat/completions  ->  上游 {base_url}/v1/chat/completions
//	客户端 POST /v1/responses         ->  上游 {base_url}/v1/responses
//	客户端 POST /v1/messages          ->  上游 {base_url}/v1/messages   (Anthropic 原生)
//
// 全程**不做协议翻译**：请求体除了把 model 换成上游模型名之外原样透传，
// 响应（含 SSE 流）原样回吐。各协议之间的差异只有三点，由 Protocol 实现封装：
//  1. 路径（网关入口路径 / 上游路径）
//  2. 鉴权头（openai: Authorization Bearer；anthropic: x-api-key + anthropic-version）
//  3. usage 字段位置与命名、以及网关自身错误体的格式
//
// 新增一个协议 = 新增一个 Protocol 实现 + init 里 Register，路由和上游可选协议会自动出现。
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
)

// Usage token 用量（各协议字段名不同，统一归一到这里）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ProtoRequest 一次转发的请求上下文。Body 已把 model 换成上游模型名，其余原样。
type ProtoRequest struct {
	UpstreamModel string
	APIKey        string // 本次选中的**上游 key**（按归属挑出来的）
	Stream        bool
	Body          []byte
	ClientHeader  http.Header
}

// Protocol 一个端点协议（同协议直转，不翻译）。
type Protocol interface {
	// Name 唯一键，同时是上游可选协议的取值，如 openai-chat / openai-responses / anthropic-messages
	Name() string
	// Label WebUI 展示名
	Label() string
	// Vendor 厂商/协议族，UI 分组用（openai / anthropic）
	Vendor() string
	// InboundPath 网关对外暴露的路径，如 /v1/chat/completions
	InboundPath() string
	// UpstreamPath 上游对应路径（一般与 InboundPath 相同）
	UpstreamPath() string

	// BuildRequest 构造发往上游的请求：URL + 鉴权头 + 原样 body
	BuildRequest(ctx context.Context, ch *store.Channel, req *ProtoRequest) (*http.Request, error)
	// ParseRequest 从请求体里读出 model 与是否流式
	ParseRequest(body []byte) (model string, stream bool, err error)
	// ReplaceModel 把请求体里的 model 换成上游模型名
	ReplaceModel(body []byte, upstreamModel string) ([]byte, error)
	// MergeUsage 从非流式响应体或一条 SSE data 载荷里累积 usage（anthropic 会分帧给）
	MergeUsage(payload []byte, acc *Usage)
	// ErrorBody 网关自身出错时返回的错误体（各协议格式不同）
	ErrorBody(status int, msg, requestID string) []byte
}

var (
	protoMu   sync.RWMutex
	protocols = map[string]Protocol{}
)

// Register 注册协议（各协议文件的 init 中调用）。
func Register(p Protocol) {
	protoMu.Lock()
	defer protoMu.Unlock()
	protocols[p.Name()] = p
}

func GetProtocol(name string) (Protocol, error) {
	protoMu.RLock()
	defer protoMu.RUnlock()
	p, ok := protocols[name]
	if !ok {
		return nil, fmt.Errorf("unsupported protocol %q", name)
	}
	return p, nil
}

// Protocols 按 name 排序的全部协议，用于注册路由和 WebUI 多选。
func Protocols() []Protocol {
	protoMu.RLock()
	defer protoMu.RUnlock()
	out := make([]Protocol, 0, len(protocols))
	for _, p := range protocols {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ProtocolInfo 给前端的协议描述。
type ProtocolInfo struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Vendor  string `json:"vendor"`
	Path    string `json:"path"`
	Default bool   `json:"default"`
}

func ProtocolInfos() []ProtocolInfo {
	list := Protocols()
	out := make([]ProtocolInfo, 0, len(list))
	for _, p := range list {
		out = append(out, ProtocolInfo{
			Name:    p.Name(),
			Label:   p.Label(),
			Vendor:  p.Vendor(),
			Path:    p.InboundPath(),
			Default: p.Name() == ProtocolOpenAIChat,
		})
	}
	return out
}

// ---------- 上游支持的协议集合（Channel.Protocols，形如 ",openai-chat,openai-responses," ）----------

// NormalizeProtocols 校验并归一化协议列表为可 LIKE 匹配的存储串。
func NormalizeProtocols(names []string) (string, error) {
	seen := map[string]bool{}
	var ok []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		if _, err := GetProtocol(n); err != nil {
			return "", fmt.Errorf("未知协议: %s", n)
		}
		seen[n] = true
		ok = append(ok, n)
	}
	if len(ok) == 0 {
		return "", fmt.Errorf("至少要选择一个协议")
	}
	sort.Strings(ok)
	return store.JoinProtocols(ok), nil
}

// SplitProtocols 把存储串还原成列表（等价 store.SplitProtocols）。
func SplitProtocols(s string) []string { return store.SplitProtocols(s) }

// ---------- 共用小工具 ----------

// JoinURL 拼接 base_url 与上游路径；base_url 已带 /v1 时不重复追加。
func JoinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

// parseModelStream 三个协议的请求体都是顶层 model + stream，共用一份实现。
func parseModelStream(body []byte) (string, bool, error) {
	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return "", false, err
	}
	return head.Model, head.Stream, nil
}

// replaceJSONModel 只改写顶层 model 字段，其余字段原样保留。
func replaceJSONModel(body []byte, upstreamModel string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	v, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	obj["model"] = v
	return json.Marshal(obj)
}

// usageKeys 各协议 usage 的字段名。
type usageKeys struct {
	in, out, total string
}

var (
	openAIChatUsage = usageKeys{in: "prompt_tokens", out: "completion_tokens", total: "total_tokens"}
	tokenStyleUsage = usageKeys{in: "input_tokens", out: "output_tokens", total: "total_tokens"} // responses / anthropic
)

// mergeUsage 在 payload 里找 usage 并累积到 acc。
// nests 用于 usage 被包在别的对象里的情况（responses 的 response.usage、anthropic 的 message.usage）。
func mergeUsage(payload []byte, acc *Usage, k usageKeys, nests ...string) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return
	}
	raw, ok := obj["usage"]
	if !ok {
		for _, n := range nests {
			nested, ok2 := obj[n]
			if !ok2 {
				continue
			}
			var inner map[string]json.RawMessage
			if json.Unmarshal(nested, &inner) != nil {
				continue
			}
			if r, ok3 := inner["usage"]; ok3 {
				raw = r
				ok = true
				break
			}
		}
	}
	if !ok {
		return
	}
	var fields map[string]json.Number
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}
	num := func(key string) int {
		if v, ok := fields[key]; ok {
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		}
		return 0
	}
	// 非零才覆盖：anthropic 的 input_tokens 只在 message_start 出现，
	// output_tokens 在 message_delta 里累计给出。
	if v := num(k.in); v > 0 {
		acc.PromptTokens = v
	}
	if v := num(k.out); v > 0 {
		acc.CompletionTokens = v
	}
	if v := num(k.total); v > 0 {
		acc.TotalTokens = v
	} else if acc.PromptTokens+acc.CompletionTokens > 0 {
		acc.TotalTokens = acc.PromptTokens + acc.CompletionTokens
	}
}
