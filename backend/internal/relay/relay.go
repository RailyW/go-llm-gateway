package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// ChatCompletions 处理 POST /v1/chat/completions
func (s *Service) ChatCompletions(w http.ResponseWriter, r *http.Request, actor Actor) {
	start := time.Now()
	reqID := uuid.NewString()
	dateDir := s.archiver.DateDir(start)

	rec := &store.RequestLog{
		ID:          reqID,
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
		s.fail(w, rec, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}

	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", false, nil)
		s.fail(w, rec, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec.ModelName = head.Model
	rec.Stream = head.Stream
	if head.Model == "" {
		s.archiveRequest(dateDir, reqID, rec, body, "", head.Stream, r.Header)
		s.fail(w, rec, http.StatusBadRequest, "缺少 model 字段")
		return
	}

	binding, model, err := s.pick(head.Model)
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", head.Stream, r.Header)
		s.fail(w, rec, http.StatusNotFound, err.Error())
		return
	}
	_ = model
	rec.ChannelID = binding.ChannelID
	rec.ChannelName = binding.Channel.Name
	rec.UpstreamModel = binding.UpstreamModel

	adapter, err := GetAdapter(binding.Channel.Type)
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", head.Stream, r.Header)
		s.fail(w, rec, http.StatusInternalServerError, err.Error())
		return
	}

	upBody, err := replaceModel(body, binding.UpstreamModel)
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", head.Stream, r.Header)
		s.fail(w, rec, http.StatusBadRequest, "改写 model 字段失败: "+err.Error())
		return
	}

	chatReq := &ChatRequest{
		UpstreamModel: binding.UpstreamModel,
		Stream:        head.Stream,
		Body:          upBody,
		ClientHeader:  r.Header,
	}

	timeout := store.GetSettingDuration(store.KeyUpstreamTimeoutSecond, time.Second, 300*time.Second)
	ctx := r.Context()
	var cancel context.CancelFunc
	if !head.Stream {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	upReq, err := adapter.BuildRequest(ctx, binding.Channel, chatReq)
	if err != nil {
		s.archiveRequest(dateDir, reqID, rec, body, "", head.Stream, r.Header)
		s.fail(w, rec, http.StatusInternalServerError, "构造上游请求失败: "+err.Error())
		return
	}
	// 归档客户端原文（含上游目标信息）
	s.archiveRequest(dateDir, reqID, rec, body, upReq.URL.String(), head.Stream, r.Header)

	resp, err := s.client.Do(upReq)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			status = 499
		}
		s.fail(w, rec, status, "请求上游失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("X-Gateway-Request-Id", reqID)
	isStream := head.Stream && strings.Contains(resp.Header.Get("Content-Type"), "event-stream")

	if isStream {
		s.pipeStream(w, resp, adapter, rec, dateDir, start)
		return
	}
	s.pipeJSON(w, resp, adapter, rec, dateDir, start)
}

// pipeJSON 非流式转发。
func (s *Service) pipeJSON(w http.ResponseWriter, resp *http.Response, adapter Adapter, rec *store.RequestLog, dateDir string, start time.Time) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.fail(w, rec, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	status, out, err := adapter.TransformResponse(resp.StatusCode, raw)
	if err != nil {
		s.fail(w, rec, http.StatusBadGateway, "转换上游响应失败: "+err.Error())
		return
	}
	u := adapter.ExtractUsage(out)
	rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
	rec.StatusCode = status
	if status >= 400 {
		rec.ErrorMessage = truncate(string(out), 1000)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = w.Write(out)

	rec.DurationMs = time.Since(start).Milliseconds()
	s.finish(rec, dateDir, status, raw)
}

// pipeStream 流式转发（SSE），逐行透传并即时 flush。
func (s *Service) pipeStream(w http.ResponseWriter, resp *http.Response, adapter Adapter, rec *store.RequestLog, dateDir string, start time.Time) {
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
			trimmed := bytes.TrimRight(line, "\r\n")
			if archive.Len() < maxArchiveBytes {
				archive.Write(line)
			}
			if payload, ok := ssePayload(trimmed); ok {
				if u := adapter.ExtractUsage(payload); u.TotalTokens > 0 || u.CompletionTokens > 0 {
					usage = u
				}
			}
			outLines, terr := adapter.TransformStreamLine(trimmed)
			if terr != nil {
				rec.ErrorMessage = truncate("转换流式响应失败: "+terr.Error(), 1000)
				break
			}
			for _, ol := range outLines {
				if _, werr := w.Write(append(ol, '\n')); werr != nil {
					rec.ErrorMessage = "客户端断开: " + werr.Error()
					err = io.EOF
					break
				}
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

// pick 找到模型并按策略选中一个可用绑定。
func (s *Service) pick(modelName string) (*store.Binding, *store.Model, error) {
	var m store.Model
	if err := s.db.Where("name = ?", modelName).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, fmt.Errorf("模型 %s 不存在", modelName)
		}
		return nil, nil, err
	}
	if !m.Enabled {
		return nil, nil, fmt.Errorf("模型 %s 已禁用", modelName)
	}
	var bindings []store.Binding
	err := s.db.Preload("Channel").
		Joins("JOIN channels ON channels.id = bindings.channel_id").
		Where("bindings.model_id = ? AND bindings.enabled = 1 AND channels.enabled = 1", m.ID).
		Find(&bindings).Error
	if err != nil {
		return nil, nil, err
	}
	if len(bindings) == 0 {
		return nil, nil, fmt.Errorf("模型 %s 没有可用上游绑定", modelName)
	}
	sel := selector.Get(store.GetSetting(store.KeyRouteStrategy))
	b, err := sel.Select(bindings)
	if err != nil {
		return nil, nil, err
	}
	if b.Channel == nil {
		return nil, nil, fmt.Errorf("绑定 %d 的上游不存在", b.ID)
	}
	return b, &m, nil
}

func (s *Service) archiveRequest(dateDir, reqID string, rec *store.RequestLog, body []byte, upURL string, stream bool, header http.Header) {
	meta := &RequestMeta{
		RequestID:      reqID,
		Time:           rec.CreatedAt,
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
		for _, k := range []string{"User-Agent", "Content-Type", "Accept", "X-Request-Id"} {
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

// fail 返回 OpenAI 风格错误，并记录日志。
func (s *Service) fail(w http.ResponseWriter, rec *store.RequestLog, status int, msg string) {
	rec.StatusCode = status
	rec.ErrorMessage = truncate(msg, 1000)
	if rec.DurationMs == 0 {
		rec.DurationMs = time.Since(rec.CreatedAt).Milliseconds()
	}
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message":    msg,
			"type":       "gateway_error",
			"code":       status,
			"request_id": rec.ID,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Request-Id", rec.ID)
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	s.finish(rec, rec.ArchivePath, status, payload)
}

// replaceModel 把请求体里的 model 换成上游模型名，其余字段原样保留。
func replaceModel(body []byte, upstreamModel string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	mv, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	obj["model"] = mv
	return json.Marshal(obj)
}

// ssePayload 取出 "data: xxx" 里的 xxx，[DONE] 返回 false。
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
