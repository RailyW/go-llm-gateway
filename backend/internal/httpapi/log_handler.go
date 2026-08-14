package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/store"
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
func (s *Server) getLogArchive(c *gin.Context) {
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
func (s *Server) stats(c *gin.Context) {
	u := currentUser(c)
	out := gin.H{}

	// 普通用户只统计自己的
	mine := func(q *gorm.DB) *gorm.DB {
		if !u.IsAdmin() {
			return q.Where("user_id = ?", u.ID)
		}
		return q
	}

	var requests, errCount, keys int64
	mine(s.db.Model(&store.RequestLog{})).Count(&requests)
	mine(s.db.Model(&store.RequestLog{})).Where("status_code >= 400").Count(&errCount)
	mine(s.db.Model(&store.APIKey{})).Count(&keys)

	var tokens struct {
		Prompt     int64
		Completion int64
	}
	mine(s.db.Model(&store.RequestLog{})).
		Select("COALESCE(SUM(prompt_tokens),0) as prompt, COALESCE(SUM(completion_tokens),0) as completion").
		Scan(&tokens)

	out["requests"] = requests
	out["errors"] = errCount
	out["keys"] = keys
	out["prompt_tokens"] = tokens.Prompt
	out["completion_tokens"] = tokens.Completion

	if u.IsAdmin() {
		var channels, models, users int64
		s.db.Model(&store.Channel{}).Count(&channels)
		s.db.Model(&store.Model{}).Count(&models)
		s.db.Model(&store.User{}).Count(&users)
		out["channels"] = channels
		out["models"] = models
		out["users"] = users
		out["cleaner"] = s.cleaner.Status()
	}
	c.JSON(http.StatusOK, out)
}
