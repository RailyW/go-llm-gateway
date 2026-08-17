package httpapi

import (
	"log"
	"net/http"
	"strings"

	"github.com/RailyW/go-llm-gateway/backend/internal/auth"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/gin-gonic/gin"
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
		if err := s.db.Preload("Group").First(&u, claims.UserID).Error; err != nil {
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

// GatewayAuth 校验本网关发放的 sk- key。
// 全程只读内存快照（registry），热路径不查库。
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
		caller, ok := s.reg.Get().Caller(key)
		if !ok {
			openaiError(c, http.StatusUnauthorized, "API key 无效")
			return
		}
		if !caller.APIKeyEnabled {
			openaiError(c, http.StatusForbidden, "API key 已禁用")
			return
		}
		if caller.UserID == 0 || !caller.UserEnabled {
			openaiError(c, http.StatusForbidden, "所属用户不可用")
			return
		}
		if caller.GroupID != 0 && !caller.GroupEnabled {
			openaiError(c, http.StatusForbidden, "所属归属("+caller.GroupName+")已被禁用")
			return
		}
		c.Set(ctxActor, relay.Actor{
			UserID:     caller.UserID,
			Username:   caller.Username,
			GroupID:    caller.GroupID,
			GroupName:  caller.GroupName,
			APIKeyID:   caller.APIKeyID,
			APIKeyName: caller.APIKeyName,
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

// invalidateRegistryOnWrite 管理 API 的写操作成功后，同步重建配置快照。
// 放在中间件里而不是散落在各 handler，避免以后新增接口忘记失效。
// invalidateRegistryOnWrite 管理 API 的写操作成功后，同步重建本地配置快照，
// 并广播给其他实例。
//
// 两步的分工：
//   - 本地同步重建：保证「读到自己的写」——管理员改完立刻用新配置试，不能还是旧的
//   - 广播：其他转发实例收到后各自重建。广播失败不影响本次操作（配置已入库），
//     最坏是其他实例晚 30 秒（兜底轮询）生效
func (s *Server) invalidateRegistryOnWrite() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			return
		}
		if c.Writer.Status() >= 400 {
			return
		}
		if err := s.reg.Invalidate(); err != nil {
			log.Printf("[registry] 重建配置快照失败: %v", err)
		}
		s.inval.Publish(c.Request.Context())
	}
}
