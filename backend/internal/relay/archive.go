package relay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Archiver 把每次请求的原文/响应原文落到本地文件。
// 文件名使用 RequestLog.ID（request id），按天分目录便于过期清理：
//
//	<root>/2024-05-01/<request-id>.request.json
//	<root>/2024-05-01/<request-id>.response.txt
type Archiver struct {
	Root string
}

const dateLayout = "2006-01-02"

func NewArchiver(root string) *Archiver { return &Archiver{Root: root} }

// DateDir 返回该时间对应的分片目录名（同时是 RequestLog.ArchivePath 的值）。
func (a *Archiver) DateDir(t time.Time) string { return t.Format(dateLayout) }

// RequestMeta 归档的请求元信息。
type RequestMeta struct {
	RequestID     string            `json:"request_id"`
	Time          time.Time         `json:"time"`
	Username      string            `json:"username"`
	APIKeyName    string            `json:"api_key_name"`
	ClientIP      string            `json:"client_ip"`
	RequestedModel string           `json:"requested_model"`
	ChannelName   string            `json:"channel_name"`
	UpstreamModel string            `json:"upstream_model"`
	UpstreamURL   string            `json:"upstream_url"`
	Stream        bool              `json:"stream"`
	ClientHeaders map[string]string `json:"client_headers,omitempty"`
	Body          json.RawMessage   `json:"body"`
}

func (a *Archiver) WriteRequest(dateDir, id string, meta *RequestMeta) error {
	dir := filepath.Join(a.Root, dateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if !json.Valid(meta.Body) {
		meta.Body = mustJSONString(string(meta.Body))
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".request.json"), b, 0o644)
}

// WriteResponse 写响应原文（非流式为完整 body，流式为完整 SSE 文本）。
func (a *Archiver) WriteResponse(dateDir, id string, status int, body []byte) error {
	dir := filepath.Join(a.Root, dateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	header := fmt.Sprintf("# request_id: %s\n# status: %d\n# time: %s\n\n", id, status, time.Now().Format(time.RFC3339))
	return os.WriteFile(filepath.Join(dir, id+".response.txt"), append([]byte(header), body...), 0o644)
}

// Read 读回归档原文，供 WebUI 日志详情展示。
func (a *Archiver) Read(dateDir, id string) (request string, response string, err error) {
	dir := filepath.Join(a.Root, dateDir)
	reqB, reqErr := os.ReadFile(filepath.Join(dir, id+".request.json"))
	respB, respErr := os.ReadFile(filepath.Join(dir, id+".response.txt"))
	if reqErr != nil && respErr != nil {
		return "", "", fmt.Errorf("archive not found (可能已被清理服务删除)")
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
		}
	}
	return removed, nil
}

func mustJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
