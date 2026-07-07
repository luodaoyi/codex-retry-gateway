package main

import (
	"embed"
	"net/http"
)

//go:embed web/index.html
var embeddedUI embed.FS

func loadEmbeddedUI() []byte {
	content, err := embeddedUI.ReadFile("web/index.html")
	if err != nil {
		return []byte("<html><body>UI not available</body></html>")
	}
	return content
}

func writeHTMLResponse(writer http.ResponseWriter, body []byte) {
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	writer.Header().Set("cache-control", "no-store, max-age=0")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func writeJSONResponse(writer http.ResponseWriter, status int, body []byte) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.Header().Set("cache-control", "no-store, max-age=0")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
