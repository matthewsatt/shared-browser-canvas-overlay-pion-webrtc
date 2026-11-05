package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/example/collab-browser/internal/config"
	"github.com/example/collab-browser/internal/netguard"
)

var (
	ErrNotFound      = errors.New("session not found")
	ErrTooMany       = errors.New("too many active sessions")
	ErrURLNotAllowed = errors.New("that page isn't on this server's list of allowed URLs")
)

// Manager owns all sessions on this server.
type Manager struct {
	cfg       *config.Config
	guard     *netguard.Guard
	allow     *netguard.URLAllowlist
	proxy     *netguard.EgressProxy
	webrtcAPI *webrtc.API

	mu       sync.Mutex
	sessions map[string]*Session
	stop     chan struct{}
}

func NewManager(cfg *config.Config) (*Manager, error) {
	guard := &netguard.Guard{AllowPrivate: cfg.AllowPrivateNetworks}
	allow, err := netguard.ParseURLAllowlist(cfg.AllowedURLPrefixes)
	if err != nil {
		return nil, fmt.Errorf("ALLOWED_URL_PREFIXES: %w", err)
	}
	proxy, err := netguard.NewEgressProxy(guard)
	if err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	if cfg.UDPPortMin > 0 && cfg.UDPPortMax >= cfg.UDPPortMin {
		if err := se.SetEphemeralUDPPortRange(cfg.UDPPortMin, cfg.UDPPortMax); err != nil {
			return nil, err
		}
	}
	if len(cfg.PublicIPs) > 0 {
		se.SetNAT1To1IPs(cfg.PublicIPs, webrtc.ICECandidateTypeHost)
	}
	// Don't advertise container-internal interfaces like docker0.
	se.SetInterfaceFilter(func(name string) bool { return name != "docker0" })
	se.SetIPFilter(func(ip net.IP) bool { return !ip.IsLinkLocalUnicast() })

	m := &Manager{
		cfg:       cfg,
		guard:     guard,
		allow:     allow,
		proxy:     proxy,
		webrtcAPI: webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)),
		sessions:  map[string]*Session{},
		stop:      make(chan struct{}),
	}
	m.sweepProfiles(0) // nothing is live yet: remove anything a previous run left
	go m.janitor()
	return m, nil
}

func (m *Manager) Config() *config.Config { return m.cfg }

func (m *Manager) pionICEServers() []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(m.cfg.ICEServers))
	for _, s := range m.cfg.ICEServers {
		out = append(out, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	return out
}

// Seat is what one participant needs to connect: hand it to that person's
// page (e.g. CollabBrowser.mount(el, seat)).
type Seat struct {
	SessionID     string `json:"sessionId"`
	ParticipantID string `json:"participantId"`
	Token         string `json:"token"`
	Role          string `json:"role"`
	Name          string `json:"name"`
}

type Created struct {
	Session     *Session
	Host, Guest Seat
}

// CreateOptions describes a new session.
type CreateOptions struct {
	URL                 string
	HostName, GuestName string
	// The shared browser's viewport in CSS px. Size it to the screen that
	// matters most (usually the kiosk) and the picture fills that screen at
	// 1:1. Zero uses VIEWPORT_WIDTH × VIEWPORT_HEIGHT. See fitViewport.
	ViewportWidth, ViewportHeight int
}

// Create validates the URL and starts a new session with its two seats.
// The browser starts in the background; status is "starting" until ready.
func (m *Manager) Create(ctx context.Context, o CreateOptions) (*Created, error) {
	rawURL := o.URL
	u, err := m.guard.ValidateURL(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if !m.allow.Allows(u.String()) {
		return nil, ErrURLNotAllowed
	}
	m.mu.Lock()
	if len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return nil, ErrTooMany
	}
	id := randomID()
	m.sessions[id] = nil // reserve the slot
	m.mu.Unlock()

	vw, vh := fitViewport(o.ViewportWidth, o.ViewportHeight, m.cfg.ViewportWidth, m.cfg.ViewportHeight)
	s, err := newSession(m, id, u.String(), o.HostName, o.GuestName, vw, vh)
	m.mu.Lock()
	if err != nil {
		delete(m.sessions, id)
		m.mu.Unlock()
		return nil, err
	}
	m.sessions[id] = s
	m.mu.Unlock()
	seat := func(p *Participant) Seat {
		return Seat{SessionID: id, ParticipantID: p.ID, Token: p.token, Role: p.Role, Name: p.Name}
	}
	return &Created{Session: s, Host: seat(s.seat(RoleHost)), Guest: seat(s.seat(RoleGuest))}, nil
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return nil, ErrNotFound
	}
	return s, nil
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// IsHost reports whether token is the host seat's token, which may manage
// (inspect, end) the session.
func (s *Session) IsHost(token string) bool {
	h := s.seat(RoleHost)
	return h != nil && tokenEqual(h.token, token)
}

// Authenticate returns the participant for a WebSocket connection.
func (s *Session) Authenticate(participantID, token string) *Participant {
	return s.authParticipant(participantID, token)
}

// Info is a summary for the API.
func (s *Session) Info() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	connected := 0
	for _, p := range s.participants {
		if p.conn != nil {
			connected++
		}
	}
	in, out := s.encoder.Stats()
	return map[string]any{
		"sessionId":    s.ID,
		"status":       s.status,
		"targetUrl":    s.targetURL,
		"currentUrl":   s.currentURL,
		"title":        s.title,
		"createdAt":    s.CreatedAt,
		"lastActivity": s.lastActivity,
		"participants": s.participantList(),
		"connected":    connected,
		"stats":        map[string]uint64{"screencastFrames": in, "encodedFrames": out},
	}
}

// Browser exposes the session's browser (tests and diagnostics).
func (s *Session) Browser() *Browser { return s.getBrowser() }

// sweepProfiles deletes browser profile directories that no live session
// owns: left by a crash, or re-created by a straggling Chromium helper after
// its session's cleanup. Profiles hold cookies and cached pages (possibly
// patient data), so none may outlive its session. Directories younger than
// minAge are skipped: a session being created owns one before it's listed.
func (m *Manager) sweepProfiles(minAge time.Duration) {
	m.mu.Lock()
	live := map[string]bool{}
	for _, s := range m.sessions {
		if s != nil {
			live[s.dataDir] = true
		}
	}
	m.mu.Unlock()
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), profilePrefix+"*"))
	for _, d := range dirs {
		fi, err := os.Lstat(d)
		if live[d] || err != nil || !fi.IsDir() || time.Since(fi.ModTime()) < minAge {
			continue
		}
		if err := os.RemoveAll(d); err == nil {
			log.Printf("removed leftover browser profile %s", filepath.Base(d))
		}
	}
}

func (m *Manager) janitor() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	sweep := time.NewTicker(30 * time.Second)
	defer sweep.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-sweep.C:
			m.sweepProfiles(30 * time.Second)
		case now := <-t.C:
			m.mu.Lock()
			list := make([]*Session, 0, len(m.sessions))
			for _, s := range m.sessions {
				if s != nil {
					list = append(list, s)
				}
			}
			m.mu.Unlock()
			for _, s := range list {
				s.janitor(now)
			}
		}
	}
}

// Shutdown closes every session.
func (m *Manager) Shutdown() {
	close(m.stop)
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s != nil {
			list = append(list, s)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Add(1)
		go func(s *Session) { defer wg.Done(); s.Close("server_shutdown") }(s)
	}
	wg.Wait()
	m.proxy.Close()
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenEqual(a, b string) bool {
	return a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
