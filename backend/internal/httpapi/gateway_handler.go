package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

// chatCompletions POST /v1/chat/completions
func (s *Server) chatCompletions(c *gin.Context) {
	s.relay.ChatCompletions(c.Writer, c.Request, currentActor(c))
}

// listUpstreamModels GET /v1/models —— 返回本网关已启用且有可用绑定的模型。
func (s *Server) listUpstreamModels(c *gin.Context) {
	var names []string
	err := s.db.Model(&store.Model{}).
		Distinct("models.name").
		Joins("JOIN bindings ON bindings.model_id = models.id").
		Joins("JOIN channels ON channels.id = bindings.channel_id").
		Where("models.enabled = 1 AND bindings.enabled = 1 AND channels.enabled = 1").
		Order("models.name asc").
		Pluck("models.name", &names).Error
	if err != nil {
		openaiError(c, http.StatusInternalServerError, err.Error())
		return
	}
	data := make([]gin.H, 0, len(names))
	for _, n := range names {
		data = append(data, gin.H{"id": n, "object": "model", "owned_by": "gateway"})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}
