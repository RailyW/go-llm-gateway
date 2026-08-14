package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

func parseUint(s string) uint {
	n, _ := strconv.ParseUint(s, 10, 64)
	return uint(n)
}

// maskKey 列表里不回传完整上游 key。
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

type channelDTO struct {
	store.Channel
	APIKey string `json:"api_key"` // 覆盖为掩码
}

func (s *Server) listChannels(c *gin.Context) {
	var list []store.Channel
	if err := s.db.Order("id asc").Find(&list).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]channelDTO, 0, len(list))
	for _, ch := range list {
		out = append(out, channelDTO{Channel: ch, APIKey: maskKey(ch.APIKey)})
	}
	c.JSON(http.StatusOK, out)
}

type channelInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) createChannel(c *gin.Context) {
	var in channelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	in.Name, in.BaseURL = strings.TrimSpace(in.Name), strings.TrimSpace(in.BaseURL)
	if in.Name == "" || in.BaseURL == "" {
		fail(c, http.StatusBadRequest, "name 和 base_url 必填")
		return
	}
	if in.Type == "" {
		in.Type = "openai"
	}
	if _, err := relay.GetAdapter(in.Type); err != nil {
		fail(c, http.StatusBadRequest, "不支持的上游类型: "+in.Type)
		return
	}
	ch := store.Channel{Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey, Enabled: true}
	if in.Enabled != nil {
		ch.Enabled = *in.Enabled
	}
	if err := s.db.Create(&ch).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, channelDTO{Channel: ch, APIKey: maskKey(ch.APIKey)})
}

func (s *Server) updateChannel(c *gin.Context) {
	var ch store.Channel
	if err := s.db.First(&ch, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "上游不存在")
		return
	}
	var in channelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{}
	if v := strings.TrimSpace(in.Name); v != "" {
		updates["name"] = v
	}
	if v := strings.TrimSpace(in.BaseURL); v != "" {
		updates["base_url"] = v
	}
	if in.Type != "" {
		if _, err := relay.GetAdapter(in.Type); err != nil {
			fail(c, http.StatusBadRequest, "不支持的上游类型: "+in.Type)
			return
		}
		updates["type"] = in.Type
	}
	// api_key 留空表示不改
	if in.APIKey != "" {
		updates["api_key"] = in.APIKey
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if len(updates) > 0 {
		if err := s.db.Model(&ch).Updates(updates).Error; err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, channelDTO{Channel: ch, APIKey: maskKey(ch.APIKey)})
}

func (s *Server) deleteChannel(c *gin.Context) {
	id := c.Param("id")
	var n int64
	s.db.Model(&store.Binding{}).Where("channel_id = ?", id).Count(&n)
	if n > 0 {
		fail(c, http.StatusBadRequest, "该上游还有模型绑定，请先解绑")
		return
	}
	if err := s.db.Delete(&store.Channel{}, id).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
