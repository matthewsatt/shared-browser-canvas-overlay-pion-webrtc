package session

import "math"

// maxViewportPixels bounds a session's shared viewport (about 1920×1080 or
// 1080×1920), which keeps encoding cost and bandwidth reasonable.
const maxViewportPixels = 1920 * 1080

// fitViewport turns a requested viewport into one a session can use: the
// server default when unset, each side clamped to 320–3840, scaled down
// (keeping its shape) to at most maxViewportPixels, and even on both sides
// for H.264.
func fitViewport(w, h, defW, defH int) (int, int) {
	if w <= 0 || h <= 0 {
		return defW, defH
	}
	fw := float64(min(max(w, 320), 3840))
	fh := float64(min(max(h, 320), 3840))
	if px := fw * fh; px > maxViewportPixels {
		k := math.Sqrt(maxViewportPixels / px)
		fw, fh = fw*k, fh*k
	}
	return int(fw) &^ 1, int(fh) &^ 1
}

// scaledBitrate grows the configured bitrate (meant for 1280×720) with the
// picture's area, within half to three times the configured value.
func scaledBitrate(base, w, h int) int {
	b := int(float64(base) * float64(w*h) / (1280 * 720))
	return min(max(b, base/2), base*3)
}
