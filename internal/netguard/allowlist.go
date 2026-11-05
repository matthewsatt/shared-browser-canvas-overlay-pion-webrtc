package netguard

import (
	"fmt"
	"net/url"
	"strings"
)

// URLAllowlist limits which pages the shared browser may show at the top
// level. It's separate from the network guard: sub-resources (images,
// scripts, APIs) may still come from any public host.
//
// Each entry is a URL prefix. Scheme and host must match exactly (default
// ports are ignored). The path matches as a prefix on path-segment
// boundaries: "https://x.com/kiosk" allows /kiosk and /kiosk/..., but not
// /kiosk-admin. Query strings and fragments are ignored. An empty list
// allows everything.
type URLAllowlist struct {
	prefixes []urlPrefix
}

type urlPrefix struct {
	scheme, host, path string
}

func ParseURLAllowlist(entries []string) (*URLAllowlist, error) {
	a := &URLAllowlist{}
	for _, e := range entries {
		u, err := url.Parse(e)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("allowed URL prefix %q: must be an absolute http(s) URL", e)
		}
		p := urlPrefix{scheme: u.Scheme, host: normHost(u), path: u.EscapedPath()}
		if p.path == "" {
			p.path = "/"
		}
		a.prefixes = append(a.prefixes, p)
	}
	return a, nil
}

// Empty reports whether the list allows everything.
func (a *URLAllowlist) Empty() bool { return a == nil || len(a.prefixes) == 0 }

func (a *URLAllowlist) Allows(raw string) bool {
	if a.Empty() {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme, host, path := strings.ToLower(u.Scheme), normHost(u), u.EscapedPath()
	if path == "" {
		path = "/"
	}
	for _, p := range a.prefixes {
		if scheme != p.scheme || host != p.host {
			continue
		}
		if strings.HasSuffix(p.path, "/") {
			if strings.HasPrefix(path, p.path) {
				return true
			}
		} else if path == p.path || strings.HasPrefix(path, p.path+"/") {
			return true
		}
	}
	return false
}

func normHost(u *url.URL) string {
	h, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if port != "" {
		return h + ":" + port
	}
	return h
}
