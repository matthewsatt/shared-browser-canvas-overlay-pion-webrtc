// Package media turns CDP screencast JPEG frames into a single H.264 WebRTC
// track that is shared by every participant of a session.
//
// Pipeline:
//
//	Page.screencastFrame (JPEG, change-driven)
//	  -> latest-frame slot
//	  -> fixed-rate ticker writes the latest JPEG to ffmpeg stdin
//	     (repeating the last frame while the page is idle, so the
//	     stream never stalls and new viewers always get pictures)
//	  -> ffmpeg/libx264 (ultrafast, zerolatency, baseline, fixed GOP)
//	  -> RTP (H.264 packetization-mode 1) over loopback UDP
//	  -> TrackLocalStaticRTP, which forwards packets to every peer
//
// Letting ffmpeg packetize avoids waiting for the next frame to find where
// the current one ends, which would add a full frame of latency.
//
// The track is encoded once and attached to every peer connection, so CPU
// cost is per session, not per viewer.
package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type EncoderConfig struct {
	FFmpegPath       string
	Width, Height    int
	FPS              int
	BitrateKbps      int
	KeyframeInterval time.Duration
}

type Encoder struct {
	cfg   EncoderConfig
	Track *webrtc.TrackLocalStaticRTP
	// ssrc is random per encoder; forwardRTP drops any packet without it,
	// so this session's track can only ever carry this session's ffmpeg
	// output, even if a stray packet reached its socket.
	ssrc uint32

	latest   atomic.Pointer[[]byte]
	frames   atomic.Uint64 // screencast frames received
	samples  atomic.Uint64 // encoded frames forwarded (RTP marker bits)
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// h264Level picks the lowest H.264 level whose frame-size limit fits the
// picture (at up to 30 fps), and the matching Constrained Baseline
// profile-level-id for SDP.
func h264Level(w, h int) (level, profileLevelID string) {
	mbs := ((w + 15) / 16) * ((h + 15) / 16)
	switch {
	case mbs <= 3600: // up to 1280×720
		return "3.1", "42e01f"
	case mbs <= 8192: // up to 1920×1080
		return "4.0", "42e028"
	default:
		return "5.1", "42e033"
	}
}

func NewEncoder(cfg EncoderConfig, streamID string) (*Encoder, error) {
	_, plid := h264Level(cfg.Width, cfg.Height)
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid,
	}, "video", streamID)
	if err != nil {
		return nil, err
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	// ffmpeg's -ssrc takes a signed 32-bit int: stay in 1..2^31-1.
	ssrc := binary.BigEndian.Uint32(b[:])&0x7fffffff | 1
	ctx, cancel := context.WithCancel(context.Background())
	e := &Encoder{cfg: cfg, Track: track, ssrc: ssrc, cancel: cancel, done: make(chan struct{})}
	go e.supervise(ctx)
	return e, nil
}

// PushJPEG stores the newest frame. Cheap; safe to call from CDP handlers.
func (e *Encoder) PushJPEG(b []byte) {
	e.latest.Store(&b)
	e.frames.Add(1)
}

// LatestJPEG returns the most recent screencast frame, or nil before the
// page has painted.
func (e *Encoder) LatestJPEG() []byte {
	if p := e.latest.Load(); p != nil {
		return *p
	}
	return nil
}

// Stats returns the number of frames received and access units encoded.
func (e *Encoder) Stats() (framesIn, samplesOut uint64) {
	return e.frames.Load(), e.samples.Load()
}

func (e *Encoder) Close() {
	e.stopOnce.Do(func() {
		e.cancel()
		<-e.done
	})
}

func (e *Encoder) supervise(ctx context.Context) {
	defer close(e.done)
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		start := time.Now()
		err := e.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("encoder: ffmpeg exited (%v); restarting", err)
		if time.Since(start) > 10*time.Second {
			backoff = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (e *Encoder) ffmpegArgs(rtpPort int) []string {
	c := e.cfg
	gop := int(c.KeyframeInterval.Seconds() * float64(c.FPS))
	if gop < 1 {
		gop = c.FPS
	}
	fps := strconv.Itoa(c.FPS)
	level, _ := h264Level(c.Width, c.Height)
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "nobuffer", "-probesize", "32", "-analyzeduration", "0",
		"-f", "image2pipe", "-c:v", "mjpeg", "-framerate", fps, "-i", "pipe:0",
		"-an",
		// Normalize size and pixel format in case a frame ever arrives at a
		// different size (e.g. during a resize or crash recovery).
		"-vf", fmt.Sprintf("scale=%d:%d:flags=fast_bilinear,format=yuv420p", c.Width, c.Height),
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-level", level,
		"-threads", "1",
		"-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop), "-sc_threshold", "0",
		"-b:v", fmt.Sprintf("%dk", c.BitrateKbps),
		"-maxrate", fmt.Sprintf("%dk", c.BitrateKbps*6/5),
		"-bufsize", fmt.Sprintf("%dk", c.BitrateKbps/2),
		"-x264-params", "repeat-headers=1",
		"-f", "rtp", "-payload_type", "96", "-ssrc", strconv.FormatUint(uint64(e.ssrc), 10),
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", rtpPort),
	}
}

func (e *Encoder) runOnce(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return err
	}
	defer udp.Close()
	_ = udp.SetReadBuffer(4 << 20) // keyframes arrive as bursts of packets
	port := udp.LocalAddr().(*net.UDPAddr).Port

	cmd := exec.CommandContext(ctx, e.cfg.FFmpegPath, e.ffmpegArgs(port)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	if err := cmd.Start(); err != nil {
		return err
	}

	readErr := make(chan error, 1)
	go func() { readErr <- e.forwardRTP(ctx, udp) }()

	writeErr := make(chan error, 1)
	go func() {
		defer stdin.Close()
		t := time.NewTicker(time.Second / time.Duration(e.cfg.FPS))
		defer t.Stop()
		w := bufio.NewWriterSize(stdin, 256<<10)
		for {
			select {
			case <-ctx.Done():
				writeErr <- nil
				return
			case <-t.C:
				p := e.latest.Load()
				if p == nil {
					continue // nothing painted yet
				}
				if _, err := w.Write(*p); err != nil {
					writeErr <- err
					return
				}
				if err := w.Flush(); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()

	var result error
	select {
	case result = <-readErr:
	case result = <-writeErr:
	}
	cancel()
	_ = cmd.Wait()
	if result == nil && stderr.Len() > 0 {
		result = errors.New(stderr.String())
	}
	return result
}

// forwardRTP relays ffmpeg's RTP packets to the shared track. Pion
// rewrites SSRC and payload type for each peer.
func (e *Encoder) forwardRTP(ctx context.Context, conn *net.UDPConn) error {
	go func() { <-ctx.Done(); conn.Close() }()
	buf := make([]byte, 1600)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !from.IP.IsLoopback() {
			continue
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil || pkt.SSRC != e.ssrc {
			continue // not this session's ffmpeg
		}
		if pkt.Marker {
			e.samples.Add(1)
		}
		if err := e.Track.WriteRTP(&pkt); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
	}
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, _ = l.w.Write(q)
	return len(p), nil
}
