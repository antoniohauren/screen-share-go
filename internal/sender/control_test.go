package sender

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlBoundary(t *testing.T) {
	c := &control{state: "stopped"}
	h := c.handler("127.0.0.1:1234", "secret")
	for _, test := range []struct {
		name, method, path, host, origin, content string
		status                                    int
	}{
		{"status", "GET", "/secret/status", "127.0.0.1:1234", "", "", 200},
		{"capability", "GET", "/wrong/status", "127.0.0.1:1234", "", "", 404},
		{"dns rebinding", "GET", "/secret/status", "evil.example", "", "", 404},
		{"cross origin", "POST", "/secret/stop", "127.0.0.1:1234", "https://evil.example", "application/json", 403},
		{"simple form", "POST", "/secret/stop", "127.0.0.1:1234", "", "application/x-www-form-urlencoded", 415},
		{"stop", "POST", "/secret/stop", "127.0.0.1:1234", "http://127.0.0.1:1234", "application/json", 200},
		{"unknown source", "POST", "/secret/start", "127.0.0.1:1234", "", "application/json", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://"+test.host+test.path, strings.NewReader(`{"source":"unknown","token":"secret"}`))
			r.Host = test.host
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Content-Type", test.content)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("got %d want %d: %s", w.Code, test.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("control responses must not be cached")
			}
		})
	}
}
