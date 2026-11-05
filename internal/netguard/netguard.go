// Package netguard keeps the shared browser off internal networks.
//
// Two layers:
//  1. ValidateURL gives a fast, friendly error when a user submits an
//     obviously internal URL.
//  2. EgressProxy is the real control. Chromium is launched with
//     --proxy-server pointing at it (and with the implicit loopback bypass
//     removed), so every HTTP, HTTPS and WebSocket connection the page makes
//     — including redirects, sub-resources and scripted fetches — goes
//     through a dialer that checks the *actual* IP being connected to. The
//     check happens in net.Dialer.Control, after DNS resolution, so DNS
//     rebinding cannot swap the address between check and connect.
//
// In production also enforce this at the network layer (Kubernetes
// NetworkPolicy / security groups) so the browser process cannot open
// sockets except to this proxy.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrBlocked = errors.New("destination blocked by network policy")

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"255.255.255.255/32",
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"2001::/23", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// IsBlockedIP reports whether ip is loopback, private, link-local (including
// the 169.254.169.254 cloud metadata endpoint), multicast or otherwise not a
// public unicast address.
func IsBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Guard decides whether a destination is allowed.
type Guard struct {
	AllowPrivate bool
}

func (g *Guard) checkAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: unresolved address %q", ErrBlocked, host)
	}
	if !g.AllowPrivate && IsBlockedIP(ip) {
		return fmt.Errorf("%w: %s", ErrBlocked, ip)
	}
	return nil
}

// Dialer returns a dialer that refuses blocked IPs at connect time.
func (g *Guard) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			return g.checkAddr(address)
		},
	}
}

// ValidateURL checks a user-supplied URL before the browser is told to open
// it. It resolves the host so obviously internal targets fail early with a
// clear message; the proxy still enforces the policy on every connection.
func (g *Guard) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 {
		return nil, errors.New("URL is empty or too long")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https URLs are allowed")
	}
	if u.User != nil {
		return nil, errors.New("URLs with embedded credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("URL has no host")
	}
	if g.AllowPrivate {
		return u, nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return nil, ErrBlocked
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if IsBlockedIP(ip) {
			return nil, ErrBlocked
		}
		return u, nil
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s", host)
	}
	for _, a := range addrs {
		if IsBlockedIP(a) {
			return nil, ErrBlocked
		}
	}
	return u, nil
}

// EgressProxy is a minimal forward proxy (HTTP + CONNECT) that only dials
// destinations allowed by Guard. It listens on loopback and is meant to be
// used only by the browsers this server launches.
type EgressProxy struct {
	guard     *Guard
	transport *http.Transport
	ln        net.Listener
	srv       *http.Server
	once      sync.Once
}

func NewEgressProxy(g *Guard) (*EgressProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	d := g.Dialer()
	p := &EgressProxy{
		guard: g,
		ln:    ln,
		transport: &http.Transport{
			Proxy:       nil,
			DialContext: d.DialContext,
			// One upstream connection per request: plain-HTTP traffic from
			// different sessions never shares a connection. (HTTPS goes
			// through CONNECT tunnels, one per browser connection.)
			DisableKeepAlives:     true,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("egress proxy stopped: %v", err)
		}
	}()
	return p, nil
}

// Addr is the host:port to pass to Chromium's --proxy-server.
func (p *EgressProxy) Addr() string { return p.ln.Addr().String() }

func (p *EgressProxy) Close() {
	p.once.Do(func() {
		_ = p.srv.Close()
		p.transport.CloseIdleConnections()
	})
}

func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "proxy: absolute http URL required", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		writeBlockedOrBadGateway(w, err)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *EgressProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	upstream, err := p.guard.Dialer().DialContext(ctx, "tcp", r.Host)
	if err != nil {
		writeBlockedOrBadGateway(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		if buf.Reader.Buffered() > 0 {
			_, _ = io.CopyN(upstream, buf, int64(buf.Reader.Buffered()))
		}
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		client.Close()
		upstream.Close()
	}()
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	c.Close()
}

func writeBlockedOrBadGateway(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBlocked) {
		http.Error(w, "Blocked by network policy", http.StatusForbidden)
		return
	}
	http.Error(w, "Upstream connection failed", http.StatusBadGateway)
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}
