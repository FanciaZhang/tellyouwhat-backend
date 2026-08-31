package adminui

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed static/*
var files embed.FS

func Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(request.URL.Path, "/")
		if name == "" || name == "setup" {
			name = "index.html"
		}
		data, err := files.ReadFile("static/" + name)
		if err != nil || strings.Contains(name, "..") {
			http.NotFound(writer, request)
			return
		}
		if name == "index.html" {
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else if strings.HasSuffix(name, ".css") {
			writer.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else if strings.HasSuffix(name, ".js") {
			writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		writer.Header().Set("Cache-Control", "no-cache")
		_, _ = writer.Write(data)
	})
}
