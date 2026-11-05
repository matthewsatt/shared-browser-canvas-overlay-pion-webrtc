package session

import (
	"encoding/json"
	"time"
)

// ClientMessage is any message a participant sends over the WebSocket.
// Coordinates (X, Y) are normalized to [0,1] relative to the shared viewport.
type ClientMessage struct {
	Type string `json:"type"`

	// Pointer.
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Button    string  `json:"button,omitempty"` // left | middle | right
	DX        float64 `json:"dx,omitempty"`     // scroll, in viewport CSS pixels
	DY        float64 `json:"dy,omitempty"`
	Modifiers int64   `json:"modifiers,omitempty"` // Alt=1 Ctrl=2 Meta=4 Shift=8
	Visible   *bool   `json:"visible,omitempty"`

	// Keyboard.
	Key     string `json:"key,omitempty"`
	Code    string `json:"code,omitempty"`
	KeyCode int64  `json:"keyCode,omitempty"`
	Text    string `json:"text,omitempty"`
	Repeat  bool   `json:"repeat,omitempty"`

	// Navigation.
	URL string `json:"url,omitempty"`

	// WebRTC signaling.
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`

	// Select / dialog responses.
	ID     string `json:"id,omitempty"`
	Index  *int   `json:"index,omitempty"`
	Accept bool   `json:"accept,omitempty"`

	// Client viewport report (informational).
	Width  float64 `json:"width,omitempty"`
	Height float64 `json:"height,omitempty"`
}

// Participant roles. Every session has one of each.
const (
	RoleHost  = "host"  // starts the session; can end it
	RoleGuest = "guest" // e.g. the kiosk
)

// Event types sent from server to clients.
const (
	EvHello               = "hello" // first message: who you are, ICE servers, viewport
	EvSessionState        = "session_state"
	EvFieldsUpdate        = "fields_update" // positions of visible text fields
	EvFocusUpdate         = "focus_update"  // whether a text field has focus
	EvParticipantJoined   = "participant_joined"
	EvParticipantLeft     = "participant_left"
	EvCursorUpdate        = "cursor_update"
	EvBrowserNavigation   = "browser_navigation"
	EvBrowserLoading      = "browser_loading"
	EvBrowserLoaded       = "browser_loaded"
	EvBrowserError        = "browser_error"
	EvInteractionUpdate   = "interaction_update"
	EvInteractionRejected = "interaction_rejected"
	EvLockUpdate          = "lock_update"
	EvWebRTCAnswer        = "webrtc_answer"
	EvWebRTCICE           = "webrtc_ice"
	EvWebRTCFailed        = "webrtc_failed"
	EvSelectOpen          = "select_open"
	EvSelectClose         = "select_close"
	EvDialogOpen          = "dialog_open"
	EvDialogClose         = "dialog_close"
	EvSessionClosed       = "session_closed"
	EvPong                = "pong"
)

// ServerMessage is a loosely typed envelope; Data fields are merged at the
// top level when encoded.
type ServerMessage map[string]any

func newMsg(typ string, kv ...any) ServerMessage {
	m := ServerMessage{"type": typ, "ts": time.Now().UnixMilli()}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// ParticipantInfo is the public view of a participant.
type ParticipantInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Color     string `json:"color"`
	Connected bool   `json:"connected"`
}
