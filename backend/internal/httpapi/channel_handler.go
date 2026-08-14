package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/RailyW/go-llm-gateway/backend/internal/relay"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func parseUint(s string) uint {
	n, _ := strconv.ParseUint(s, 10, 64)
	return uint(n)
}

// listChannels 上游列表，带出每个上游的 key（按归属）。
func (s *Server) listChannels(c *gin.Context) {
	var list []store.Channel
	err := s.db.
		Preload("Keys", func(db *gorm.DB) *gorm.DB { return db.Order("channel_keys.group_id asc, channel_keys.id asc") }).
		Preload("Keys.Group").
		Order("id asc").Find(&list).Error
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, list)
}

type channelInput struct {
	Name      string   `json:"name"`
	Protocols []string `json:"protocols"`
	BaseURL   string   `json:"base_url"`
	Enabled   *bool    `json:"enabled"`
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
	if len(in.Protocols) == 0 {
		in.Protocols = []string{relay.ProtocolOpenAIChat}
	}
	protocols, err := relay.NormalizeProtocols(in.Protocols)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ch := store.Channel{Name: in.Name, Protocols: protocols, BaseURL: in.BaseURL, Enabled: true}
	if in.Enabled != nil {
		ch.Enabled = *in.Enabled
	}
	if err := s.db.Create(&ch).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ch.ProtocolList = store.SplitProtocols(ch.Protocols)
	c.JSON(http.StatusOK, ch)
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
	if len(in.Protocols) > 0 {
		protocols, err := relay.NormalizeProtocols(in.Protocols)
		if err != nil {
			fail(c, http.StatusBadRequest, err.Error())
			return
		}
		updates["protocols"] = protocols
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
	ch.ProtocolList = store.SplitProtocols(ch.Protocols)
	c.JSON(http.StatusOK, ch)
}

func (s *Server) deleteChannel(c *gin.Context) {
	id := c.Param("id")
	var n int64
	s.db.Model(&store.Binding{}).Where("channel_id = ?", id).Count(&n)
	if n > 0 {
		fail(c, http.StatusBadRequest, "该上游还有模型绑定，请先解绑")
		return
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("channel_id = ?", id).Delete(&store.ChannelKey{}).Error; err != nil {
			return err
		}
		return tx.Delete(&store.Channel{}, id).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- 上游 key（按归属）----------

type channelKeyInput struct {
	GroupID uint   `json:"group_id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	Weight  *int   `json:"weight"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) listChannelKeys(c *gin.Context) {
	var list []store.ChannelKey
	err := s.db.Preload("Group").Where("channel_id = ?", c.Param("id")).
		Order("group_id asc, id asc").Find(&list).Error
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) createChannelKey(c *gin.Context) {
	channelID := parseUint(c.Param("id"))
	if err := s.db.First(&store.Channel{}, channelID).Error; err != nil {
		fail(c, http.StatusNotFound, "上游不存在")
		return
	}
	var in channelKeyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		fail(c, http.StatusBadRequest, "key 必填")
		return
	}
	var g store.Group
	if err := s.db.First(&g, in.GroupID).Error; err != nil {
		fail(c, http.StatusBadRequest, "归属不存在")
		return
	}
	k := store.ChannelKey{
		ChannelID: channelID,
		GroupID:   in.GroupID,
		Name:      strings.TrimSpace(in.Name),
		Key:       in.Key,
		Weight:    1,
		Enabled:   true,
	}
	if k.Name == "" {
		k.Name = g.Name + "-key"
	}
	if in.Weight != nil && *in.Weight > 0 {
		k.Weight = *in.Weight
	}
	if in.Enabled != nil {
		k.Enabled = *in.Enabled
	}
	if err := s.db.Create(&k).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	k.KeyMasked = store.MaskSecret(k.Key)
	k.Group = &g
	c.JSON(http.StatusOK, k)
}

func (s *Server) updateChannelKey(c *gin.Context) {
	var k store.ChannelKey
	if err := s.db.First(&k, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "key 不存在")
		return
	}
	var in channelKeyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{}
	if v := strings.TrimSpace(in.Name); v != "" {
		updates["name"] = v
	}
	// key 留空表示不修改
	if v := strings.TrimSpace(in.Key); v != "" {
		updates["key"] = v
	}
	if in.GroupID > 0 && in.GroupID != k.GroupID {
		if err := s.db.First(&store.Group{}, in.GroupID).Error; err != nil {
			fail(c, http.StatusBadRequest, "归属不存在")
			return
		}
		updates["group_id"] = in.GroupID
	}
	if in.Weight != nil && *in.Weight > 0 {
		updates["weight"] = *in.Weight
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if len(updates) > 0 {
		if err := s.db.Model(&k).Updates(updates).Error; err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	k.KeyMasked = store.MaskSecret(k.Key)
	c.JSON(http.StatusOK, k)
}

func (s *Server) deleteChannelKey(c *gin.Context) {
	if err := s.db.Delete(&store.ChannelKey{}, c.Param("id")).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
