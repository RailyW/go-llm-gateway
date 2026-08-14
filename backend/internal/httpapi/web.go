package httpapi

import (
	"io/fs"
	"net/http"
	"strings"

	webassets "github.com/RailyW/go-llm-gateway/backend/internal/web"
	"github.com/gin-gonic/gin"
)

// mountWeb 挂载内嵌前端，SPA 路由回退到 index.html。
func (s *Server) mountWeb(r *gin.Engine) {
	sub := webassets.FS()
	fileServer := http.FileServer(http.FS(sub))

	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		clean := strings.TrimPrefix(p, "/")
		if clean != "" {
			if f, err := sub.Open(clean); err == nil {
				if st, serr := f.Stat(); serr == nil && !st.IsDir() {
					f.Close()
					fileServer.ServeHTTP(c.Writer, c.Request)
					return
				}
				f.Close()
			}
		}
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			c.String(http.StatusNotFound, "前端未构建：请执行 make web 后重新 go build（开发模式请访问 http://localhost:5173）")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	})
}
