package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rin/go-llm-gateway/backend/internal/store"
)

func init() { Register(&OpenAIAdapter{}) }

// OpenAIAdapter openai 兼容上游：请求体/响应体都不做协议翻译，只换 URL、鉴权头和模型名。
type OpenAIAdapter struct{}

func (a *OpenAIAdapter) Name() string { return "openai" }

func (a *OpenAIAdapter) BuildRequest(ctx context.Context, ch *store.Channel, req *ChatRequest) (*http.Request, error) {
	url := JoinURL(ch.BaseURL, "/v1/chat/completions")
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+ch.APIKey)
	if req.Stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	// 少量客户端头透传（不带客户端的 Authorization）
	for _, h := range []string{"OpenAI-Organization", "OpenAI-Beta", "X-Request-Id"} {
		if v := req.ClientHeader.Get(h); v != "" {
			r.Header.Set(h, v)
		}
	}
	return r, nil
}

func (a *OpenAIAdapter) TransformResponse(status int, body []byte) (int, []byte, error) {
	return status, body, nil
}

func (a *OpenAIAdapter) TransformStreamLine(line []byte) ([][]byte, error) {
	return [][]byte{line}, nil
}

func (a *OpenAIAdapter) ExtractUsage(payload []byte) Usage {
	var resp struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil || resp.Usage == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}
}

// JoinURL 拼接 base_url 与路径。base_url 已带 /v1 时不重复追加。
func JoinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}
