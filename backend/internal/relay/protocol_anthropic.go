package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/rin/go-llm-gateway/backend/internal/store"
)

// ProtocolAnthropicMessages Anthropic 原生 /v1/messages，直转不翻译。
const ProtocolAnthropicMessages = "anthropic-messages"

const defaultAnthropicVersion = "2023-06-01"

func init() { Register(&AnthropicProtocol{}) }

// AnthropicProtocol 与 OpenAI 系的差异：x-api-key 鉴权 + anthropic-version 头 + usage 字段/分帧方式。
type AnthropicProtocol struct{}

func (p *AnthropicProtocol) Name() string         { return ProtocolAnthropicMessages }
func (p *AnthropicProtocol) Label() string        { return "Anthropic /messages" }
func (p *AnthropicProtocol) Vendor() string       { return "anthropic" }
func (p *AnthropicProtocol) InboundPath() string  { return "/v1/messages" }
func (p *AnthropicProtocol) UpstreamPath() string { return "/v1/messages" }

func (p *AnthropicProtocol) BuildRequest(ctx context.Context, ch *store.Channel, req *ProtoRequest) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, JoinURL(ch.BaseURL, p.UpstreamPath()), bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", req.APIKey)

	version := defaultAnthropicVersion
	if v := req.ClientHeader.Get("anthropic-version"); v != "" {
		version = v // 客户端指定了就用客户端的
	}
	r.Header.Set("anthropic-version", version)
	if req.Stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	passthroughHeaders(r.Header, req.ClientHeader, "anthropic-beta", "X-Request-Id")
	return r, nil
}

func (p *AnthropicProtocol) ParseRequest(body []byte) (string, bool, error) {
	return parseModelStream(body)
}

func (p *AnthropicProtocol) ReplaceModel(body []byte, upstreamModel string) ([]byte, error) {
	return replaceJSONModel(body, upstreamModel)
}

// MergeUsage 非流式在顶层 usage；流式里 input_tokens 在 message_start 的 message.usage，
// output_tokens 在 message_delta 的顶层 usage 中累计给出。
func (p *AnthropicProtocol) MergeUsage(payload []byte, acc *Usage) {
	mergeUsage(payload, acc, tokenStyleUsage, "message")
}

func (p *AnthropicProtocol) ErrorBody(status int, msg, requestID string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "gateway_error",
			"message": msg,
		},
		"request_id": requestID,
	})
	return b
}
