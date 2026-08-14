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

	"github.com/google/uuid"
	"github.com/rin/go-llm-gateway/backend/internal/relay/selector"
	"github.com/rin/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

const (
	maxRequestBody  = 20 << 20 // 20MB
	maxArchiveBytes = 8 << 20  // 归档单个响应最多留 8MB
)

// Actor 调用方身份（由网关鉴权中间件解析出来）。
type Actor struct {
	UserID     uint
	Username   string
	APIKeyID   uint
	APIKeyName string
	ClientIP   string
}

type Service struct {
	db       *gorm.DB
	archiver *Archiver
	client   *http.Client
}

func NewService(db *gorm.DB, archiver *Archiver) *Service {
	return &Service{
		db:       db,
		archiver: archiver,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Handle 处理某个协议端点的一次转发。整个流程与具体协议无关：
// 解析 model -> 选上游(必须支持该协议) -> 换模型名 -> 直转 -> 原样回吐 -> 落日志/归档。
func (s *Service) Handle(p Protocol, w http.ResponseWriter, r *http.Request, actor Actor) {
	start := time.Now()
	reqID := uuid.NewString()
	dateDir := s.archiver.DateDir(start)

	rec := &store.RequestLog{
		ID:          reqID,
		Protocol:    p.Name(),
		Endpoint:    p.InboundPath(),
		UserID:      actor.UserID,
		Username:    actor.Username,
		APIKeyID:    actor.APIKeyID,
		APIKeyName:  actor.APIKeyName,
		ClientIP:    actor.ClientIP,
		CreatedAt:   start,
		ArchivePath: dateDir,
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		s.fail(w, p, rec, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}

	model, stream, err := p.ParseRequest(body)
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", false, nil)
		s.fail(w, p, rec, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec.ModelName, rec.Stream = model, stream
	if model == "" {
		s.archiveRequest(dateDir, reqID, rec, body, "", stream, r.Header)
		s.fail(w, p, rec, http.StatusBadRequest, "缺少 model 字段")
		return
	}

	binding, err := s.pick(model, p.Name())
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", stream, r.Header)
		s.fail(w, p, rec, http.StatusNotFound, err.Error())
		return
	}
	rec.ChannelID = binding.ChannelID
	rec.ChannelName = binding.Channel.Name
	rec.UpstreamModel = binding.UpstreamModel

	upBody, err := p.ReplaceModel(body, binding.UpstreamModel)
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", stream, r.Header)
		s.fail(w, p, rec, http.StatusBadRequest, "改写 model 字段失败: "+err.Error())
		return
	}

	timeout := store.GetSettingDuration(store.KeyUpstreamTimeoutSecond, time.Second, 300*time.Second)
	ctx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	upReq, err := p.BuildRequest(ctx, binding.Channel, &ProtoRequest{
		UpstreamModel: binding.UpstreamModel,
		Stream:        stream,
		Body:          upBody,
		ClientHeader:  r.Header,
	})
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", stream, r.Header)
		s.fail(w, p, rec, http.StatusInternalServerError, "构造上游请求失败: "+err.Error())
		return
	}
	// 归档客户端原文（含上游目标信息）
	s.archiveRequest(dateDir, reqID, rec, body, upReq.URL.String(), stream, r.Header)

	resp, err := s.client.Do(upReq)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			status = 499
		}
		s.fail(w, p, rec, status, "请求上游失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("X-Gateway-Request-Id", reqID)
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		s.pipeStream(w, resp, p, rec, dateDir, start)
		return
	}
	s.pipeBody(w, resp, p, rec, dateDir, start)
}

// pipeBody 非流式：原样回吐上游响应。
func (s *Service) pipeBody(w http.ResponseWriter, resp *http.Response, p Protocol, rec *store.RequestLog, dateDir string, start time.Time) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.fail(w, p, rec, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	var u Usage
	p.MergeUsage(raw, &u)
	rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
	rec.StatusCode = resp.StatusCode
	if resp.StatusCode >= 400 {
		rec.ErrorMessage = truncate(string(raw), 1000)
	}

	copyRespHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)

	rec.DurationMs = time.Since(start).Milliseconds()
	s.finish(rec, dateDir, resp.StatusCode, raw)
}

// pipeStream 流式：SSE 逐行原样透传并即时 flush，顺带抽 usage、留归档。
func (s *Service) pipeStream(w http.ResponseWriter, resp *http.Response, p Protocol, rec *store.RequestLog, dateDir string, start time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	rec.StatusCode = resp.StatusCode

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var archive bytes.Buffer
	var usage Usage

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if archive.Len() < maxArchiveBytes {
				archive.Write(line)
			}
			if payload, ok := ssePayload(bytes.TrimRight(line, "\r\n")); ok {
				p.MergeUsage(payload, &usage)
			}
			if _, werr := w.Write(line); werr != nil {
				rec.ErrorMessage = "客户端断开: " + werr.Error()
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && rec.ErrorMessage == "" {
				rec.ErrorMessage = truncate("读取上游流失败: "+err.Error(), 1000)
			}
			break
		}
	}

	rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
	rec.DurationMs = time.Since(start).Milliseconds()
	s.finish(rec, dateDir, resp.StatusCode, archive.Bytes())
}

// pick 找模型 -> 只保留**支持该协议**的上游绑定 -> 按策略选一个。
func (s *Service) pick(modelName, protocol string) (*store.Binding, error) {
	var m store.Model
	if err := s.db.Where("name = ?", modelName).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("模型 %s 不存在", modelName)
		}
		return nil, err
	}
	if !m.Enabled {
		return nil, fmt.Errorf("模型 %s 已禁用", modelName)
	}

	var bindings []store.Binding
	err := s.db.Preload("Channel").
		Joins("JOIN channels ON channels.id = bindings.channel_id").
		Where("bindings.model_id = ? AND bindings.enabled = 1 AND channels.enabled = 1", m.ID).
		Where("channels.protocols LIKE ?", "%,"+protocol+",%").
		Find(&bindings).Error
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, fmt.Errorf("模型 %s 没有支持 %s 协议的可用上游绑定", modelName, protocol)
	}

	sel := selector.Get(store.GetSetting(store.KeyRouteStrategy))
	b, err := sel.Select(bindings)
	if err != nil {
		return nil, err
	}
	if b.Channel == nil {
		return nil, fmt.Errorf("绑定 %d 的上游不存在", b.ID)
	}
	return b, nil
}

func (s *Service) archiveRequest(dateDir, reqID string, rec *store.RequestLog, body []byte, upURL string, stream bool, header http.Header) {
	meta := &RequestMeta{
		RequestID:      reqID,
		Time:           rec.CreatedAt,
		Protocol:       rec.Protocol,
		Endpoint:       rec.Endpoint,
		Username:       rec.Username,
		APIKeyName:     rec.APIKeyName,
		ClientIP:       rec.ClientIP,
		RequestedModel: rec.ModelName,
		ChannelName:    rec.ChannelName,
		UpstreamModel:  rec.UpstreamModel,
		UpstreamURL:    upURL,
		Stream:         stream,
		Body:           body,
	}
	if header != nil {
		meta.ClientHeaders = map[string]string{}
		for _, k := range []string{"User-Agent", "Content-Type", "Accept", "X-Request-Id", "anthropic-version", "anthropic-beta", "OpenAI-Beta"} {
			if v := header.Get(k); v != "" {
				meta.ClientHeaders[k] = v
			}
		}
	}
	if err := s.archiver.WriteRequest(dateDir, reqID, meta); err != nil {
		log.Printf("[archive] 写请求原文失败 %s: %v", reqID, err)
	}
}

// finish 落库 + 写响应原文 + 更新 key 使用时间。
func (s *Service) finish(rec *store.RequestLog, dateDir string, status int, respRaw []byte) {
	if err := s.archiver.WriteResponse(dateDir, rec.ID, status, respRaw); err != nil {
		log.Printf("[archive] 写响应原文失败 %s: %v", rec.ID, err)
	}
	if err := s.db.Create(rec).Error; err != nil {
		log.Printf("[log] 写日志失败 %s: %v", rec.ID, err)
	}
	if rec.APIKeyID > 0 {
		now := time.Now()
		s.db.Model(&store.APIKey{}).Where("id = ?", rec.APIKeyID).Update("last_used_at", now)
	}
}

// fail 用当前协议的错误格式返回，并记录日志。
func (s *Service) fail(w http.ResponseWriter, p Protocol, rec *store.RequestLog, status int, msg string) {
	rec.StatusCode = status
	rec.ErrorMessage = truncate(msg, 1000)
	if rec.DurationMs == 0 {
		rec.DurationMs = time.Since(rec.CreatedAt).Milliseconds()
	}
	payload := p.ErrorBody(status, msg, rec.ID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Request-Id", rec.ID)
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	s.finish(rec, rec.ArchivePath, status, payload)
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
