package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/auth"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

// listKeys 普通用户看自己的；admin 加 ?all=1 看全部。
func (s *Server) listKeys(c *gin.Context) {
	u := currentUser(c)
	q := s.db.Order("id desc")
	if !(u.IsAdmin() && c.Query("all") == "1") {
		q = q.Where("user_id = ?", u.ID)
	}
	var list []store.APIKey
	if err := q.Find(&list).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) createKey(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&in)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "default"
	}
	k := store.APIKey{
		UserID:  currentUser(c).ID,
		Name:    name,
		Key:     auth.GenerateAPIKey(),
		Enabled: true,
	}
	if err := s.db.Create(&k).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, k)
}

func (s *Server) updateKey(c *gin.Context) {
	k, ok := s.ownedKey(c)
	if !ok {
		return
	}
	var in struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		updates["name"] = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if len(updates) > 0 {
		if err := s.db.Model(k).Updates(updates).Error; err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, k)
}

func (s *Server) deleteKey(c *gin.Context) {
	k, ok := s.ownedKey(c)
	if !ok {
		return
	}
	if err := s.db.Delete(k).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) ownedKey(c *gin.Context) (*store.APIKey, bool) {
	var k store.APIKey
	if err := s.db.First(&k, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "key 不存在")
		return nil, false
	}
	u := currentUser(c)
	if k.UserID != u.ID && !u.IsAdmin() {
		fail(c, http.StatusForbidden, "无权操作他人的 key")
		return nil, false
	}
	return &k, true
}
