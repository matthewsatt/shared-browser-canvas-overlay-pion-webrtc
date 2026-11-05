// Example "your app" backend: a receptionist page and a kiosk page that
// share a browser through the collab server. Node 18+, no dependencies.
//
//   COLLAB_SERVER=https://collab.example.com COLLAB_API_TOKEN=... node server.js
//
// What it does:
//   - serves receptionist.html and kiosk.html (each loads sdk.js from COLLAB_SERVER)
//   - POST /api/sessions: the receptionist starts a session for a kiosk. This
//     calls the collab API with the API token (which never reaches a browser),
//     returns the host seat to the receptionist, and pushes the guest seat to
//     that kiosk
//   - GET /api/kiosks/:id/events: each kiosk keeps this open (Server-Sent
//     Events) and is told when a session starts for it
//
// A real app would add its own login for receptionists and kiosks.

const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const PORT = Number(process.env.PORT || 3000);
const COLLAB_SERVER = (process.env.COLLAB_SERVER || 'http://localhost:8080').replace(/\/$/, '');
const COLLAB_API_TOKEN = process.env.COLLAB_API_TOKEN || '';
// The pages a session may start on. Keep in step with the collab server's
// ALLOWED_URL_PREFIXES.
const START_PAGES = (process.env.START_PAGES || 'https://example.com/,https://www.iana.org/').split(',').map((s) => s.trim()).filter(Boolean);
const PUBLIC = path.join(__dirname, 'public');

// kioskId -> { streams: Set<SSE response>, seat: guest seat waiting for the kiosk, or null }
const kiosks = new Map();
const kiosk = (id) => {
  if (!kiosks.has(id)) kiosks.set(id, { streams: new Set(), seat: null });
  return kiosks.get(id);
};
const push = (k, event, data) => {
  for (const res of k.streams) res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
};

async function startSession(kioskId, url) {
  const res = await fetch(`${COLLAB_SERVER}/api/browser-sessions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...(COLLAB_API_TOKEN && { Authorization: `Bearer ${COLLAB_API_TOKEN}` }) },
    body: JSON.stringify({ url, host: { name: 'Receptionist' }, guest: { name: `Kiosk ${kioskId}` } }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `collab server returned ${res.status}`);
  const k = kiosk(kioskId);
  k.seat = body.guest;
  push(k, 'session', body.guest);
  return body.host;
}

function readJSON(req) {
  return new Promise((resolve, reject) => {
    let data = '';
    req.on('data', (c) => { data += c; if (data.length > 64 << 10) req.destroy(); });
    req.on('end', () => { try { resolve(JSON.parse(data || '{}')); } catch (e) { reject(e); } });
  });
}

function send(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css' };

function serveFile(res, name) {
  const file = path.join(PUBLIC, path.normalize(name).replace(/^(\.\.[/\\])+/, ''));
  if (!file.startsWith(PUBLIC + path.sep)) return send(res, 404, { error: 'not found' });
  fs.readFile(file, 'utf8', (err, text) => {
    if (err) return send(res, 404, { error: 'not found' });
    // The pages load the SDK from the collab server: fill in its address.
    if (file.endsWith('.html')) text = text.replaceAll('__COLLAB_SERVER__', COLLAB_SERVER);
    res.writeHead(200, { 'Content-Type': TYPES[path.extname(file)] || 'application/octet-stream' });
    res.end(text);
  });
}

http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://localhost');
  const kioskMatch = url.pathname.match(/^\/api\/kiosks\/([\w-]{1,64})\/(events|idle)$/);
  try {
    if (req.method === 'GET' && url.pathname === '/') {
      res.writeHead(302, { Location: '/receptionist.html' });
      return res.end();
    }
    if (req.method === 'GET' && url.pathname === '/api/config') {
      return send(res, 200, { collabServer: COLLAB_SERVER, startPages: START_PAGES });
    }
    if (req.method === 'POST' && url.pathname === '/api/sessions') {
      const { kioskId, url: startUrl } = await readJSON(req);
      if (!/^[\w-]{1,64}$/.test(kioskId || '')) return send(res, 400, { error: 'kioskId is required' });
      if (!START_PAGES.includes(startUrl)) return send(res, 400, { error: 'not one of the start pages' });
      if (!kiosk(kioskId).streams.size) return send(res, 409, { error: `kiosk ${kioskId} isn't online` });
      return send(res, 201, { host: await startSession(kioskId, startUrl) });
    }
    if (kioskMatch && req.method === 'GET' && kioskMatch[2] === 'events') {
      const k = kiosk(kioskMatch[1]);
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' });
      res.write(': connected\n\n');
      k.streams.add(res);
      if (k.seat) res.write(`event: session\ndata: ${JSON.stringify(k.seat)}\n\n`); // e.g. the kiosk reloaded mid-session
      const ping = setInterval(() => res.write(': ping\n\n'), 25000);
      req.on('close', () => { clearInterval(ping); k.streams.delete(res); });
      return;
    }
    if (kioskMatch && req.method === 'POST' && kioskMatch[2] === 'idle') {
      kiosk(kioskMatch[1]).seat = null; // the kiosk's session ended
      return send(res, 200, { ok: true });
    }
    if (req.method === 'GET') return serveFile(res, url.pathname.slice(1));
    send(res, 404, { error: 'not found' });
  } catch (e) {
    send(res, 502, { error: e.message });
  }
}).listen(PORT, () => {
  console.log(`reception-kiosk example on http://localhost:${PORT} (collab server: ${COLLAB_SERVER})`);
});
