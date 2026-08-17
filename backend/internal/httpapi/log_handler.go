package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// listLogs 分页日志。普通用户只能看自己的。
func (s *Server) listLogs(c *gin.Context) {
	u := currentUser(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 200 {
		size = 20
	}

	q := s.db.Model(&store.RequestLog{})
	if !u.IsAdmin() {
		q = q.Where("user_id = ?", u.ID)
	}
	if v := c.Query("model"); v != "" {
		q = q.Where("model_name = ?", v)
	}
	if v := c.Query("status"); v == "error" {
		q = q.Where("status_code >= 400")
	} else if v == "ok" {
		q = q.Where("status_code < 400")
	}
	if v := c.Query("keyword"); v != "" {
		like := "%" + v + "%"
		q = q.Where("id LIKE ? OR username LIKE ? OR model_name LIKE ? OR channel_name LIKE ?", like, like, like, like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	var list []store.RequestLog
	if err := q.Order("created_at desc").Offset((page - 1) * size).Limit(size).Find(&list).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "page": page, "page_size": size, "items": list})
}

func (s *Server) getLog(c *gin.Context) {
	l, ok := s.visibleLog(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, l)
}

// getLogArchive 读取本地归档的请求/响应原文（文件名 = 日志 id）。
//
// 归档功能目前停用（store.ArchiveFeatureEnabled=false）：它写本地磁盘，
// 而转发已经是多实例了，文件只在处理该请求的那个实例上。
// 接口保留，但直接返回 501 并说清楚原因，别让人以为是数据丢了。
func (s *Server) getLogArchive(c *gin.Context) {
	if !store.ArchiveFeatureEnabled {
		fail(c, http.StatusNotImplemented,
			"原文归档功能已暂停：归档写在单台实例的本地磁盘上，跟多实例转发不兼容。"+
				"待改成共享存储（S3/MinIO）后重新开启")
		return
	}
	l, ok := s.visibleLog(c)
	if !ok {
		return
	}
	req, resp, err := s.archiver.Read(l.ArchivePath, l.ID)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"request": req, "response": resp})
}

func (s *Server) visibleLog(c *gin.Context) (*store.RequestLog, bool) {
	var l store.RequestLog
	if err := s.db.Where("id = ?", c.Param("id")).First(&l).Error; err != nil {
		fail(c, http.StatusNotFound, "日志不存在")
		return nil, false
	}
	u := currentUser(c)
	if !u.IsAdmin() && l.UserID != u.ID {
		fail(c, http.StatusForbidden, "无权查看")
		return nil, false
	}
	return &l, true
}

// stats 首页概览。
//
// 只统计一个**时间窗口**内的数据（1h / 24h），不做全表 COUNT/SUM：
// logs 表会一直增长，全表扫描的概览接口迟早会拖垮首页。
// 更长周期的统计等以后做小时级 rollup 再说。
func (s *Server) stats(c *gin.Context) {
	u := currentUser(c)

	window := 1 * time.Hour
	label := "1h"
	if c.Query("window") == "24h" {
		window, label = 24*time.Hour, "24h"
	}
	since := time.Now().Add(-window)

	// 普通用户只统计自己的；同时始终带上时间窗口（命中 created_at 索引）
	scoped := func() *gorm.DB {
		q := s.db.Model(&store.RequestLog{}).Where("created_at >= ?", since)
		if !u.IsAdmin() {
			q = q.Where("user_id = ?", u.ID)
		}
		return q
	}

	var requests, errCount int64
	scoped().Count(&requests)
	scoped().Where("status_code >= 400").Count(&errCount)

	var tokens struct {
		Prompt     int64
		Completion int64
	}
	scoped().Select("COALESCE(SUM(prompt_tokens),0) as prompt, COALESCE(SUM(completion_tokens),0) as completion").
		Scan(&tokens)

	// key 数量与日志无关，是小表，直接算
	var keys int64
	keyQ := s.db.Model(&store.APIKey{})
	if !u.IsAdmin() {
		keyQ = keyQ.Where("user_id = ?", u.ID)
	}
	keyQ.Count(&keys)

	out := gin.H{
		"window":            label,
		"since":             since,
		"requests":          requests,
		"errors":            errCount,
		"prompt_tokens":     tokens.Prompt,
		"completion_tokens": tokens.Completion,
		"keys":              keys,
	}

	if u.IsAdmin() {
		var channels, models, users, groups int64
		s.db.Model(&store.Channel{}).Count(&channels)
		s.db.Model(&store.Model{}).Count(&models)
		s.db.Model(&store.User{}).Count(&users)
		s.db.Model(&store.Group{}).Count(&groups)
		out["channels"] = channels
		out["models"] = models
		out["users"] = users
		out["groups"] = groups
		if s.cleaner != nil {
			out["cleaner"] = s.cleaner.Status()
		}
		out["sink"] = s.sink.Stats() // 异步落库管道健康度（丢弃必须可见）
		if s.consumer != nil {
			// Stream 消费端：积压是这条链路唯一的慢性病信号
			out["consumer"] = s.consumer.Stats()
		}
		out["registry"] = s.reg.Stats() // 配置快照状态
		// 多实例相关：角色、Redis 健康度、降级情况、广播与选主。
		// fail-open 最大的风险是「静默裸奔」，所以降级必须在这里看得见。
		out["instance"] = gin.H{
			"id":             s.cfg.InstanceID,
			"role":           string(s.cfg.Role),
			"role_label":     s.cfg.Role.Label(),
			"archive_usable": store.ArchiveFeatureEnabled,
		}
		out["redis"] = s.rc.Stats()
		out["invalidate"] = s.inval.Stats()
		// 集群视图：各实例上报的心跳。多实例下这是唯一能看到 gateway 实例状态的地方
		if peers := s.hb.Peers(c.Request.Context()); peers != nil {
			out["cluster"] = peers
		}
	}
	c.JSON(http.StatusOK, out)
}
