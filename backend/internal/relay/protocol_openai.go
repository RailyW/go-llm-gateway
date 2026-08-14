package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/rin/go-llm-gateway/backend/internal/store"
)

// OpenAI 系协议：/v1/chat/completions 与 /v1/responses，各自直转到上游同名端点。
const (
	ProtocolOpenAIChat      = "openai-chat"
	ProtocolOpenAIResponses = "openai-responses"
)

func init() {
	Register(&OpenAIProtocol{
		name:  ProtocolOpenAIChat,
		label: "OpenAI /chat/completions",
		path:  "/v1/chat/completions",
		usage: openAIChatUsage,
	})
	Register(&OpenAIProtocol{
		name:  ProtocolOpenAIResponses,
		label: "OpenAI /responses",
		path:  "/v1/responses",
		usage: tokenStyleUsage,
		// responses 的流式事件把 usage 放在 response.usage 里
		usageNests: []string{"response"},
	})
}

// OpenAIProtocol OpenAI 协议族（Bearer 鉴权），不同端点只有路径与 usage 字段差异。
type OpenAIProtocol struct {
	name       string
	label      string
	path       string
	usage      usageKeys
	usageNests []string
}

func (p *OpenAIProtocol) Name() string         { return p.name }
func (p *OpenAIProtocol) Label() string        { return p.label }
func (p *OpenAIProtocol) Vendor() string       { return "openai" }
func (p *OpenAIProtocol) InboundPath() string  { return p.path }
func (p *OpenAIProtocol) UpstreamPath() string { return p.path }

func (p *OpenAIProtocol) BuildRequest(ctx context.Context, ch *store.Channel, req *ProtoRequest) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, JoinURL(ch.BaseURL, p.UpstreamPath()), bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+req.APIKey)
	if req.Stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	// 少量与协议相关的客户端头透传（不带客户端自己的 Authorization）
	passthroughHeaders(r.Header, req.ClientHeader, "OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta", "X-Request-Id")
	return r, nil
}

func (p *OpenAIProtocol) ParseRequest(body []byte) (string, bool, error) {
	return parseModelStream(body)
}

func (p *OpenAIProtocol) ReplaceModel(body []byte, upstreamModel string) ([]byte, error) {
	return replaceJSONModel(body, upstreamModel)
}

func (p *OpenAIProtocol) MergeUsage(payload []byte, acc *Usage) {
	mergeUsage(payload, acc, p.usage, p.usageNests...)
}

func (p *OpenAIProtocol) ErrorBody(status int, msg, requestID string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message":    msg,
			"type":       "gateway_error",
			"code":       status,
			"request_id": requestID,
		},
	})
	return b
}

func passthroughHeaders(dst, src http.Header, keys ...string) {
	if src == nil {
		return
	}
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}
