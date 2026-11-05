package session

import (
	"log"
	"math"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Input arbitration
//
// A browser has one pointer, one hover state and one focused element, so
// truly simultaneous input cannot work. All input goes through a single
// ordered queue per session (this worker), tagged with the participant, and
// two short-lived locks keep overlapping operations from corrupting each
// other:
//
//   - Keyboard lease: whoever types owns the keyboard until they have been
//     idle for KeyboardLease (default 1.5s). While held, other participants'
//     keystrokes and clicks are rejected, so nobody can move focus out from
//     under a typist.
//   - Pointer lock: held from mouse_down to mouse_up (drags, text selection).
//     Others' clicks/scrolls are rejected and their mouse moves are not
//     forwarded to the browser (their cursors still show in the overlay).
//
// Nobody has to request control; locks are automatic and brief.

const maxBatch = 256

func (s *Session) inputWorker() {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	batch := make([]inputEvent, 0, maxBatch)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.expireLocks(time.Now())
		case ev := <-s.input:
			batch = append(batch[:0], ev)
		drain:
			for len(batch) < maxBatch {
				select {
				case ev := <-s.input:
					batch = append(batch, ev)
				default:
					break drain
				}
			}
			for _, ev := range coalesceMoves(batch) {
				s.processInput(ev)
			}
			s.expireLocks(time.Now())
		}
	}
}

// coalesceMoves drops a cursor_move when a later event from the same
// participant in the batch supersedes it (another move). Order between
// different event kinds is preserved.
func coalesceMoves(batch []inputEvent) []inputEvent {
	if len(batch) < 2 {
		return batch
	}
	out := make([]inputEvent, 0, len(batch))
	lastMove := map[*Participant]int{}
	for i := len(batch) - 1; i >= 0; i-- {
		ev := batch[i]
		if ev.msg.Type == "cursor_move" {
			if _, seen := lastMove[ev.p]; seen {
				continue
			}
			lastMove[ev.p] = i
		} else {
			// A non-move resets: earlier moves must still be replayed
			// before this event so hover/drag paths stay correct.
			delete(lastMove, ev.p)
		}
		out = append(out, ev)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (s *Session) reject(p *Participant, reason, holder string) {
	s.mu.Lock()
	c := p.conn
	s.mu.Unlock()
	if c != nil {
		c.send(newMsg(EvInteractionRejected, "reason", reason, "lockedBy", holder))
	}
}

func (s *Session) kbLockedFor(p *Participant, now time.Time) (bool, string) {
	if s.kbOwner != "" && s.kbOwner != p.ID && now.Before(s.kbUntil) {
		return true, s.kbOwner
	}
	return false, ""
}

func (s *Session) ptrLockedFor(p *Participant, now time.Time) (bool, string) {
	if s.ptrOwner != "" && s.ptrOwner != p.ID && now.Sub(s.ptrSince) < s.mgr.cfg.PointerLockTimeout {
		return true, s.ptrOwner
	}
	return false, ""
}

func (s *Session) expireLocks(now time.Time) {
	if s.kbOwner != "" && !now.Before(s.kbUntil) {
		s.kbOwner = ""
	}
	if s.ptrOwner != "" && now.Sub(s.ptrSince) >= s.mgr.cfg.PointerLockTimeout {
		s.ptrOwner = ""
	}
	cur := [2]string{s.kbOwner, s.ptrOwner}
	s.mu.Lock()
	changed := cur != s.lastLocks
	s.lastLocks = cur
	s.mu.Unlock()
	if changed {
		s.broadcast(newMsg(EvLockUpdate, "keyboard", cur[0], "pointer", cur[1]), "")
	}
}

func buttonMask(b string) int64 {
	switch b {
	case "left":
		return 1
	case "right":
		return 2
	case "middle":
		return 4
	}
	return 0
}

func cdpButton(b string) input.MouseButton {
	switch b {
	case "right":
		return input.Right
	case "middle":
		return input.Middle
	case "left":
		return input.Left
	}
	return input.None
}

func (s *Session) toViewport(nx, ny float64) (float64, float64) {
	return viewportPoint(nx, ny, float64(s.vw), float64(s.vh))
}

// viewportPoint maps normalized coordinates to viewport CSS px. The client
// normalizes over the whole displayed picture, so [0,1] spans [0,w]: 0.5 is
// the centre line and pixel p's centre is (p+0.5)/w. The result is kept just
// inside the viewport so a point on the far edge still hits the page.
func viewportPoint(nx, ny, w, h float64) (float64, float64) {
	x := math.Min(math.Round(clamp01(nx)*w*100)/100, w-0.01)
	y := math.Min(math.Round(clamp01(ny)*h*100)/100, h-0.01)
	return x, y
}

func (s *Session) processInput(ev inputEvent) {
	p, m, now := ev.p, ev.msg, time.Now()

	if m.Type == "_release" {
		if s.kbOwner == p.ID {
			s.kbOwner = ""
		}
		if s.ptrOwner == p.ID {
			s.ptrOwner = ""
			s.releaseButtons(p)
		}
		return
	}

	b := s.getBrowser()
	if b == nil {
		return
	}
	mods := input.Modifier(m.Modifiers & 0xf)
	x, y := s.toViewport(m.X, m.Y)

	switch m.Type {
	case "cursor_move":
		if locked, _ := s.ptrLockedFor(p, now); locked {
			return // show their cursor, but don't disturb the drag
		}
		a := input.DispatchMouseEvent(input.MouseMoved, x, y).WithModifiers(mods)
		if p.buttons != 0 {
			a = a.WithButtons(p.buttons).WithButton(cdpButton(p.lastButton))
		}
		s.do(b, a)

	case "mouse_down", "mouse_click":
		if locked, by := s.ptrLockedFor(p, now); locked {
			s.reject(p, "pointer_locked", by)
			return
		}
		if locked, by := s.kbLockedFor(p, now); locked {
			s.reject(p, "keyboard_locked", by)
			return
		}
		button := m.Button
		if button == "" {
			button = "left"
		}
		// Double/triple click detection, per participant.
		if button == p.lastButton && now.Sub(p.lastPress) < 500*time.Millisecond &&
			math.Abs(m.X-p.lastPressX) < 0.01 && math.Abs(m.Y-p.lastPressY) < 0.01 {
			p.clickCount++
		} else {
			p.clickCount = 1
		}
		p.lastPress, p.lastPressX, p.lastPressY, p.lastButton = now, m.X, m.Y, button
		p.buttons |= buttonMask(button)
		s.ptrOwner, s.ptrSince = p.ID, now
		s.mu.Lock()
		s.lastPointerBy = p.ID
		s.mu.Unlock()

		s.do(b, input.DispatchMouseEvent(input.MouseMoved, x, y).WithModifiers(mods))
		s.do(b, input.DispatchMouseEvent(input.MousePressed, x, y).
			WithButton(cdpButton(button)).WithButtons(p.buttons).
			WithClickCount(p.clickCount).WithModifiers(mods))
		s.broadcast(newMsg(EvInteractionUpdate, "kind", "mouse_down", "participantId", p.ID,
			"x", m.X, "y", m.Y, "button", button), "")
		if m.Type == "mouse_click" {
			s.mouseUp(b, p, button, x, y, mods)
		}

	case "mouse_up":
		if s.ptrOwner != "" && s.ptrOwner != p.ID {
			return // their press was rejected; ignore the release
		}
		button := m.Button
		if button == "" {
			button = "left"
		}
		if p.buttons&buttonMask(button) == 0 {
			return
		}
		s.mouseUp(b, p, button, x, y, mods)

	case "scroll":
		if locked, by := s.ptrLockedFor(p, now); locked {
			s.reject(p, "pointer_locked", by)
			return
		}
		dx := math.Max(-5000, math.Min(5000, m.DX))
		dy := math.Max(-5000, math.Min(5000, m.DY))
		s.do(b, input.DispatchMouseEvent(input.MouseWheel, x, y).
			WithDeltaX(dx).WithDeltaY(dy).WithModifiers(mods))

	case "key_down", "key_up", "text_input":
		if m.Type != "key_up" {
			if locked, by := s.kbLockedFor(p, now); locked {
				s.reject(p, "keyboard_locked", by)
				return
			}
			s.kbOwner, s.kbUntil = p.ID, now.Add(s.mgr.cfg.KeyboardLease)
		} else if s.kbOwner == p.ID {
			s.kbUntil = now.Add(s.mgr.cfg.KeyboardLease)
		}
		s.dispatchKeyboard(b, m, mods)
	}
}

func (s *Session) mouseUp(b *Browser, p *Participant, button string, x, y float64, mods input.Modifier) {
	p.buttons &^= buttonMask(button)
	s.do(b, input.DispatchMouseEvent(input.MouseReleased, x, y).
		WithButton(cdpButton(button)).WithButtons(p.buttons).
		WithClickCount(p.clickCount).WithModifiers(mods))
	if p.buttons == 0 && s.ptrOwner == p.ID {
		s.ptrOwner = ""
	}
	s.broadcast(newMsg(EvInteractionUpdate, "kind", "mouse_up", "participantId", p.ID,
		"button", button), "")
}

// releaseButtons lifts any buttons a participant left pressed (e.g. they
// disconnected mid-drag).
func (s *Session) releaseButtons(p *Participant) {
	b := s.getBrowser()
	if b == nil || p.buttons == 0 {
		p.buttons = 0
		return
	}
	for _, name := range []string{"left", "right", "middle"} {
		if p.buttons&buttonMask(name) != 0 {
			p.buttons &^= buttonMask(name)
			s.do(b, input.DispatchMouseEvent(input.MouseReleased, 0, 0).WithButton(cdpButton(name)).WithButtons(p.buttons))
		}
	}
}

func (s *Session) dispatchKeyboard(b *Browser, m ClientMessage, mods input.Modifier) {
	switch m.Type {
	case "text_input":
		if m.Text == "" || len(m.Text) > 10000 {
			return
		}
		s.do(b, input.InsertText(m.Text))
		return
	}
	if m.Key == "" || len(m.Key) > 32 || len(m.Code) > 32 {
		return
	}
	text := ""
	if m.Type == "key_down" && mods&(input.ModifierCtrl|input.ModifierMeta) == 0 {
		switch {
		case m.Key == "Enter":
			text = "\r"
		case len([]rune(m.Key)) == 1:
			text = m.Key
		}
	}
	typ := input.KeyUp
	if m.Type == "key_down" {
		typ = input.KeyRawDown
		if text != "" {
			typ = input.KeyDown
		}
	}
	kc := m.KeyCode
	if kc < 0 || kc > 255 {
		kc = 0
	}
	a := input.DispatchKeyEvent(typ).
		WithKey(m.Key).WithCode(m.Code).
		WithWindowsVirtualKeyCode(kc).WithNativeVirtualKeyCode(kc).
		WithModifiers(mods).WithAutoRepeat(m.Repeat)
	if text != "" {
		a = a.WithText(text).WithUnmodifiedText(text)
	}
	s.do(b, a)
}

func (s *Session) do(b *Browser, a chromedp.Action) {
	if err := b.Do(a); err != nil && s.ctx.Err() == nil {
		log.Printf("session %s: input dispatch: %v", s.ID, err)
	}
}
