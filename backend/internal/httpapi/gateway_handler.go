package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/store"
)

// listGatewayModels GET /v1/models —— 返回已启用且有可用绑定的模型，
// 并带上每个模型可用的协议端点（非标字段 protocols，方便自查）。
func (s *Server) listGatewayModels(c *gin.Context) {
	type row struct {
		Name      string
		Protocols string
	}
	var rows []row
	err := s.db.Model(&store.Model{}).
		Select("models.name as name, channels.protocols as protocols").
		Joins("JOIN bindings ON bindings.model_id = models.id").
		Joins("JOIN channels ON channels.id = bindings.channel_id").
		Where("models.enabled = 1 AND bindings.enabled = 1 AND channels.enabled = 1").
		Order("models.name asc").
		Scan(&rows).Error
	if err != nil {
		openaiError(c, http.StatusInternalServerError, err.Error())
		return
	}

	order := []string{}
	protos := map[string]map[string]bool{}
	for _, r := range rows {
		if _, ok := protos[r.Name]; !ok {
			protos[r.Name] = map[string]bool{}
			order = append(order, r.Name)
		}
		for _, p := range relay.SplitProtocols(r.Protocols) {
			protos[r.Name][p] = true
		}
	}

	data := make([]gin.H, 0, len(order))
	for _, name := range order {
		list := []string{}
		for _, p := range relay.Protocols() {
			if protos[name][p.Name()] {
				list = append(list, p.Name())
			}
		}
		data = append(data, gin.H{"id": name, "object": "model", "owned_by": "gateway", "protocols": list})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}
