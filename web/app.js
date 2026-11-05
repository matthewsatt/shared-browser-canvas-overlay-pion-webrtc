// Demo app for the shared browser, built only on the public SDK (sdk.js).
//
// The landing form plays the part of your backend: it calls
// POST /api/browser-sessions and gets two seats back. The host's seat
// opens here; the guest's seat goes into a link (credentials in the URL
// fragment, so they never reach server logs). Each page then calls
// CollabBrowser.mount(element, seat) and builds its toolbar from the view's
// methods and events.

const $ = (id) => document.getElementById(id);
const els = {
  landing: $('landing'), createForm: $('createForm'), createUrl: $('createUrl'), allowedUrls: $('allowedUrls'),
  apiTokenRow: $('apiTokenRow'), apiToken: $('apiToken'), hostName: $('hostName'), guestName: $('guestName'),
  createError: $('createError'), message: $('message'), messageText: $('messageText'),
  app: $('app'), viewSlot: $('viewSlot'), toasts: $('toasts'),
  urlForm: $('urlForm'), urlInput: $('urlInput'), loadingDot: $('loadingDot'), participants: $('participants'),
  btnBack: $('btnBack'), btnForward: $('btnForward'), btnReload: $('btnReload'),
  btnVideo: $('btnVideo'), videoStats: $('videoStats'), btnGuestLink: $('btnGuestLink'), btnEnd: $('btnEnd'),
};

const store = {
  get(k) { try { return JSON.parse(sessionStorage.getItem(k) || 'null'); } catch { return null; } },
  set(k, v) { try { sessionStorage.setItem(k, JSON.stringify(v)); } catch {} },
  del(k) { try { sessionStorage.removeItem(k); } catch {} },
};

async function api(method, path, body, token) {
  const headers = { 'Content-Type': 'application/json' };
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  let data = {};
  try { data = await res.json(); } catch {}
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data;
}

// ------------------------------------------------------------------ boot

function boot() {
  const sid = new URLSearchParams(location.search).get('s');
  if (!sid) return showLanding();
  // A guest link carries its seat in the fragment: #p=<participantId>&t=<token>.
  const hash = new URLSearchParams(location.hash.slice(1));
  if (hash.get('p') && hash.get('t')) {
    store.set(`collab:seat:${sid}`, { sessionId: sid, participantId: hash.get('p'), token: hash.get('t') });
    history.replaceState(null, '', `${location.pathname}${location.search}`);
  }
  const seat = store.get(`collab:seat:${sid}`);
  if (!seat) return showMessage('This link is missing its access details. Ask for a new link.');
  start(seat);
}

async function showLanding() {
  els.landing.hidden = false;
  try {
    const cfg = await api('GET', '/api/config');
    els.apiTokenRow.hidden = !cfg.requireApiToken;
    for (const u of cfg.allowedUrlPrefixes || []) els.allowedUrls.append(new Option(u, u));
    if (cfg.allowedUrlPrefixes?.length) els.createUrl.value = cfg.allowedUrlPrefixes[0];
  } catch {}
  els.createForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    els.createError.textContent = '';
    const btn = els.createForm.querySelector('button');
    btn.disabled = true;
    try {
      const r = await api('POST', '/api/browser-sessions', {
        url: els.createUrl.value,
        host: { name: els.hostName.value.trim() },
        guest: { name: els.guestName.value.trim() },
      }, els.apiToken.value.trim() || undefined);
      store.set(`collab:seat:${r.sessionId}`, r.host);
      store.set(`collab:guest:${r.sessionId}`, r.guest);
      location.href = `/?s=${encodeURIComponent(r.sessionId)}`;
    } catch (err) {
      els.createError.textContent = err.message;
      btn.disabled = false;
    }
  });
}

function showMessage(text) {
  els.message.hidden = false;
  els.messageText.textContent = text;
}

// ------------------------------------------------------------------ session page

function start(seat) {
  els.app.hidden = false;
  const view = CollabBrowser.mount(els.viewSlot, {
    ...seat,
    video: store.get('collab:video') === 'websocket' ? 'websocket' : 'auto',
  });
  window.__collab = Object.assign({ view }, view.debug); // for diagnostics and tests

  const guest = store.get(`collab:guest:${seat.sessionId}`);
  view.on('state', (s) => render(view, s, guest));
  view.on('stats', () => renderVideo(view));
  view.on('transport', () => renderVideo(view));
  view.on('participant', ({ participant, connected }) => {
    if (participant.id !== seat.participantId) toast(`${participant.name} ${connected ? 'joined' : 'left'}`);
  });
  view.on('error', (e) => { if (e.kind !== 'navigation_blocked') toast(e.message, true); });
  view.on('ended', () => store.del(`collab:seat:${seat.sessionId}`));

  els.btnBack.addEventListener('click', () => view.back());
  els.btnForward.addEventListener('click', () => view.forward());
  els.btnReload.addEventListener('click', () => view.reload());
  els.urlForm.addEventListener('submit', (e) => {
    e.preventDefault();
    const url = els.urlInput.value.trim();
    if (url) view.navigate(url);
    els.urlInput.blur();
    view.focus();
  });
  els.btnVideo.addEventListener('click', () => {
    const next = view.state.video === 'websocket' ? 'auto' : 'websocket';
    view.setVideoTransport(next);
    store.set('collab:video', next);
    renderVideo(view);
  });
  els.btnGuestLink.addEventListener('click', async () => {
    const link = `${location.origin}/?s=${encodeURIComponent(guest.sessionId)}#p=${encodeURIComponent(guest.participantId)}&t=${encodeURIComponent(guest.token)}`;
    try { await navigator.clipboard.writeText(link); toast('Guest link copied'); }
    catch { window.prompt('Copy this guest link', link); }
  });
  els.btnEnd.addEventListener('click', () => {
    if (window.confirm('End this session for everyone?')) view.end();
  });
  render(view, view.state, guest);
  renderVideo(view);
}

function render(view, s, guest) {
  if (document.activeElement !== els.urlInput) els.urlInput.value = s.url || '';
  els.loadingDot.classList.toggle('loading', s.loading);
  document.title = s.title ? `${s.title} · Shared Browser` : 'Shared Browser';
  const host = s.role === 'host';
  els.btnEnd.hidden = !host || s.ended;
  els.btnGuestLink.hidden = !host || !guest || s.ended;
  const frag = document.createDocumentFragment();
  for (const p of s.participants) {
    const a = document.createElement('div');
    a.className = 'avatar' + (p.you ? ' me' : '') + (p.connected ? '' : ' away');
    a.style.background = p.color;
    a.title = `${p.name} (${p.role})` + (p.you ? ', you' : '') + (p.connected ? '' : ', not connected');
    a.textContent = initials(p.name);
    frag.append(a);
  }
  els.participants.replaceChildren(frag);
}

function renderVideo(view) {
  const off = view.state.video === 'websocket';
  const b = els.btnVideo;
  b.textContent = off ? 'WebRTC: off' : 'WebRTC: on';
  b.classList.toggle('off', off);
  b.setAttribute('aria-pressed', String(!off));
  b.title = off
    ? 'Video is coming over the WebSocket. Click to turn WebRTC back on.'
    : 'Click to turn WebRTC off and send video over the WebSocket instead.';
  const s = view.stats, mode = view.state.transport;
  if (mode === 'none' || !s || s.transport !== mode) { els.videoStats.textContent = ''; return; }
  const rate = s.kbps >= 1000 ? `${(s.kbps / 1000).toFixed(1)} Mbps` : `${Math.round(s.kbps)} kbps`;
  els.videoStats.textContent = `${mode === 'rtc' ? 'WebRTC' : 'WebSocket'} · ${Math.round(s.fps)} fps · ${rate}`;
}

function initials(name) {
  return (name || '?').trim().split(/\s+/).slice(0, 2).map((w) => w[0]).join('').toUpperCase();
}

function toast(text, isError = false) {
  const t = document.createElement('div');
  t.className = 'toast' + (isError ? ' error' : '');
  t.textContent = text;
  els.toasts.append(t);
  setTimeout(() => t.remove(), isError ? 6000 : 3000);
  while (els.toasts.children.length > 4) els.toasts.firstChild.remove();
}

boot();
