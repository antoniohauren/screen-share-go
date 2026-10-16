package sender

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed control.html
var controlHTML string

type control struct {
	mu        sync.Mutex
	sources   []Source
	cancel    context.CancelFunc
	done      chan struct{}
	state     string
	lastError string
	dir       string
}

func (c *control) start(id, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("sender already running; stop before starting again")
	}
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return errors.New("enter a valid publishing token")
	}
	var selected *Source
	for _, s := range c.sources {
		if s.ID == id {
			copy := s
			selected = &copy
			break
		}
	}
	if selected == nil {
		return errors.New("select a current capture source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	c.state = "starting"
	c.lastError = ""
	done := c.done
	go func() {
		err := runSession(ctx, *selected, token, c.dir, func() {
			c.mu.Lock()
			if ctx.Err() == nil {
				c.state = "streaming"
			}
			c.mu.Unlock()
		})
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			c.lastError = err.Error()
		}
		c.state = "stopped"
		c.cancel = nil
		close(done)
	}()
	return nil
}

func (c *control) stop() error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	if cancel != nil {
		c.state = "stopping"
		cancel()
	}
	c.mu.Unlock()
	if cancel != nil {
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			return errors.New("capture shutdown timed out")
		}
	}
	return nil
}

func (c *control) handler(host, capability string) http.Handler {
	prefix := "/" + capability + "/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Host != host || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+host {
			http.Error(w, "origin rejected", http.StatusForbidden)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == "" && r.Method == "GET" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, controlHTML)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case path == "sources" && r.Method == "GET":
			sources, err := enumerateSources()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			c.mu.Lock()
			c.sources = sources
			c.mu.Unlock()
			json.NewEncoder(w).Encode(sources)
		case path == "status" && r.Method == "GET":
			c.mu.Lock()
			defer c.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"state": c.state, "error": c.lastError})
		case (path == "start" || path == "stop") && r.Method == "POST":
			if r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "JSON required", 415)
				return
			}
			var err error
			if path == "start" {
				var input struct {
					Source string `json:"source"`
					Token  string `json:"token"`
				}
				decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
				decoder.DisallowUnknownFields()
				if err = decoder.Decode(&input); err == nil {
					err = c.start(input.Source, input.Token)
				}
			} else {
				err = c.stop()
			}
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r)
		}
	})
}

func Run() error {
	if err := platformSupported(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		listener.Close()
		return err
	}
	host := listener.Addr().String()
	capability := hex.EncodeToString(secret[:])
	c := &control{dir: filepath.Dir(exe), state: "stopped"}
	server := &http.Server{Handler: c.handler(host, capability), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	url := "http://" + host + "/" + capability + "/"
	fmt.Println("Native sender control panel:", url)
	go func() {
		if err := openBrowser(url); err != nil {
			fmt.Println("Open the control-panel URL manually:", err)
		}
	}()
	go func() { <-ctx.Done(); c.stop(); server.Close() }()
	err = server.Serve(listener)
	c.stop()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
