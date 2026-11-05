// Mounts both seats of one session side by side, as a partner site would,
// using the SDK from the collab server. Seats arrive in the fragment:
// #server=...&session=...&host=<participantId>.<token>&guest=<participantId>.<token>
(() => {
  const p = new URLSearchParams(location.hash.slice(1));
  const server = p.get('server');
  const s = document.createElement('script');
  s.src = `${server}/sdk.js`;
  s.onload = () => {
    window.views = {};
    window.ended = [];
    for (const role of ['host', 'guest']) {
      const [participantId, token] = p.get(role).split('.');
      const view = CollabBrowser.mount(document.getElementById(role), { server, sessionId: p.get('session'), participantId, token });
      view.on('ended', (e) => window.ended.push([role, e.reason]));
      window.views[role] = view;
    }
  };
  document.head.append(s);
})();
