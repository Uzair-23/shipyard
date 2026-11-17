package router

import (
	"html/template"
	"io"
	"net/http"
)

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;background:#f6f7f9;color:#1c2024}
main{max-width:30rem;padding:2rem}h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#555;line-height:1.5}
code{background:#e8eaee;padding:.1rem .3rem;border-radius:4px}</style></head>
<body><main><h1>{{.Title}}</h1><p>{{.Message}}</p></main></body></html>`))

// wakingPage is shown to a browser while a sleeping app starts. It polls the
// router's ready endpoint (which also keeps the wake going) and reloads.
const wakingPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Waking up&hellip;</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;background:#f6f7f9;color:#1c2024}
main{max-width:30rem;padding:2rem}h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#555;line-height:1.5}</style></head>
<body><main><h1>Waking up&hellip;</h1><p id="m">This app was asleep. It is starting now and this page will reload by itself.</p></main>
<script>
(function () {
  var tries = 0;
  function poll() {
    fetch('/__shipyard/ready', { cache: 'no-store', credentials: 'same-origin' }).then(function (r) {
      if (r.status === 200) { location.reload(); return; }
      if (r.status !== 202 || ++tries > 60) {
        document.getElementById('m').textContent = 'The app did not start. Reload this page to try again.';
        return;
      }
      setTimeout(poll, 1000);
    }).catch(function () { setTimeout(poll, 2000); });
  }
  poll();
})();
</script></body></html>`

func writeWakingPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, wakingPage)
}

// writeBusy answers 503 with Retry-After so clients try again shortly.
func writeBusy(w http.ResponseWriter, title, message string) {
	w.Header().Set("Retry-After", "2")
	writePage(w, http.StatusServiceUnavailable, title, message)
}

// writePage renders a friendly error page. Title and message are escaped, so a
// slug from the Host header cannot inject markup.
func writePage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, map[string]string{"Title": title, "Message": message})
}
