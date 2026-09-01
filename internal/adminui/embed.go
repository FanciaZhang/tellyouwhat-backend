package adminui

import (
	"embed"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed static/*
var files embed.FS

// Handle serves the embedded administration shell directly from Gin.
func Handle(context *gin.Context) {
	name := strings.TrimPrefix(context.Request.URL.Path, "/")
	if name == "" || name == "setup" || name == "enroll" {
		name = "index.html"
	}
	data, err := files.ReadFile("static/" + name)
	if err != nil || strings.Contains(name, "..") {
		context.Status(404)
		return
	}
	contentType := "application/octet-stream"
	if name == "index.html" {
		contentType = "text/html; charset=utf-8"
	} else if strings.HasSuffix(name, ".css") {
		contentType = "text/css; charset=utf-8"
	} else if strings.HasSuffix(name, ".js") {
		contentType = "text/javascript; charset=utf-8"
	}
	context.Header("Cache-Control", "no-cache")
	context.Data(200, contentType, data)
}
