// Package api exposes the session REST endpoints, the WebSocket endpoint and
// the static client.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/example/collab-browser/internal/config"
	"github.com/example/collab-browser/internal/netguard"
	"github.com/example/collab-browser/internal/session"
)

type Server struct {
	cfg      *config.Config
	mgr      *session.Manager
	upgrader websocket.Upgrader
	static   fs.FS
}

func New(cfg *config.Config, mgr *session.Manager, static fs.FS) *Server {
	s := &Server{cfg: cfg, mgr: mgr, static: static}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  16 << 10,
		WriteBufferSize: 16 << 10,
		CheckOrigin:     s.checkOrigin,
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/browser-sessions", s.handleCreate)
	mux.HandleFunc("GET /api/browser-sessions/{id}", s.handleGet)
	mux.HandleFunc("DELETE /api/browser-sessions/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/browser-sessions/{id}/connect", s.handleConnect)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	files := http.FileServerFS(s.static)
	mux.HandleFunc("GET /sdk.js", func(w http.ResponseWriter, r *http.Request) {
		// Loaded by other sites' pages, so it must be usable cross-origin.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	mux.Handle("GET /", files)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; media-src 'self' blob:; img-src 'self' data:; style-src 'self'; script-src 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

// checkOrigin allows WebSockets from this server's own pages and from the
// sites listed in ALLOWED_ORIGINS (the apps that embed the SDK).
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients; they still need a participant token
	}
	for _, o := range s.cfg.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func (s *Server) apiAuthorized(r *http.Request) bool {
	if s.cfg.APIToken == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.cfg.APIToken)) == 1
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<10)
	return json.NewDecoder(r.Body).Decode(v)
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	prefixes := s.cfg.AllowedURLPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requireApiToken": s.cfg.APIToken != "", "allowedUrlPrefixes": prefixes})
}

// handleCreate starts a session and returns its two seats. Call it from
// your backend (with API_TOKEN), then give `host` to the host's page and
// `guest` to the guest's page; each passes its seat to CollabBrowser.mount.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		URL  string `json:"url"`
		Host struct {
			Name string `json:"name"`
		} `json:"host"`
		Guest struct {
			Name string `json:"name"`
		} `json:"guest"`
		// Optional: size the shared browser to the kiosk's screen.
		Viewport struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"viewport"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	c, err := s.mgr.Create(r.Context(), session.CreateOptions{
		URL: req.URL, HostName: req.Host.Name, GuestName: req.Guest.Name,
		ViewportWidth: req.Viewport.Width, ViewportHeight: req.Viewport.Height,
	})
	switch {
	case errors.Is(err, session.ErrTooMany):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, netguard.ErrBlocked):
		writeErr(w, http.StatusBadRequest, "That address is not allowed (private or internal network).")
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"sessionId": c.Session.ID,
		"status":    c.Session.Status(),
		"viewport":  c.Session.Viewport(),
		"host":      c.Host,
		"guest":     c.Guest,
	})
}

func (s *Server) sessionFor(w http.ResponseWriter, r *http.Request) *session.Session {
	sess, err := s.mgr.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return nil
	}
	return sess
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFor(w, r)
	if sess == nil {
		return
	}
	if !s.canManage(sess, r) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	writeJSON(w, http.StatusOK, sess.Info())
}

// canManage: the host seat's token, or the API token (your backend).
func (s *Server) canManage(sess *session.Session, r *http.Request) bool {
	return sess.IsHost(bearer(r)) || (s.cfg.APIToken != "" && s.apiAuthorized(r))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFor(w, r)
	if sess == nil {
		return
	}
	if !s.canManage(sess, r) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	reason := "ended_by_host"
	if !sess.IsHost(bearer(r)) {
		reason = "ended_by_api"
	}
	go sess.Close(reason)
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": sess.ID, "status": "closing"})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	sess, err := s.mgr.Get(r.PathValue("id"))
	var p *session.Participant
	if err == nil {
		q := r.URL.Query()
		p = sess.Authenticate(q.Get("participantId"), q.Get("token"))
	}
	if p == nil {
		status, reason := http.StatusUnauthorized, "unauthorized"
		if err != nil {
			status, reason = http.StatusNotFound, "not_found"
		}
		// A browser can't see the HTTP status of a refused WebSocket and
		// would just keep retrying. So for real upgrade requests, accept,
		// say why, and close: the SDK then reports the session as ended.
		if websocket.IsWebSocketUpgrade(r) {
			if ws, uerr := s.upgrader.Upgrade(w, r, nil); uerr == nil {
				_ = ws.WriteJSON(map[string]any{"type": "session_closed", "reason": reason, "ts": time.Now().UnixMilli()})
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason), time.Now().Add(time.Second))
				ws.Close()
			}
			return
		}
		writeErr(w, status, reason)
		return
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	start := time.Now()
	sess.Attach(p, ws)
	log.Printf("session %s: participant %s disconnected after %s", sess.ID, p.ID, time.Since(start).Round(time.Second))
}
