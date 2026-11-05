package session

import "testing"

func TestFitViewport(t *testing.T) {
	cases := []struct{ w, h, wantW, wantH int }{
		{0, 0, 1280, 720},        // unset: server default
		{1280, 756, 1280, 756},   // a landscape kiosk, as is
		{1080, 1920, 1080, 1920}, // a portrait kiosk, as is
		{1281, 757, 1280, 756},   // even sides for H.264
		{100, 100, 320, 320},     // too small
		{2560, 1440, 1920, 1080}, // too many pixels: scaled down, same shape
		{3840, 2160, 1920, 1080},
	}
	for _, c := range cases {
		w, h := fitViewport(c.w, c.h, 1280, 720)
		if w != c.wantW || h != c.wantH {
			t.Errorf("fitViewport(%d, %d) = %dx%d, want %dx%d", c.w, c.h, w, h, c.wantW, c.wantH)
		}
		if w*h > maxViewportPixels || w%2 != 0 || h%2 != 0 {
			t.Errorf("fitViewport(%d, %d) = %dx%d breaks the limits", c.w, c.h, w, h)
		}
	}
	if b := scaledBitrate(2500, 1920, 1080); b != 5625 {
		t.Errorf("scaledBitrate 1080p = %d, want 5625", b)
	}
	if b := scaledBitrate(2500, 320, 320); b != 1250 {
		t.Errorf("scaledBitrate tiny = %d, want the 1250 floor", b)
	}
}
