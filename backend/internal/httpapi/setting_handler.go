package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/relay/selector"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

func (s *Server) getSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"settings":   store.AllSettings(),
		"strategies": selector.Names(),
		"types":      relay.AdapterNames(),
		"cleaner":    s.cleaner.Status(),
	})
}

func (s *Server) updateSettings(c *gin.Context) {
	var in map[string]string
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	// 数字类配置做个基本校验
	for _, k := range []string{store.KeyArchiveRetentionDays, store.KeyLogRetentionDays, store.KeyCleanupIntervalMin, store.KeyUpstreamTimeoutSecond} {
		if v, ok := in[k]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				fail(c, http.StatusBadRequest, k+" 必须是非负整数")
				return
			}
			if k == store.KeyCleanupIntervalMin && n < 1 {
				fail(c, http.StatusBadRequest, "清理间隔至少 1 分钟")
				return
			}
		}
	}
	if v, ok := in[store.KeyRouteStrategy]; ok {
		valid := false
		for _, n := range selector.Names() {
			if n == v {
				valid = true
			}
		}
		if !valid {
			fail(c, http.StatusBadRequest, "未知路由策略: "+v)
			return
		}
	}
	if err := store.SetSettings(in); err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": store.AllSettings()})
}

// triggerCleanup 立即跑一次清理。
func (s *Server) triggerCleanup(c *gin.Context) {
	s.cleaner.RunOnce()
	c.JSON(http.StatusOK, gin.H{"ok": true, "cleaner": s.cleaner.Status()})
}
