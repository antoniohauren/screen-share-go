package main

import "testing"

func TestValidDomain(t *testing.T) {
	for _, domain := range []string{"share.detrouse.com", "Share.Example.com", "screen-share.example.com"} {
		if !validDomain(domain) {
			t.Errorf("rejected hostname %q", domain)
		}
	}
	for _, domain := range []string{"", "localhost", "https://share.detrouse.com", "example.com:443", "example.com/path", "127.0.0.1", ".example.com", "-share.example.com", "share-.example.com", "share_example.com"} {
		if validDomain(domain) {
			t.Errorf("accepted invalid hostname %q", domain)
		}
	}
}

func TestLoopbackHTTPAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.2:8080", "[::1]:8080"} {
		if !loopbackHTTPAddress(addr) {
			t.Errorf("rejected loopback %q", addr)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.1:8080", "203.0.113.1:8080", "localhost:8080", "invalid"} {
		if loopbackHTTPAddress(addr) {
			t.Errorf("accepted non-loopback HTTP %q", addr)
		}
	}
}
