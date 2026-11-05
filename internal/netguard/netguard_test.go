package netguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "224.0.0.1",
	}
	for _, s := range blocked {
		if !IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestValidateURL(t *testing.T) {
	g := &Guard{}
	ctx := context.Background()
	for _, raw := range []string{
		"http://127.0.0.1/", "http://localhost:8080/", "http://foo.localhost/", "http://[::1]/",
		"http://169.254.169.254/latest/meta-data/", "ftp://example.com/", "file:///etc/passwd",
		"javascript:alert(1)", "http://user:pass@example.com/", "",
	} {
		if _, err := g.ValidateURL(ctx, raw); err == nil {
			t.Errorf("%q should be rejected", raw)
		}
	}
	u, err := g.ValidateURL(ctx, "93.184.215.14/path")
	if err != nil || u.String() != "https://93.184.215.14/path" {
		t.Errorf("bare host should default to https, got %v %v", u, err)
	}
}

// The proxy is the real enforcement point: even when a hostname resolves
// to a private address at connect time (the DNS-rebinding case), the dial
// is refused because the check runs on the resolved IP.
func TestEgressProxyBlocksPrivateDestinations(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "secret")
	}))
	defer internal.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))

	p, err := NewEgressProxy(&Guard{AllowPrivate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse("http://" + p.Addr())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}

	for _, target := range []string{
		"http://127.0.0.1:" + port + "/",
		"http://localhost:" + port + "/", // hostname that resolves to loopback
	} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s via proxy: got %d, want 403", target, resp.StatusCode)
		}
	}

	// CONNECT (used for HTTPS and WebSockets).
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT 127.0.0.1:%s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\n\r\n", port, port)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT to loopback: got %d, want 403", resp.StatusCode)
	}
}

func TestEgressProxyAllowsWhenPermitted(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer internal.Close()
	p, err := NewEgressProxy(&Guard{AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse("http://" + p.Addr())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	resp, err := client.Get(internal.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestDialerControlRejects(t *testing.T) {
	d := (&Guard{}).Dialer()
	_, err := d.Dial("tcp", "127.0.0.1:1")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
}
