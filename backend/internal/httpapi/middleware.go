package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/auth"
	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

const (
	ctxUser  = "currentUser"
	ctxActor = "gatewayActor"
)

// JWTAuth 管理后台鉴权。
func (s *Server) JWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := auth.BearerToken(c.GetHeader("Authorization"))
		if tok == "" {
			fail(c, http.StatusUnauthorized, "未登录")
			return
		}
		claims, err := auth.ParseToken(tok)
		if err != nil {
			fail(c, http.StatusUnauthorized, "登录已过期，请重新登录")
			return
		}
		var u store.User
		if err := s.db.First(&u, claims.UserID).Error; err != nil {
			fail(c, http.StatusUnauthorized, "用户不存在")
			return
		}
		if !u.Enabled {
			fail(c, http.StatusForbidden, "用户已被禁用")
			return
		}
		c.Set(ctxUser, &u)
		c.Next()
	}
}

func (s *Server) AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		u := currentUser(c)
		if u == nil || !u.IsAdmin() {
			fail(c, http.StatusForbidden, "需要管理员权限")
			return
		}
		c.Next()
	}
}

// GatewayAuth 校验本网关发放的 sk- key，返回 OpenAI 风格错误。
func (s *Server) GatewayAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := auth.BearerToken(c.GetHeader("Authorization"))
		if key == "" {
			key = c.GetHeader("X-Api-Key")
		}
		if key == "" || !strings.HasPrefix(key, "sk-") {
			openaiError(c, http.StatusUnauthorized, "缺少或非法的 API key")
			return
		}
		var k store.APIKey
		if err := s.db.Where("key = ?", key).First(&k).Error; err != nil {
			openaiError(c, http.StatusUnauthorized, "API key 无效")
			return
		}
		if !k.Enabled {
			openaiError(c, http.StatusForbidden, "API key 已禁用")
			return
		}
		var u store.User
		if err := s.db.First(&u, k.UserID).Error; err != nil || !u.Enabled {
			openaiError(c, http.StatusForbidden, "所属用户不可用")
			return
		}
		c.Set(ctxActor, relay.Actor{
			UserID:     u.ID,
			Username:   u.Username,
			APIKeyID:   k.ID,
			APIKeyName: k.Name,
			ClientIP:   c.ClientIP(),
		})
		c.Next()
	}
}

func currentUser(c *gin.Context) *store.User {
	v, ok := c.Get(ctxUser)
	if !ok {
		return nil
	}
	u, _ := v.(*store.User)
	return u
}

func currentActor(c *gin.Context) relay.Actor {
	v, _ := c.Get(ctxActor)
	a, _ := v.(relay.Actor)
	return a
}

func openaiError(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"message": msg,
		"type":    "gateway_error",
		"code":    status,
	}})
}
