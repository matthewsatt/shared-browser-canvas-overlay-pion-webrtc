/*!
 * Collab Browser SDK
 *
 * Mounts a live view of a shared browser session into your page:
 *
 *   <script src="https://collab.example.com/sdk.js"></script>
 *   <script>
 *     // `seat` is the host or guest object from POST /api/browser-sessions
 *     const view = CollabBrowser.mount(document.getElementById('slot'), seat);
 *     view.on('ended', ({ reason }) => { ... });
 *     view.navigate('https://checkin.example.com/');
 *   </script>
 *
 * The view fills the element you give it (give that element a size). It
 * lives in its own shadow root, so your page's CSS doesn't affect it, and
 * several views can share one page. It talks to the server over one
 * WebSocket; video arrives over WebRTC, or as JPEG frames on the WebSocket
 * when WebRTC can't connect.
 */
(function () {
  'use strict';
  if (window.CollabBrowser) return;

  const SCRIPT_ORIGIN = (() => {
    try { return new URL(document.currentScript.src).origin; } catch { return location.origin; }
  })();
  const BUTTONS = ['left', 'middle', 'right'];
  const KB_MODES = new Set(['text', 'search', 'email', 'tel', 'url', 'numeric', 'decimal']);

  // The host element sits in your page's DOM, so page rules can reach it;
  // !important here beats even the page's !important (shadow DOM cascade).
  const CSS = `
:host { all: initial !important; display: block !important; position: relative !important; box-sizing: border-box !important;
  width: 100% !important; height: 100% !important; min-height: 120px !important; margin: 0 !important; padding: 0 !important;
  border: 0 !important; overflow: hidden !important; visibility: visible !important; opacity: 1 !important;
  transform: none !important; filter: none !important; cursor: default !important;
  background: #000 !important; color: #e8eaee !important; font: 14px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif !important; }
:host([hidden]) { display: none !important; }
* { box-sizing: border-box; }
[hidden] { display: none !important; }
.stage { position: absolute; inset: 0; overflow: hidden; background: #000; touch-action: none; user-select: none;
  -webkit-user-select: none; -webkit-touch-callout: none; -webkit-tap-highlight-color: transparent; }
.video, .screen { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; background: #000; pointer-events: none; }
.input { position: absolute; inset: 0; width: 100%; height: 100%; cursor: default; outline: none; }
.cursors { position: absolute; inset: 0; width: 100%; height: 100%; pointer-events: none; }
.kb { position: absolute; left: 0; top: 0; width: 1px; height: 1px; opacity: 0; border: 0; padding: 0; margin: 0; resize: none;
  pointer-events: none; font-size: 16px; color: transparent; background: transparent; caret-color: transparent; }
.banner, .notice { position: absolute; left: 50%; transform: translateX(-50%); background: rgba(23,26,33,.94); border: 1px solid #2c313c;
  color: #e8eaee; padding: 8px 14px; font-size: 14px; pointer-events: none; max-width: 90%; }
.banner { top: 12px; border-radius: 999px; white-space: nowrap; }
.notice { bottom: 14px; border-radius: 8px; }
.select-layer { position: absolute; inset: 0; pointer-events: none; }
.select-list { position: absolute; pointer-events: auto; background: #fff; color: #111; border: 1px solid #888; border-radius: 4px;
  box-shadow: 0 8px 24px rgba(0,0,0,.35); overflow-y: auto; font-size: 14px; min-width: 120px; }
.select-list .who { font-size: 11px; color: #666; padding: 4px 8px; border-bottom: 1px solid #ddd; }
.select-list .group { font-weight: 700; padding: 4px 8px; color: #333; }
.select-list button { display: block; width: 100%; text-align: left; border: 0; border-radius: 0; background: transparent; color: #111;
  padding: 5px 10px; font: inherit; cursor: pointer; }
.select-list button:hover, .select-list button:focus { background: #1e66f5; color: #fff; outline: none; }
.select-list button.selected { font-weight: 700; }
.select-list button.indent { padding-left: 20px; }
.select-list button:disabled { color: #999; background: transparent; cursor: default; }
.dialog-layer { position: absolute; inset: 0; background: rgba(0,0,0,.35); display: grid; place-items: start center; padding-top: 60px; }
.dialog { background: #171a21; border: 1px solid #2c313c; border-radius: 10px; padding: 18px; width: min(420px, 92%); display: grid; gap: 12px; }
.dialog .msg { white-space: pre-wrap; word-break: break-word; max-height: 40vh; overflow: auto; }
.dialog .note { font-size: 12px; color: #9aa1ad; }
.dialog input { font: inherit; color: inherit; background: #1f232c; border: 1px solid #2c313c; border-radius: 8px; padding: 8px 10px; }
.dialog .actions { display: flex; justify-content: flex-end; gap: 8px; }
.dialog button { font: inherit; color: inherit; background: #1f232c; border: 1px solid #2c313c; border-radius: 8px; padding: 8px 14px; cursor: pointer; }
.dialog button.primary { background: #4c8dff; border-color: #4c8dff; color: #fff; font-weight: 600; }
`;

  let sharedSheet = null;
  function styleRoot(root) {
    try {
      if (!sharedSheet) { sharedSheet = new CSSStyleSheet(); sharedSheet.replaceSync(CSS); }
      root.adoptedStyleSheets = [sharedSheet];
    } catch {
      const s = document.createElement('style');
      s.textContent = CSS;
      root.append(s);
    }
  }

  function h(tag, cls, extra) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (extra) Object.assign(el, extra);
    return el;
  }

  function mount(container, opts = {}) {
    if (!(container instanceof Element)) throw new TypeError('CollabBrowser.mount: the first argument must be an element');
    const { sessionId, participantId, token } = opts;
    if (!sessionId || !participantId || !token) {
      throw new TypeError('CollabBrowser.mount: sessionId, participantId and token are required (a seat from POST /api/browser-sessions)');
    }
    const server = new URL(opts.server || SCRIPT_ORIGIN);
    const showNotices = opts.notices !== false;

    // ---------------------------------------------------------------- DOM

    const hostEl = h('div', 'collab-browser');
    const root = hostEl.attachShadow({ mode: 'open' });
    styleRoot(root);
    const els = {
      stage: h('div', 'stage'),
      video: h('video', 'video', { autoplay: true, muted: true, playsInline: true, disablePictureInPicture: true }),
      screen: h('canvas', 'screen', { hidden: true }),
      canvas: h('canvas', 'input', { tabIndex: -1 }),
      selectLayer: h('div', 'select-layer'),
      dialogLayer: h('div', 'dialog-layer', { hidden: true }),
      cursorCanvas: h('canvas', 'cursors'),
      kb: h('textarea', 'kb', { autocapitalize: 'off', autocomplete: 'off', spellcheck: false }),
      banner: h('div', 'banner', { hidden: true }),
      notice: h('div', 'notice', { hidden: true }),
    };
    els.video.setAttribute('muted', '');
    els.canvas.setAttribute('aria-label', 'Shared browser');
    els.kb.setAttribute('autocorrect', 'off');
    els.kb.setAttribute('aria-label', 'Keyboard input');
    els.screen.setAttribute('aria-hidden', 'true');
    els.cursorCanvas.setAttribute('aria-hidden', 'true');
    els.stage.append(els.video, els.screen, els.canvas, els.selectLayer, els.dialogLayer, els.cursorCanvas, els.kb, els.banner, els.notice);
    root.append(els.stage);
    container.append(hostEl);

    // ---------------------------------------------------------------- state & events

    const state = {
      sessionId,
      me: { participantId },
      role: null,
      iceServers: [],
      viewport: { width: 1280, height: 720 },
      participants: new Map(), // id -> {id,name,role,color,connected}
      cursors: new Map(),      // id -> {x,y,tx,ty,visible,seen}
      ripples: [],
      locks: { keyboard: '', pointer: '' },
      status: 'connecting', url: '', title: '', loading: false,
      select: null, dialog: null,
      fields: null, focus: null,
      connected: false, ended: false, endReason: null,
    };
    const diag = { framesRendered: 0, messages: {}, rejections: 0, rtcState: 'new', transport: 'none', stats: null };
    const listeners = new Map();
    let destroyed = false;
    let readyFired = false;

    function emit(type, detail) {
      for (const fn of listeners.get(type) || []) {
        try { fn(detail); } catch (e) { console.error(`CollabBrowser "${type}" listener:`, e); }
      }
    }

    function snapshot() {
      return {
        sessionId, participantId, role: state.role,
        status: state.status, connected: state.connected, ended: state.ended, endReason: state.endReason,
        url: state.url, title: state.title, loading: state.loading,
        participants: [...state.participants.values()].map((p) => ({ ...p, you: p.id === participantId })),
        transport: media.mode, video: media.pref === 'ws' ? 'websocket' : 'auto',
      };
    }
    const emitState = () => emit('state', snapshot());

    // ---------------------------------------------------------------- connection

    const conn = {
      ws: null, pc: null, backoff: 500, timer: null, pendingIce: [], closing: false,
      rtcBackoff: 1000, rtcTimer: null,

      start() {
        banner('Connecting…');
        this.openWS();
      },

      openWS() {
        clearTimeout(this.timer);
        const q = new URLSearchParams({ participantId, token });
        const proto = server.protocol === 'https:' ? 'wss:' : 'ws:';
        const ws = new WebSocket(`${proto}//${server.host}/api/browser-sessions/${encodeURIComponent(sessionId)}/connect?${q}`);
        ws.binaryType = 'blob';
        this.ws = ws;
        ws.onopen = () => {
          this.backoff = 500;
          state.connected = true;
          emit('connection', { connected: true });
          this.send({ type: 'viewport_ready', width: els.stage.clientWidth, height: els.stage.clientHeight });
          media.reset();
          media.fallback(); // WebSocket frames until WebRTC is playing
        };
        ws.onmessage = (e) => {
          if (typeof e.data !== 'string') { media.onWSFrame(e.data); return; }
          let m; try { m = JSON.parse(e.data); } catch { return; }
          diag.messages[m.type] = (diag.messages[m.type] || 0) + 1;
          onServerMessage(m);
        };
        ws.onclose = () => {
          const was = state.connected;
          state.connected = false;
          if (this.ws !== ws) return;
          this.closeRTC();
          if (was) emit('connection', { connected: false });
          if (this.closing || state.ended) return;
          banner('Reconnecting…');
          this.timer = setTimeout(() => this.openWS(), this.backoff);
          this.backoff = Math.min(this.backoff * 2, 8000);
        };
      },

      send(m) {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
      },

      async startRTC() {
        this.closeRTC();
        if (media.pref !== 'auto' || state.ended || destroyed) return;
        let pc;
        try { pc = new RTCPeerConnection({ iceServers: state.iceServers }); }
        catch (e) { console.warn('CollabBrowser: WebRTC unavailable; using WebSocket video', e); return; }
        this.pc = pc;
        this.pendingIce = [];
        pc.addTransceiver('video', { direction: 'recvonly' });
        pc.ontrack = (e) => {
          // Interactive use: play frames as soon as they arrive.
          try { e.receiver.jitterBufferTarget = 0; } catch {}
          try { e.receiver.playoutDelayHint = 0; } catch {}
          const stream = e.streams[0] || new MediaStream([e.track]);
          if (els.video.srcObject !== stream) els.video.srcObject = stream;
          els.video.play().catch(() => {});
        };
        pc.onicecandidate = (e) => {
          if (e.candidate) this.send({ type: 'webrtc_ice', candidate: e.candidate.toJSON() });
        };
        pc.onconnectionstatechange = () => {
          diag.rtcState = pc.connectionState;
          if (pc !== this.pc) return;
          const st = pc.connectionState;
          if (st === 'connected') this.rtcBackoff = 1000;
          if (st === 'disconnected' || st === 'failed') media.fallback();
          if (st === 'failed') this.retryRTC();
        };
        try {
          await pc.setLocalDescription(await pc.createOffer());
        } catch (e) { console.warn('CollabBrowser: offer', e); return; }
        if (pc !== this.pc) return;
        this.send({ type: 'webrtc_offer', sdp: pc.localDescription.sdp });
      },

      // Try WebRTC again later, backing off so a network that blocks it
      // isn't hammered. WebSocket video keeps the picture going meanwhile.
      retryRTC() {
        clearTimeout(this.rtcTimer);
        this.rtcTimer = setTimeout(() => this.startRTC(), this.rtcBackoff);
        this.rtcBackoff = Math.min(this.rtcBackoff * 2, 60000);
      },

      async onAnswer(sdp) {
        if (!this.pc) return;
        await this.pc.setRemoteDescription({ type: 'answer', sdp });
        for (const c of this.pendingIce.splice(0)) this.pc.addIceCandidate(c).catch(() => {});
      },

      onRemoteIce(c) {
        if (!this.pc || !c) return;
        if (this.pc.remoteDescription) this.pc.addIceCandidate(c).catch(() => {});
        else this.pendingIce.push(c);
      },

      closeRTC() {
        clearTimeout(this.rtcTimer);
        if (this.pc) { try { this.pc.close(); } catch {} this.pc = null; }
      },

      stop() {
        this.closing = true;
        clearTimeout(this.timer);
        this.send({ type: 'participant_leave' });
        this.closeRTC();
        if (this.ws) this.ws.close();
      },
    };

    // ---------------------------------------------------------------- video transport
    //
    // WebRTC (H.264 over UDP) is the normal path. WebSocket video sends one
    // JPEG per repaint on the existing connection: it works wherever the
    // page loads (UDP blocked, proxy-only networks). Each connection starts
    // on WebSocket frames and switches to WebRTC once its frames play;
    // if WebRTC drops or never connects, WebSocket frames take over.

    const media = {
      pref: opts.video === 'websocket' ? 'ws' : 'auto',
      mode: 'none',      // what's on screen: 'none' | 'ws' | 'rtc'
      wsOn: false,       // the server is sending us WebSocket frames
      rtcLive: false,    // WebRTC frames are arriving on a connected peer
      pending: null,     // newest JPEG Blob not yet drawn
      decoding: false,
      clearBanner: false,
      wsBytes: 0, wsFrames: 0, wsSince: performance.now(), rtcPrev: null,

      reset() { this.wsOn = false; this.pending = null; this.rtcLive = false; this.clearBanner = true; },
      startWS() { if (!this.wsOn) { this.wsOn = true; conn.send({ type: 'video_ws_start' }); } },
      stopWS() { if (this.wsOn) { this.wsOn = false; this.pending = null; conn.send({ type: 'video_ws_stop' }); } },
      fallback() { this.rtcLive = false; this.startWS(); },

      onWSFrame(blob) {
        if (!this.wsOn) return; // was in flight when we switched to WebRTC
        this.wsBytes += blob.size;
        this.pending = blob;
        if (!this.decoding) this.drawPending();
      },

      // Decode and draw only the newest frame.
      async drawPending() {
        this.decoding = true;
        while (this.pending && !destroyed) {
          const blob = this.pending;
          this.pending = null;
          let bmp;
          try { bmp = await createImageBitmap(blob); } catch { continue; }
          if (!this.rtcLive) {
            const c = els.screen;
            if (c.width !== bmp.width || c.height !== bmp.height) { c.width = bmp.width; c.height = bmp.height; }
            c.getContext('2d').drawImage(bmp, 0, 0);
            this.wsFrames++;
            this.frameShown('ws');
          }
          bmp.close();
        }
        this.decoding = false;
      },

      onRTCFrame() {
        if (this.pref !== 'auto' || conn.pc?.connectionState !== 'connected') return;
        this.rtcLive = true;
        this.stopWS();
        this.frameShown('rtc');
      },

      frameShown(mode) {
        diag.framesRendered++;
        if (this.mode !== mode) {
          this.mode = mode;
          diag.transport = mode;
          els.screen.hidden = mode !== 'ws'; // the canvas covers the <video> while shown
          emit('transport', { transport: mode });
        }
        if (this.clearBanner && !state.ended) { this.clearBanner = false; banner(null); }
        if (!readyFired) { readyFired = true; emit('ready', snapshot()); }
      },

      setPref(pref) {
        this.pref = pref;
        if (pref === 'ws') { conn.closeRTC(); this.fallback(); }
        else { conn.rtcBackoff = 1000; conn.startRTC(); }
        emitState();
      },

      async sampleStats() {
        const now = performance.now();
        const secs = (now - this.wsSince) / 1000;
        let fps = 0, kbps = 0;
        if (this.mode === 'ws') {
          fps = this.wsFrames / secs;
          kbps = (this.wsBytes * 8) / 1000 / secs;
        } else if (this.mode === 'rtc' && conn.pc) {
          const pc = conn.pc;
          try {
            for (const s of (await pc.getStats()).values()) {
              if (s.type !== 'inbound-rtp' || s.kind !== 'video') continue;
              const p = this.rtcPrev;
              if (p && p.pc === pc && p.id === s.id && s.timestamp > p.timestamp) {
                const dt = (s.timestamp - p.timestamp) / 1000;
                fps = (s.framesDecoded - p.framesDecoded) / dt;
                kbps = ((s.bytesReceived - p.bytesReceived) * 8) / 1000 / dt;
              }
              this.rtcPrev = { pc, id: s.id, timestamp: s.timestamp, framesDecoded: s.framesDecoded, bytesReceived: s.bytesReceived };
            }
          } catch {}
        }
        this.wsFrames = 0; this.wsBytes = 0; this.wsSince = now;
        diag.stats = { transport: this.mode, fps, kbps };
        if (this.mode !== 'none') emit('stats', diag.stats);
      },
    };

    // ---------------------------------------------------------------- server messages

    function onServerMessage(m) {
      switch (m.type) {
        case 'hello':
          state.role = m.role;
          state.viewport = m.viewport || state.viewport;
          state.iceServers = m.iceServers || [];
          conn.startRTC();
          break;
        case 'session_state':
          state.status = m.status || state.status;
          state.viewport = m.viewport || state.viewport;
          state.url = m.url || ''; state.title = m.title || ''; state.loading = !!m.loading;
          state.participants = new Map((m.participants || []).map((p) => [p.id, p]));
          for (const id of [...state.cursors.keys()]) if (!state.participants.get(id)?.connected) state.cursors.delete(id);
          for (const c of m.cursors || []) if (c.participantId !== participantId) setCursor(c.participantId, c.x, c.y, true);
          state.locks = m.locks || state.locks;
          state.fields = m.fields || null;
          state.focus = m.focus || null;
          showDialog(m.dialog || null);
          showSelect(m.select || null, '');
          if (m.status === 'starting') banner('Starting browser…');
          else if (m.status === 'restarting') banner('Browser restarting…');
          else if (m.status === 'ready' && media.mode !== 'none') banner(null);
          emitState();
          break;
        case 'participant_joined':
          state.participants.set(m.participant.id, m.participant);
          emit('participant', { participant: { ...m.participant }, connected: true });
          emitState();
          break;
        case 'participant_left': {
          const p = state.participants.get(m.participantId);
          if (p) {
            p.connected = false;
            emit('participant', { participant: { ...p }, connected: false });
          }
          state.cursors.delete(m.participantId);
          emitState();
          break;
        }
        case 'cursor_update':
          setCursor(m.participantId, m.x, m.y, m.visible !== false);
          break;
        case 'interaction_update':
          if (m.kind === 'mouse_down') state.ripples.push({ id: m.participantId, x: m.x, y: m.y, t: performance.now() });
          break;
        case 'interaction_rejected': {
          diag.rejections++;
          diag.lastRejection = m;
          const by = state.participants.get(m.lockedBy)?.name || 'Someone';
          emit('rejected', { reason: m.reason, by, participantId: m.lockedBy });
          notice(m.reason === 'keyboard_locked' ? `${by} is typing. Try again in a moment.` : `${by} is dragging. Try again in a moment.`, 1500);
          break;
        }
        case 'lock_update':
          state.locks = { keyboard: m.keyboard || '', pointer: m.pointer || '' };
          break;
        case 'browser_navigation':
          state.url = m.url;
          emit('navigation', { url: m.url });
          emitState();
          break;
        case 'browser_loading':
          state.loading = true;
          emitState();
          break;
        case 'browser_loaded':
          state.loading = false; state.url = m.url || state.url; state.title = m.title || state.title;
          emitState();
          break;
        case 'browser_error':
          emit('error', { message: m.message, kind: m.kind || 'browser', url: m.url });
          if (m.kind === 'navigation_blocked') {
            emit('blocked', { url: m.url });
            notice("That page isn't available here.");
          }
          break;
        case 'fields_update':
          state.fields = m.fields || null;
          break;
        case 'focus_update':
          state.focus = m.focus || null;
          onPageFocus();
          break;
        case 'webrtc_answer':
          conn.onAnswer(m.sdp).catch((e) => console.warn('CollabBrowser: answer', e));
          break;
        case 'webrtc_ice':
          conn.onRemoteIce(m.candidate);
          break;
        case 'webrtc_failed':
          media.fallback();
          conn.retryRTC();
          break;
        case 'select_open':
          showSelect(m.select, m.participantId);
          break;
        case 'select_close':
          showSelect(null);
          break;
        case 'dialog_open':
          showDialog(m.dialog);
          break;
        case 'dialog_close':
          showDialog(null);
          break;
        case 'session_closed':
          state.ended = true;
          state.endReason = m.reason || 'closed';
          state.status = 'closed';
          conn.closeRTC();
          banner(m.reason === 'idle' ? 'Session ended (idle).' : 'This session has ended.');
          emit('ended', { reason: state.endReason });
          emitState();
          break;
      }
    }

    function setCursor(id, x, y, visible) {
      if (id === participantId) return;
      let c = state.cursors.get(id);
      if (!c) { c = { x, y, tx: x, ty: y }; state.cursors.set(id, c); }
      c.tx = x; c.ty = y; c.visible = visible; c.seen = performance.now();
    }

    // ---------------------------------------------------------------- geometry

    // Where (in client CSS px) the picture is drawn, accounting for
    // object-fit: contain letterboxing.
    function contentRect() {
      const r = els.stage.getBoundingClientRect();
      const vw = state.viewport.width, vh = state.viewport.height;
      const scale = Math.min(r.width / vw, r.height / vh);
      const w = vw * scale, h = vh * scale;
      return { left: r.left + (r.width - w) / 2, top: r.top + (r.height - h) / 2, width: w, height: h, scale, stage: r };
    }

    function toNorm(clientX, clientY, clamp = false) {
      const cr = contentRect();
      let x = (clientX - cr.left) / cr.width;
      let y = (clientY - cr.top) / cr.height;
      if (clamp) { x = Math.min(1, Math.max(0, x)); y = Math.min(1, Math.max(0, y)); }
      else if (x < 0 || x > 1 || y < 0 || y > 1) return null;
      return { x, y };
    }

    const modifiers = (e) => (e.altKey ? 1 : 0) | (e.ctrlKey ? 2 : 0) | (e.metaKey ? 4 : 0) | (e.shiftKey ? 8 : 0);

    // ---------------------------------------------------------------- keyboard
    //
    // A hidden textarea receives keys, IME composition and paste. With a
    // mouse it stays focused. On touch, it's focused only when the tap lands
    // on a text field in the shared page, so the on-screen keyboard appears
    // just for fields, with the layout the field asks for (numbers for a
    // phone field, etc.). Focusing inside the tap handler is what lets iOS
    // and Android show the keyboard.

    let lastPointer = 'mouse';
    let lastTapAt = 0;
    const kbFocused = () => root.activeElement === els.kb;
    const focusKeyboard = () => { if (!kbFocused()) els.kb.focus({ preventScroll: true }); };

    function showTouchKeyboard(mode) {
      const want = KB_MODES.has(mode) ? mode : 'text';
      if (kbFocused() && els.kb.inputMode !== want) els.kb.blur(); // re-focus to switch layouts
      els.kb.inputMode = want;
      els.kb.focus({ preventScroll: true });
    }
    function hideTouchKeyboard() { if (kbFocused()) els.kb.blur(); }

    // The text field (from the page's reports) under a normalized point.
    function fieldAt(n) {
      const f = state.fields;
      if (!f || !Array.isArray(f.rects)) return null;
      const x = n.x * (f.viewport?.w || state.viewport.width);
      const y = n.y * (f.viewport?.h || state.viewport.height);
      return f.rects.find((r) => x >= r.x && x <= r.x + r.w && y >= r.y && y <= r.y + r.h) || null;
    }

    // Page focus moved without a tap on a field (a label, a script, the
    // other person). Hide the keyboard when focus leaves text; show it
    // only right after this person's own tap.
    function onPageFocus() {
      const f = state.focus;
      if (lastPointer !== 'touch' || !f || state.dialog) return;
      if (!f.editable) hideTouchKeyboard();
      else if (performance.now() - lastTapAt < 1500 && (!kbFocused() || els.kb.inputMode !== (f.mode || 'text'))) showTouchKeyboard(f.mode);
    }

    // ---------------------------------------------------------------- input

    let pendingMove = null, pendingScroll = null, rafQueued = false;
    function flushQueued() {
      rafQueued = false;
      if (pendingMove) { conn.send({ type: 'cursor_move', ...pendingMove }); pendingMove = null; }
      if (pendingScroll) { conn.send({ type: 'scroll', ...pendingScroll }); pendingScroll = null; }
    }
    function queueFlush() { if (!rafQueued) { rafQueued = true; requestAnimationFrame(flushQueued); } }
    const sendKey = (key, code, keyCode) => {
      conn.send({ type: 'key_down', key, code, keyCode });
      conn.send({ type: 'key_up', key, code, keyCode });
    };

    const cv = els.canvas;
    let capturing = false;
    let touch = null; // {id, sx, sy, lx, ly, t, scrolling}

    cv.addEventListener('pointerdown', (e) => {
      if (state.ended) return;
      lastPointer = e.pointerType || 'mouse';
      if (lastPointer !== 'touch') focusKeyboard();
      const n = toNorm(e.clientX, e.clientY);
      if (!n) return;
      e.preventDefault();
      if (e.pointerType === 'touch') {
        if (touch) return; // single-finger only
        touch = { id: e.pointerId, sx: e.clientX, sy: e.clientY, lx: e.clientX, ly: e.clientY, t: performance.now(), scrolling: false };
        cv.setPointerCapture(e.pointerId);
        pendingMove = { x: n.x, y: n.y }; queueFlush();
        return;
      }
      flushQueued();
      cv.setPointerCapture(e.pointerId);
      capturing = true;
      conn.send({ type: 'mouse_down', x: n.x, y: n.y, button: BUTTONS[e.button] || 'left', modifiers: modifiers(e) });
    });

    cv.addEventListener('pointermove', (e) => {
      if (state.ended) return;
      if (touch && e.pointerId === touch.id) {
        const dx = e.clientX - touch.lx, dy = e.clientY - touch.ly;
        if (!touch.scrolling && Math.hypot(e.clientX - touch.sx, e.clientY - touch.sy) > 8) touch.scrolling = true;
        if (touch.scrolling) {
          const cr = contentRect();
          const n = toNorm(touch.sx, touch.sy, true);
          pendingScroll = { x: n.x, y: n.y, dx: (pendingScroll?.dx || 0) - dx / cr.scale, dy: (pendingScroll?.dy || 0) - dy / cr.scale };
          queueFlush();
        }
        touch.lx = e.clientX; touch.ly = e.clientY;
        return;
      }
      const n = toNorm(e.clientX, e.clientY, capturing);
      if (!n) { pendingMove = { x: 0, y: 0, visible: false }; queueFlush(); return; }
      pendingMove = { x: n.x, y: n.y };
      queueFlush();
    });

    const endPointer = (e) => {
      if (touch && e.pointerId === touch.id) {
        const t = touch; touch = null;
        if (!t.scrolling && performance.now() - t.t < 600 && e.type === 'pointerup') {
          const n = toNorm(t.sx, t.sy, true);
          flushQueued();
          conn.send({ type: 'mouse_down', x: n.x, y: n.y, button: 'left' });
          conn.send({ type: 'mouse_up', x: n.x, y: n.y, button: 'left' });
          lastTapAt = performance.now();
          const field = fieldAt(n);
          if (field) showTouchKeyboard(field.mode);
          else hideTouchKeyboard();
        }
        return;
      }
      if (!capturing) return;
      capturing = false;
      const n = toNorm(e.clientX, e.clientY, true);
      flushQueued();
      conn.send({ type: 'mouse_up', x: n.x, y: n.y, button: BUTTONS[e.button] || 'left', modifiers: modifiers(e) });
    };
    cv.addEventListener('pointerup', endPointer);
    cv.addEventListener('pointercancel', endPointer);
    cv.addEventListener('pointerleave', () => { if (!capturing && !touch) { pendingMove = { x: 0, y: 0, visible: false }; queueFlush(); } });
    cv.addEventListener('contextmenu', (e) => e.preventDefault());
    cv.addEventListener('auxclick', (e) => e.preventDefault());

    cv.addEventListener('wheel', (e) => {
      e.preventDefault();
      const n = toNorm(e.clientX, e.clientY, true);
      const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? state.viewport.height : 1;
      pendingScroll = {
        x: n.x, y: n.y, modifiers: modifiers(e),
        dx: (pendingScroll?.dx || 0) + e.deltaX * unit,
        dy: (pendingScroll?.dy || 0) + e.deltaY * unit,
      };
      queueFlush();
    }, { passive: false });

    const kb = els.kb;
    const isPaste = (e) => (e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'v';
    const imeKey = (e) => e.isComposing || e.key === 'Process' || e.keyCode === 229 || e.key === 'Unidentified';
    kb.addEventListener('keydown', (e) => {
      if (state.ended || state.dialog) return;
      if (imeKey(e)) return; // IME / on-screen keyboards: use input events
      if (isPaste(e)) return; // handled by the paste event
      e.preventDefault();
      flushQueued();
      conn.send({ type: 'key_down', key: e.key, code: e.code, keyCode: e.keyCode, modifiers: modifiers(e), repeat: e.repeat });
    });
    kb.addEventListener('keyup', (e) => {
      if (state.ended || state.dialog) return;
      if (imeKey(e) || isPaste(e)) return;
      e.preventDefault();
      conn.send({ type: 'key_up', key: e.key, code: e.code, keyCode: e.keyCode, modifiers: modifiers(e) });
    });
    kb.addEventListener('input', (e) => {
      if (e.inputType === 'insertCompositionText') return; // committed on compositionend
      if (e.inputType && e.inputType.startsWith('insert') && e.data) conn.send({ type: 'text_input', text: e.data });
      else if (e.inputType === 'insertLineBreak') sendKey('Enter', 'Enter', 13);
      else if (e.inputType === 'deleteContentBackward') sendKey('Backspace', 'Backspace', 8);
      kb.value = '';
    });
    kb.addEventListener('compositionend', (e) => {
      if (e.data) conn.send({ type: 'text_input', text: e.data });
      kb.value = '';
    });
    kb.addEventListener('paste', (e) => {
      e.preventDefault();
      const text = e.clipboardData?.getData('text/plain');
      if (text) conn.send({ type: 'text_input', text: text.slice(0, 10000) });
    });

    const onPageHide = () => conn.send({ type: 'cursor_move', x: 0, y: 0, visible: false });
    window.addEventListener('pagehide', onPageHide);

    // ---------------------------------------------------------------- overlay drawing

    let ctx = null, rafId = 0;
    function resizeCanvas() {
      const r = els.stage.getBoundingClientRect();
      const dpr = window.devicePixelRatio || 1;
      els.cursorCanvas.width = Math.round(r.width * dpr);
      els.cursorCanvas.height = Math.round(r.height * dpr);
      ctx = els.cursorCanvas.getContext('2d');
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      if (state.select) showSelect(state.select.data, state.select.by);
    }

    function draw(now) {
      rafId = requestAnimationFrame(draw);
      if (!ctx) return;
      const cr = contentRect();
      const ox = cr.left - cr.stage.left, oy = cr.top - cr.stage.top;
      ctx.clearRect(0, 0, cr.stage.width, cr.stage.height);
      state.ripples = state.ripples.filter((r) => now - r.t < 600);
      for (const r of state.ripples) {
        const p = state.participants.get(r.id);
        const k = (now - r.t) / 600;
        ctx.beginPath();
        ctx.arc(ox + r.x * cr.width, oy + r.y * cr.height, 6 + k * 22, 0, Math.PI * 2);
        ctx.strokeStyle = p?.color || '#fff';
        ctx.globalAlpha = 1 - k;
        ctx.lineWidth = 3;
        ctx.stroke();
        ctx.globalAlpha = 1;
      }
      for (const [id, c] of state.cursors) {
        if (!c.visible) continue;
        const p = state.participants.get(id);
        if (!p) continue;
        c.x += (c.tx - c.x) * 0.5;
        c.y += (c.ty - c.y) * 0.5;
        const x = ox + c.x * cr.width, y = oy + c.y * cr.height;
        drawCursor(x, y, p.color);
        let label = p.name;
        if (state.locks.keyboard === id) label += ' · typing';
        else if (state.locks.pointer === id) label += ' · dragging';
        drawLabel(x + 14, y + 18, label, p.color);
      }
    }

    function drawCursor(x, y, color) {
      ctx.save();
      ctx.translate(x, y);
      ctx.beginPath();
      ctx.moveTo(0, 0); ctx.lineTo(0, 17); ctx.lineTo(4.5, 13); ctx.lineTo(7.5, 20);
      ctx.lineTo(10.5, 18.8); ctx.lineTo(7.6, 12); ctx.lineTo(13, 12); ctx.closePath();
      ctx.fillStyle = color; ctx.fill();
      ctx.lineWidth = 1.5; ctx.strokeStyle = '#fff'; ctx.stroke();
      ctx.restore();
    }

    function drawLabel(x, y, text, color) {
      ctx.font = '600 12px system-ui, sans-serif';
      const w = ctx.measureText(text).width + 12;
      ctx.fillStyle = color;
      ctx.beginPath();
      ctx.roundRect ? ctx.roundRect(x, y, w, 20, 10) : ctx.rect(x, y, w, 20);
      ctx.fill();
      ctx.fillStyle = '#fff';
      ctx.textBaseline = 'middle';
      ctx.fillText(text, x + 6, y + 10.5);
    }

    // ---------------------------------------------------------------- select & dialog overlays

    let selectCancel = null;
    function showSelect(data, by) {
      els.selectLayer.replaceChildren();
      if (selectCancel) { document.removeEventListener('pointerdown', selectCancel, true); selectCancel = null; }
      if (!data) { state.select = null; return; }
      state.select = { data, by };
      const cr = contentRect();
      const ox = cr.left - cr.stage.left, oy = cr.top - cr.stage.top;
      const sx = cr.width / (data.viewport?.w || state.viewport.width);
      const sy = cr.height / (data.viewport?.h || state.viewport.height);
      const list = h('div', 'select-list');
      list.setAttribute('role', 'listbox');
      const left = ox + data.rect.x * sx;
      const top = oy + (data.rect.y + data.rect.h) * sy;
      const below = cr.stage.height - top - 8;
      const above = oy + data.rect.y * sy - 8;
      list.style.left = `${Math.max(4, Math.min(left, cr.stage.width - 160))}px`;
      list.style.minWidth = `${Math.max(120, data.rect.w * sx)}px`;
      if (below >= 160 || below >= above) { list.style.top = `${top}px`; list.style.maxHeight = `${Math.max(100, below)}px`; }
      else { list.style.bottom = `${cr.stage.height - oy - data.rect.y * sy}px`; list.style.maxHeight = `${Math.max(100, above)}px`; }

      const who = state.participants.get(by)?.name;
      if (who && by !== participantId) list.append(h('div', 'who', { textContent: `Opened by ${who}` }));
      let group = null;
      for (const o of data.options || []) {
        if (o.group !== group) {
          group = o.group;
          if (group) list.append(h('div', 'group', { textContent: group }));
        }
        const b = h('button', '', { type: 'button', textContent: o.label || ' ', disabled: !!o.disabled });
        b.setAttribute('role', 'option');
        if (o.group) b.classList.add('indent');
        if (o.index === data.selectedIndex) b.classList.add('selected');
        b.addEventListener('click', () => conn.send({ type: 'select_choose', id: data.id, index: o.index }));
        list.append(b);
      }
      list.addEventListener('keydown', (e) => { if (e.key === 'Escape') conn.send({ type: 'select_cancel', id: data.id }); });
      els.selectLayer.append(list);
      (list.querySelector('button.selected') || list.querySelector('button:not(:disabled)'))?.scrollIntoView({ block: 'nearest' });
      // A press anywhere outside the list cancels it for everyone. The list
      // is in a shadow root, so look at the composed path.
      setTimeout(() => {
        if (!list.isConnected) return;
        selectCancel = (e) => {
          if (!list.isConnected) { document.removeEventListener('pointerdown', selectCancel, true); selectCancel = null; return; }
          if (!e.composedPath().includes(list)) {
            conn.send({ type: 'select_cancel', id: data.id });
            document.removeEventListener('pointerdown', selectCancel, true);
            selectCancel = null;
          }
        };
        document.addEventListener('pointerdown', selectCancel, true);
      });
    }

    function showDialog(d) {
      // Only move focus if this view had it: never pull it out of the host page.
      const hadFocus = !!root.activeElement;
      state.dialog = d;
      const layer = els.dialogLayer;
      layer.replaceChildren();
      layer.hidden = !d;
      if (!d) {
        if (hadFocus && lastPointer !== 'touch' && !destroyed) focusKeyboard();
        return;
      }
      const box = h('div', 'dialog');
      box.setAttribute('role', 'alertdialog');
      const msg = h('div', 'msg', { textContent: d.kind === 'beforeunload' ? 'This page asks you to confirm leaving it.' : (d.message || '') });
      const note = h('div', 'note', { textContent: 'The page is showing a dialog. Anyone can answer it.' });
      box.append(msg, note);
      let input = null;
      if (d.kind === 'prompt') { input = h('input', '', { value: d.defaultPrompt || '' }); box.append(input); }
      const actions = h('div', 'actions');
      if (d.kind !== 'alert') {
        const cancel = h('button', '', { type: 'button', textContent: 'Cancel' });
        cancel.addEventListener('click', () => conn.send({ type: 'dialog_response', accept: false }));
        actions.append(cancel);
      }
      const ok = h('button', 'primary', { type: 'button', textContent: d.kind === 'beforeunload' ? 'Leave' : 'OK' });
      ok.addEventListener('click', () => conn.send({ type: 'dialog_response', accept: true, text: input ? input.value : '' }));
      actions.append(ok);
      box.append(actions);
      layer.append(box);
      if (hadFocus || lastPointer === 'touch') (input || ok).focus({ preventScroll: true });
    }

    // ---------------------------------------------------------------- notices

    function banner(text) {
      if (!showNotices) return;
      if (state.ended && text !== null && !/ended/.test(text)) return;
      if (state.ended && text === null) return;
      els.banner.hidden = !text;
      if (text) els.banner.textContent = text;
    }

    let noticeTimer = null;
    function notice(text, ms = 3000) {
      if (!showNotices) return;
      els.notice.textContent = text;
      els.notice.hidden = false;
      clearTimeout(noticeTimer);
      noticeTimer = setTimeout(() => { els.notice.hidden = true; }, ms);
    }

    // ---------------------------------------------------------------- start

    let rvfcHandle = 0;
    if ('requestVideoFrameCallback' in HTMLVideoElement.prototype) {
      const onFrame = () => { media.onRTCFrame(); rvfcHandle = els.video.requestVideoFrameCallback(onFrame); };
      rvfcHandle = els.video.requestVideoFrameCallback(onFrame);
    } else {
      els.video.addEventListener('timeupdate', () => media.onRTCFrame());
    }
    const ro = new ResizeObserver(resizeCanvas);
    ro.observe(els.stage);
    resizeCanvas();
    rafId = requestAnimationFrame(draw);
    const statsTimer = setInterval(() => media.sampleStats(), 1000);
    conn.start();

    // ---------------------------------------------------------------- public API

    const view = {
      /** Current session info: url, title, loading, participants, role, transport... */
      get state() { return snapshot(); },
      /** Latest video stats: {transport, fps, kbps}, updated every second. */
      get stats() { return diag.stats; },
      /** The element the view created inside your container. */
      get element() { return hostEl; },

      navigate(url) { conn.send({ type: 'navigate', url: String(url) }); },
      back() { conn.send({ type: 'back' }); },
      forward() { conn.send({ type: 'forward' }); },
      reload() { conn.send({ type: 'reload' }); },
      /** Ends the session for both people. Host only; returns false for the guest. */
      end() {
        if (state.role !== 'host') return false;
        conn.send({ type: 'end_session' });
        return true;
      },
      /** 'auto' (WebRTC, falling back to WebSocket) or 'websocket' (WebRTC off). */
      setVideoTransport(mode) { media.setPref(mode === 'websocket' ? 'ws' : 'auto'); },
      /** Gives keyboard focus to the shared page. */
      focus() { focusKeyboard(); },

      on(type, fn) {
        if (!listeners.has(type)) listeners.set(type, new Set());
        listeners.get(type).add(fn);
        return () => view.off(type, fn);
      },
      off(type, fn) { listeners.get(type)?.delete(fn); },

      /** Disconnects and removes the view. The session itself keeps running. */
      destroy() {
        if (destroyed) return;
        destroyed = true;
        conn.stop();
        cancelAnimationFrame(rafId);
        clearInterval(statsTimer);
        clearTimeout(noticeTimer);
        if (rvfcHandle && els.video.cancelVideoFrameCallback) els.video.cancelVideoFrameCallback(rvfcHandle);
        ro.disconnect();
        window.removeEventListener('pagehide', onPageHide);
        if (selectCancel) document.removeEventListener('pointerdown', selectCancel, true);
        els.video.srcObject = null;
        hostEl.remove();
        listeners.clear();
      },

      /** Internals for diagnostics and tests; not a stable API. */
      debug: { state, diag, els, conn, media, root, kbFocused, contentRect },
    };
    return view;
  }

  window.CollabBrowser = Object.freeze({ mount, version: '1' });
})();
