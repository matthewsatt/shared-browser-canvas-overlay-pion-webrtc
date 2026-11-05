// Kiosk: wait for the receptionist to start a session, then show the shared
// browser full screen until the session ends. Open as kiosk.html?id=lobby-1.
const $ = (id) => document.getElementById(id);
const kioskId = new URLSearchParams(location.search).get('id') || 'lobby-1';
let view = null;

function showIdle() {
  $('screen').hidden = true;
  $('idle').hidden = false;
}

// Your backend tells this kiosk when a session starts for it, with its seat.
const events = new EventSource(`/api/kiosks/${encodeURIComponent(kioskId)}/events`);
events.addEventListener('session', (e) => {
  const seat = JSON.parse(e.data);
  if (view) view.destroy();
  $('idle').hidden = true;
  $('screen').hidden = false;
  view = CollabBrowser.mount($('screen'), seat);
  view.on('ended', () => {
    view.destroy();
    view = null;
    fetch(`/api/kiosks/${encodeURIComponent(kioskId)}/idle`, { method: 'POST' });
    showIdle();
  });
});
