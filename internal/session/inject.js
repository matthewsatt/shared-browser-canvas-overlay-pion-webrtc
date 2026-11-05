// Runs in an isolated world (not visible to page scripts) in every frame.
// Native <select> popups are not part of the page screencast, so instead of
// letting Chromium open one we report the options to the server, which
// shows them to everyone in the collaborative overlay.
(() => {
  if (window.top !== window) return; // main frame only for now
  if (window.__collabInstalled) return;
  window.__collabInstalled = true;
  let seq = 0;
  const report = (s) => {
    if (!s.dataset.collabSelect) {
      s.dataset.collabSelect = (++seq) + '-' + Math.random().toString(36).slice(2, 8);
    }
    const r = s.getBoundingClientRect();
    const options = Array.from(s.options).slice(0, 1000).map((o, i) => {
      const g = o.parentElement && o.parentElement.tagName === 'OPTGROUP' ? o.parentElement : null;
      return {
        index: i,
        label: String(o.label || o.text || '').slice(0, 300),
        disabled: !!(o.disabled || (g && g.disabled)),
        group: g ? String(g.label || '').slice(0, 300) : null,
      };
    });
    try {
      window.__collabSelect(JSON.stringify({
        id: s.dataset.collabSelect,
        selectedIndex: s.selectedIndex,
        options,
        rect: { x: r.left, y: r.top, w: r.width, h: r.height },
        viewport: { w: window.innerWidth, h: window.innerHeight },
      }));
    } catch (_) {}
  };
  document.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    const t = e.target;
    const s = t && t.closest ? t.closest('select') : null;
    if (!s || s.multiple || s.disabled || (s.size && s.size > 1)) return;
    e.preventDefault(); // suppress the invisible native popup
    s.focus();
    report(s);
  }, true);
  document.addEventListener('keydown', (e) => {
    const s = document.activeElement;
    if (!s || s.tagName !== 'SELECT' || s.multiple) return;
    if ((e.altKey && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) || e.key === 'F4' || (e.key === ' ' && !e.ctrlKey)) {
      e.preventDefault();
      report(s);
    }
  }, true);

  // ---- Text fields, for on-screen keyboards on touch devices ----
  // Reports whether a text field has focus, and where the visible text
  // fields are with the keyboard each wants (numeric, tel, email...). A
  // touch client uses the positions to decide *during the tap* whether to
  // open its on-screen keyboard: iOS only opens it for focus inside a tap.
  const TEXT_TYPES = new Set(['text', 'search', 'email', 'tel', 'url', 'number', 'password']);
  const MODES = new Set(['text', 'search', 'email', 'tel', 'url', 'numeric', 'decimal', 'none']);
  const isField = (el) => {
    if (!el || el.disabled || el.readOnly) return false;
    if (el.isContentEditable || el.tagName === 'TEXTAREA') return true;
    return el.tagName === 'INPUT' && TEXT_TYPES.has(el.type);
  };
  const modeOf = (el) => {
    const im = String(el.inputMode || el.getAttribute('inputmode') || '').toLowerCase();
    if (MODES.has(im)) return im;
    if (el.tagName === 'INPUT') {
      if (el.type === 'number') return 'decimal';
      if (el.type === 'tel' || el.type === 'email' || el.type === 'url' || el.type === 'search') return el.type;
    }
    return 'text';
  };
  const activeDeep = () => {
    let el = document.activeElement;
    while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
    return el;
  };
  const send = (msg) => { try { window.__collabPage(JSON.stringify(msg)); } catch (_) {} };

  let lastFocus = '';
  const reportFocus = () => {
    const el = activeDeep();
    const msg = isField(el) ? { kind: 'focus', editable: true, mode: modeOf(el) } : { kind: 'focus', editable: false };
    const s = JSON.stringify(msg);
    if (s !== lastFocus) { lastFocus = s; send(msg); }
  };

  let lastFields = '';
  const reportFields = () => {
    const vw = window.innerWidth, vh = window.innerHeight, rects = [];
    for (const el of document.querySelectorAll('input, textarea, [contenteditable]')) {
      if (rects.length >= 200) break;
      if (!isField(el)) continue;
      const mode = modeOf(el);
      if (mode === 'none') continue; // the page brings its own keyboard
      const r = el.getBoundingClientRect();
      if (r.width < 2 || r.height < 2 || r.bottom <= 0 || r.right <= 0 || r.top >= vh || r.left >= vw) continue;
      // Skip fields hidden under something else (a modal, say). Elements
      // with pointer-events: none (floating labels) don't count.
      const cx = Math.min(Math.max(r.left + r.width / 2, 0), vw - 1);
      const cy = Math.min(Math.max(r.top + r.height / 2, 0), vh - 1);
      const top = document.elementFromPoint(cx, cy);
      if (top && top !== el && !el.contains(top) && top.control !== el) continue;
      rects.push({ x: Math.round(r.left), y: Math.round(r.top), w: Math.round(r.width), h: Math.round(r.height), mode });
    }
    const s = JSON.stringify(rects);
    if (s !== lastFields) { lastFields = s; send({ kind: 'fields', viewport: { w: vw, h: vh }, rects }); }
  };

  let queued = false;
  const update = () => {
    if (queued) return;
    queued = true;
    setTimeout(() => { queued = false; reportFields(); reportFocus(); }, 120);
  };
  document.addEventListener('focusin', () => setTimeout(reportFocus, 0), true);
  document.addEventListener('focusout', () => setTimeout(reportFocus, 0), true);
  window.addEventListener('scroll', update, true);
  window.addEventListener('resize', update);
  window.addEventListener('load', update);
  window.addEventListener('pageshow', update);
  document.addEventListener('DOMContentLoaded', update);
  document.addEventListener('transitionend', update, true);
  document.addEventListener('animationend', update, true);
  new MutationObserver(update).observe(document, {
    subtree: true, childList: true, attributes: true,
    attributeFilter: ['style', 'class', 'hidden', 'disabled', 'readonly', 'type', 'contenteditable', 'inputmode'],
  });
  update();
})();
