package adminui

import (
	"embed"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed static/*
var files embed.FS

// Render serves the embedded administration shell directly from Gin.
func Render(context *gin.Context, testEnvironment bool) {
	name := strings.TrimPrefix(context.Request.URL.Path, "/")
	if name == "" || name == "setup" || name == "enroll" {
		name = "index.html"
	}
	if name == "offer-claim" {
		name = "offer-claim.html"
	}
	data, err := files.ReadFile("static/" + name)
	if err != nil || strings.Contains(name, "..") {
		context.Status(404)
		return
	}
	if name == "index.html" && testEnvironment {
		shell := strings.NewReplacer("Tellyouwhat 管理后台", "Tellyouwhat 测试管理后台",
			"<h1>管理后台</h1>", "<h1>测试管理后台</h1>",
			`<p class="eyebrow">Tellyouwhat</p>`, `<p class="eyebrow">Tellyouwhat · 测试环境</p>`)
		data = []byte(shell.Replace(string(data)))
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(name, ".html") {
		contentType = "text/html; charset=utf-8"
	} else if strings.HasSuffix(name, ".css") {
		contentType = "text/css; charset=utf-8"
	} else if strings.HasSuffix(name, ".js") {
		contentType = "text/javascript; charset=utf-8"
	}
	context.Header("Cache-Control", "no-cache")
	context.Data(200, contentType, data)
}
