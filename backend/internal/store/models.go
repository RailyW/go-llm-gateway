package store

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// User 平台用户。role: admin | user
type User struct {
	ID           uint      `gorm:"primarykey" json:"id"`
	Username     string    `gorm:"uniqueIndex;size:64;not null" json:"username"`
	PasswordHash string    `gorm:"not null" json:"-"`
	Role         string    `gorm:"size:16;not null;default:user" json:"role"`
	Enabled      bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Channel 上游资产：base_url + api_key。
// Protocols 是该上游**支持的端点协议**列表，存成 ",openai-chat,openai-responses," 便于 LIKE 匹配。
type Channel struct {
	ID        uint   `gorm:"primarykey" json:"id"`
	Name      string `gorm:"size:128;not null" json:"name"`
	Protocols string `gorm:"size:255;not null;default:',openai-chat,'" json:"-"`
	// ProtocolList 非数据库字段，AfterFind 里从 Protocols 展开，给前端用
	ProtocolList []string  `gorm:"-" json:"protocols"`
	BaseURL      string    `gorm:"size:512;not null" json:"base_url"`
	APIKey       string    `gorm:"size:512" json:"api_key"`
	Enabled      bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Model 对外暴露的模型名（客户端请求里填的 model）。
type Model struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	Name      string    `gorm:"uniqueIndex;size:128;not null" json:"name"`
	Remark    string    `gorm:"size:255" json:"remark"`
	Enabled   bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Bindings []Binding `gorm:"foreignKey:ModelID" json:"bindings,omitempty"`
}

// Binding 模型 <-> 上游 的绑定，同时记录上游真实模型名。
// 一个 Model 可绑定多个 Channel，由路由策略选一个。
type Binding struct {
	ID            uint      `gorm:"primarykey" json:"id"`
	ModelID       uint      `gorm:"index;not null" json:"model_id"`
	ChannelID     uint      `gorm:"index;not null" json:"channel_id"`
	UpstreamModel string    `gorm:"size:128;not null" json:"upstream_model"`
	Weight        int       `gorm:"not null;default:1" json:"weight"` // 预留：加权策略用
	Enabled       bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	Channel *Channel `gorm:"foreignKey:ChannelID" json:"channel,omitempty"`
}

// APIKey 本网关自己发放的 key（sk-...），归属某个用户。
type APIKey struct {
	ID         uint       `gorm:"primarykey" json:"id"`
	UserID     uint       `gorm:"index;not null" json:"user_id"`
	Name       string     `gorm:"size:128" json:"name"`
	Key        string     `gorm:"uniqueIndex;size:128;not null" json:"key"`
	Enabled    bool       `gorm:"not null;default:true" json:"enabled"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// RequestLog 一次网关调用的结构化记录。ID 即 request id，
// 与 archive 目录下的原文文件名一一对应。
type RequestLog struct {
	ID               string    `gorm:"primarykey;size:36" json:"id"`
	Protocol         string    `gorm:"size:32;index" json:"protocol"` // openai-chat / openai-responses / anthropic-messages
	Endpoint         string    `gorm:"size:64" json:"endpoint"`       // 客户端请求的路径
	UserID           uint      `gorm:"index" json:"user_id"`
	Username         string    `gorm:"size:64" json:"username"`
	APIKeyID         uint      `gorm:"index" json:"api_key_id"`
	APIKeyName       string    `gorm:"size:128" json:"api_key_name"`
	ModelName        string    `gorm:"size:128;index" json:"model_name"`
	ChannelID        uint      `json:"channel_id"`
	ChannelName      string    `gorm:"size:128" json:"channel_name"`
	UpstreamModel    string    `gorm:"size:128" json:"upstream_model"`
	Stream           bool      `json:"stream"`
	StatusCode       int       `json:"status_code"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	DurationMs       int64     `json:"duration_ms"`
	ClientIP         string    `gorm:"size:64" json:"client_ip"`
	ErrorMessage     string    `gorm:"size:1024" json:"error_message"`
	ArchivePath      string    `gorm:"size:255" json:"archive_path"` // 相对 archive 根目录
	CreatedAt        time.Time `gorm:"index" json:"created_at"`
}

// Setting 简单 KV 配置表，供 WebUI 修改（如归档保留天数）。
type Setting struct {
	Key       string    `gorm:"primarykey;size:64" json:"key"`
	Value     string    `gorm:"size:512" json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---------- Channel.Protocols 的存储格式辅助 ----------
//
// 存成 ",openai-chat,openai-responses," 这种带前后逗号的串，
// 好处是 SQL 里可以用 protocols LIKE '%,openai-chat,%' 精确匹配单个协议。

// JoinProtocols 列表 -> 存储串。
func JoinProtocols(names []string) string {
	if len(names) == 0 {
		return ","
	}
	return "," + strings.Join(names, ",") + ","
}

// SplitProtocols 存储串 -> 列表。
func SplitProtocols(s string) []string {
	out := []string{}
	for _, p := range strings.Split(strings.Trim(s, ","), ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// AfterFind 让 API 直接返回 protocols 数组，无需每处手动展开。
func (c *Channel) AfterFind(*gorm.DB) error {
	c.ProtocolList = SplitProtocols(c.Protocols)
	return nil
}
