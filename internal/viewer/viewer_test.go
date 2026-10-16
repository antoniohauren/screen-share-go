package viewer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedAssets(t *testing.T) {
	for path, contentType := range map[string]string{
		"/": "text/html", "/styles.css": "text/css",
		"/app.mjs": "text/javascript", "/state.mjs": "text/javascript",
	} {
		t.Run(path, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := httptest.NewRecorder()
				Handler().ServeHTTP(w, httptest.NewRequest(method, path, nil))
				if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), contentType) {
					t.Fatalf("%s: status %d, headers %v", method, w.Code, w.Header())
				}
				if w.Header().Get("Content-Length") == "" {
					t.Fatal("missing asset length")
				}
				if (method == http.MethodHead) != (w.Body.Len() == 0) {
					t.Fatalf("unexpected %s body length: %d", method, w.Body.Len())
				}
				if w.Header().Get("X-Content-Type-Options") != "nosniff" ||
					!strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
					!strings.Contains(w.Header().Get("Permissions-Policy"), "microphone=()") {
					t.Fatal("missing security headers")
				}
			}
		})
	}
}

func TestRejectUnknownPaths(t *testing.T) {
	for _, path := range []string{"/index.html", "/api/status", "/api/watch", "/missing", "/../viewer.go", "/%2e%2e/viewer.go", "//styles.css", "/styles.css/", "/app.mjs/../viewer.go"} {
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, w.Code)
		}
	}
}

func TestRejectOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest(method, "/", nil))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s: got %d, Allow %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
}
