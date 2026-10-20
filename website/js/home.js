/* Landing page.
 *
 * Two small jobs. The bar is parked above the fold by the stylesheet and this
 * file reveals it once the reader has scrolled past the panel; that state is a
 * single class on <body>, so nothing here measures or moves anything. It also
 * fills in the values that depend on where the router is running - the base URL
 * an OpenAI-compatible client is pointed at, and the origin in the quick-start
 * lines. */
(function () {
  'use strict';

  /* ---------------------------------------------------------- reveal bar */
  (function () {
    var body = document.body;
    if (!body.classList.contains('page-home')) return;

    // Far enough that a stray pixel of scroll does not pop the bar in.
    var THRESHOLD = 80;
    var queued = false;

    function update() {
      queued = false;
      var y = window.scrollY || window.pageYOffset || document.documentElement.scrollTop || 0;
      body.classList.toggle('nav-visible', y > THRESHOLD);
    }
    function onScroll() {
      // rAF-coalesced: a scroll fires far faster than the class needs to change.
      if (queued) return;
      queued = true;
      window.requestAnimationFrame(update);
    }
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onScroll, { passive: true });
    update();
  })();

  /* --------------------------------------------------------------- clipboard */
  var COPY_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';
  var CHECK_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 12.5 9.5 18 20 6.5"/></svg>';

  function copy(text, btn) {
    if (!btn) return;
    var done = function () {
      var old = btn.innerHTML;
      btn.innerHTML = CHECK_ICON;
      btn.classList.add('done');
      setTimeout(function () { btn.innerHTML = old; btn.classList.remove('done'); }, 1200);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text); done(); });
    } else { fallbackCopy(text); done(); }
  }
  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
  }

  /* -------------------------------------------------------- origin-derived */
  // The base URL an OpenAI-compatible client is pointed at: this origin plus
  // /v1. location.origin keeps the scheme and port, so it reads correctly on
  // loopback (http://127.0.0.1:32256/v1) and behind TLS
  // (https://llm.example.com/v1).
  var origin = location.origin;
  var base = origin + '/v1';

  var urlEl = document.getElementById('base-url');
  var urlBtn = document.getElementById('base-url-copy');
  if (urlEl) urlEl.textContent = base;
  if (urlBtn) urlBtn.onclick = function () { copy(base, urlBtn); };

  var termOrigin = document.getElementById('term-origin');
  if (termOrigin) termOrigin.textContent = origin;

  var termCopy = document.getElementById('term-copy');
  if (termCopy) {
    termCopy.onclick = function () {
      var block = document.querySelector('.term-body');
      if (!block) return;
      // The "$ " prompts are for reading, not for pasting: strip them so the
      // copied text runs in a shell as-is.
      var cmds = block.textContent.replace(/^[ \t]*\$\s?/gm, '').replace(/\s+$/, '');
      copy(cmds + '\n', termCopy);
    };
  }

  // The footer span is the same element /models and /docs fill in with when
  // their data arrived; writing the origin into it (as this used to) put a
  // URL where the reader expects a time. The landing page fetches nothing, so
  // the honest time is when this page loaded, kept relative so it does not go
  // stale while the tab sits open.
  var fetchedAt = document.getElementById('fetched-at');
  if (fetchedAt) {
    var loadedAt = Date.now();
    var renderAge = function () {
      var s = Math.floor((Date.now() - loadedAt) / 1000);
      fetchedAt.textContent =
        s < 5 ? 'loaded just now' :
        s < 60 ? 'loaded ' + s + 's ago' :
        s < 3600 ? 'loaded ' + Math.floor(s / 60) + 'm ago' :
        'loaded ' + Math.floor(s / 3600) + 'h ago';
    };
    renderAge();
    setInterval(renderAge, 30000);
  }
})();
