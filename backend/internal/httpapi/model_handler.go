package httpapi

import (
	"net/http"
	"strings"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// listModels 返回模型及其绑定（含上游名字），前端一次渲染完。
func (s *Server) listModels(c *gin.Context) {
	var list []store.Model
	err := s.db.Preload("Bindings", func(db *gorm.DB) *gorm.DB {
		return db.Order("bindings.id asc")
	}).Preload("Bindings.Channel").Order("id asc").Find(&list).Error
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, list)
}

type modelInput struct {
	Name    string `json:"name"`
	Remark  string `json:"remark"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) createModel(c *gin.Context) {
	var in modelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(c, http.StatusBadRequest, "模型名必填")
		return
	}
	m := store.Model{Name: in.Name, Remark: in.Remark, Enabled: true}
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	if err := s.db.Create(&m).Error; err != nil {
		fail(c, http.StatusConflict, "创建失败（模型名可能已存在）: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, m)
}

func (s *Server) updateModel(c *gin.Context) {
	var m store.Model
	if err := s.db.First(&m, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "模型不存在")
		return
	}
	var in modelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{}
	if v := strings.TrimSpace(in.Name); v != "" {
		updates["name"] = v
	}
	updates["remark"] = in.Remark
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if err := s.db.Model(&m).Updates(updates).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, m)
}

func (s *Server) deleteModel(c *gin.Context) {
	id := c.Param("id")
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("model_id = ?", id).Delete(&store.Binding{}).Error; err != nil {
			return err
		}
		return tx.Delete(&store.Model{}, id).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- 绑定 ----------

type bindingInput struct {
	ChannelID     uint   `json:"channel_id"`
	UpstreamModel string `json:"upstream_model"`
	Weight        *int   `json:"weight"`
	Enabled       *bool  `json:"enabled"`
}

func (s *Server) createBinding(c *gin.Context) {
	modelID := parseUint(c.Param("id"))
	var m store.Model
	if err := s.db.First(&m, modelID).Error; err != nil {
		fail(c, http.StatusNotFound, "模型不存在")
		return
	}
	var in bindingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	var ch store.Channel
	if err := s.db.First(&ch, in.ChannelID).Error; err != nil {
		fail(c, http.StatusBadRequest, "上游不存在")
		return
	}
	in.UpstreamModel = strings.TrimSpace(in.UpstreamModel)
	if in.UpstreamModel == "" {
		in.UpstreamModel = m.Name // 默认与对外模型名一致
	}
	b := store.Binding{
		ModelID:       modelID,
		ChannelID:     in.ChannelID,
		UpstreamModel: in.UpstreamModel,
		Weight:        1,
		Enabled:       true,
	}
	if in.Weight != nil && *in.Weight > 0 {
		b.Weight = *in.Weight
	}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	if err := s.db.Create(&b).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ch.ProtocolList = store.SplitProtocols(ch.Protocols)
	b.Channel = &ch
	c.JSON(http.StatusOK, b)
}

func (s *Server) updateBinding(c *gin.Context) {
	var b store.Binding
	if err := s.db.First(&b, c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "绑定不存在")
		return
	}
	var in bindingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "参数错误")
		return
	}
	updates := map[string]any{}
	if in.ChannelID > 0 {
		updates["channel_id"] = in.ChannelID
	}
	if v := strings.TrimSpace(in.UpstreamModel); v != "" {
		updates["upstream_model"] = v
	}
	if in.Weight != nil && *in.Weight > 0 {
		updates["weight"] = *in.Weight
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if len(updates) > 0 {
		if err := s.db.Model(&b).Updates(updates).Error; err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, b)
}

func (s *Server) deleteBinding(c *gin.Context) {
	if err := s.db.Delete(&store.Binding{}, c.Param("id")).Error; err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
