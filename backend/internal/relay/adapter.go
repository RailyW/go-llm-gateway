// Package relay 负责把客户端（OpenAI 协议）的请求转发到上游。
//
// 协议适配层：当前只实现 openai 兼容上游（透传）。
// 之后要支持 anthropic / gemini 等原生协议时，只需新增一个文件实现 Adapter
// 接口并在 init 里 Register，Channel.Type 填对应名字即可，relay 主流程无需改动。
package relay

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/rin/go-llm-gateway/backend/internal/store"
)

// Usage token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatRequest 转发上下文。Body 是**已经把 model 换成上游模型名**的 OpenAI 格式请求体。
type ChatRequest struct {
	UpstreamModel string
	Stream        bool
	Body          []byte
	ClientHeader  http.Header
}

// Adapter 上游协议适配器。
type Adapter interface {
	// Name 适配器名，对应 Channel.Type
	Name() string

	// BuildRequest 构造发往上游的 HTTP 请求（URL、鉴权头、请求体格式转换都在这里做）
	BuildRequest(ctx context.Context, ch *store.Channel, req *ChatRequest) (*http.Request, error)

	// TransformResponse 非流式：上游响应体 -> 返回给客户端的 OpenAI 格式响应体
	TransformResponse(status int, body []byte) (int, []byte, error)

	// TransformStreamLine 流式：上游 SSE 的一行 -> 发给客户端的若干行。
	// 返回 nil 表示丢弃该行。openai 兼容上游直接原样返回。
	TransformStreamLine(line []byte) ([][]byte, error)

	// ExtractUsage 从非流式响应体或流式 data 载荷中提取 usage，取不到返回零值
	ExtractUsage(payload []byte) Usage
}

var (
	adaptersMu sync.RWMutex
	adapters   = map[string]Adapter{}
)

// Register 注册协议适配器（在各适配器文件的 init 中调用）。
func Register(a Adapter) {
	adaptersMu.Lock()
	defer adaptersMu.Unlock()
	adapters[a.Name()] = a
}

// GetAdapter 按 Channel.Type 取适配器。
func GetAdapter(name string) (Adapter, error) {
	if name == "" {
		name = "openai"
	}
	adaptersMu.RLock()
	defer adaptersMu.RUnlock()
	a, ok := adapters[name]
	if !ok {
		return nil, fmt.Errorf("unsupported channel type %q", name)
	}
	return a, nil
}

// AdapterNames 已注册的协议列表，给 WebUI 下拉框用。
func AdapterNames() []string {
	adaptersMu.RLock()
	defer adaptersMu.RUnlock()
	out := make([]string, 0, len(adapters))
	for k := range adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
