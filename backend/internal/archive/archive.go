// Package archive 负责把每次请求的原文/响应原文落到本地文件。
//
// 文件名使用 RequestLog.ID（request id），按天分目录便于过期清理：
//
//	<root>/2024-05-01/<request-id>.request.json
//	<root>/2024-05-01/<request-id>.response.txt
package archive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 单个响应归档的上限。流式响应是**边转发边追加写文件**，不在内存里攒，
// 这个上限只用来防止磁盘被超长响应打满。
var MaxResponseBytes = 32 << 20

type Archiver struct {
	Root string

	mkdirMu   sync.Mutex
	mkdirDone map[string]bool // 已创建过的日期目录，省掉每次 MkdirAll
}

const dateLayout = "2006-01-02"

func NewArchiver(root string) *Archiver {
	return &Archiver{Root: root, mkdirDone: map[string]bool{}}
}

// DateDir 返回该时间对应的分片目录名（同时是 RequestLog.ArchivePath 的值）。
func (a *Archiver) DateDir(t time.Time) string { return t.Format(dateLayout) }

func RequestFileName(id string) string  { return id + ".request.json" }
func ResponseFileName(id string) string { return id + ".response.txt" }

// RequestMeta 归档的请求元信息。
type RequestMeta struct {
	RequestID      string            `json:"request_id"`
	Time           time.Time         `json:"time"`
	Protocol       string            `json:"protocol"`
	Endpoint       string            `json:"endpoint"`
	Username       string            `json:"username"`
	GroupName      string            `json:"group_name"`
	APIKeyName     string            `json:"api_key_name"`
	ClientIP       string            `json:"client_ip"`
	RequestedModel string            `json:"requested_model"`
	ChannelName    string            `json:"channel_name"`
	UpstreamModel  string            `json:"upstream_model"`
	ChannelKeyName string            `json:"channel_key_name"`
	UpstreamURL    string            `json:"upstream_url"`
	Stream         bool              `json:"stream"`
	ClientHeaders  map[string]string `json:"client_headers,omitempty"`
	Body           json.RawMessage   `json:"body"`
}

// File 一个待写入的归档文件（由 sink 在后台批量写）。
type File struct {
	DateDir string
	Name    string
	Data    []byte
}

// MarshalRequest 把请求元信息序列化成待写文件（在请求协程里做，只是 CPU）。
func MarshalRequest(dateDir, id string, meta *RequestMeta) File {
	if !json.Valid(meta.Body) {
		meta.Body = mustJSONString(string(meta.Body))
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf("{\"error\":%q}", err.Error()))
	}
	return File{DateDir: dateDir, Name: RequestFileName(id), Data: b}
}

// MarshalResponse 非流式响应：body 已经在内存里，直接组装成待写文件。
func MarshalResponse(dateDir, id string, status int, body []byte) File {
	head := responseHeader(id, status)
	buf := make([]byte, 0, len(head)+len(body))
	buf = append(buf, head...)
	if len(body) > MaxResponseBytes {
		buf = append(buf, body[:MaxResponseBytes]...)
		buf = append(buf, truncatedMark...)
	} else {
		buf = append(buf, body...)
	}
	return File{DateDir: dateDir, Name: ResponseFileName(id), Data: buf}
}

// WriteFiles 批量写入（sink 后台协程调用）。
func (a *Archiver) WriteFiles(files []File) error {
	var firstErr error
	for _, f := range files {
		dir, err := a.ensureDir(f.DateDir)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.Data, 0o644); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ResponseFile 流式响应的增量写入器：边转发边追加，内存占用只有一个 bufio 缓冲。
type ResponseFile struct {
	f       *os.File
	w       *bufio.Writer
	written int
	capped  bool
}

// OpenResponse 打开流式响应的归档文件。
func (a *Archiver) OpenResponse(dateDir, id string, status int) (*ResponseFile, error) {
	dir, err := a.ensureDir(dateDir)
	if err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, ResponseFileName(id)))
	if err != nil {
		return nil, err
	}
	rf := &ResponseFile{f: f, w: bufio.NewWriterSize(f, 64<<10)}
	_, _ = rf.w.WriteString(responseHeader(id, status))
	return rf, nil
}

// Write 追加一段响应内容；超过上限后只记一次截断标记，不再增长。
func (r *ResponseFile) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	if r.capped {
		return len(p), nil
	}
	if r.written+len(p) > MaxResponseBytes {
		r.capped = true
		_, _ = r.w.Write(p[:max(0, MaxResponseBytes-r.written)])
		_, _ = r.w.WriteString(truncatedMark)
		return len(p), nil
	}
	n, err := r.w.Write(p)
	r.written += n
	return len(p), err
}

func (r *ResponseFile) Close() error {
	if r == nil {
		return nil
	}
	if err := r.w.Flush(); err != nil {
		r.f.Close()
		return err
	}
	return r.f.Close()
}

// Read 读回归档原文，供 WebUI 日志详情展示。
func (a *Archiver) Read(dateDir, id string) (request string, response string, err error) {
	dir := filepath.Join(a.Root, dateDir)
	reqB, reqErr := os.ReadFile(filepath.Join(dir, RequestFileName(id)))
	respB, respErr := os.ReadFile(filepath.Join(dir, ResponseFileName(id)))
	if reqErr != nil && respErr != nil {
		return "", "", fmt.Errorf("archive not found（可能已被清理服务删除，或仍在异步写入队列中）")
	}
	return string(reqB), string(respB), nil
}

// Cleanup 删除早于 retentionDays 天的分片目录，返回被删除的目录名。
func (a *Archiver) Cleanup(retentionDays int) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	cutoffDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.Local)
	var removed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d, err := time.ParseInLocation(dateLayout, e.Name(), time.Local)
		if err != nil {
			continue // 不认识的目录不动
		}
		if d.Before(cutoffDay) {
			if err := os.RemoveAll(filepath.Join(a.Root, e.Name())); err != nil {
				return removed, err
			}
			removed = append(removed, e.Name())
			a.forgetDir(e.Name())
		}
	}
	return removed, nil
}

func (a *Archiver) ensureDir(dateDir string) (string, error) {
	dir := filepath.Join(a.Root, dateDir)
	a.mkdirMu.Lock()
	done := a.mkdirDone[dateDir]
	a.mkdirMu.Unlock()
	if done {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}
	a.mkdirMu.Lock()
	a.mkdirDone[dateDir] = true
	a.mkdirMu.Unlock()
	return dir, nil
}

func (a *Archiver) forgetDir(dateDir string) {
	a.mkdirMu.Lock()
	delete(a.mkdirDone, dateDir)
	a.mkdirMu.Unlock()
}

const truncatedMark = "\n...[gateway] 响应过长，归档已截断\n"

func responseHeader(id string, status int) string {
	return fmt.Sprintf("# request_id: %s\n# status: %d\n# time: %s\n\n", id, status, time.Now().Format(time.RFC3339))
}

func mustJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
