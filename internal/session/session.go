package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/example/collab-browser/internal/media"
)

type Status string

const (
	StatusStarting   Status = "starting"
	StatusReady      Status = "ready"
	StatusRestarting Status = "restarting"
	StatusClosed     Status = "closed"
	StatusError      Status = "error"
)

var palette = []string{
	"#e6194b", "#3cb44b", "#4363d8", "#f58231", "#911eb4", "#42d4f4",
	"#f032e6", "#9a6324", "#469990", "#800000", "#808000", "#000075",
}

// Participant is one of the session's two seats (host or guest). Seats
// exist for the session's whole life; connecting and disconnecting only
// changes conn.
type Participant struct {
	ID    string
	Name  string
	Role  string // RoleHost or RoleGuest
	Color string
	token string

	// Guarded by Session.mu.
	conn         *Conn
	lastSeen     time.Time
	cursorX      float64
	cursorY      float64
	cursorActive bool

	// Input state, owned by the input worker goroutine.
	buttons    int64
	lastPress  time.Time
	lastPressX float64
	lastPressY float64
	lastButton string
	clickCount int64
}

func (p *Participant) info() ParticipantInfo {
	return ParticipantInfo{ID: p.ID, Name: p.Name, Role: p.Role, Color: p.Color, Connected: p.conn != nil}
}

type inputEvent struct {
	p   *Participant
	msg ClientMessage
	at  time.Time
}

type dialogState struct {
	Kind          string `json:"kind"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"defaultPrompt"`
}

// Session is one shared browser and everyone connected to it.
type Session struct {
	ID        string
	CreatedAt time.Time
	vw, vh    int // the shared viewport, fixed for the session's life

	mgr     *Manager
	ctx     context.Context
	cancel  context.CancelFunc
	encoder *media.Encoder
	dataDir string
	input   chan inputEvent

	navMu sync.Mutex // serializes navigation commands

	mu            sync.Mutex
	status        Status
	browser       *Browser
	restarts      int
	targetURL     string
	currentURL    string
	title         string
	loading       bool
	lastActivity  time.Time
	emptySince    time.Time
	participants  map[string]*Participant
	colorNext     int
	dialog        *dialogState
	openSelect    json.RawMessage
	pageFields    json.RawMessage // latest text-field positions (inject.js)
	pageFocus     json.RawMessage // whether a text field has focus
	lastPointerBy string
	closeOnce     sync.Once

	// Connections receiving video as JPEG frames over the WebSocket instead
	// of (or until) WebRTC. See setWSVideo.
	wsMu      sync.Mutex
	wsViewers map[*Conn]struct{}

	// Lock state, owned by the input worker.
	kbOwner   string
	kbUntil   time.Time
	ptrOwner  string
	ptrSince  time.Time
	lastLocks [2]string
}

func newSession(m *Manager, id, targetURL, hostName, guestName string, vw, vh int) (*Session, error) {
	dir, err := os.MkdirTemp("", profilePrefix+"*")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	enc, err := media.NewEncoder(media.EncoderConfig{
		FFmpegPath:       m.cfg.FFmpegPath,
		Width:            vw,
		Height:           vh,
		FPS:              m.cfg.FPS,
		BitrateKbps:      scaledBitrate(m.cfg.BitrateKbps, vw, vh),
		KeyframeInterval: m.cfg.KeyframeInterval,
	}, "session-"+id)
	if err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}
	now := time.Now()
	s := &Session{
		ID: id, CreatedAt: now, vw: vw, vh: vh,
		mgr: m, ctx: ctx, cancel: cancel, encoder: enc, dataDir: dir,
		input:  make(chan inputEvent, 4096),
		status: StatusStarting, targetURL: targetURL, currentURL: targetURL,
		lastActivity: now, emptySince: now, participants: map[string]*Participant{},
		wsViewers: map[*Conn]struct{}{},
	}
	s.addSeat(RoleHost, seatName(hostName, "Host"))
	s.addSeat(RoleGuest, seatName(guestName, "Guest"))
	go s.inputWorker()
	go s.start()
	return s, nil
}

// ---------------------------------------------------------------- lifecycle

func (s *Session) start() {
	if err := s.launchBrowser(); err != nil {
		log.Printf("session %s: %v", s.ID, err)
		s.setStatus(StatusError)
		s.broadcast(newMsg(EvBrowserError, "message", "Could not start the browser"), "")
		s.Close("browser_failed")
		return
	}
	s.setStatus(StatusReady)
	s.navigate(s.targetURL, "")
}

func (s *Session) launchBrowser() error {
	b, err := LaunchBrowser(s.ctx, BrowserOptions{
		ChromePath:  s.mgr.cfg.ChromePath,
		NoSandbox:   s.mgr.cfg.ChromeNoSandbox,
		UserDataDir: s.dataDir,
		ProxyAddr:   s.mgr.proxy.Addr(),
		Width:       s.vw,
		Height:      s.vh,
		JPEGQuality: s.mgr.cfg.JPEGQuality,
		AllowURL:    s.allowURLFunc(),
	}, s)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.status == StatusClosed {
		s.mu.Unlock()
		b.Close()
		return fmt.Errorf("session closed during launch")
	}
	s.browser = b
	s.mu.Unlock()
	return nil
}

// Viewport is the shared browser's size in CSS px.
func (s *Session) Viewport() map[string]int { return map[string]int{"width": s.vw, "height": s.vh} }

func (s *Session) getBrowser() *Browser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.browser
}

func (s *Session) setStatus(st Status) {
	s.mu.Lock()
	if s.status != StatusClosed {
		s.status = st
	}
	s.mu.Unlock()
	s.broadcastState()
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Close terminates the browser, encoder and all connections and deletes the
// profile directory.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.status = StatusClosed
		b := s.browser
		s.browser = nil
		conns := make([]*Conn, 0, len(s.participants))
		for _, p := range s.participants {
			if p.conn != nil {
				conns = append(conns, p.conn)
			}
		}
		s.mu.Unlock()

		for _, c := range conns {
			c.sendCritical(newMsg(EvSessionClosed, "reason", reason))
			c.close()
		}
		s.cancel()
		if b != nil {
			b.Close()
		}
		s.encoder.Close()
		removeProfile(s.dataDir)
		s.mgr.remove(s.ID)
		log.Printf("session %s closed (%s)", s.ID, reason)
	})
}

// profilePrefix names each session's temporary browser profile directory.
const profilePrefix = "collab-session-"

// removeProfile deletes a session's browser profile (cookies, storage,
// cache). Chromium's helper processes can still be writing into it for a
// moment after the browser exits, which makes a single RemoveAll fail with
// "directory not empty", so retry briefly.
func removeProfile(dir string) {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(dir); err == nil {
			if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		log.Printf("could not remove browser profile %s: %v", dir, err)
	}
	// A helper process that hadn't exited yet can re-create the directory
	// just after it was removed. Check again shortly after; the manager's
	// sweep (sweepProfiles) is the backstop after that.
	for _, d := range []time.Duration{2 * time.Second, 10 * time.Second} {
		time.AfterFunc(d, func() { _ = os.RemoveAll(dir) })
	}
}

// ---------------------------------------------------------------- participants

// addSeat creates one of the session's two seats. Only called while the
// session is being built.
func (s *Session) addSeat(role, name string) {
	p := &Participant{
		ID: randomID(), Name: name, Role: role, Color: palette[s.colorNext%len(palette)],
		token: randomToken(), lastSeen: time.Now(),
	}
	s.colorNext++
	s.participants[p.ID] = p
}

func (s *Session) seat(role string) *Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.participants {
		if p.Role == role {
			return p
		}
	}
	return nil
}

func seatName(name, def string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return def
	}
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}
	return name
}

func (s *Session) authParticipant(id, token string) *Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.participants[id]
	if p == nil || !tokenEqual(p.token, token) {
		return nil
	}
	return p
}

func (s *Session) participantList() []ParticipantInfo {
	out := make([]ParticipantInfo, 0, len(s.participants))
	for _, p := range s.participants {
		out = append(out, p.info())
	}
	return out
}

func (s *Session) stateMsg(you string) ServerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursors := []map[string]any{}
	for _, p := range s.participants {
		if p.conn != nil && p.cursorActive {
			cursors = append(cursors, map[string]any{"participantId": p.ID, "x": p.cursorX, "y": p.cursorY})
		}
	}
	return newMsg(EvSessionState,
		"sessionId", s.ID,
		"status", s.status,
		"you", you,
		"url", s.currentURL,
		"title", s.title,
		"loading", s.loading,
		"viewport", s.Viewport(),
		"participants", s.participantList(),
		"cursors", cursors,
		"dialog", s.dialog,
		"select", s.openSelect,
		"fields", s.pageFields,
		"focus", s.pageFocus,
		"locks", map[string]string{"keyboard": s.lastLocks[0], "pointer": s.lastLocks[1]},
	)
}

func (s *Session) broadcastState() {
	s.mu.Lock()
	conns := s.connsLocked("")
	s.mu.Unlock()
	for _, c := range conns {
		c.send(s.stateMsg(c.p.ID))
	}
}

func (s *Session) connsLocked(except string) []*Conn {
	out := make([]*Conn, 0, len(s.participants))
	for id, p := range s.participants {
		if id != except && p.conn != nil {
			out = append(out, p.conn)
		}
	}
	return out
}

func (s *Session) broadcast(m ServerMessage, except string) {
	s.mu.Lock()
	conns := s.connsLocked(except)
	s.mu.Unlock()
	for _, c := range conns {
		c.send(m)
	}
}

// janitor is called periodically by the manager.
func (s *Session) janitor(now time.Time) {
	cfg := s.mgr.cfg
	s.mu.Lock()
	connected := 0
	for _, p := range s.participants {
		if p.conn != nil {
			connected++
		}
	}
	if connected > 0 {
		s.emptySince = time.Time{}
	} else if s.emptySince.IsZero() {
		s.emptySince = now
	}
	idle := !s.emptySince.IsZero() && now.Sub(s.emptySince) > cfg.IdleTimeout
	expired := now.Sub(s.CreatedAt) > cfg.MaxSessionLifetime
	s.mu.Unlock()

	switch {
	case expired:
		s.Close("max_lifetime")
	case idle:
		s.Close("idle")
	}
}

// ---------------------------------------------------------------- connections

// Conn is one WebSocket connection from a participant, plus its WebRTC peer.
type Conn struct {
	s    *Session
	p    *Participant
	ws   *websocket.Conn
	out  chan ServerMessage
	done chan struct{}
	once sync.Once

	pcMu sync.Mutex
	pc   *webrtc.PeerConnection

	// WebSocket video: the newest JPEG waiting to be written. A newer frame
	// replaces an unsent one, so a slow connection drops frames instead of
	// falling behind.
	frame      atomic.Pointer[[]byte]
	frameReady chan struct{}

	// rate limiting (reader goroutine only)
	bucket     float64
	bucketTime time.Time
}

func (c *Conn) send(m ServerMessage) {
	select {
	case c.out <- m:
	case <-c.done:
	default:
		// Slow consumer: drop non-critical updates (cursor moves etc.).
	}
}

func (c *Conn) sendCritical(m ServerMessage) {
	select {
	case c.out <- m:
	case <-c.done:
	case <-time.After(time.Second):
	}
}

// offerFrame hands the writer a new WebSocket video frame.
func (c *Conn) offerFrame(b []byte) {
	c.frame.Store(&b)
	select {
	case c.frameReady <- struct{}{}:
	default:
	}
}

func (c *Conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.s.setWSVideo(c, false)
		c.pcMu.Lock()
		if c.pc != nil {
			_ = c.pc.Close()
			c.pc = nil
		}
		c.pcMu.Unlock()
		_ = c.ws.Close()
	})
}

func (c *Conn) writer() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	// WebSocket video is sent as binary messages (one JPEG each), at most at
	// the encoder's frame rate. Chromium only emits frames when the page
	// repaints, so an idle page costs nothing.
	minGap := time.Second / time.Duration(c.s.mgr.cfg.FPS)
	var lastFrame time.Time
	var frameTimer <-chan time.Time // set while waiting out minGap
	sendFrame := func() bool {
		p := c.frame.Swap(nil)
		if p == nil {
			return true // video was turned off, or already sent
		}
		lastFrame = time.Now()
		_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return c.ws.WriteMessage(websocket.BinaryMessage, *p) == nil
	}

	for {
		select {
		case <-c.done:
			// Flush anything queued (e.g. session_closed) then stop.
			for {
				select {
				case m := <-c.out:
					_ = c.ws.SetWriteDeadline(time.Now().Add(time.Second))
					_ = c.ws.WriteJSON(m)
				default:
					return
				}
			}
		case m := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.ws.WriteJSON(m); err != nil {
				c.close()
				return
			}
		case <-c.frameReady:
			if frameTimer != nil {
				continue // the pending timer sends whatever frame is newest then
			}
			if wait := time.Until(lastFrame.Add(minGap)); wait > 0 {
				frameTimer = time.After(wait)
				continue
			}
			if !sendFrame() {
				c.close()
				return
			}
		case <-frameTimer:
			frameTimer = nil
			if !sendFrame() {
				c.close()
				return
			}
		case <-ping.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *Conn) allow() bool {
	now := time.Now()
	rate := float64(c.s.mgr.cfg.MaxEventsPerSecond)
	if c.bucketTime.IsZero() {
		c.bucket, c.bucketTime = rate, now
	}
	c.bucket = math.Min(rate, c.bucket+now.Sub(c.bucketTime).Seconds()*rate)
	c.bucketTime = now
	if c.bucket < 1 {
		return false
	}
	c.bucket--
	return true
}

// Attach runs the connection until it closes. A participant reconnecting
// replaces their previous connection; the shared browser is untouched.
func (s *Session) Attach(p *Participant, ws *websocket.Conn) {
	c := &Conn{s: s, p: p, ws: ws, out: make(chan ServerMessage, 512), done: make(chan struct{}), frameReady: make(chan struct{}, 1)}

	s.mu.Lock()
	if s.status == StatusClosed {
		s.mu.Unlock()
		_ = ws.WriteJSON(newMsg(EvSessionClosed, "reason", "closed"))
		ws.Close()
		return
	}
	old := p.conn
	p.conn = c
	p.lastSeen = time.Now()
	s.lastActivity = time.Now()
	s.emptySince = time.Time{}
	s.mu.Unlock()
	if old != nil {
		old.close()
	}

	go c.writer()
	cfg := s.mgr.cfg
	c.send(newMsg(EvHello, "you", p.ID, "role", p.Role, "sessionId", s.ID,
		"iceServers", cfg.ICEServers, "viewport", s.Viewport()))
	c.send(s.stateMsg(p.ID))
	s.broadcast(newMsg(EvParticipantJoined, "participant", p.info()), p.ID)

	ws.SetReadLimit(256 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	for {
		var msg ClientMessage
		if err := ws.ReadJSON(&msg); err != nil {
			break
		}
		_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		if msg.Type == "participant_leave" {
			break
		}
		if !c.allow() {
			continue
		}
		s.handleClientMessage(c, msg)
	}
	c.close()

	s.mu.Lock()
	still := p.conn == c
	if still {
		p.conn = nil
		p.lastSeen = time.Now()
		p.cursorActive = false
	}
	s.mu.Unlock()
	if still {
		s.broadcast(newMsg(EvParticipantLeft, "participantId", p.ID), p.ID)
		// Release any input locks this participant held.
		s.enqueue(inputEvent{p: p, msg: ClientMessage{Type: "_release"}, at: time.Now()})
	}
}

func (s *Session) enqueue(ev inputEvent) {
	select {
	case s.input <- ev:
	default:
		if ev.msg.Type != "cursor_move" {
			log.Printf("session %s: input queue full, dropping %s", s.ID, ev.msg.Type)
		}
	}
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func (s *Session) handleClientMessage(c *Conn, m ClientMessage) {
	p := c.p
	now := time.Now()
	s.mu.Lock()
	s.lastActivity = now
	p.lastSeen = now
	s.mu.Unlock()

	switch m.Type {
	case "ping":
		c.send(newMsg(EvPong))

	case "viewport_ready":
		c.send(s.stateMsg(p.ID))

	case "webrtc_offer":
		s.handleOffer(c, m.SDP)

	case "webrtc_ice":
		s.handleRemoteICE(c, m.Candidate)

	case "video_ws_start":
		s.setWSVideo(c, true)
	case "video_ws_stop":
		s.setWSVideo(c, false)

	case "end_session":
		if p.Role == RoleHost {
			go s.Close("ended_by_host")
		} else {
			c.send(newMsg(EvBrowserError, "message", "Only the host can end the session.", "kind", "forbidden"))
		}

	case "cursor_move":
		m.X, m.Y = clamp01(m.X), clamp01(m.Y)
		visible := m.Visible == nil || *m.Visible
		s.mu.Lock()
		p.cursorX, p.cursorY, p.cursorActive = m.X, m.Y, visible
		s.mu.Unlock()
		s.broadcast(newMsg(EvCursorUpdate, "participantId", p.ID, "x", m.X, "y", m.Y, "visible", visible), p.ID)
		if visible {
			s.enqueue(inputEvent{p: p, msg: m, at: now})
		}

	case "mouse_down", "mouse_up", "mouse_click", "scroll", "key_down", "key_up", "text_input":
		m.X, m.Y = clamp01(m.X), clamp01(m.Y)
		s.enqueue(inputEvent{p: p, msg: m, at: now})

	case "navigate":
		go s.navigate(m.URL, p.ID)
	case "back", "forward", "reload":
		go s.history(m.Type, p.ID)

	case "select_choose":
		if m.Index != nil {
			go s.chooseSelect(m.ID, *m.Index, p.ID)
		}
	case "select_cancel":
		s.mu.Lock()
		s.openSelect = nil
		s.mu.Unlock()
		s.broadcast(newMsg(EvSelectClose, "id", m.ID, "participantId", p.ID), "")

	case "dialog_response":
		if b := s.getBrowser(); b != nil {
			go func() {
				if err := b.HandleDialog(m.Accept, m.Text); err != nil {
					log.Printf("session %s: dialog: %v", s.ID, err)
				}
			}()
		}
	}
}

// ---------------------------------------------------------------- WebRTC

// setWSVideo turns WebSocket video on or off for one connection. Clients
// use it while WebRTC is starting, when WebRTC can't connect (UDP blocked,
// proxy-only networks), or when the user picks it.
func (s *Session) setWSVideo(c *Conn, on bool) {
	s.wsMu.Lock()
	if on {
		select {
		case <-c.done:
			s.wsMu.Unlock()
			return
		default:
		}
		s.wsViewers[c] = struct{}{}
	} else {
		delete(s.wsViewers, c)
	}
	s.wsMu.Unlock()
	if !on {
		c.frame.Store(nil)
		return
	}
	// Send the current picture right away instead of waiting for a repaint.
	if b := s.encoder.LatestJPEG(); b != nil {
		c.offerFrame(b)
	}
}

func (s *Session) handleOffer(c *Conn, sdp string) {
	// With WEBRTC_PUBLIC_IPS set the server already advertises its public
	// address, so gathering STUN/TURN candidates here would only hold a
	// second UDP port per viewer. Clients still get ICE_SERVERS.
	pcCfg := webrtc.Configuration{}
	if len(s.mgr.cfg.PublicIPs) == 0 {
		pcCfg.ICEServers = s.mgr.pionICEServers()
	}
	pc, err := s.mgr.webrtcAPI.NewPeerConnection(pcCfg)
	if err != nil {
		log.Printf("session %s: new peer: %v", s.ID, err)
		return
	}
	c.pcMu.Lock()
	if c.pc != nil {
		_ = c.pc.Close()
	}
	c.pc = pc
	c.pcMu.Unlock()

	sender, err := pc.AddTrack(s.encoder.Track)
	if err != nil {
		log.Printf("session %s: add track: %v", s.ID, err)
		return
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		c.send(newMsg(EvWebRTCICE, "candidate", cand.ToJSON()))
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed {
			c.send(newMsg(EvWebRTCFailed))
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		log.Printf("session %s: remote description: %v", s.ID, err)
		c.send(newMsg(EvWebRTCFailed, "message", "bad offer"))
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.send(newMsg(EvWebRTCFailed, "message", err.Error()))
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		c.send(newMsg(EvWebRTCFailed, "message", err.Error()))
		return
	}
	c.send(newMsg(EvWebRTCAnswer, "sdp", pc.LocalDescription().SDP))
}

func (s *Session) handleRemoteICE(c *Conn, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var cand webrtc.ICECandidateInit
	if err := json.Unmarshal(raw, &cand); err != nil || cand.Candidate == "" {
		return
	}
	c.pcMu.Lock()
	pc := c.pc
	c.pcMu.Unlock()
	if pc != nil {
		_ = pc.AddICECandidate(cand)
	}
}

// ---------------------------------------------------------------- navigation

func (s *Session) navigate(raw, by string) {
	u, err := s.mgr.guard.ValidateURL(s.ctx, raw)
	if err != nil {
		s.broadcast(newMsg(EvBrowserError, "message", "Navigation blocked: "+err.Error(), "url", raw, "participantId", by), "")
		return
	}
	if !s.mgr.allow.Allows(u.String()) {
		s.broadcast(newMsg(EvBrowserError, "message", "That page isn't allowed here.", "kind", "navigation_blocked", "url", u.String(), "participantId", by), "")
		return
	}
	b := s.getBrowser()
	if b == nil {
		return
	}
	s.navMu.Lock()
	defer s.navMu.Unlock()
	s.mu.Lock()
	s.targetURL = u.String()
	s.mu.Unlock()
	if err := b.Navigate(u.String()); err != nil {
		s.broadcast(newMsg(EvBrowserError, "message", "Navigation failed: "+err.Error(), "url", u.String(), "participantId", by), "")
	}
}

func (s *Session) history(kind, by string) {
	b := s.getBrowser()
	if b == nil {
		return
	}
	s.navMu.Lock()
	defer s.navMu.Unlock()
	var err error
	switch kind {
	case "back":
		err = b.Back()
	case "forward":
		err = b.Forward()
	case "reload":
		err = b.Reload()
	}
	if err != nil {
		s.broadcast(newMsg(EvBrowserError, "message", kind+" failed: "+err.Error(), "participantId", by), "")
	}
}

func (s *Session) chooseSelect(id string, index int, by string) {
	if b := s.getBrowser(); b != nil {
		if err := b.ChooseSelectOption(id, index); err != nil {
			log.Printf("session %s: select: %v", s.ID, err)
		}
	}
	s.mu.Lock()
	s.openSelect = nil
	s.mu.Unlock()
	s.broadcast(newMsg(EvSelectClose, "id", id, "participantId", by, "index", index), "")
}

// ---------------------------------------------------------------- BrowserHooks

func (s *Session) OnFrame(jpeg []byte) {
	s.encoder.PushJPEG(jpeg)
	s.wsMu.Lock()
	for c := range s.wsViewers {
		c.offerFrame(jpeg)
	}
	s.wsMu.Unlock()
}

func (s *Session) OnNavigated(url string) {
	s.mu.Lock()
	changed := s.currentURL != url
	s.currentURL = url
	hadSelect := s.openSelect != nil
	s.openSelect = nil
	s.mu.Unlock()
	if changed {
		s.broadcast(newMsg(EvBrowserNavigation, "url", url), "")
	}
	if hadSelect {
		s.broadcast(newMsg(EvSelectClose), "")
	}
}

func (s *Session) OnLoading() {
	s.mu.Lock()
	s.loading = true
	s.mu.Unlock()
	s.broadcast(newMsg(EvBrowserLoading), "")
}

func (s *Session) OnLoaded(url, title string) {
	s.mu.Lock()
	s.loading = false
	if url != "" {
		s.currentURL = url
	}
	if title != "" {
		s.title = title
	}
	cur, t := s.currentURL, s.title
	s.mu.Unlock()
	s.broadcast(newMsg(EvBrowserLoaded, "url", cur, "title", t), "")
}

func (s *Session) OnTitle(title string) {
	s.mu.Lock()
	changed := s.title != title
	s.title = title
	cur := s.currentURL
	loading := s.loading
	s.mu.Unlock()
	if changed && !loading {
		s.broadcast(newMsg(EvBrowserLoaded, "url", cur, "title", title), "")
	}
}

func (s *Session) OnDialogOpen(kind, message, def string) {
	d := &dialogState{Kind: kind, Message: truncate(message, 2000), DefaultPrompt: truncate(def, 2000)}
	s.mu.Lock()
	s.dialog = d
	s.mu.Unlock()
	s.broadcast(newMsg(EvDialogOpen, "dialog", d), "")
}

func (s *Session) OnDialogClosed() {
	s.mu.Lock()
	s.dialog = nil
	s.mu.Unlock()
	s.broadcast(newMsg(EvDialogClose), "")
}

func (s *Session) OnSelectOpen(payload json.RawMessage) {
	if !json.Valid(payload) {
		return
	}
	s.mu.Lock()
	s.openSelect = payload
	by := s.lastPointerBy
	s.mu.Unlock()
	s.broadcast(newMsg(EvSelectOpen, "select", payload, "participantId", by), "")
}

func (s *Session) allowURLFunc() func(string) bool {
	if s.mgr.allow.Empty() {
		return nil
	}
	return s.mgr.allow.Allows
}

func (s *Session) OnNavigationBlocked(url string) {
	s.broadcast(newMsg(EvBrowserError, "message", "That page isn't allowed here.", "kind", "navigation_blocked", "url", truncate(url, 2000)), "")
}

// OnPageEvent stores and forwards the injected script's text-field
// reports: {"kind":"focus",...} or {"kind":"fields",...}.
func (s *Session) OnPageEvent(payload json.RawMessage) {
	var head struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(payload, &head) != nil {
		return
	}
	s.mu.Lock()
	switch head.Kind {
	case "fields":
		s.pageFields = payload
	case "focus":
		s.pageFocus = payload
	default:
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if head.Kind == "fields" {
		s.broadcast(newMsg(EvFieldsUpdate, "fields", payload), "")
	} else {
		s.broadcast(newMsg(EvFocusUpdate, "focus", payload), "")
	}
}

func (s *Session) OnFileChooser() {
	s.broadcast(newMsg(EvBrowserError, "message", "File uploads are not supported in shared sessions yet."), "")
}

func (s *Session) OnRendererCrashed() {
	s.broadcast(newMsg(EvBrowserError, "message", "The page crashed; reloading."), "")
	go s.history("reload", "")
}

func (s *Session) OnBrowserLost() {
	s.mu.Lock()
	if s.status == StatusClosed {
		s.mu.Unlock()
		return
	}
	s.restarts++
	n := s.restarts
	old := s.browser
	s.browser = nil
	url := s.currentURL
	s.mu.Unlock()
	if old != nil {
		go old.Close()
	}
	if n > s.mgr.cfg.MaxBrowserRestarts {
		s.broadcast(newMsg(EvBrowserError, "message", "The browser crashed too many times."), "")
		s.Close("browser_crashed")
		return
	}
	log.Printf("session %s: browser lost, restarting (%d)", s.ID, n)
	s.setStatus(StatusRestarting)
	s.broadcast(newMsg(EvBrowserError, "message", "The browser stopped unexpectedly; restarting."), "")
	go func() {
		// The profile directory is reused, so cookies and storage survive.
		if err := s.launchBrowser(); err != nil {
			log.Printf("session %s: restart failed: %v", s.ID, err)
			s.Close("browser_crashed")
			return
		}
		s.setStatus(StatusReady)
		s.navigate(url, "")
	}()
}

// ResolvePopup opens a popup's URL in the shared tab, subject to the same
// checks as any navigation. Clients learn the new URL from OnNavigated
// once it actually loads.
func (s *Session) ResolvePopup(url string) {
	go s.navigate(url, "")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
