# Collaborative Remote Browser (Go)

A shared browser for two people, such as a receptionist and a kiosk. The page runs in one Chromium on the server. Your app starts a session through the API and gets two **seats**, host and guest. Each person's page puts the live view into a `<div>` with the JavaScript SDK. Both see the same page and can click, type, scroll, use dropdowns and answer dialogs, and each sees the other's cursor. Neither device loads the target page itself.

```
 Host's page (your app)          Collab server                  Guest's page (your app)
 ┌──────────────────────┐   ┌─────────────────────────┐   ┌──────────────────────┐
 │ <div> + mount(seat)  │◀──│ Chromium running the    │──▶│ <div> + mount(seat)  │
 │                      │vid│ page (the only browser) │vid│                      │
 │ clicks, typing ──────┼──▶│                         │◀──┼────── taps, typing   │
 └──────────────────────┘   └─────────────────────────┘   └──────────────────────┘
            ▲                            ▲                             ▲
            │ host seat                  │ POST /api/browser-sessions  │ guest seat
            └────────────────────── your backend ──────────────────────┘
```

- **Control:** chromedp (Chrome DevTools Protocol), used directly from Go.
- **Video:** CDP screencast frames (JPEG, sent when the page repaints) are repeated at a fixed rate into one ffmpeg/libx264 encoder per session. ffmpeg sends RTP over loopback, and a single Pion `TrackLocalStaticRTP` is attached to both peer connections, so **encoding happens once per session**.
- **Video without WebRTC:** the same JPEG frames can also go straight over the participant's WebSocket. Every view starts this way, so the first picture appears instantly. It switches to WebRTC once WebRTC frames are playing, and falls back if WebRTC drops or never connects. That covers networks that block UDP or only allow a web proxy, as many corporate networks do.
- **Input:** WebSocket JSON. Coordinates are normalized to `[0,1]` over the displayed picture, excluding letterbox bars. The server maps them onto a fixed viewport (default 1280×720, DPR 1); clicks land on the exact page pixel at any window size.
- **Isolation:** each session gets its own Chromium process and a temporary profile directory, which is deleted on close.

## Using it from your app

### 1. Your backend starts a session

```bash
curl -X POST https://collab.example.com/api/browser-sessions \
  -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" \
  -d '{"url": "https://checkin.example.com/", "host": {"name": "Front desk"}, "guest": {"name": "Lobby kiosk"}}'
```

```json
{
  "sessionId": "8b3f1111eebfd21290493955",
  "status": "starting",
  "viewport": { "width": 1280, "height": 720 },
  "host":  { "sessionId": "8b3f…", "participantId": "529b…", "token": "…", "role": "host",  "name": "Front desk" },
  "guest": { "sessionId": "8b3f…", "participantId": "6c06…", "token": "…", "role": "guest", "name": "Lobby kiosk" }
}
```

Call this from your server, never from a browser, because it uses your API token. The URL must match `ALLOWED_URL_PREFIXES` if that's set.

### 2. Your backend hands out the seats

Give `host` to the host's page and `guest` to the guest's device, through whatever channel your app already uses (an API response, a push to the kiosk, and so on). A seat works like a password for that seat: send it only to that person, over HTTPS. Each session has exactly these two seats; nobody else can join.

### 3. Each page mounts its view

```html
<div id="screen" style="width: 960px; height: 540px"></div>
<script src="https://collab.example.com/sdk.js"></script>
<script>
  const view = CollabBrowser.mount(document.getElementById('screen'), seat);
  view.on('ended', ({ reason }) => { /* back to your own UI */ });
</script>
```

`sdk.js` is a single file with no dependencies. Load it from the collab server so it always matches the server's version. The view fills the element you give it: size that element, and the picture keeps its aspect ratio and letterboxes. The view lives in its own shadow root, so your page's CSS can't break it, and several views can share a page.

**Server settings for embedding:** add your app's origin(s) to `ALLOWED_ORIGINS` (e.g. `https://app.example.com,https://kiosk.example.com`); the WebSocket refuses other sites. If your pages have a Content-Security-Policy, allow `script-src https://collab.example.com` and `connect-src wss://collab.example.com`. Nothing else is needed: the SDK makes no HTTP requests and injects no inline styles or scripts.

### SDK reference

`CollabBrowser.mount(element, options)` returns a view. Options are the seat fields (`sessionId`, `participantId`, `token`) plus:

| Option | Default | |
|---|---|---|
| `server` | where `sdk.js` was loaded from | Collab server URL |
| `video` | `'auto'` | `'websocket'` turns WebRTC off for this view |
| `notices` | `true` | Built-in status messages in the view ("Connecting…", "That page isn't available here."). Set `false` to show your own from events. |

| Method | |
|---|---|
| `navigate(url)`, `back()`, `forward()`, `reload()` | Either person. Subject to `ALLOWED_URL_PREFIXES`. |
| `end()` | Ends the session for both. Host only; returns `false` for the guest. |
| `setVideoTransport('auto' \| 'websocket')` | Turn WebRTC on or off. |
| `focus()` | Give keyboard focus to the shared page. |
| `on(event, fn)` → unsubscribe, `off(event, fn)` | |
| `destroy()` | Disconnect and remove the view. The session keeps running, and the seat can mount again. |
| `state` | `{ sessionId, participantId, role, status, connected, ended, endReason, url, title, loading, participants: [{id, name, role, color, connected, you}], transport, video }` |
| `stats` | `{ transport, fps, kbps }`, updated every second |

| Event | Payload |
|---|---|
| `ready` | first picture shown (`state`) |
| `state` | anything in `state` changed (`state`) |
| `navigation` | `{ url }` |
| `participant` | `{ participant, connected }`: the other person connected or left |
| `blocked` | `{ url }`: the page tried to leave the allowed URLs and stayed put |
| `rejected` | `{ reason, by }`: an input was refused because the other person is typing or dragging |
| `error` | `{ message, kind, url }` |
| `connection` | `{ connected }` |
| `transport` | `{ transport: 'rtc' \| 'ws' }` |
| `stats` | `{ transport, fps, kbps }` |
| `ended` | `{ reason }`: `ended_by_host`, `ended_by_api`, `idle`, `max_lifetime`, … |

A view reconnects on its own after network drops. If the page reloads, mount again with the same seat.

### Touch kiosks

On touch, the on-screen keyboard opens only when someone taps a text field in the shared page, and with the layout the field asks for: a number pad for `type="tel"` or `inputmode="numeric"`, an `@` key for email, and so on. Tapping anywhere else closes it, and so does focus leaving the field (for example when the host clicks a button). The shared page reports where its text fields are, so the decision happens during the tap itself, which iOS requires. With a mouse, the view always takes keyboard input, as on a desktop.

### Locking the browser to your pages

Set `ALLOWED_URL_PREFIXES` to the pages the shared browser may show, for example `https://checkin.example.com/,https://visitors.example.com/`. Then:
- sessions can only start on those URLs;
- page navigation anywhere else is stopped before anything loads, and the page stays where it was. That covers links, redirects at every hop, scripts, the address bar and popups.

Images, scripts and API calls from other domains still load. If a page sends people through a login provider, add its prefix too. Each prefix matches on path-segment boundaries: `https://x.com/kiosk` allows `/kiosk/...` but not `/kiosk-admin`.

## Run it

Requirements: Go 1.24+, Chrome or Chromium, and ffmpeg with libx264.

```bash
go build -o bin/collab-browser ./cmd/server
CHROME_NO_SANDBOX=true ./bin/collab-browser        # if running as root in a container
open http://localhost:8080
```

Or with Docker (nothing else to install):

```bash
docker compose up --build
open http://localhost:8080
```

The page at `/` is a **demo app built only on the SDK** ([web/app.js](web/app.js)). Its form plays the role of your backend: it creates a session, opens the host's seat with a toolbar (address bar, back/forward, WebRTC toggle, End), and gives you a guest link that opens the other seat. The guest link carries the seat in the URL fragment, so it never reaches server logs.

To let other computers join, pass the IP they use to reach this machine: `PUBLIC_IP=192.168.1.50 docker compose up --build`. To open sites on your own machine or network (dev only), uncomment `ALLOW_PRIVATE_NETWORKS` in `docker-compose.yml`.

### On a public server (e.g. EC2)

1. Give the server a fixed public IP (on EC2, an Elastic IP) and point a DNS name at it.
2. Open inbound TCP 80 and 443, and UDP 50000–50100 (WebRTC video). Don't open 8080.
3. `cp .env.example .env` and fill in `DOMAIN`, `PUBLIC_IP`, `API_TOKEN`, `ALLOWED_ORIGINS` and `ALLOWED_URL_PREFIXES`.
4. `docker compose up -d --build`

`.env` turns on the `https` profile, which starts [Caddy](Caddyfile) in front of the app. Caddy gets a Let's Encrypt certificate, redirects HTTP to HTTPS and proxies the WebSocket. Plain HTTP on 8080 stays bound to localhost. WebRTC video bypasses Caddy and goes straight to the UDP ports; when UDP is blocked, video falls back to the WebSocket through Caddy on 443.

`PUBLIC_IP` matters on EC2 and anywhere else behind 1:1 NAT. Without it, the server advertises its private address and WebRTC never connects; views then stay on WebSocket video.

## Tests

```bash
go test ./internal/...                      # SSRF guard, URL allowlist, input coalescing and mapping, tokens
python3 tests/e2e/run_e2e.py bin/collab-browser   # needs: pip install playwright; Chrome on PATH
```

The end-to-end suite starts the server (locked to the test site) and a test site, then drives real Chrome: the host in a 1400×900 desktop window, and the guest in a 760×620 touch-capable window. It checks:
- creating a session returns host and guest seats; wrong tokens and off-list start URLs are refused;
- both see the same page; clicks from either window land on the same element and show in **both** decoded video streams;
- clicks landing on the exact page pixel;
- typing, special keys and the keyboard lease;
- the kiosk keyboard: a tap on a text field opens it (phone keypad for a phone field), and it closes when the tap misses or focus leaves the field;
- cursors, scrolling, dropdowns, dialogs, address bar, back/forward and links;
- the URL lock: links, redirects, popups and the address bar can't leave the allowed site;
- turning WebRTC off and on (WebSocket video, with its latency and idle bandwidth), and a viewer whose WebRTC can't connect staying on WebSocket video;
- reload and network-drop reconnects;
- the SDK on a partner site: another origin, a strict CSP and hostile CSS, with host and guest views side by side on one page. A click in one view shows in both; the guest's `end()` is refused and the host's ends both;
- cookie and storage isolation between sessions, SSRF refusals on a strict instance, and process and profile cleanup on close.

Measured median click→pixel-change latency on loopback: **~180–220 ms** over WebRTC and **~60–90 ms** over WebSocket video, which skips the H.264 encode and the jitter buffer. On real networks WebSocket video is the more fragile of the two: it runs over TCP, so packet loss stalls it, and it uses much more bandwidth while the page is changing (see *Known limitations*).

## Configuration (environment variables)

| Variable | Default | |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | |
| `API_TOKEN` | – | If set, `POST /api/browser-sessions` requires `Authorization: Bearer …`. **Set it in production.** |
| `ALLOWED_ORIGINS` | – | Sites (besides this server) whose pages may connect, i.e. your apps that embed the SDK. Comma list, e.g. `https://app.example.com`. |
| `ALLOWED_URL_PREFIXES` | – (any public page) | Pages the shared browser may show. Comma list of URL prefixes (see *Locking the browser*). |
| `CHROME_PATH` | auto | |
| `CHROME_NO_SANDBOX` | `false` | Needed as root in containers without user namespaces |
| `VIEWPORT_WIDTH` / `VIEWPORT_HEIGHT` | `1280` / `720` | Shared viewport, fixed for the session (must be even) |
| `FPS` / `BITRATE_KBPS` / `KEYFRAME_INTERVAL` | `25` / `2500` / `1s` | Also caps WebSocket video at `FPS` |
| `JPEG_QUALITY` | `80` | Screencast frame quality |
| `ICE_SERVERS` | Google STUN | Given to clients. JSON, e.g. `[{"urls":["turn:turn.example.com:3478"],"username":"u","credential":"p"}]`. The server's own peer connections only use these when `WEBRTC_PUBLIC_IPS` is unset. |
| `WEBRTC_UDP_PORT_MIN/MAX`, `WEBRTC_PUBLIC_IPS` | – | For running behind NAT / in containers. Each WebRTC viewer holds one port from the range. |
| `MAX_SESSIONS` | `10` | |
| `IDLE_TIMEOUT` | `2m` | Close a session when nobody has been connected this long (including after it's created) |
| `MAX_SESSION_LIFETIME` | `4h` | |
| `KEYBOARD_LEASE` / `POINTER_LOCK_TIMEOUT` | `1.5s` / `10s` | Input locks (see below) |
| `MAX_BROWSER_RESTARTS` | `3` | Crash recovery attempts per session |
| `ALLOW_PRIVATE_NETWORKS` | `false` | **Dev only.** Lets the browser reach private/loopback addresses |

## API

| | |
|---|---|
| `POST /api/browser-sessions` `{url, host: {name}, guest: {name}}` | API token. → `201 {sessionId, status, viewport, host, guest}`, where each seat is `{sessionId, participantId, token, role, name}`. |
| `GET /api/browser-sessions/{id}` | Host token or API token. Status, participants, URL, encoder stats. |
| `DELETE /api/browser-sessions/{id}` | Host token or API token. Ends the session for both. |
| `WS /api/browser-sessions/{id}/connect?participantId=…&token=…` | The SDK's connection: input, signaling, video fallback, state. |
| `GET /sdk.js` | The SDK. Served with `Access-Control-Allow-Origin: *`. |
| `GET /api/config` | `{requireApiToken, allowedUrlPrefixes}`, for the demo app. |

The WebSocket protocol is internal to the SDK, but here it is for reference. **Client → server:**
- input: `cursor_move`, `mouse_down`, `mouse_up`, `mouse_click`, `scroll`, `key_down`, `key_up`, `text_input`;
- navigation and session: `navigate`, `back`, `forward`, `reload`, `end_session` (host only);
- signaling: `webrtc_offer`, `webrtc_ice`, `viewport_ready`, `participant_leave`, `ping`;
- video over the WebSocket: `video_ws_start` (the current frame is sent right away), `video_ws_stop`;
- overlay replies: `select_choose`, `select_cancel`, `dialog_response`.

**Server → client:**
- first message: `hello` `{you, role, iceServers, viewport}`;
- session and participants: `session_state`, `participant_joined`, `participant_left`, `session_closed`, `pong`;
- cursors and input: `cursor_update`, `interaction_update`, `interaction_rejected`, `lock_update`;
- browser: `browser_navigation`, `browser_loading`, `browser_loaded`, `browser_error` (`kind: "navigation_blocked"` for the URL lock);
- text fields, for touch keyboards: `fields_update` (positions and keyboard type of visible text fields), `focus_update`;
- signaling: `webrtc_answer`, `webrtc_ice`, `webrtc_failed`;
- overlays: `select_open`, `select_close`, `dialog_open`, `dialog_close`.

Every server message carries `ts`. Messages about a person's action carry `participantId`. The full shape is in `internal/session/protocol.go`. **Binary server messages** are video frames: one complete JPEG each, of the shared viewport. They're sent only after `video_ws_start`, only when the page repaints, and at most `FPS` per second. A connection that can't keep up gets the newest frame next; frames never queue up.

## How simultaneous input is handled

A browser has one pointer and one focused element, so input from both people goes through **one ordered queue per session**. Each event is tagged with who sent it, and runs of mouse moves are coalesced. Two automatic, short-lived locks prevent the cases that would corrupt each other:

- **Keyboard lease:** whoever types holds the keyboard until they've been idle for 1.5 s. Meanwhile, the other person's keystrokes *and clicks* are rejected, so nobody can move focus out from under a typist. The view shows "Alice is typing…" and emits `rejected`.
- **Pointer lock:** held from mouse-down to mouse-up (drags, text selection). The other person's clicks and scrolls are rejected. Their cursor still moves in the overlay, but their moves aren't sent to the page.

Nobody requests control. Outside those windows, clicks and scrolls from both interleave freely.

## Security model

- **SSRF:** Chromium runs with `--proxy-server` pointing at a built-in egress proxy, and `--proxy-bypass-list=<-loopback>` removes Chromium's implicit loopback bypass. The proxy checks the **IP actually being dialed** (`net.Dialer.Control`, after DNS resolution). That covers redirects, sub-resources, `fetch`, WebSockets and DNS-rebinding tricks. It blocks loopback, RFC 1918, CGNAT, link-local (including `169.254.169.254`), ULA, multicast and reserved ranges. Non-proxied WebRTC UDP is disabled in the page.
- **URL lock** (`ALLOWED_URL_PREFIXES`): top-level page requests are paused through CDP `Fetch` and aborted if off the list, including every redirect hop.
- **CDP is never exposed.** chromedp's debugging port binds to 127.0.0.1 inside the server's process space. All control goes through the authenticated WebSocket.
- **Auth:**
  - Creating sessions takes the API token.
  - Each seat has its own random token, compared in constant time.
  - The host's token can also inspect and end the session; the guest's can't.
  - WebSocket `Origin` is checked against this server and `ALLOWED_ORIGINS`.
  - There's a per-connection rate limit (`MAX_EVENTS_PER_SECOND`).
- **Untrusted pages:**
  - downloads are denied;
  - file choosers are intercepted;
  - popups are folded into the shared tab and re-checked;
  - the helper scripts for `<select>` and text fields run in an isolated JS world;
  - text from the page shown in the overlay (dialog messages, option labels) is inserted with `textContent` only;
  - the demo app has a strict CSP.

## Known limitations

- **Video is screencast-based.** Chromium only emits frames when the page repaints. The server repeats the last frame to keep a steady stream, but fast animation tops out around Chromium's screencast rate (~30 fps), and each frame is JPEG-encoded and then decoded before H.264 encoding. If that becomes the bottleneck, switch to capturing a virtual display (Xvfb + GStreamer `ximagesrc`). Only the capture step changes.
- **Native UI isn't in the screencast.** `<select>` popups and `alert`/`confirm`/`prompt`/`beforeunload` are handled through the overlay. File pickers are blocked, with a notice. Other native UI isn't covered: autofill, permission prompts (auto-denied in headless mode), datalist and date-picker popups, and `<select>` inside iframes.
- **One tab.** `window.open` and `target=_blank` load in the shared tab. A popup to an off-list URL is closed and never shown, but it can start loading in the background before it's closed.
- **Touch keyboard edge cases.** Text fields inside iframes, and inside web components' shadow roots, aren't in the field map. Tapping them still focuses them in the page, but the on-screen keyboard only opens if the device allows a keyboard shortly after a tap (Android and Windows usually do; iOS doesn't). The keyboard can cover the lower part of the view; the page doesn't know about it. Test backspace and autocorrect on your actual kiosk device and keyboard app.
- **No audio, no clipboard sync out of the page, no file upload/download.** Paste into the page works.
- **WebRTC waits for a keyframe.** A new WebRTC stream can take up to `KEYFRAME_INTERVAL` (1 s) to show a picture; WebSocket video covers the gap.
- **WebSocket video bandwidth.** An idle page costs nothing; WebRTC sends ~180 kbps of repeated frames. A page that keeps changing costs one full JPEG per repaint, for example ~63 KB for example.com at 1280×720, quality 80. At the 25 fps cap that's roughly 12 Mbps per viewer while it lasts, against WebRTC's ≤ 3 Mbps. Slow connections get fewer frames, not a growing delay. Networks that block WebSockets break the whole app, not just video.
- **Scale:** every session costs one Chromium plus one ffmpeg. Measured: ~20–25 % of one core with two viewers, and ~1.3 GB summed RSS across Chromium's processes (shared pages are counted more than once, so real usage is lower). Each WebRTC viewer holds one UDP port from `WEBRTC_UDP_PORT_MIN/MAX` (101 ports in the Docker setup); viewers beyond that stay on WebSocket video.
- **Bot detection.** Headless Chromium on datacenter IPs gets more CAPTCHAs. DRM video (Widevine) won't play.

## Production checklist

- Set `API_TOKEN`, `ALLOWED_ORIGINS` and `ALLOWED_URL_PREFIXES`.
- Run each session (or each node) in a container with CPU and memory limits, and a NetworkPolicy that allows egress **only** through the proxy, as defense in depth beyond the in-process proxy.
- Networks that block UDP already work, through WebSocket video. For WebRTC quality on those networks too, run a TURN server (coturn) with TLS on port 443 and pass it in `ICE_SERVERS`.
- Serve over HTTPS/WSS (the `https` compose profile does this).
- Pin and patch Chromium; it renders untrusted content. Keep the Chromium sandbox on (don't set `CHROME_NO_SANDBOX`) when the container allows it.
- To scale horizontally: sessions live in-process, so route `/api/browser-sessions/{id}/*` to the node that owns the session (sticky routing by session ID).

## Layout

```
cmd/server/            entry point
internal/api/          REST + WebSocket handlers
internal/session/      sessions, seats, input arbitration, browser control (chromedp), URL lock
internal/media/        screencast → ffmpeg → RTP → shared WebRTC track
internal/netguard/     URL validation, egress proxy (SSRF protection), URL allowlist
web/sdk.js             the SDK your pages load (CollabBrowser.mount)
web/                   demo app built on the SDK, embedded into the binary
tests/e2e/             Playwright acceptance suite + test site (incl. a partner-site embed page)
```
# shared-browser-canvas-overlay-pion-webrtc
