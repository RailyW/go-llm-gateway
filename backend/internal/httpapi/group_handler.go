package httpapi

import (
	"net/http"
	"strings"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/gin-gonic/gin"
)

// 归属（部门）枚举维护：设置页用。

func (s *Server) listGroups(c *gin.Context) {
	var list []store.Group
	if err := s.db.Order("id asc").Find(&list).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	// 附带每个归属下的用户数 / 上游 key 数，方便判断能不能删
	type stat struct {
		GroupID uint
		N       int64
	}
	users := map[uint]int64{}
	keys := map[uint]int64{}
	var rows []stat
	s.db.Model(&store.User{}).Select("group_id, count(*) as n").Group("group_id").Scan(&rows)
	for _, r := range rows {
		users[r.GroupID] = r.N
	}
	rows = nil
	s.db.Model(&store.ChannelKey{}).Select("group_id, count(*) as n").Group("group_id").Scan(&rows)
	for _, r := range rows {
		keys[r.GroupID] = r.N
	}

	out := make([]gin.H, 0, len(list))
	for _, g := range list {
		out = append(out, gin.H{
			"id": g.ID, "name": g.Name, "remark": g.Remark, "enabled": g.Enabled,
			"created_at": g.CreatedAt, "user_count": users[g.ID], "key_count": keys[g.ID],
		})
	}
	c.JSON(http.StatusOK, out)
}

type groupInput struct {
	Name    string `json:"name"`
	Remark  string `json:"remark"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) createGroup(c *gin.Context) {
	var in groupInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(c, http.StatusBadRequest, "归属名必填")
		return
	}
	g := store.Group{Name: in.Name, Remark: in.Remark, Enabled: true}
	if in.Enabled != nil {
		g.Enabled = *in.Enabled
	}
	if err := s.db.Create(&g).Error; err != nil {
		fail(c, http.StatusConflict, "创建失败（归属名可能已存在）: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, g)
}

func (s *Server) updateGroup(c *gin.Context) {
	var g store.Group
	if err := s.db.First(&g, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "归属不存在")
		return
	}
	var in groupInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{"remark": in.Remark}
	if v := strings.TrimSpace(in.Name); v != "" {
		updates["name"] = v
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if err := s.db.Model(&g).Updates(updates).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, g)
}

// deleteGroup 有用户或上游 key 挂着就拒绝，避免出现悬空归属。
func (s *Server) deleteGroup(c *gin.Context) {
	id := parseUint(c.Param("id"))
	if id == store.GetSettingUint(store.KeyDefaultGroupID, 1) {
		fail(c, http.StatusBadRequest, "默认归属不可删除，请先在设置里换一个默认归属")
		return
	}
	var users, keys int64
	s.db.Model(&store.User{}).Where("group_id = ?", id).Count(&users)
	s.db.Model(&store.ChannelKey{}).Where("group_id = ?", id).Count(&keys)
	if users > 0 || keys > 0 {
		fail(c, http.StatusBadRequest, "该归属下还有用户或上游 key，请先迁移")
		return
	}
	if err := s.db.Delete(&store.Group{}, id).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
