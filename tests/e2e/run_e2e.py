"""End-to-end acceptance tests for the collaborative browser.

Starts the Go server (twice: a dev instance that may reach the local test
site and only allows it as a destination, and a strict instance with the
default network policy), serves a test site that reports its own state,
and drives real Chrome participants with Playwright:
  - Alice, the host, in a large desktop window (mouse);
  - Bob, the guest, in a small touch-capable window (the kiosk);
  - a partner site on another origin that mounts both views with the SDK.

Usage:  python3 tests/e2e/run_e2e.py [path/to/collab-browser-binary]
"""

import asyncio
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright

ROOT = Path(__file__).resolve().parents[2]
SITE_DIR = Path(__file__).resolve().parent / "site"
BIN = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "bin" / "collab-browser"
CHROME = os.environ.get("CLIENT_CHROME", shutil.which("google-chrome") or "")
SITE_PORT, DEV_PORT, STRICT_PORT = 8765, 8080, 8081
SITE = f"http://127.0.0.1:{SITE_PORT}"
OFF_LIST = f"http://localhost:{SITE_PORT}"  # same site, but not on the allowed list
VW, VH = 1280, 720

reports = []
reports_lock = threading.Lock()


class SiteHandler(SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=str(SITE_DIR), **kw)

    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path.startswith("/redirect?"):
            self.send_response(302)
            self.send_header("Location", parse_qs(urlparse(self.path).query).get("to", ["/"])[0])
            self.end_headers()
            return
        super().do_GET()

    def do_POST(self):
        if self.path == "/report":
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            with reports_lock:
                reports.append(json.loads(body or b"{}"))
            self.send_response(204)
            self.end_headers()
            return
        self.send_response(404)
        self.end_headers()


def last_report(page=None, tag=None):
    """Newest report for `page`. Index pages of extra sessions carry a ?tag;
    by default only the main session's (untagged) reports count."""
    with reports_lock:
        for r in reversed(reports):
            if (page is None or r.get("page") == page) and r.get("tag") == tag:
                return r
    return None


def http(method, url, body=None, token=None):
    req = urllib.request.Request(url, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def start_server(port, allow_private, extra=None):
    env = dict(os.environ,
               LISTEN_ADDR=f"127.0.0.1:{port}",
               CHROME_NO_SANDBOX="true",
               ALLOW_PRIVATE_NETWORKS="true" if allow_private else "false",
               IDLE_TIMEOUT="20s",
               ICE_SERVERS="[]",
               # Advertise the real local address (the Docker image sets 127.0.0.1
               # for port publishing, which doesn't work inside one container).
               WEBRTC_PUBLIC_IPS="",
               **(extra or {}))
    log = open(f"/tmp/collab-{port}.log", "w")
    p = subprocess.Popen([str(BIN)], env=env, stdout=log, stderr=subprocess.STDOUT)
    for _ in range(50):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1)
            return p
        except Exception:
            time.sleep(0.1)
    raise RuntimeError("server did not start")


async def wait_for(pred, timeout=10.0, interval=0.05, what="condition"):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        v = pred()
        if asyncio.iscoroutine(v):
            v = await v
        if v:
            return v
        await asyncio.sleep(interval)
    raise AssertionError(f"timed out waiting for {what}")


def chrome_count():
    out = subprocess.run(["pgrep", "-fc", "collab-session-"], capture_output=True, text=True).stdout.strip()
    return int(out or 0)


class Client:
    """One mounted view. `root` is the JS expression for the view's debug
    object: the demo app exposes it as window.__collab."""

    def __init__(self, page, name, root="window.__collab"):
        self.page, self.name, self.root = page, name, root

    async def js(self, body, arg=None):
        return await self.page.evaluate(f"(arg) => {{ const R = {self.root}; {body} }}", arg)

    async def to_screen(self, sx, sy):
        return await self.js("""const [sx, sy, vw, vh] = arg; const r = R.els.stage.getBoundingClientRect();
              const s = Math.min(r.width / vw, r.height / vh), w = vw * s, h = vh * s;
              return [r.left + (r.width - w) / 2 + (sx + 0.5) * s, r.top + (r.height - h) / 2 + (sy + 0.5) * s];""",
                             [sx, sy, VW, VH])

    async def click(self, sx, sy):
        x, y = await self.to_screen(sx, sy)
        await self.page.mouse.click(x, y)

    async def tap(self, sx, sy):
        x, y = await self.to_screen(sx, sy)
        await self.page.touchscreen.tap(x, y)

    async def move(self, sx, sy):
        x, y = await self.to_screen(sx, sy)
        await self.page.mouse.move(x, y, steps=4)

    async def pixel(self, sx, sy):
        """A server-viewport pixel from whichever surface is on screen
        (the WebRTC <video> or the WebSocket <canvas>)."""
        return await self.js("""const [sx, sy, vw, vh] = arg; const t = R.diag.transport;
              if (t === 'none') return null;
              const v = t === 'ws' ? R.els.screen : R.els.video;
              const w = t === 'ws' ? v.width : v.videoWidth, h = t === 'ws' ? v.height : v.videoHeight;
              if (!w) return null;
              const c = document.createElement('canvas'); c.width = w; c.height = h;
              const g = c.getContext('2d'); g.drawImage(v, 0, 0);
              const d = g.getImageData(Math.round(sx * w / vw), Math.round(sy * h / vh), 1, 1).data;
              return [d[0], d[1], d[2]];""", [sx, sy, VW, VH])

    async def state(self, expr):
        return await self.js(f"const s = R.state, d = R.diag; return {expr};")

    async def frames(self):
        return await self.state("d.framesRendered")


def is_green(px):
    return px is not None and px[1] > 150 and px[0] < 90 and px[2] < 90


def is_white(px):
    return px is not None and min(px) > 220


def is_blue(px):
    return px is not None and px[2] > 150 and px[0] < 80 and px[1] < 80


def same_color(p, q):
    return (is_white(p) and is_white(q)) or (is_green(p) and is_green(q))


def fmt_stats(s):
    return f"{s['transport']} {s['fps']:.0f} fps, {s['kbps']:.0f} kbps" if s else "n/a"


def create(base, url, host="Alice", guest="Bob"):
    return http("POST", f"{base}/api/browser-sessions", {"url": url, "host": {"name": host}, "guest": {"name": guest}})


async def open_client(browser, base, seat, name, size, transport="rtc", touch=False):
    ctx = await browser.new_context(viewport=size, has_touch=touch)
    return await open_client_in(ctx, base, seat, name, transport)


async def open_client_in(ctx, base, seat, name, transport="rtc"):
    """Opens the demo app with a seat, the way a guest link does."""
    page = await ctx.new_page()
    page.on("pageerror", lambda e: print(f"  [{name} pageerror] {e}"))
    await page.goto(f"{base}/?s={seat['sessionId']}#p={seat['participantId']}&t={seat['token']}")
    c = Client(page, name)
    await wait_for(lambda: c.frames(), timeout=15, what=f"{name} video frames")
    # Every client starts on WebSocket frames and upgrades to WebRTC.
    await wait_for(lambda: c.state(f"d.transport === '{transport}'"), timeout=10, what=f"{name} on {transport} video")
    await wait_for(lambda: c.pixel(900, 600), timeout=10, what=f"{name} decodable video")
    return c


results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""))


async def main():
    if not BIN.exists():
        sys.exit(f"server binary not found: {BIN}")
    site = ThreadingHTTPServer(("127.0.0.1", SITE_PORT), SiteHandler)
    threading.Thread(target=site.serve_forever, daemon=True).start()
    dev = start_server(DEV_PORT, allow_private=True, extra={
        "ALLOWED_URL_PREFIXES": f"{SITE}/",  # the shared browser may only show the test site
        "ALLOWED_ORIGINS": SITE,             # the partner page (embed.html) lives there
    })
    strict = start_server(STRICT_PORT, allow_private=False)
    base = f"http://127.0.0.1:{DEV_PORT}"
    site_url = f"{SITE}/index.html"
    try:
        async with async_playwright() as pw:
            browser = await pw.chromium.launch(
                executable_path=CHROME or None,
                args=["--no-sandbox", "--autoplay-policy=no-user-gesture-required"])

            print("\n1. Create session (API)")
            st, r = create(base, site_url + "?set=1")
            host, guest = r.get("host", {}), r.get("guest", {})
            check("POST /api/browser-sessions returns a host seat and a guest seat",
                  st == 201 and host.get("role") == "host" and guest.get("role") == "guest" and host.get("token") and guest.get("token"),
                  f"status={r.get('status')}")
            sid, owner = r["sessionId"], host["token"]
            await wait_for(lambda: last_report("index"), timeout=20, what="server browser loaded test page")
            st, _ = http("GET", f"{base}/api/browser-sessions/{sid}/connect?participantId={guest['participantId']}&token=wrong")
            check("connecting with a wrong token is refused", st == 401, f"status={st}")
            st, r_ = create(base, f"{OFF_LIST}/index.html")
            check("starting a session on a URL that isn't allowed is refused", st == 400, r_.get("error", ""))

            print("\n2. Host (desktop) and guest (touch kiosk) open their seats")
            a = await open_client(browser, base, host, "Alice", {"width": 1400, "height": 900})
            b = await open_client(browser, base, guest, "Bob", {"width": 760, "height": 620}, touch=True)
            pa, pb = await a.pixel(900, 600), await b.pixel(900, 600)
            check("both see the same page (white background)", is_white(pa) and is_white(pb), f"A={pa} B={pb}")
            n = await a.state("[...s.participants.values()].filter(p => p.connected).length")
            roles = [await a.state("s.role"), await b.state("s.role")]
            check("both are connected, as host and guest", n == 2 and roles == ["host", "guest"], f"connected={n} roles={roles}")

            print("\n3. Click from the small window; both see the result")
            await b.js("""window.__greenAt = null; const v = R.els.video;
              const c = document.createElement('canvas'); const g = c.getContext('2d', { willReadFrequently: true });
              const tick = () => { if (v.videoWidth) { c.width = 8; c.height = 8; g.drawImage(v, 900*v.videoWidth/1280, 600*v.videoHeight/720, 4, 4, 0, 0, 8, 8);
                const d = g.getImageData(4, 4, 1, 1).data; if (d[1] > 150 && d[0] < 90) { window.__greenAt = Date.now(); return; } }
                v.requestVideoFrameCallback(tick); }; v.requestVideoFrameCallback(tick);""")
            t0 = time.time() * 1000
            await b.click(200, 130)  # button centre in server coordinates
            await wait_for(lambda: (last_report("index") or {}).get("count") == 1, what="click registered")
            check("click in Bob's 760px window hit the button", True, "count=1")
            await wait_for(lambda: b.page.evaluate("window.__greenAt"), timeout=5, what="green in Bob's stream")
            green_at = await b.page.evaluate("window.__greenAt")
            await wait_for(lambda: _green(a), timeout=5, what="green in Alice's stream")
            check("Alice's stream shows the change", True)
            check("Bob's stream shows the change", True, f"click→pixel latency {green_at - t0:.0f} ms")
            latency_ms = green_at - t0

            await a.click(200, 130)
            await wait_for(lambda: (last_report("index") or {}).get("count") == 2, what="second click")
            await wait_for(lambda: _white(b), timeout=5, what="white in Bob's stream")
            check("click from Alice's 1400px window hits the same button", True, "count=2")

            print("\n3b. Interaction latency (click in Alice → pixel change in Bob's decoded video)")
            samples = []
            for i in range(8):
                want_green = (await _count()) % 2 == 0  # next click toggles
                await b.js("""window.__hitAt = null; const v = R.els.video;
                  const c = document.createElement('canvas'); c.width = 4; c.height = 4; const g = c.getContext('2d', { willReadFrequently: true });
                  const tick = () => { g.drawImage(v, 900*v.videoWidth/1280, 600*v.videoHeight/720, 4, 4, 0, 0, 4, 4);
                    const d = g.getImageData(2, 2, 1, 1).data; const green = d[1] > 150 && d[0] < 90;
                    if (green === arg) { window.__hitAt = Date.now(); return; } v.requestVideoFrameCallback(tick); };
                  v.requestVideoFrameCallback(tick);""", want_green)
                t0 = time.time() * 1000
                await a.click(200, 130)
                await wait_for(lambda: b.page.evaluate("window.__hitAt"), timeout=5, what="latency sample")
                samples.append(await b.page.evaluate("window.__hitAt") - t0)
                await asyncio.sleep(0.25)
            samples.sort()
            median = samples[len(samples) // 2]
            latency_ms = median
            check("median click→video latency under 300 ms (local)", median < 300,
                  f"median {median:.0f} ms, min {samples[0]:.0f}, max {samples[-1]:.0f}")

            print("\n3c. Turning WebRTC off (video over the WebSocket)")
            await asyncio.sleep(2.2)  # let stats settle on an idle page
            idle_rtc = await a.state("d.stats")
            await a.page.click("#btnVideo")
            await wait_for(lambda: a.state("d.transport === 'ws'"), timeout=5, what="Alice on WebSocket video")
            label = await a.page.inner_text("#btnVideo")
            check("the button switches Alice to WebSocket video", label == "WebRTC: off", f"button says {label!r}")
            pa, pb = await a.pixel(900, 600), await b.pixel(900, 600)
            check("Alice's WebSocket picture matches Bob's WebRTC picture", same_color(pa, pb), f"A={pa} B={pb}")
            ws_samples = []
            for i in range(8):
                want_green = (await _count()) % 2 == 0
                await a.js("""window.__hitAt = null; const g = R.els.screen.getContext('2d');
                  const tick = () => { const d = g.getImageData(900, 600, 1, 1).data;
                    if ((d[1] > 150 && d[0] < 90) === arg) { window.__hitAt = Date.now(); return; } requestAnimationFrame(tick); };
                  requestAnimationFrame(tick);""", want_green)
                t0 = time.time() * 1000
                await b.click(200, 130)
                await wait_for(lambda: a.page.evaluate("window.__hitAt"), timeout=5, what="WebSocket latency sample")
                ws_samples.append(await a.page.evaluate("window.__hitAt") - t0)
                await asyncio.sleep(0.25)
            ws_samples.sort()
            ws_median = ws_samples[len(ws_samples) // 2]
            check("Bob's clicks show up in Alice's WebSocket video", True,
                  f"median {ws_median:.0f} ms, min {ws_samples[0]:.0f}, max {ws_samples[-1]:.0f} (WebRTC median {median:.0f} ms)")
            await asyncio.sleep(2.2)
            idle_ws = await a.state("d.stats")
            check("an idle page costs nothing over the WebSocket", idle_ws and idle_ws["transport"] == "ws" and idle_ws["kbps"] < 50,
                  f"idle: {fmt_stats(idle_ws)} vs {fmt_stats(idle_rtc)}")
            await a.page.click("#btnVideo")
            await wait_for(lambda: a.state("d.transport === 'rtc'"), timeout=10, what="Alice back on WebRTC")
            check("pressing it again switches Alice back to WebRTC", await a.page.inner_text("#btnVideo") == "WebRTC: on")

            print("\n3d. A participant whose WebRTC can't get through (e.g. UDP blocked)")
            st, rb = create(base, site_url + "?tag=b", host="Carol")
            # No ICE candidates cross in either direction, so connectivity
            # checks never happen: the same outcome as a firewall dropping UDP.
            blocked = await browser.new_context(viewport={"width": 1000, "height": 700})
            await blocked.add_init_script("""(() => {
              const Orig = window.RTCPeerConnection;
              window.RTCPeerConnection = class extends Orig {
                addIceCandidate() { return Promise.resolve(); }
                setRemoteDescription(d) { return super.setRemoteDescription({ type: d.type, sdp: d.sdp.replace(/^a=candidate:.*\\r?\\n/gm, '') }); }
                set onicecandidate(fn) {}
                get onicecandidate() { return null; }
              };
            })()""")
            await wait_for(lambda: last_report("index", tag="b"), timeout=20, what="session B loaded")
            cc = await open_client_in(blocked, base, rb["host"], "Carol", transport="ws")
            await asyncio.sleep(6)  # WebRTC gets a chance to connect (it shouldn't)
            t_, rtc_ = await cc.state("[d.transport, d.rtcState]")
            check("Carol's WebRTC can't connect, so she stays on WebSocket video", t_ == "ws" and rtc_ != "connected",
                  f"transport={t_} rtcState={rtc_}")
            check("Carol sees the page", is_white(await cc.pixel(900, 600)))
            await cc.click(200, 130)
            await wait_for(lambda: (last_report("index", tag="b") or {}).get("count") == 1, what="Carol's click")
            await wait_for(lambda: _px_is(cc, is_green), timeout=5, what="result in Carol's WebSocket video")
            check("Carol's click lands and she sees the result", True)
            await blocked.close()
            http("DELETE", f"{base}/api/browser-sessions/{rb['sessionId']}", token=rb["host"]["token"])

            print("\n4. Typing")
            await a.click(400, 290)
            await a.page.keyboard.type("Hello team", delay=15)
            await wait_for(lambda: (last_report("index") or {}).get("name") == "Hello team", what="typed text")
            check("Alice's typing reached the input", True, "'Hello team'")
            await a.page.keyboard.press("Backspace")
            await a.page.keyboard.press("Shift+A")
            await wait_for(lambda: (last_report("index") or {}).get("name") == "Hello teaA", what="special keys")
            check("Backspace and Shift+key work", True, "'Hello teaA'")
            before = await b.state("d.rejections")
            count_before = await _count()
            await a.page.keyboard.type("!", delay=0)
            await b.click(200, 130)
            await wait_for(lambda: _gt(b.state("d.rejections"), before), timeout=3, what="lock rejection")
            r_ = last_report("index") or {}
            check("Bob's click during Alice's typing is rejected (keyboard lease)", r_.get("count") == count_before, f"count stayed {r_.get('count')}")
            await asyncio.sleep(1.7)  # lease expires
            await b.click(400, 290)
            await b.page.keyboard.type("?", delay=10)
            await wait_for(lambda: (last_report("index") or {}).get("name", "").endswith("?"), what="Bob typing after lease")
            check("after the lease expires Bob can type", True, repr(last_report('index')['name']))

            print("\n4b. Kiosk on-screen keyboard (Bob taps)")
            await asyncio.sleep(1.7)  # Bob's keyboard lease from step 4 lapses
            kb = "[R.kbFocused(), R.els.kb.inputMode]"
            await b.tap(900, 500)  # empty area
            await wait_for(lambda: b.js(f"return !R.kbFocused();"), timeout=3, what="keyboard closed")
            await b.tap(400, 290)  # the name field
            st_ = await wait_for(lambda: b.js(f"const v = {kb}; return v[0] && v;"), timeout=3, what="keyboard for the name field")
            check("tapping a text field opens the on-screen keyboard", st_[1] == "text", f"inputmode={st_[1]}")
            await b.page.keyboard.type("Z", delay=10)
            await wait_for(lambda: (last_report("index") or {}).get("name", "").endswith("Z"), what="typing after tap")
            check("what's typed on it reaches the field", True, repr(last_report('index')['name']))
            await b.tap(900, 290)  # the phone field (type=tel)
            st_ = await wait_for(lambda: b.js(f"const v = {kb}; return v[0] && v[1] === 'tel' && v;"), timeout=3, what="phone keypad")
            check("a phone field gets the phone keypad", True, f"inputmode={st_[1]}")
            await b.tap(900, 500)
            await wait_for(lambda: b.js("return !R.kbFocused();"), timeout=3, what="keyboard closed after tapping elsewhere")
            check("tapping outside a field closes the keyboard", True)
            await b.tap(400, 290)
            await wait_for(lambda: b.js("return R.kbFocused();"), timeout=3, what="keyboard open again")
            await asyncio.sleep(1.7)  # Bob's keyboard lease from typing lapses
            await a.click(200, 130)  # the host clicks a button: focus leaves the field
            await wait_for(lambda: b.js("return !R.kbFocused();"), timeout=3, what="keyboard closed when focus left the field")
            check("the keyboard closes when the page's focus leaves the field", True)

            print("\n5. Cursors")
            await a.move(640, 360)
            await wait_for(lambda: b.state(f"(() => {{ const c = s.cursors.get([...s.participants.values()].find(p => p.name === 'Alice').id); return c && Math.abs(c.tx - 0.5) < 0.01 && Math.abs(c.ty - 0.5) < 0.01; }})()"), timeout=5, what="Alice cursor on Bob")
            ac = await b.state("(() => { const id = [...s.participants.values()].find(p => p.name === 'Alice').id; const c = s.cursors.get(id); return [c.tx, c.ty]; })()")
            check("Bob sees Alice's cursor at the same page point", True, f"normalized {ac[0]:.3f},{ac[1]:.3f} (expected 0.5,0.5)")
            await b.move(100, 100)
            await wait_for(lambda: a.state("(() => { const id = [...s.participants.values()].find(p => p.name === 'Bob').id; const c = s.cursors.get(id); return c && Math.abs(c.tx*1280 - 100) < 3 && Math.abs(c.ty*720 - 100) < 3; })()"), timeout=5, what="Bob cursor on Alice")
            check("Alice sees Bob's cursor at the same page point", True, "≈(100,100) server px")

            print("\n6. Scrolling")
            await a.move(900, 500)
            await a.page.mouse.wheel(0, 600)
            await wait_for(lambda: (last_report("index") or {}).get("scrollY", 0) > 300, what="scroll")
            check("Alice scrolls the page", True, f"scrollY={last_report('index')['scrollY']}")
            x, y = await b.to_screen(900, 500)
            await b.page.mouse.move(x, y)
            await b.page.mouse.wheel(0, -2000)
            await wait_for(lambda: (last_report("index") or {}).get("scrollY", 1) == 0, what="scroll back")
            check("Bob scrolls it back", True, "scrollY=0")

            print("\n7. Dropdown (select)")
            await b.click(210, 380)
            await wait_for(lambda: a.state("!!s.select"), timeout=5, what="select overlay on Alice")
            await wait_for(lambda: b.state("!!s.select"), timeout=5, what="select overlay on Bob")
            check("both see the dropdown options in the overlay", True)
            await a.page.click(".select-list button:nth-of-type(3)")
            await wait_for(lambda: (last_report("index") or {}).get("sel") == "Blue", what="select change")
            await wait_for(lambda: b.state("!s.select"), timeout=3, what="overlay closes")
            check("Alice picks an option Bob opened", True, "sel=Blue")

            print("\n8. Page dialogs")
            await asyncio.sleep(1.6)
            await a.click(500, 130)
            await wait_for(lambda: b.state("!!s.dialog"), timeout=5, what="dialog on Bob")
            check("confirm() dialog shown to both", await a.state("!!s.dialog"))
            await b.page.click(".dialog button.primary")
            await wait_for(lambda: (last_report("index") or {}).get("dialog") is True, what="dialog accepted")
            check("Bob answers it", True, "confirm() returned true")

            print("\n9. Navigation")
            await b.page.fill("#urlInput", f"{SITE}/page2.html")
            await b.page.press("#urlInput", "Enter")
            await wait_for(lambda: last_report() and last_report()["page"] == "page2", what="page2 loaded")
            await wait_for(lambda: a.state("s.url.endsWith('page2.html')"), timeout=5, what="Alice URL bar")
            await wait_for(lambda: _blue(a), timeout=5, what="page2 in Alice's stream")
            check("Bob navigates; Alice's address bar and video follow", True)
            await a.page.click("#btnBack")
            await wait_for(lambda: a.state("s.url.includes('index.html')"), timeout=8, what="back")
            await wait_for(lambda: b.state("s.url.includes('index.html')"), timeout=8, what="back on Bob")
            check("Alice goes Back; both return to page 1", True)
            await b.page.click("#btnForward")
            await wait_for(lambda: a.state("s.url.endsWith('page2.html')"), timeout=8, what="forward")
            check("Bob goes Forward", True)
            await a.page.click("#btnBack")
            await wait_for(lambda: a.state("s.url.includes('index.html')"), timeout=8, what="back again")
            await a.click(100 + 60, 440 + 14)  # the link
            await wait_for(lambda: a.state("s.url.endsWith('page2.html')"), timeout=8, what="link navigation")
            check("clicking a link navigates everyone", True)
            st, r_ = http("POST", f"{base}/api/browser-sessions", {"url": "file:///etc/passwd"})
            check("non-http URLs are refused", st == 400, r_.get("error", ""))

            print("\n9b. Locked to the allowed URLs")
            await b.page.fill("#urlInput", f"{SITE}/lock.html")
            await b.page.press("#urlInput", "Enter")
            await wait_for(lambda: a.state("s.url.endsWith('lock.html') && !s.loading"), timeout=8, what="lock page")
            await asyncio.sleep(0.5)

            async def blocked_click(sy, label, check_no_load=True):
                n0 = await b.state("d.messages.browser_error || 0")
                with reports_lock:
                    seen = len(reports)
                await a.click(350, sy)
                await wait_for(lambda: _gt(b.state("d.messages.browser_error || 0"), n0), timeout=5, what=f"{label} blocked")
                await asyncio.sleep(1.0)
                url = await a.state("s.url")
                with reports_lock:
                    loaded = any(rp.get("page") == "page2" for rp in reports[seen:])
                notice = await b.js("return R.els.notice.hidden ? '' : R.els.notice.textContent;")
                check(f"{label}: blocked, the page stays put", url.endswith("lock.html") and (not loaded or not check_no_load),
                      f"url={url.rsplit('/', 1)[-1]}, notice={notice!r}")

            await blocked_click(125, "a link to another site")
            await blocked_click(225, "an allowed URL that redirects to another site")
            # A popup is closed and never shown; it can start loading in the
            # background before it's closed, so only the shared tab is checked.
            await blocked_click(325, "a popup to another site", check_no_load=False)
            check("the address bar never showed the blocked popup's URL", not await a.state("s.url.includes('localhost')"))
            await a.click(350, 425)
            await wait_for(lambda: a.state("s.url.endsWith('page2.html')"), timeout=8, what="allowed link")
            check("links within the allowed site still work", True)
            n0 = await b.state("d.messages.browser_error || 0")
            await b.page.fill("#urlInput", f"{OFF_LIST}/index.html")
            await b.page.press("#urlInput", "Enter")
            await wait_for(lambda: _gt(b.state("d.messages.browser_error || 0"), n0), timeout=5, what="address bar blocked")
            check("typing another site in the address bar is blocked", await a.state("s.url.endsWith('page2.html')"))

            print("\n10. Reconnect")
            a_id = await a.state("s.me.participantId")
            await a.page.reload()
            await wait_for(lambda: a.frames(), timeout=15, what="Alice video after reload")
            a_id2 = await a.state("s.me.participantId")
            check("Alice reloads and is back in her seat", a_id == a_id2, a_id2)
            check("the shared page survived (still on page 2)", await a.state("s.url.endsWith('page2.html')"))
            await a.page.context.set_offline(True)
            await asyncio.sleep(1.5)
            await a.page.context.set_offline(False)
            await wait_for(lambda: a.state("s.connected"), timeout=15, what="ws reconnect")
            f0 = await a.frames()
            await b.page.click("#btnReload")  # make the page repaint
            await wait_for(lambda: _gt(a.frames(), f0 + 2), timeout=15, what="frames after reconnect")
            check("after a network drop Alice reconnects automatically", True)

            print("\n11. Isolation between sessions")
            st, r2 = create(base, site_url)
            sid2 = r2["sessionId"]
            await wait_for(lambda: any(rp.get("page") == "index" and rp.get("cookie") == "" for rp in reports[-5:]), timeout=20, what="session 2 report")
            r_iso = [rp for rp in reports if rp.get("page") == "index"][-1]
            check("a second session has its own cookies and storage", r_iso.get("cookie") == "" and r_iso.get("ls") is None,
                  f"cookie={r_iso.get('cookie')!r} ls={r_iso.get('ls')!r} (session 1 has k=session-one)")
            st, _ = http("GET", f"{base}/api/browser-sessions/{sid2}", token=owner)
            check("session 1's host token cannot manage session 2", st == 403, f"status={st}")
            http("DELETE", f"{base}/api/browser-sessions/{sid2}", token=r2["host"]["token"])

            print("\n12. Network policy (strict server)")
            sbase = f"http://127.0.0.1:{STRICT_PORT}"
            for target in ["http://127.0.0.1:8765/", "http://localhost/", "http://169.254.169.254/latest/meta-data/",
                           "http://10.0.0.5/", "http://[::1]/", "http://0x7f000001/", "http://192.168.1.1/"]:
                st, r_ = http("POST", f"{sbase}/api/browser-sessions", {"url": target})
                check(f"refused: {target}", st == 400, r_.get("error", "")[:60])

            print("\n12b. Click precision")
            await b.page.fill("#urlInput", f"{SITE}/precision.html")
            await b.page.press("#urlInput", "Enter")
            await wait_for(lambda: a.state("s.url.endsWith('precision.html') && !s.loading"), timeout=8, what="precision page")
            await asyncio.sleep(0.5)
            for c in (a, b):
                worst = 0.0
                for (px, py) in [(40, 40), (640, 360), (1239, 679), (200, 600), (1100, 120)]:
                    prev = (last_report("precision") or {}).get("n")
                    await c.click(px, py)  # centre of server pixel (px, py)
                    await wait_for(lambda: (last_report("precision") or {}).get("n") != prev, what="precision click")
                    r_ = last_report("precision")
                    worst = max(worst, abs(r_["x"] - (px + 0.5)), abs(r_["y"] - (py + 0.5)))
                    await asyncio.sleep(0.1)
                check(f"{c.name}'s clicks land on the exact page pixel", worst < 0.05, f"worst error {worst:.2f} px")

            print("\n12c. The SDK on a partner site (another origin, strict CSP, two views on one page)")
            st, rc = create(base, site_url + "?tag=c", host="Desk", guest="Kiosk")
            await wait_for(lambda: last_report("index", tag="c"), timeout=20, what="session C loaded")
            seat = lambda s: f"{s['participantId']}.{s['token']}"
            pctx = await browser.new_context(viewport={"width": 1400, "height": 900})
            pp = await pctx.new_page()
            pp.on("pageerror", lambda e: print(f"  [partner pageerror] {e}"))
            await pp.goto(f"{SITE}/embed.html#server={base}&session={rc['sessionId']}&host={seat(rc['host'])}&guest={seat(rc['guest'])}")
            ph, pg = Client(pp, "partner host", "window.views.host.debug"), Client(pp, "partner guest", "window.views.guest.debug")
            await wait_for(lambda: pp.evaluate("!!window.views"), timeout=10, what="SDK loaded")
            await wait_for(lambda: _both(ph.frames(), pg.frames()), timeout=15, what="both partner views showing video")
            csp = await pp.evaluate("""() => { const d = document.createElement('div'); d.setAttribute('style', 'color: rgb(255, 0, 0)');
              document.body.append(d); const c = getComputedStyle(d).color; d.remove(); return c; }""")
            check("the partner page's CSP really blocks inline styles", csp != "rgb(255, 0, 0)", f"inline style color → {csp}")
            w_ = await ph.js("return Math.round(R.els.video.getBoundingClientRect().width);")
            check("both views render despite the page's CSP and hostile CSS", w_ > 600 and is_white(await ph.pixel(900, 600)) and is_white(await pg.pixel(900, 600)),
                  f"view width {w_}px")
            await pg.click(200, 130)
            await wait_for(lambda: (last_report("index", tag="c") or {}).get("count") == 1, what="click in the guest view")
            await wait_for(lambda: _both(_px_is(ph, is_green), _px_is(pg, is_green)), timeout=5, what="both partner views green")
            check("a click in the guest view shows up in both views", True)
            ok_guest = await pp.evaluate("window.views.guest.end()")
            await asyncio.sleep(1)
            check("the guest can't end the session", ok_guest is False and await pp.evaluate("window.ended.length") == 0)
            await pp.evaluate("window.views.host.end()")
            await wait_for(lambda: pp.evaluate("window.ended.length === 2"), timeout=5, what="both views ended")
            check("the host's end() ends it for both views", True, str(await pp.evaluate("window.ended")))
            await pctx.close()

            print("\n12d. A session sized to the kiosk's screen")
            st, rd = http("POST", f"{base}/api/browser-sessions", {"url": site_url + "?tag=d", "viewport": {"width": 900, "height": 1200}})
            check("the session takes the requested viewport", st == 201 and rd.get("viewport") == {"width": 900, "height": 1200}, str(rd.get("viewport")))
            await wait_for(lambda: last_report("index", tag="d"), timeout=20, what="session D loaded")
            dctx = await browser.new_context(viewport={"width": 700, "height": 900})
            dk = await open_client_in(dctx, base, rd["guest"], "Tall kiosk", transport="rtc")
            dims = await dk.js("const v = R.els.video; return [v.videoWidth, v.videoHeight, R.state.viewport.width, R.state.viewport.height];")
            check("WebRTC video plays at that size (H.264 level picked to fit)", dims[:2] == [900, 1200] and dims[2:] == [900, 1200], f"video {dims[0]}x{dims[1]}")
            await dctx.close()
            http("DELETE", f"{base}/api/browser-sessions/{rd['sessionId']}", token=rd["host"]["token"])

            print("\n12e. Stream isolation: 4 sessions at once, each view only ever shows its own")
            palette = {"ff0000": (255, 0, 0), "00c800": (0, 200, 0), "0000ff": (0, 0, 255), "ff00ff": (255, 0, 255)}
            iso = []
            for hexc in palette:
                st, ri = http("POST", f"{base}/api/browser-sessions", {"url": f"{SITE}/color.html?c={hexc}"})
                iso.append((hexc, ri))
            for hexc, ri in iso:
                await wait_for(lambda: any(r.get("page") == "color" and r.get("tag") == f"?c={hexc}" for r in reports), timeout=20, what=f"{hexc} loaded")
            views = []  # (color, transport, client)
            for hexc, ri in iso:
                ictx = await browser.new_context(viewport={"width": 640, "height": 420})
                views.append((hexc, "rtc", await open_client_in(ictx, base, ri["host"], f"{hexc} host", transport="rtc")))
                ictx2 = await browser.new_context(viewport={"width": 640, "height": 420})
                g = await open_client_in(ictx2, base, ri["guest"], f"{hexc} guest", transport="rtc")
                await g.page.click("#btnVideo")  # this one on WebSocket video
                await wait_for(lambda: g.state("d.transport === 'ws'"), timeout=5, what="guest on ws")
                views.append((hexc, "ws", g))

            def which(px):
                for name, rgb in palette.items():
                    if px and all(abs(px[i] - rgb[i]) < 60 for i in range(3)):
                        return name
                return None

            wrong, samples = [], 0
            for _ in range(25):  # 5 seconds
                for hexc, transport, c in views:
                    for (sx, sy) in [(100, 100), (640, 360), (1180, 620)]:
                        got = which(await c.pixel(sx, sy))
                        samples += 1
                        if got != hexc:
                            wrong.append((hexc, transport, got))
                await asyncio.sleep(0.2)
            check("8 views of 4 concurrent sessions never show another session's picture", not wrong,
                  f"{samples} samples over WebRTC and WebSocket, {len(wrong)} wrong" + (f": {wrong[:5]}" if wrong else ""))
            for _, _, c in views:
                await c.page.context.close()
            for _, ri in iso:
                http("DELETE", f"{base}/api/browser-sessions/{ri['sessionId']}", token=ri["host"]["token"])

            print("\n13. Close session")
            procs_before = chrome_count()
            st, _ = http("DELETE", f"{base}/api/browser-sessions/{sid}", token=guest["token"])
            check("the guest's token can't end the session", st == 403, f"status={st}")
            got_closed = asyncio.ensure_future(wait_for(lambda: b.state("s.ended"), timeout=10, what="session_closed"))
            st, _ = http("DELETE", f"{base}/api/browser-sessions/{sid}", token=owner)
            await got_closed
            await asyncio.sleep(1.5)
            procs_after = chrome_count()
            st2, _ = http("GET", f"{base}/api/browser-sessions/{sid}", token=owner)
            check("DELETE with the host token ends the session for both", st == 200, "clients got session_closed")
            check("its Chromium processes are gone", procs_after < procs_before and procs_after == 0,
                  f"session chrome procs {procs_before} → {procs_after}")
            check("the session no longer exists", st2 == 404)
            gone = await (await browser.new_context()).new_page()
            await gone.goto(f"{base}/?s={guest['sessionId']}#p={guest['participantId']}&t={guest['token']}")
            gc = Client(gone, "late guest")
            await wait_for(lambda: gc.state("s.ended"), timeout=8, what="view for a gone session ends")
            check("a view for a session that's gone ends instead of reconnecting forever", await gc.state("s.endReason") == "not_found",
                  f"reason={await gc.state('s.endReason')}")
            leftover = [p for p in Path("/tmp").glob("collab-session-*")]
            check("profile directories cleaned up", not leftover,
                  f"{len(leftover)} left: " + ", ".join(f"{p.name} ({sum(1 for _ in p.rglob('*'))} files)" for p in leftover) if leftover else "")
            for p in leftover:
                print("   leftover:", p.name, sorted(str(f.relative_to(p)) for f in p.rglob("*"))[:20])
                print("   created:", time.strftime("%H:%M:%S", time.localtime(p.stat().st_ctime)), "now", time.strftime("%H:%M:%S"))
                for port in (DEV_PORT, STRICT_PORT):
                    for line in open(f"/tmp/collab-{port}.log").read().splitlines():
                        if "closed" in line or "profile" in line or "restart" in line or "lost" in line:
                            print(f"   [{port}] {line}")

            await browser.close()
    finally:
        for p in (dev, strict):
            p.terminate()
            try:
                p.wait(10)
            except Exception:
                p.kill()
        site.shutdown()

    passed = sum(1 for _, ok, _ in results if ok)
    print(f"\n{passed}/{len(results)} checks passed")
    try:
        print(f"click→video latency (local): WebRTC {latency_ms:.0f} ms, WebSocket {ws_median:.0f} ms")
    except NameError:
        pass
    sys.exit(0 if passed == len(results) else 1)


async def _count():
    return (last_report("index") or {}).get("count", 0)


async def _gt(coro, v):
    return (await coro) > v


async def _both(c1, c2):
    r1, r2 = await c1, await c2
    return bool(r1) and bool(r2)


async def _px_is(c, pred):
    return pred(await c.pixel(900, 600))


async def _green(c):
    return is_green(await c.pixel(900, 600))


async def _white(c):
    return is_white(await c.pixel(900, 600))


async def _blue(c):
    return is_blue(await c.pixel(900, 600))


if __name__ == "__main__":
    asyncio.run(main())
