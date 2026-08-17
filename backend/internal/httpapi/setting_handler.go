package httpapi

import (
	"net/http"
	"strconv"

	"github.com/RailyW/go-llm-gateway/backend/internal/relay"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay/keyselector"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay/selector"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) getSettings(c *gin.Context) {
	out := gin.H{
		"settings":       store.AllSettings(),
		"strategies":     selector.Names(),
		"key_strategies": keyselector.Names(),
		"protocols":      relay.ProtocolInfos(),
	}
	// console 角色不跑清理任务，此时没有 cleaner
	if s.cleaner != nil {
		out["cleaner"] = s.cleaner.Status()
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) updateSettings(c *gin.Context) {
	var in map[string]string
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	// 数字类配置做个基本校验
	for _, k := range []string{store.KeyArchiveRetentionDays, store.KeyLogRetentionDays, store.KeyCleanupIntervalMin, store.KeyUpstreamTimeoutSecond, store.KeyDefaultGroupID, store.KeyLogFlushIntervalMs, store.KeyLogFlushBatch} {
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
			if k == store.KeyLogFlushIntervalMs && (n < 10 || n > 60000) {
				fail(c, http.StatusBadRequest, "攒批间隔取值范围 10~60000 毫秒")
				return
			}
			if k == store.KeyLogFlushBatch && (n < 1 || n > 5000) {
				fail(c, http.StatusBadRequest, "单批条数取值范围 1~5000")
				return
			}
		}
	}
	for key, names := range map[string][]string{
		store.KeyRouteStrategy:       selector.Names(),
		store.KeyUpstreamKeyStrategy: keyselector.Names(),
	} {
		v, ok := in[key]
		if !ok {
			continue
		}
		valid := false
		for _, n := range names {
			if n == v {
				valid = true
			}
		}
		if !valid {
			fail(c, http.StatusBadRequest, "未知策略: "+v)
			return
		}
	}
	if v, ok := in[store.KeyDefaultGroupID]; ok {
		if err := s.db.First(&store.Group{}, v).Error; err != nil {
			fail(c, http.StatusBadRequest, "默认归属不存在")
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
//
// 多实例下清理由 worker 角色选主执行，console 自己不持有 cleaner；
// 这时候得告诉用户去哪儿看，而不是默默什么都不做。
func (s *Server) triggerCleanup(c *gin.Context) {
	if s.cleaner == nil {
		fail(c, http.StatusNotImplemented,
			"本实例（角色 "+string(s.cfg.Role)+"）不运行清理任务，请在 worker/all 实例上触发或等它按周期执行")
		return
	}
	s.cleaner.RunOnce()
	c.JSON(http.StatusOK, gin.H{"ok": true, "cleaner": s.cleaner.Status()})
}
