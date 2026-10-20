/* Docs page.
 *
 * The docs shell is served for any /docs/*.md; this script asks its own URL
 * for the raw markdown (Accept: text/markdown) and renders it in the browser
 * with the vendored marked. It builds the "on this page" list, adds a heading
 * anchor and a copy button to every code block, and wraps wide tables so they
 * scroll instead of breaking the layout. */
(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };
  var content = $('content');
  var toc = $('toc');
  var tocHead = $('toc-head');

  // Which guide is this? The shell is served for any /docs/*.md, so the page
  // asks its own URL and fetches the same path back as raw markdown.
  // README.md is special: it lives at the project root, not under /docs/.
  var name = location.pathname.replace(/^\/docs\//, '').replace(/\/+$/, '');
  if (!name) { location.replace('/docs/configuration.md'); return; }

  var isReadme = name === 'README.md';
  var fetchURL = '/docs/' + name;
  var rawURL   = location.pathname + '?format=json';

  $('src-path').textContent = 'GET /docs/' + name;
  $('raw-link').href = rawURL;
  if (isReadme) $('raw-link').title = 'Read the raw README';

  Array.prototype.forEach.call(document.querySelectorAll('[data-doc]'), function (a) {
    a.classList.toggle('active', a.getAttribute('data-doc') === name);
  });

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function slug(s) {
    return String(s).toLowerCase().trim()
      .replace(/[^\w\s-]/g, '').replace(/\s+/g, '-');
  }
  var COPY_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';
  var CHECK_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 12.5 9.5 18 20 6.5"/></svg>';

  function copy(text, btn) {
    var done = function () {
      btn.innerHTML = CHECK_ICON;
      btn.classList.add('done');
      setTimeout(function () { btn.innerHTML = COPY_ICON; btn.classList.remove('done'); }, 1200);
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

  function renderSkeleton() {
    var bars = '';
    var widths = ['w90', 'w75', 'w80', 'w60', 'w90', 'w70'];
    for (var i = 0; i < 6; i++) {
      bars += '<div class="skel"><div class="bar ' + widths[i % widths.length] + '"></div></div>';
    }
    content.innerHTML = bars;
  }

  function renderError(msg) {
    content.innerHTML =
      '<div class="state error">' +
      '<h3>The guide didn’t load</h3>' +
      '<p><span class="mono">' + esc(msg) + '</span></p>' +
      '<a class="btn btn-secondary" href="' + esc(location.pathname + '?format=json') + '">Read the raw markdown</a>' +
      '</div>';
  }

  function render(md) {
    content.innerHTML = marked.parse(md, { gfm: true });

    // The guide's own H1 becomes the page title; the rest get ids so headings
    // can be linked and listed in the sidebar.
    var headings = content.querySelectorAll('h1, h2, h3, h4');
    var first = true;
    var tocItems = [];
    Array.prototype.forEach.call(headings, function (h) {
      var id = slug(h.textContent);
      if (id) h.id = id;
      if (first) {
        first = false;
        document.title = h.textContent + (isReadme ? '' : ' · noderouter docs');
      } else if (h.tagName === 'H2' || h.tagName === 'H3') {
        tocItems.push(
          '<li class="toc-' + h.tagName.toLowerCase() + '">' +
          '<a href="#' + id + '">' + esc(h.textContent) + '</a></li>'
        );
      }
      h.insertAdjacentHTML('beforeend',
        '<a class="h-anchor" href="#' + id + '" aria-label="Link to this section">#</a>');
    });
    tocHead.hidden = tocItems.length === 0;
    toc.innerHTML = tocItems.join('');

    // Wrap each table in a scroll container. createElement + appendChild is
    // used instead of insertAdjacentHTML: a lone "<div>" fragment is parsed
    // as a *closed* empty div, so the table would never actually be inside it.
    Array.prototype.forEach.call(content.querySelectorAll('table'), function (t) {
      var wrap = document.createElement('div');
      wrap.className = 'table-wrap';
      t.parentNode.insertBefore(wrap, t);
      wrap.appendChild(t);
    });

    // Code blocks get a copy button.
    Array.prototype.forEach.call(content.querySelectorAll('pre'), function (pre) {
      var btn = document.createElement('button');
      btn.className = 'copy-btn';
      btn.type = 'button';
      btn.title = 'Copy code';
      btn.setAttribute('aria-label', 'Copy code');
      btn.innerHTML = COPY_ICON;
      btn.addEventListener('click', function () {
        var code = pre.querySelector('code');
        copy(code ? code.textContent : pre.textContent, btn);
      });
      pre.appendChild(btn);
    });
  }

  renderSkeleton();
  var started = performance.now();
  var ctrl = new AbortController();
  var timer = setTimeout(function () { ctrl.abort(); }, 15000);
  // The shell itself was served as HTML; asking for the same URL with a
  // markdown Accept gets the raw guide back (see wantsPage). Bypass the HTTP
  // cache: the shell must render the current guide, and this keeps the
  // markdown response from ever being replayed to a browser navigation of the
  // same URL (Vary: Accept guards that server-side).
  fetch(fetchURL, { headers: { 'Accept': 'text/markdown' }, cache: 'no-store', signal: ctrl.signal })
    .then(function (res) {
      if (!res.ok) throw new Error('GET ' + fetchURL + ' \u2192 ' + res.status);
      return res.text();
    })
    .then(function (md) {
      clearTimeout(timer);
      if (typeof marked === 'undefined' || !marked.parse) {
        renderError('the markdown renderer failed to load');
        return;
      }
      render(md);
      var ms = Math.max(1, Math.round(performance.now() - started));
      $('fetched-at').textContent = 'rendered ' + ms + 'ms';
    })
    .catch(function (err) {
      clearTimeout(timer);
      renderError(err && err.name === 'AbortError'
        ? 'the request timed out after 15s'
        : (err && err.message) || 'network error');
    });
})();
