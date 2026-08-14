// Package web 内嵌前端构建产物（web/dist -> backend/internal/web/dist）。
// 未执行 make web 时该目录只有 .gitkeep，Built() 返回 false。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS 返回 dist 子目录的文件系统。
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// Built 是否已打包前端产物。
func Built() bool {
	b, err := fs.ReadFile(dist, "dist/index.html")
	return err == nil && len(b) > 0
}
