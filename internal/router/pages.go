package router

import (
	"html/template"
	"net/http"
)

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;background:#f6f7f9;color:#1c2024}
main{max-width:30rem;padding:2rem}h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#555;line-height:1.5}
code{background:#e8eaee;padding:.1rem .3rem;border-radius:4px}</style></head>
<body><main><h1>{{.Title}}</h1><p>{{.Message}}</p></main></body></html>`))

// writePage renders a friendly error page. Title and message are escaped, so a
// slug from the Host header cannot inject markup.
func writePage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, map[string]string{"Title": title, "Message": message})
}
