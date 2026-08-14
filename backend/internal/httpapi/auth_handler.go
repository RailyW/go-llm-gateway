package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/auth"
	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/relay/selector"
	"github.com/rin/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

const tokenTTL = 7 * 24 * time.Hour

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// meta 未登录也能拿的信息：是否允许注册、可用协议/策略。
func (s *Server) meta(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"allow_register": store.GetSettingBool(store.KeyAllowRegister, true),
		"protocols":      relay.ProtocolInfos(),
		"strategies":     selector.Names(),
	})
}

func (s *Server) register(c *gin.Context) {
	if !store.GetSettingBool(store.KeyAllowRegister, true) {
		fail(c, http.StatusForbidden, "注册已关闭")
		return
	}
	var in credentials
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if len(in.Username) < 3 || len(in.Password) < 4 {
		fail(c, http.StatusBadRequest, "用户名至少 3 位、密码至少 4 位")
		return
	}
	var n int64
	s.db.Model(&store.User{}).Where("username = ?", in.Username).Count(&n)
	if n > 0 {
		fail(c, http.StatusConflict, "用户名已存在")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	u := store.User{Username: in.Username, PasswordHash: hash, Role: store.RoleUser, Enabled: true}
	if err := s.db.Create(&u).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.issue(c, &u)
}

func (s *Server) login(c *gin.Context) {
	var in credentials
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	var u store.User
	if err := s.db.Where("username = ?", strings.TrimSpace(in.Username)).First(&u).Error; err != nil {
		fail(c, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if !auth.CheckPassword(u.PasswordHash, in.Password) {
		fail(c, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if !u.Enabled {
		fail(c, http.StatusForbidden, "用户已被禁用")
		return
	}
	s.issue(c, &u)
}

func (s *Server) issue(c *gin.Context, u *store.User) {
	tok, err := auth.IssueToken(u.ID, u.Username, u.Role, tokenTTL)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": tok, "user": u})
}

func (s *Server) me(c *gin.Context) {
	c.JSON(http.StatusOK, currentUser(c))
}

func (s *Server) changePassword(c *gin.Context) {
	var in struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || len(in.NewPassword) < 4 {
		fail(c, http.StatusBadRequest, "新密码至少 4 位")
		return
	}
	u := currentUser(c)
	if !auth.CheckPassword(u.PasswordHash, in.OldPassword) {
		fail(c, http.StatusBadRequest, "原密码错误")
		return
	}
	hash, err := auth.HashPassword(in.NewPassword)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.db.Model(u).Update("password_hash", hash).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- 用户管理（admin）----------

func (s *Server) listUsers(c *gin.Context) {
	var list []store.User
	if err := s.db.Order("id asc").Find(&list).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) updateUser(c *gin.Context) {
	var u store.User
	if err := s.db.First(&u, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "用户不存在")
		return
	}
	var in struct {
		Role     *string `json:"role"`
		Enabled  *bool   `json:"enabled"`
		Password *string `json:"password"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{}
	if in.Role != nil && (*in.Role == store.RoleAdmin || *in.Role == store.RoleUser) {
		updates["role"] = *in.Role
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if in.Password != nil && len(*in.Password) >= 4 {
		hash, err := auth.HashPassword(*in.Password)
		if err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
		updates["password_hash"] = hash
	}
	if u.ID == currentUser(c).ID {
		delete(updates, "enabled") // 不允许禁用自己
	}
	if len(updates) > 0 {
		if err := s.db.Model(&u).Updates(updates).Error; err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, u)
}

func (s *Server) deleteUser(c *gin.Context) {
	id := c.Param("id")
	if currentUser(c).ID == parseUint(id) {
		fail(c, http.StatusBadRequest, "不能删除自己")
		return
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", id).Delete(&store.APIKey{}).Error; err != nil {
			return err
		}
		return tx.Delete(&store.User{}, id).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
