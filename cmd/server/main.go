package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"screen-share/internal/relay"
	"screen-share/internal/viewer"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "loopback HTTP listen address (development/reverse proxy only; ignored with -domain)")
	domain := flag.String("domain", "", "public DNS hostname, e.g. share.detrouse.com; enables HTTPS :443 and ACME/redirect :80")
	certDir := flag.String("cert-dir", "./certs", "ACME certificate cache directory")
	tokenFile := flag.String("token-file", "", "broadcaster secret file (otherwise SCREENSHARE_TOKEN)")
	udpPort := flag.Int("udp-port", 50000, "single WebRTC UDP port")
	publicIP := flag.String("public-ip", "", "public IPv4 address advertised when behind NAT")
	maxViewers := flag.Int("max-viewers", 10, "maximum simultaneous viewers (1-10)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if _, _, err := net.SplitHostPort(*addr); err != nil {
		return fmt.Errorf("invalid -addr: %w", err)
	}
	if *domain == "" && !loopbackHTTPAddress(*addr) {
		return errors.New("HTTP requires a numeric loopback -addr; use -domain for public HTTPS")
	}
	if *udpPort < 1 || *udpPort > 65535 {
		return errors.New("-udp-port must be between 1 and 65535")
	}
	if *publicIP != "" && (net.ParseIP(*publicIP) == nil || net.ParseIP(*publicIP).To4() == nil) {
		return errors.New("-public-ip must be an IPv4 address")
	}
	if *domain != "" && !validDomain(*domain) {
		return errors.New("-domain must be a DNS hostname without scheme, port, or path")
	}
	token := os.Getenv("SCREENSHARE_TOKEN")
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		token = string(data)
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return errors.New("set a nonempty single-line secret using -token-file or SCREENSHARE_TOKEN")
	}
	r, err := relay.New(relay.Config{Token: token, UDPPort: *udpPort, PublicIP: *publicIP, MaxViewers: *maxViewers})
	if err != nil {
		return err
	}
	defer r.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/", r)
	mux.Handle("/", viewer.Handler())
	server := newServer(*addr, mux)
	servers := []*http.Server{server}
	errorsCh := make(chan error, 2)
	if *domain == "" {
		log.Printf("development HTTP on %s; WebRTC UDP :%d", *addr, *udpPort)
		go func() { errorsCh <- server.ListenAndServe() }()
	} else {
		if err := os.MkdirAll(*certDir, 0700); err != nil {
			return fmt.Errorf("create certificate cache: %w", err)
		}
		manager := &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(*certDir), HostPolicy: autocert.HostWhitelist(*domain)}
		server.Addr = ":443"
		server.TLSConfig = manager.TLSConfig()
		server.TLSConfig.MinVersion = tls.VersionTLS12
		challenge := newServer(":80", manager.HTTPHandler(nil))
		servers = append(servers, challenge)
		log.Printf("HTTPS for %s on :443, ACME/redirect :80; WebRTC UDP :%d", *domain, *udpPort)
		go func() { errorsCh <- challenge.ListenAndServe() }()
		go func() { errorsCh <- server.ListenAndServeTLS("", "") }()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
			_ = server.Close()
		}
	}
	return err
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 25 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
}

func validDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func loopbackHTTPAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}
