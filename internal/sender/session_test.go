package sender

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestSignaling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid request")
		}
		var offer webrtc.SessionDescription
		if err := json.NewDecoder(r.Body).Decode(&offer); err != nil || offer.Type != webrtc.SDPTypeOffer {
			t.Error("invalid offer", err)
		}
		json.NewEncoder(w).Encode(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "test answer"})
	}))
	defer server.Close()
	answer, err := exchange(context.Background(), server.Client(), server.URL, "secret", webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "test offer"})
	if err != nil || answer.SDP != "test answer" {
		t.Fatal(answer, err)
	}
}

func TestSignalingRejectsRedirectAndInvalidAnswer(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusCreated, http.StatusOK} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://127.0.0.1:1/leak")
				w.WriteHeader(code)
				w.Write([]byte(`{"type":"offer","sdp":"not an answer"}`))
			}))
			defer server.Close()
			_, err := exchange(context.Background(), server.Client(), server.URL, "secret", webrtc.SessionDescription{})
			if err == nil {
				t.Fatal("accepted rejected or invalid answer")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exchange(ctx, &http.Client{Timeout: time.Second}, "http://127.0.0.1:1", "secret", webrtc.SessionDescription{})
	if err == nil {
		t.Fatal("ignored cancellation")
	}
	_, err = exchange(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "secret\r\nInjected: true", webrtc.SessionDescription{})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatal("accepted header injection", err)
	}
}
