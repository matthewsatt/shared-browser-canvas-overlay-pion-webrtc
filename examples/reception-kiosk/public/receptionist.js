// Receptionist: start a session on one of the two pages for a kiosk, then
// show the shared browser with a few controls built on the SDK.
const $ = (id) => document.getElementById(id);
let view = null;

(async () => {
  const cfg = await fetch('/api/config').then((r) => r.json());
  for (const url of cfg.startPages) $('startPage').append(new Option(url, url));

  $('startForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    $('error').textContent = '';
    // Your backend starts the session and keeps the guest seat for the kiosk.
    const res = await fetch('/api/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ kioskId: $('kioskId').value.trim(), url: $('startPage').value }),
    });
    const body = await res.json();
    if (!res.ok) { $('error').textContent = body.error; return; }

    // Put the shared browser in #screen.
    view = CollabBrowser.mount($('screen'), body.host);
    view.on('state', (s) => {
      const kiosk = s.participants.find((p) => p.role === 'guest');
      $('status').textContent = `${s.title || s.url} · kiosk ${kiosk?.connected ? 'connected' : 'not connected yet'}`;
    });
    view.on('ended', () => {
      view.destroy();
      view = null;
      $('startForm').hidden = false;
      $('controls').hidden = true;
      $('status').textContent = '';
    });
    $('startForm').hidden = true;
    $('controls').hidden = false;
  });

  $('btnBack').addEventListener('click', () => view?.back());
  $('btnReload').addEventListener('click', () => view?.reload());
  $('btnEnd').addEventListener('click', () => view?.end());
})();
