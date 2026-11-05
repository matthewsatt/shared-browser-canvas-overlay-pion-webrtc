# Reception desk + kiosk example

A small, separate app showing how your own project uses a deployed collab server. A receptionist starts a session on one of two pages for a kiosk. Both then see and use the same browser, and the receptionist ends it. Copy this folder anywhere; it doesn't depend on the collab server's code.

```
 receptionist.html ──┐                         ┌── kiosk.html?id=lobby-1
   (load sdk.js,     │  POST /api/sessions     │   (waits on /api/kiosks/lobby-1/events,
    mount host seat) │  ──────────────▶        │    mounts the guest seat when told)
                     ▼                         │
                  server.js  ── POST /api/browser-sessions (API token) ──▶  collab server
                     │       ◀── host seat + guest seat ───────────────────
                     └── host seat to the receptionist, guest seat pushed to the kiosk
```

Your app never contains collab code. The pages load `sdk.js` from the collab server, and the backend calls the collab API.

## Run it

You need Node 18 or newer and a running collab server. On the collab server, set:

```bash
API_TOKEN=some-long-secret
ALLOWED_ORIGINS=http://localhost:3000                                  # this app's address
ALLOWED_URL_PREFIXES=https://example.com/,https://www.iana.org/        # your two pages
```

Then start this app:

```bash
COLLAB_SERVER=https://collab.example.com COLLAB_API_TOKEN=some-long-secret \
START_PAGES=https://example.com/,https://www.iana.org/ npm start
```

1. Open http://localhost:3000/kiosk.html?id=lobby-1 on the kiosk (or in a second window).
2. Open http://localhost:3000/receptionist.html, pick a page and press **Start session**.

## Files

| File | What it does |
|---|---|
| `server.js` | The backend. Calls the collab API with the API token (which never reaches a browser), returns the host seat to the receptionist, and pushes the guest seat to the right kiosk over Server-Sent Events. |
| `public/receptionist.html` / `.js` | Start form, then `CollabBrowser.mount(div, hostSeat)` with Back, Reload and End buttons. |
| `public/kiosk.html` / `.js` | Welcome screen; when its seat arrives, `CollabBrowser.mount(div, guestSeat)` full screen; back to the welcome screen on `ended`. |

The line that brings in the SDK, in both pages:

```html
<script src="https://collab.example.com/sdk.js"></script>
```

(`server.js` fills in the address from `COLLAB_SERVER`.)

## Moving it into your real app

- **Logins.** This example trusts anyone who can reach it. Put your receptionists' login in front of `POST /api/sessions`, and identify kiosks by a device credential rather than a URL parameter.
- **Delivering the kiosk's seat.** Any channel works: Server-Sent Events as here, your existing WebSocket, a push service, or polling. Send the seat only to that kiosk, over HTTPS.
- **Kiosk hardware.** On a touch kiosk the on-screen keyboard opens only for text fields in the shared page, with the right layout (a number pad for phone fields). Run the kiosk browser full screen (kiosk mode).
- **HTTPS.** In production both this app and the collab server should be on HTTPS; the SDK then connects over `wss://` automatically.
