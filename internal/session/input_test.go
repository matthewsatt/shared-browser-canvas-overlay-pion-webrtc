package session

import "testing"

func TestCoalesceMoves(t *testing.T) {
	a, b := &Participant{ID: "a"}, &Participant{ID: "b"}
	mv := func(p *Participant, x float64) inputEvent {
		return inputEvent{p: p, msg: ClientMessage{Type: "cursor_move", X: x}}
	}
	ev := func(p *Participant, typ string) inputEvent {
		return inputEvent{p: p, msg: ClientMessage{Type: typ}}
	}
	in := []inputEvent{
		mv(a, 0.1), mv(a, 0.2), mv(b, 0.5), mv(a, 0.3), // a's first two collapse into 0.3
		ev(a, "mouse_down"),
		mv(a, 0.4), mv(a, 0.5), // collapse into 0.5, after mouse_down
		ev(a, "mouse_up"), mv(b, 0.6),
	}
	out := coalesceMoves(in)
	want := []struct {
		id  string
		typ string
		x   float64
	}{
		// b's 0.5 is superseded by its later 0.6 (no other b event in between).
		{"a", "cursor_move", 0.3}, {"a", "mouse_down", 0},
		{"a", "cursor_move", 0.5}, {"a", "mouse_up", 0}, {"b", "cursor_move", 0.6},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i].p.ID != w.id || out[i].msg.Type != w.typ || out[i].msg.X != w.x {
			t.Errorf("event %d = %s/%s/%v, want %s/%s/%v", i, out[i].p.ID, out[i].msg.Type, out[i].msg.X, w.id, w.typ, w.x)
		}
	}
}

func TestViewportPoint(t *testing.T) {
	const w, h = 1280, 720
	cases := []struct{ nx, ny, x, y float64 }{
		{0, 0, 0, 0},
		{0.5, 0.5, 640, 360},                   // centre of the picture is the centre of the page
		{100.5 / w, 600.5 / h, 100.5, 600.5},   // a pixel centre maps to that pixel centre
		{1239.5 / w, 679.5 / h, 1239.5, 679.5}, // ...including near the far edges
		{1, 1, w - 0.01, h - 0.01},             // the far edge stays inside the page
		{-0.2, 1.7, 0, h - 0.01},               // out-of-range input is clamped
	}
	for _, c := range cases {
		x, y := viewportPoint(c.nx, c.ny, w, h)
		if x != c.x || y != c.y {
			t.Errorf("viewportPoint(%v, %v) = (%v, %v), want (%v, %v)", c.nx, c.ny, x, y, c.x, c.y)
		}
	}
}

func TestTokenEqual(t *testing.T) {
	if tokenEqual("", "") {
		t.Error("empty tokens must never match")
	}
	if !tokenEqual("abc", "abc") || tokenEqual("abc", "abd") || tokenEqual("abc", "ab") {
		t.Error("tokenEqual mismatch")
	}
}
