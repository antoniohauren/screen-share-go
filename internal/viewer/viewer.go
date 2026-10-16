// Package viewer serves the public, embedded viewing room.
package viewer

import (
	"embed"
	"net/http"
	"strconv"
)

//go:embed index.html styles.css app.mjs state.mjs
var assets embed.FS

// Handler serves only the viewer's known assets. API routes belong to the server.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; media-src 'self' blob:; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-cache")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var name, contentType string
		switch r.URL.Path {
		case "/":
			name, contentType = "index.html", "text/html; charset=utf-8"
		case "/styles.css":
			name, contentType = "styles.css", "text/css; charset=utf-8"
		case "/app.mjs", "/state.mjs":
			name, contentType = r.URL.Path[1:], "text/javascript; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		body, err := assets.ReadFile(name)
		if err != nil {
			http.Error(w, "Asset unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}
