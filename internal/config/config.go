// Package config loads server settings from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ICEServer mirrors the browser RTCIceServer shape so it can be passed to
// clients unchanged.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type Config struct {
	ListenAddr string // HTTP listen address, e.g. ":8080"

	// Auth. If APIToken is set, creating sessions requires
	// "Authorization: Bearer <APIToken>". Replace with your real auth.
	APIToken       string
	AllowedOrigins []string // WebSocket Origin allowlist; empty = same host only

	// Browser.
	ChromePath      string
	ChromeNoSandbox bool // needed when running as root in a container without user namespaces
	ViewportWidth   int
	ViewportHeight  int
	JPEGQuality     int

	// Video.
	FFmpegPath       string
	FPS              int
	BitrateKbps      int
	KeyframeInterval time.Duration

	// WebRTC networking.
	ICEServers []ICEServer
	UDPPortMin uint16
	UDPPortMax uint16
	PublicIPs  []string // NAT 1:1 mapping, e.g. the node's public IP

	// Limits and lifecycle. Every session has exactly two seats (host and
	// guest), created with the session.
	MaxSessions        int
	IdleTimeout        time.Duration // close when nobody has been connected this long
	MaxSessionLifetime time.Duration
	KeyboardLease      time.Duration
	PointerLockTimeout time.Duration
	MaxBrowserRestarts int
	MaxEventsPerSecond int

	// Pages the shared browser may show: URL prefixes such as
	// "https://checkin.example.com/". Empty allows any public page.
	AllowedURLPrefixes []string

	// Network policy for the shared browser. Leave false in production.
	AllowPrivateNetworks bool
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:           env("LISTEN_ADDR", ":8080"),
		APIToken:             os.Getenv("API_TOKEN"),
		AllowedOrigins:       splitList(os.Getenv("ALLOWED_ORIGINS")),
		ChromePath:           os.Getenv("CHROME_PATH"),
		ChromeNoSandbox:      envBool("CHROME_NO_SANDBOX", false),
		ViewportWidth:        envInt("VIEWPORT_WIDTH", 1280),
		ViewportHeight:       envInt("VIEWPORT_HEIGHT", 720),
		JPEGQuality:          envInt("JPEG_QUALITY", 80),
		FFmpegPath:           env("FFMPEG_PATH", "ffmpeg"),
		FPS:                  envInt("FPS", 25),
		BitrateKbps:          envInt("BITRATE_KBPS", 2500),
		KeyframeInterval:     envDur("KEYFRAME_INTERVAL", time.Second),
		UDPPortMin:           uint16(envInt("WEBRTC_UDP_PORT_MIN", 0)),
		UDPPortMax:           uint16(envInt("WEBRTC_UDP_PORT_MAX", 0)),
		PublicIPs:            splitList(os.Getenv("WEBRTC_PUBLIC_IPS")),
		MaxSessions:          envInt("MAX_SESSIONS", 10),
		IdleTimeout:          envDur("IDLE_TIMEOUT", 2*time.Minute),
		MaxSessionLifetime:   envDur("MAX_SESSION_LIFETIME", 4*time.Hour),
		AllowedURLPrefixes:   splitList(os.Getenv("ALLOWED_URL_PREFIXES")),
		KeyboardLease:        envDur("KEYBOARD_LEASE", 1500*time.Millisecond),
		PointerLockTimeout:   envDur("POINTER_LOCK_TIMEOUT", 10*time.Second),
		MaxBrowserRestarts:   envInt("MAX_BROWSER_RESTARTS", 3),
		MaxEventsPerSecond:   envInt("MAX_EVENTS_PER_SECOND", 400),
		AllowPrivateNetworks: envBool("ALLOW_PRIVATE_NETWORKS", false),
	}
	if raw := os.Getenv("ICE_SERVERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.ICEServers); err != nil {
			return nil, fmt.Errorf("ICE_SERVERS: %w", err)
		}
	} else {
		c.ICEServers = []ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	}
	if c.ViewportWidth%2 != 0 || c.ViewportHeight%2 != 0 {
		return nil, fmt.Errorf("viewport dimensions must be even (H.264 4:2:0)")
	}
	if c.FPS < 5 || c.FPS > 60 {
		return nil, fmt.Errorf("FPS must be between 5 and 60")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
