package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/archive"
	"github.com/RailyW/go-llm-gateway/backend/internal/registry"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay/keyselector"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay/selector"
	"github.com/RailyW/go-llm-gateway/backend/internal/sink"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/google/uuid"
)

const maxRequestBody = 20 << 20 // 20MB

// Actor 调用方身份（由网关鉴权中间件从 registry 快照里解析出来）。
type Actor struct {
	UserID     uint
	Username   string
	GroupID    uint // 归属：决定能用哪些上游 key
	GroupName  string
	APIKeyID   uint
	APIKeyName string
	ClientIP   string
}

// GroupLabel 归属展示名。
func (a Actor) GroupLabel() string {
	if a.GroupName != "" {
		return a.GroupName
	}
	return fmt.Sprintf("#%d", a.GroupID)
}

// AffinityKey 亲和性维度：默认按「网关 key」粘住同一把上游 key。
func (a Actor) AffinityKey() string {
	if a.APIKeyID > 0 {
		return fmt.Sprintf("gwkey:%d", a.APIKeyID)
	}
	return fmt.Sprintf("user:%d", a.UserID)
}

type Service struct {
	reg      *registry.Registry
	sink     sink.Sink
	archiver *archive.Archiver
	client   *http.Client
}

func NewService(reg *registry.Registry, sk sink.Sink, archiver *archive.Archiver) *Service {
	return &Service{
		reg:      reg,
		sink:     sk,
		archiver: archiver,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        512,
				MaxIdleConnsPerHost: 128,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// call 一次转发的可变状态。
//
// 注意这里**不再持有原文字节**：归档在请求协程里当场写盘，
// 交给 sink 的只有 ~400 字节的日志行。早期版本把原文也塞进队列，
// 于是队列内存取决于请求体大小——每条 64KB 原文 × 8192 深 = 1GB。
type call struct {
	proto   Protocol
	rec     *store.RequestLog
	meta    *archive.RequestMeta
	dateDir string
	// archiving 本次是否归档原文（设置页开关，默认关）；
	// 请求开始时读一次并固定住，避免一次请求中途改配置导致只写了一半
	archiving bool
	// respFile 响应归档文件（流式与非流式都用它，边收边写）
	respFile  *archive.ResponseFile
	submitted bool
}

// Handle 处理某个协议端点的一次转发。整个流程与具体协议无关：
// 解析 model -> 选上游(支持该协议 + 该归属下有 key) -> 选上游 key -> 换模型名 -> 直转 -> 异步落库。
func (s *Service) Handle(p Protocol, w http.ResponseWriter, r *http.Request, actor Actor) {
	start := time.Now()
	reqID := uuid.NewString()
	dateDir := s.archiver.DateDir(start)

	c := &call{
		proto:     p,
		dateDir:   dateDir,
		archiving: store.ArchiveEnabled(),
		rec: &store.RequestLog{
			ID:          reqID,
			Protocol:    p.Name(),
			Endpoint:    p.InboundPath(),
			UserID:      actor.UserID,
			Username:    actor.Username,
			GroupID:     actor.GroupID,
			GroupName:   actor.GroupName,
			APIKeyID:    actor.APIKeyID,
			APIKeyName:  actor.APIKeyName,
			ClientIP:    actor.ClientIP,
			CreatedAt:   start,
			ArchivePath: dateDir,
		},
		meta: &archive.RequestMeta{
			RequestID:  reqID,
			Time:       start,
			Protocol:   p.Name(),
			Endpoint:   p.InboundPath(),
			Username:   actor.Username,
			GroupName:  actor.GroupName,
			APIKeyName: actor.APIKeyName,
			ClientIP:   actor.ClientIP,
		},
	}
	defer s.submit(c) // 无论走哪条分支，只投递一次

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		s.fail(w, c, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	c.meta.Body = body
	c.meta.ClientHeaders = pickHeaders(r.Header)

	model, stream, err := p.ParseRequest(body)
	if err != nil {
		s.fail(w, c, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	c.rec.ModelName, c.rec.Stream = model, stream
	c.meta.RequestedModel, c.meta.Stream = model, stream
	if model == "" {
		s.fail(w, c, http.StatusBadRequest, "缺少 model 字段")
		return
	}
	binding, upKey, err := s.pick(model, p.Name(), actor)
	if err != nil {
		s.fail(w, c, http.StatusNotFound, err.Error())
		return
	}
	c.rec.ChannelID, c.rec.ChannelName = binding.ChannelID, binding.Channel.Name
	c.rec.UpstreamModel = binding.UpstreamModel
	c.rec.ChannelKeyID, c.rec.ChannelKeyName = upKey.ID, upKey.Name
	c.meta.ChannelName, c.meta.UpstreamModel, c.meta.ChannelKeyName = binding.Channel.Name, binding.UpstreamModel, upKey.Name

	upBody, err := p.ReplaceModel(body, binding.UpstreamModel)
	if err != nil {
		s.fail(w, c, http.StatusBadRequest, "改写 model 字段失败: "+err.Error())
		return
	}

	ctx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, store.GetSettingDuration(store.KeyUpstreamTimeoutSecond, time.Second, 300*time.Second))
		defer cancel()
	}

	upReq, err := p.BuildRequest(ctx, binding.Channel, &ProtoRequest{
		UpstreamModel: binding.UpstreamModel,
		APIKey:        upKey.Key,
		Stream:        stream,
		Body:          upBody,
		ClientHeader:  r.Header,
	})
	if err != nil {
		s.fail(w, c, http.StatusInternalServerError, "构造上游请求失败: "+err.Error())
		return
	}
	c.meta.UpstreamURL = upReq.URL.String()
	// 请求归档在这里写：已经拿到全部元信息（含上游 URL），
	// 且就算上游调用失败也能留下现场。写完立刻释放 body 引用。
	s.archiveRequest(c)

	resp, err := s.client.Do(upReq)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			status = 499
		}
		s.fail(w, c, status, "请求上游失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("X-Gateway-Request-Id", c.rec.ID)
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		s.pipeStream(w, resp, c, start)
		return
	}
	s.pipeBody(w, resp, c, start)
}

// pipeBody 非流式：原样回吐上游响应。
func (s *Service) pipeBody(w http.ResponseWriter, resp *http.Response, c *call, start time.Time) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.fail(w, c, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	var u Usage
	c.proto.MergeUsage(raw, &u)
	c.rec.PromptTokens, c.rec.CompletionTokens, c.rec.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
	c.rec.Usage = store.JSONB(u.Raw)
	c.rec.StatusCode = resp.StatusCode
	if resp.StatusCode >= 400 {
		c.rec.ErrorMessage = truncate(string(raw), 1000)
	}

	copyRespHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)

	// 归档写在回吐响应**之后**：不抬高客户端看到的首字节延迟。
	if c.archiving {
		if err := s.archiver.WriteResponse(c.dateDir, c.rec.ID, resp.StatusCode, raw); err != nil {
			log.Printf("[archive] 写响应归档失败 %s: %v", c.rec.ID, err)
		}
	}
	c.rec.DurationMs = time.Since(start).Milliseconds()
}

// pipeStream 流式：SSE 逐行原样透传并即时 flush。
//
// 归档是**边转发边追加写文件**（bufio 64KB），不再把整个响应攒在内存里——
// 原先每个在途流式请求最多占 8MB，高并发下比落库更容易打爆进程。
func (s *Service) pipeStream(w http.ResponseWriter, resp *http.Response, c *call, start time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	c.rec.StatusCode = resp.StatusCode

	archiveFile, err := s.openResponseArchive(c, resp.StatusCode)
	if err != nil {
		log.Printf("[archive] 打开响应归档失败 %s: %v", c.rec.ID, err)
	}
	defer archiveFile.Close()

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	var usage Usage

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = archiveFile.Write(line)
			if payload, ok := ssePayload(bytes.TrimRight(line, "\r\n")); ok {
				c.proto.MergeUsage(payload, &usage)
			}
			if _, werr := w.Write(line); werr != nil {
				c.rec.ErrorMessage = "客户端断开: " + werr.Error()
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && c.rec.ErrorMessage == "" {
				c.rec.ErrorMessage = truncate("读取上游流失败: "+err.Error(), 1000)
			}
			break
		}
	}

	c.rec.PromptTokens, c.rec.CompletionTokens, c.rec.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
	c.rec.Usage = store.JSONB(usage.Raw)
	c.rec.DurationMs = time.Since(start).Milliseconds()
}

// pick 路由核心（全部走内存快照，零查询）：
//
//	网关模型名 -> 绑定(上游 + 上游模型名)，要求上游支持本次协议、且在调用方归属下有可用 key
//	          -> 归属下的 key 集合 -> 按 key 策略选一把
func (s *Service) pick(modelName, protocol string, actor Actor) (*store.Binding, *store.ChannelKey, error) {
	snap := s.reg.Get()

	m, ok := snap.Model(modelName)
	if !ok {
		return nil, nil, fmt.Errorf("模型 %s 不存在", modelName)
	}
	if !m.Enabled {
		return nil, nil, fmt.Errorf("模型 %s 已禁用", modelName)
	}
	if actor.GroupID == 0 {
		return nil, nil, fmt.Errorf("用户 %s 未设置归属，请联系管理员", actor.Username)
	}

	candidates := make([]store.Binding, 0, len(m.Bindings))
	for _, b := range m.Bindings {
		if b.Channel == nil || !supportsProtocol(b.Channel, protocol) {
			continue
		}
		if !snap.HasKeyFor(b.ChannelID, actor.GroupID) {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("模型 %s 在归属 %s 下没有可用上游（需要上游支持 %s 协议且该归属下已配置 key）",
			modelName, actor.GroupLabel(), protocol)
	}

	b, err := selector.Get(store.GetSetting(store.KeyRouteStrategy)).Select(candidates)
	if err != nil {
		return nil, nil, err
	}

	keys := snap.ChannelKeys(b.ChannelID, actor.GroupID)
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("上游 %s 在归属 %s 下没有可用 key", b.Channel.Name, actor.GroupLabel())
	}
	k, err := keyselector.Get(store.GetSetting(store.KeyUpstreamKeyStrategy)).Select(keyselector.Context{
		ChannelID:     b.ChannelID,
		GroupID:       actor.GroupID,
		UserID:        actor.UserID,
		GatewayKeyID:  actor.APIKeyID,
		Model:         modelName,
		UpstreamModel: b.UpstreamModel,
		Protocol:      protocol,
		AffinityKey:   actor.AffinityKey(),
	}, keys)
	if err != nil {
		return nil, nil, err
	}
	return b, k, nil
}

// fail 用当前协议的错误格式返回。日志仍由 defer 里的 submit 统一处理。
func (s *Service) fail(w http.ResponseWriter, c *call, status int, msg string) {
	c.rec.StatusCode = status
	c.rec.ErrorMessage = truncate(msg, 1000)
	if c.rec.DurationMs == 0 {
		c.rec.DurationMs = time.Since(c.rec.CreatedAt).Milliseconds()
	}
	payload := c.proto.ErrorBody(status, msg, c.rec.ID)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Request-Id", c.rec.ID)
	w.WriteHeader(status)
	_, _ = w.Write(payload)

	// 失败也要留下现场：请求归档可能还没写（比如选不到上游就失败了），补上。
	if c.archiving {
		s.archiveRequest(c)
		if err := s.archiver.WriteResponse(c.dateDir, c.rec.ID, status, payload); err != nil {
			log.Printf("[archive] 写错误归档失败 %s: %v", c.rec.ID, err)
		}
	}
}

// archiveRequest 写请求原文（幂等：写过就不再写，并释放 body 引用）。
func (s *Service) archiveRequest(c *call) {
	if !c.archiving || c.meta == nil {
		return
	}
	if err := s.archiver.WriteRequest(c.dateDir, c.rec.ID, c.meta); err != nil {
		log.Printf("[archive] 写请求归档失败 %s: %v", c.rec.ID, err)
	}
	// 写完就丢掉引用，别让 body 跟着请求生命周期多活一会
	c.meta = nil
}

// openResponseArchive 关闭归档时返回 nil（*ResponseFile 的方法对 nil 安全，写入直接丢弃）。
func (s *Service) openResponseArchive(c *call, status int) (*archive.ResponseFile, error) {
	if !c.archiving {
		return nil, nil
	}
	f, err := s.archiver.OpenResponse(c.dateDir, c.rec.ID, status)
	if err != nil {
		return nil, err
	}
	c.respFile = f
	return f, nil
}

// submit 只把**日志行**丢进异步管道（~400 字节）；原文已经当场写完了。
// 队列满则丢弃该日志行（有计数，WebUI 可见），但原文不会跟着丢。
func (s *Service) submit(c *call) {
	if c.submitted {
		return
	}
	c.submitted = true

	s.sink.Submit(sink.Entry{
		Log:               c.rec,
		TouchGatewayKeyID: c.rec.APIKeyID,
		TouchChannelKeyID: c.rec.ChannelKeyID,
	})
}

func supportsProtocol(ch *store.Channel, protocol string) bool {
	for _, p := range ch.ProtocolList {
		if p == protocol {
			return true
		}
	}
	return false
}

func pickHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"User-Agent", "Content-Type", "Accept", "X-Request-Id", "anthropic-version", "anthropic-beta", "OpenAI-Beta"} {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// copyRespHeaders 透传上游响应头（跳过逐跳头与由 Go 自己管理的头）。
func copyRespHeaders(dst, src http.Header) {
	skip := map[string]bool{
		"Connection": true, "Keep-Alive": true, "Transfer-Encoding": true,
		"Content-Length": true, "Trailer": true, "Upgrade": true,
	}
	for k, vs := range src {
		if skip[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

// ssePayload 取出 "data: xxx" 里的 xxx；[DONE] 与非 data 行返回 false。
func ssePayload(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	p := bytes.TrimSpace(line[len("data:"):])
	if len(p) == 0 || bytes.Equal(p, []byte("[DONE]")) {
		return nil, false
	}
	return p, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
