/* Models page.
 *
 * Reads GET /v1/models and renders the catalog: one card per model, filterable
 * by provider and by kind (chat, image, video, System One, embedding, audio,
 * tts, stt, OCR, moderation), with a search box and a sort selector. The card's
 * "try it" snippet is built for the model's kind, so an image model gets the images
 * endpoint and a System One model gets the systemone shape. No framework, no
 * build step. */
(function () {
  'use strict';

  /* ------------------------------------------------------------------ state */
  var state = {
    cards: [],
    providers: [],
    kinds: [],
    query: '',
    provider: 'all',
    kind: 'all',
    sort: 'name',
    loading: true,
    error: null,
    fetchedAt: null,
    fetchMs: 0
  };

  var $ = function (id) { return document.getElementById(id); };
  var panel = $('panel');

  /* -------------------------------------------------------------- kind meta */
  // One entry per kind: the label, the endpoint that answers, the svg icon and
  // the body of a minimal request. The body is a function of the model id so a
  // snippet is always tailored to the card it came from.
  var KINDS = {
    chat: {
      label: 'Chat',
      endpoint: '/v1/chat/completions',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a8 8 0 0 1-8 8H7l-4 3V12a8 8 0 0 1 8-8h2a8 8 0 0 1 8 8z"/></svg>',
      body: function (id) {
        return {
          model: id,
          messages: [{ role: 'user', content: 'Hello' }]
        };
      }
    },
    image: {
      label: 'Image',
      endpoint: '/v1/images/generations',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-4.5-4.5L6 21"/></svg>',
      body: function (id) {
        return {
          model: id,
          prompt: 'A lighthouse in a storm, long exposure',
          n: 1,
          size: '1024x1024'
        };
      }
    },
    image_edit: {
      label: 'Image edit',
      endpoint: '/v1/images/edits',
      // An edit is a multipart upload: the image rides as a file part beside
      // the model and prompt, so any snippet built from this entry has to be
      // a curl with -F fields rather than a JSON body.
      multipart: true,
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18.5 3.5a2.1 2.1 0 0 1 3 3L12 16l-4 1 1-4z"/><path d="M4 5v14a2 2 0 0 0 2 2h13"/></svg>',
      body: function (id) {
        return {
          model: id,
          prompt: 'Replace the background with a quiet studio wall',
          image: '@image.png'
        };
      }
    },
    video: {
      label: 'Video',
      endpoint: '/v1/videos/generations',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="6" width="13" height="12" rx="2"/><path d="m15 10 6-3v10l-6-3"/></svg>',
      body: function (id) {
        return {
          model: id,
          prompt: 'A drone shot over a pine forest at dawn',
          seconds: '8',
          size: '1280x720'
        };
      }
    },
    systemone: {
      label: 'System One',
      endpoint: '/v1/systemone',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v3M12 18v3M3 12h3M18 12h3"/><circle cx="12" cy="12" r="4"/><path d="m5.6 5.6 2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1"/></svg>',
      body: function (id) {
        return {
          model: id,
          state: 'Customer has been trying to connect their account for three days.',
          questions: {
            urgency: { type: 'noul', instructions: 'Does this message express urgency?' }
          }
        };
      }
    },
    embedding: {
      label: 'Embedding',
      endpoint: '/v1/embeddings',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7h18M3 12h18M3 17h18"/><circle cx="7" cy="7" r="1.5"/><circle cx="15" cy="12" r="1.5"/><circle cx="10" cy="17" r="1.5"/></svg>',
      body: function (id) {
        return { model: id, input: 'The quick brown fox' };
      }
    },
    audio: {
      label: 'Audio',
      endpoint: '/v1/audio/speech',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M3 12h2l2-7 3 14 3-10 2 3h6"/></svg>',
      body: function (id) {
        return { model: id, input: 'Hello from noderouter', voice: 'alloy' };
      }
    },
    tts: {
      label: 'TTS',
      endpoint: '/v1/audio/speech',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5 6 9H2v6h4l5 4z"/><path d="M15.5 8.5a5 5 0 0 1 0 7"/><path d="M18.5 5.5a9 9 0 0 1 0 13"/></svg>',
      body: function (id) {
        return { model: id, input: 'Hello from noderouter', voice: 'alloy', response_format: 'mp3' };
      }
    },
    stt: {
      label: 'STT',
      endpoint: '/v1/audio/transcriptions',
      // A transcription upload is multipart/form-data: the audio rides as a
      // file part beside the model id, so the try-it snippet is a curl with
      // -F fields rather than a JSON body.
      multipart: true,
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 10v1a7 7 0 0 0 14 0v-1"/><path d="M12 18v4M8 22h8"/></svg>',
      body: function (id) {
        return { file: '@audio.mp3', model: id, response_format: 'json' };
      }
    },
    ocr: {
      label: 'OCR',
      endpoint: '/v1/ocr',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/><path d="M9 15l2 2 4-4"/></svg>',
      body: function (id) {
        return {
          model: id,
          document: {
            type: 'document_url',
            document_url: 'https://arxiv.org/pdf/2201.04234'
          }
        };
      }
    },
    moderation: {
      label: 'Moderation',
      endpoint: '/v1/moderations',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="m9 12 2 2 4-4"/></svg>',
      body: function (id) {
        return { model: id, input: 'Example input.' };
      }
    }
  };

  function kindMeta(kind) { return KINDS[kind] || KINDS.chat; }
  function supportsImageEdit(card) {
    var accepts = card && Array.isArray(card.accepts) ? card.accepts : [];
    return !!(card && (
      card.kind === 'image_edit' ||
      (Array.isArray(card.kinds) && card.kinds.indexOf('image_edit') !== -1) ||
      card.endpoint === '/v1/images/edits' ||
      accepts.indexOf('/v1/images/edits') !== -1
    ));
  }
  function addCapability(list, kind) {
    if (KINDS[kind] && list.indexOf(kind) === -1) list.push(kind);
  }
  function capabilitiesOf(card) {
    var out = [];
    (card && Array.isArray(card.kinds) ? card.kinds : []).forEach(function (kind) {
      addCapability(out, kind);
    });
    if (card && card.kind) addCapability(out, card.kind);
    var accepts = card && Array.isArray(card.accepts) ? card.accepts : [];
    if ((card && card.endpoint) === '/v1/images/generations' || accepts.indexOf('/v1/images/generations') !== -1) {
      addCapability(out, 'image');
    }
    if (!out.length) addCapability(out, kindOf(card));
    return out;
  }
  function supportsKind(card, kind) { return capabilitiesOf(card).indexOf(kind) !== -1; }
  function kindOf(card) {
    if (card && card.kind === 'image_edit') return 'image';
    if (card && KINDS[card.kind]) return card.kind;
    // Fall back to the output modalities when an older router omits kind.
    var out = (card && card.modalities && card.modalities.output) ||
              (card && card.output_modalities) || [];
    if (out.indexOf('image') !== -1) return 'image';
    if (out.indexOf('video') !== -1) return 'video';
    if (out.indexOf('audio') !== -1) return 'audio';
    return 'chat';
  }
  // The endpoint a card's kind answers on. The router's own card carries the
  // answer when it offers one, so the page shows the endpoint the server
  // really routes to rather than a second copy of the mapping kept here; the
  // fallbacks below keep the page working against an older router that omits
  // the field, and still account for the Anthropic dialect (chat models at
  // /v1/messages) and for a provider that speaks the Responses API, where the
  // snippet should show the shape it really takes.
  function endpointOf(card) {
    if (card && card.endpoint) return card.endpoint;
    if (card && card.api === 'anthropic' && kindOf(card) === 'chat') return '/v1/messages';
    if (card && card.surface === 'responses' && kindOf(card) === 'chat') return '/v1/responses';
    return kindMeta(kindOf(card)).endpoint;
  }
  function bodyOf(card) {
    if (card && card.api === 'anthropic' && kindOf(card) === 'chat') {
      return { model: card.id, max_tokens: 1024, messages: [{ role: 'user', content: 'Hello' }] };
    }
    if (card && card.surface === 'responses' && kindOf(card) === 'chat') {
      return { model: card.id, input: 'Hello' };
    }
    return kindMeta(kindOf(card)).body(card.id);
  }
  // The limits row under the card's name. A chat model is read by its context
  // window; every other kind is read by what it takes in - its max input
  // context - plus the endpoint that answers for it, because image, OCR,
  // moderation, embedding and audio models are called on an endpoint of their
  // own and a bare context window says nothing about them. "max output" rides
  // along whenever the config declares one and is left out otherwise, rather
  // than shown as an em dash for a model that has no output cap at all.
  function metaFor(card, kind, lim) {
    var html = '';
    if (kind === 'chat') {
      html += metaItem('context', fmtTokens(lim.context), 'context window');
    } else {
      html += metaItem('input', fmtTokens(lim.context), 'max input context');
      html += metaItem('endpoint', String(endpointOf(card)).replace(/^\/v1/, ''), 'endpoint');
    }
    if (lim.output > 0) {
      html += metaItem('max output', fmtTokens(lim.output), 'max output tokens');
    }
    return html;
  }
  function metaItem(k, v, title) {
    return '<div class="meta-item"' + (title ? ' title="' + esc(title) + '"' : '') +
      '><span class="k">' + esc(k) + '</span><span class="v">' + esc(v) + '</span></div>';
  }

  /* -------------------------------------------------------------- utilities */
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function fmtTokens(n) {
    n = Number(n) || 0;
    if (n <= 0) return '—';
    if (n >= 1e6) return (n / 1e6).toFixed(n % 1e6 === 0 ? 0 : 1).replace(/\.0$/, '') + 'M';
    if (n >= 1e3) return Math.round(n / 1e3) + 'K';
    return String(n);
  }
  function timeAgo(ts) {
    var s = Math.max(0, (Date.now() - ts) / 1000);
    if (s < 5) return 'just now';
    if (s < 60) return Math.floor(s) + 's ago';
    var m = s / 60;
    if (m < 60) return Math.floor(m) + 'm ago';
    return Math.floor(m / 60) + 'h ago';
  }
  function modalitiesOf(card) {
    var inp = (card.modalities && card.modalities.input) || card.input_modalities || ['text'];
    var out = (card.modalities && card.modalities.output) || card.output_modalities || ['text'];
    return { input: inp.length ? inp : ['text'], output: out.length ? out : ['text'] };
  }
  function ctxOf(card) {
    return (card.limit && (card.limit.context || card.limit.output))
      ? card.limit
      : { context: card.context_length || 0, output: card.max_output_tokens || 0 };
  }

  var MODALITY_ICONS = {
    text: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 6h16M4 12h10M4 18h7"/></svg>',
    image: KINDS.image.icon,
    audio: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M3 12h2l2-7 3 14 3-10 2 3h6"/></svg>',
    video: KINDS.video.icon,
    file: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/></svg>'
  };
  function icon(name) { return MODALITY_ICONS[name] || MODALITY_ICONS.text; }

  var COPY_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';
  var CHECK_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 12.5 9.5 18 20 6.5"/></svg>';

  function copy(text, btn) {
    var done = function () {
      if (!btn) return;
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

  /* ------------------------------------------------------------------ fetch */
  function load() {
    state.loading = true;
    state.error = null;
    render();
    var started = performance.now();
    var ctrl = new AbortController();
    var timer = setTimeout(function () { ctrl.abort(); }, 15000);
    fetch('/v1/models', { headers: { 'Accept': 'application/json' }, signal: ctrl.signal })
      .then(function (res) {
        if (!res.ok) throw new Error('GET /v1/models → ' + res.status);
        return res.json();
      })
      .then(function (data) {
        state.fetchMs = Math.max(1, Math.round(performance.now() - started));
        state.cards = Array.isArray(data && data.data) ? data.data : [];
        state.providers = uniqueProviders(state.cards);
        state.kinds = uniqueKinds(state.cards);
        state.loading = false;
        state.fetchedAt = Date.now();
        render();
      })
      .catch(function (err) {
        state.loading = false;
        state.error = err && err.name === 'AbortError'
          ? 'the request timed out after 15s'
          : (err && err.message) || 'network error';
        render();
      })
      .finally(function () { clearTimeout(timer); });
  }

  function uniqueProviders(cards) {
    var seen = Object.create(null);
    var out = [];
    cards.forEach(function (c) {
      var p = c.owned_by || 'unknown';
      if (!seen[p]) { seen[p] = true; out.push(p); }
    });
    return out.sort(function (a, b) { return a.localeCompare(b); });
  }
  function uniqueKinds(cards) {
    var seen = Object.create(null);
    var out = [];
    cards.forEach(function (c) {
      capabilitiesOf(c).forEach(function (k) {
        if (!seen[k]) { seen[k] = true; out.push(k); }
      });
    });
    // Stable order: the canonical kind order rather than discovery order.
    var order = ['chat', 'image', 'image_edit', 'video', 'systemone', 'embedding', 'audio', 'tts', 'stt', 'ocr', 'moderation'];
    return out.sort(function (a, b) {
      var ai = order.indexOf(a), bi = order.indexOf(b);
      return (ai === -1 ? order.length : ai) - (bi === -1 ? order.length : bi);
    });
  }

  /* --------------------------------------------------------------- filtering */
  function visible() {
    var q = state.query.trim().toLowerCase();
    var list = state.cards.filter(function (c) {
      if (state.provider !== 'all' && (c.owned_by || 'unknown') !== state.provider) return false;
      if (state.kind !== 'all' && !supportsKind(c, state.kind)) return false;
      if (!q) return true;
      var mo = modalitiesOf(c);
      var hay = [c.id, c.name, c.description, c.owned_by, capabilitiesOf(c).join(' '), endpointOf(c),
        (c.accepts || []).join(' '),
        mo.input.join(' '), mo.output.join(' ')
      ].join(' ').toLowerCase();
      return hay.indexOf(q) !== -1;
    });
    return sortCards(list);
  }

  function sortCards(list) {
    var by = state.sort;
    var copy = list.slice();
    if (by === 'context') {
      copy.sort(function (a, b) { return (ctxOf(b).context || 0) - (ctxOf(a).context || 0); });
    } else if (by === 'output') {
      copy.sort(function (a, b) { return (ctxOf(b).output || 0) - (ctxOf(a).output || 0); });
    } else if (by === 'provider') {
      copy.sort(function (a, b) {
        var p = String(a.owned_by || '').localeCompare(String(b.owned_by || ''));
        return p !== 0 ? p : String(a.id).localeCompare(String(b.id));
      });
    } else {
      copy.sort(function (a, b) { return String(a.id).localeCompare(String(b.id)); });
    }
    return copy;
  }

  /* ------------------------------------------------------------------ render */
  function render() {
    renderLive();
    panel.setAttribute('aria-busy', state.loading ? 'true' : 'false');
    if (state.loading) { renderSkeleton(); return; }
    if (state.error) { renderError(); return; }
    renderStats();
    renderFilters();
    renderGrid();
    renderFetchedAt();
  }

  function renderLive() {
    var el = $('live');
    el.classList.toggle('off', !!state.error);
    $('live-label').textContent = state.error ? 'error' : (state.loading ? 'syncing' : 'live');
  }

  function renderSkeleton() {
    $('stats').hidden = true;
    $('controls').hidden = true;
    var bars = '';
    for (var i = 0; i < 6; i++) {
      bars += '<div class="skel"><div class="bar w60"></div><div class="bar w90"></div>' +
        '<div class="bar w75"></div><div class="bar w40"></div></div>';
    }
    panel.innerHTML = '<div class="grid-wrap"><div class="skel-grid">' + bars + '</div></div>';
  }

  function renderError() {
    $('stats').hidden = true;
    $('controls').hidden = true;
    panel.innerHTML =
      '<div class="state error" role="alert">' +
      '<h3>The router didn’t answer</h3>' +
      '<p><span class="mono">' + esc(state.error) + '</span></p>' +
      '<button class="btn btn-primary" id="retry-btn">Try again</button>' +
      '</div>';
    $('retry-btn').addEventListener('click', load);
  }

  function renderStats() {
    var cards = state.cards;
    $('stats').hidden = cards.length === 0;
    $('controls').hidden = cards.length === 0;
    var providers = Object.create(null);
    var maxCtx = 0, multimodal = 0;
    cards.forEach(function (c) {
      providers[c.owned_by || 'unknown'] = true;
      maxCtx = Math.max(maxCtx, ctxOf(c).context || 0);
      var m = modalitiesOf(c);
      var rich = m.input.some(function (x) { return x !== 'text'; }) ||
                 m.output.some(function (x) { return x !== 'text'; });
      if (rich) multimodal++;
    });
    $('stat-models').textContent = cards.length;
    $('stat-providers').textContent = Object.keys(providers).length;
    $('stat-context').textContent = fmtTokens(maxCtx);
    $('stat-multimodal').textContent = multimodal;
  }

  function renderFilters() {
    var total = state.cards.length;
    var shown = visible().length;
    $('count').textContent = shown === total
      ? total + ' model' + (total === 1 ? '' : 's')
      : shown + ' of ' + total;

    // Kind pills.
    var kindHTML = '<button class="pill' + (state.kind === 'all' ? ' on' : '') + '" aria-pressed="' + (state.kind === 'all') + '" data-kind="all">' +
      'All kinds <span class="n">' + total + '</span></button>';
    // Kind pills count capabilities, not a partition of the catalog: a model
    // that answers on two kinds (chat + image edits, say) is counted on both,
    // because the number on a pill has to match the grid that pill shows when
    // clicked. So the kind counts can sum past "All kinds" - they are tags
    // over one catalog, not slices of it. Intended.
    state.kinds.forEach(function (k) {
      var n = state.cards.filter(function (c) { return supportsKind(c, k); }).length;
      kindHTML += '<button class="pill' + (state.kind === k ? ' on' : '') + '" aria-pressed="' + (state.kind === k) + '" data-kind="' + esc(k) + '">' +
        '<span class="dot dot-' + esc(k) + '"></span>' + esc(kindMeta(k).label) +
        ' <span class="n">' + n + '</span></button>';
    });
    $('kind-pills').innerHTML = kindHTML;

    // Provider pills.
    var provHTML = '<button class="pill' + (state.provider === 'all' ? ' on' : '') + '" aria-pressed="' + (state.provider === 'all') + '" data-provider="all">' +
      'All <span class="n">' + total + '</span></button>';
    state.providers.forEach(function (p) {
      var n = state.cards.filter(function (c) { return (c.owned_by || 'unknown') === p; }).length;
      provHTML += '<button class="pill' + (state.provider === p ? ' on' : '') + '" aria-pressed="' + (state.provider === p) + '" data-provider="' + esc(p) + '">' +
        esc(p) + ' <span class="n">' + n + '</span></button>';
    });
    $('provider-pills').innerHTML = provHTML;

    wirePills('kind-pills', 'kind', 'data-kind');
    wirePills('provider-pills', 'provider', 'data-provider');
  }

  function wirePills(container, key, attr) {
    Array.prototype.forEach.call($(container).querySelectorAll('.pill'), function (btn) {
      btn.addEventListener('click', function () {
        state[key] = btn.getAttribute(attr);
        renderFilters();
        renderGrid();
        Array.prototype.some.call($(container).querySelectorAll('.pill'), function (next) {
          if (next.getAttribute(attr) === state[key]) {
            next.focus();
            return true;
          }
          return false;
        });
      });
    });
  }

  function capabilityBadges(card, primary) {
    return capabilitiesOf(card).map(function (capability) {
      if (capability === primary) return '';
      return '<span class="badge badge-capability">' + esc(kindMeta(capability).label) + '</span>';
    }).join('');
  }

  function renderGrid() {
    var list = visible();
    if (!list.length) {
      if (!state.cards.length) {
        panel.innerHTML =
          '<div class="state">' +
          '<h3>No models are available</h3>' +
          '<p>The router returned an empty catalog. Refresh when a provider is ready.</p>' +
          '<button class="btn btn-primary" id="empty-refresh">Refresh catalog</button>' +
          '</div>';
        $('empty-refresh').addEventListener('click', load);
        return;
      }
      panel.innerHTML =
        '<div class="state">' +
        '<h3>Nothing matches</h3>' +
        '<p>No model fits that search' +
        (state.provider !== 'all' ? ' in <strong>' + esc(state.provider) + '</strong>' : '') +
        (state.kind !== 'all' ? ' of kind <strong>' + esc(kindMeta(state.kind).label) + '</strong>' : '') +
        '.</p>' +
        '<button class="btn btn-secondary" id="clear-btn">Clear filters</button>' +
        '</div>';
      $('clear-btn').addEventListener('click', function () {
        state.query = ''; state.provider = 'all'; state.kind = 'all';
        $('q').value = '';
        renderFilters(); renderGrid();
      });
      return;
    }
    var html = '<div class="grid-wrap"><div class="grid">';
    list.forEach(function (c, i) {
      var m = modalitiesOf(c);
      var lim = ctxOf(c);
      var name = c.name || c.id;
      var kind = kindOf(c);
      var km = kindMeta(kind);
      var badges = m.input.map(function (x) {
        return '<span class="badge" title="input: ' + esc(x) + '">' + icon(x) + esc(x) + '</span>';
      }).join('') +
      '<span class="badge badge-ghost" aria-hidden="true">→</span>' +
        m.output.map(function (x) {
          return '<span class="badge" title="output: ' + esc(x) + '">' + icon(x) + esc(x) + '</span>';
        }).join('');
      html +=
        '<article class="card" data-i="' + i + '" tabindex="0" role="button" aria-haspopup="dialog" aria-controls="modal-overlay" aria-label="Details for ' + esc(c.id) + '">' +
          '<div class="card-top">' +
            '<span class="kind kind-' + esc(kind) + '">' + km.icon + esc(km.label) + '</span>' +
            '<span class="card-actions">' +
              (c.api === 'anthropic' ? '<span class="badge badge-ghost" title="Anthropic Messages API">anthropic</span>' : '') +
              (c.surface === 'responses'
                ? '<span class="badge badge-ghost" title="Native OpenAI Responses API. Chat-completions requests are translated, so both endpoints work.">responses</span>'
                : '') +
              '<button class="copy-btn" data-copy="' + esc(c.id) + '" title="Copy model id" aria-label="Copy model id">' + COPY_ICON + '</button>' +
            '</span>' +
          '</div>' +
          '<div class="card-id">' + esc(c.id) + '</div>' +
          '<div class="card-name">' + esc(name) + '</div>' +
          (c.description ? '<div class="card-desc">' + esc(c.description) + '</div>' : '') +
          '<div class="card-route"><span class="card-endpoint">' + esc(endpointOf(c)) + '</span>' +
            (c.accepts && c.accepts.length > 1 ? '<span class="card-accepts">' + esc(c.accepts.length) + ' routes</span>' : '') +
            (supportsImageEdit(c) ? '<span class="card-support">image edits supported</span>' : '') +
          '</div>' +
          '<div class="card-meta">' +
            metaFor(c, kind, lim) +
            '<span class="card-provider badge">' + esc(c.owned_by || 'unknown') + '</span>' +
          '</div>' +
          '<div class="card-capabilities">' + capabilityBadges(c, kind) + '</div>' +
          '<div class="card-badges">' + badges + '</div>' +
          '<span class="card-open-hint">View details</span>' +
        '</article>';
    });
    html += '</div></div>';
    panel.innerHTML = html;

    Array.prototype.forEach.call(panel.querySelectorAll('.card'), function (el) {
      var card = list[Number(el.getAttribute('data-i'))];
      el.addEventListener('click', function (e) {
        if (e.target.closest('.copy-btn')) return;
        openModal(card);
      });
      el.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openModal(card); }
      });
    });
    Array.prototype.forEach.call(panel.querySelectorAll('.copy-btn'), function (btn) {
      btn.addEventListener('click', function (e) {
        e.stopPropagation();
        copy(btn.getAttribute('data-copy'), btn);
      });
    });
  }

  function renderFetchedAt() {
    $('fetched-at').textContent = state.fetchedAt
      ? 'fetched ' + timeAgo(state.fetchedAt) + ' · ' + state.fetchMs + 'ms'
      : 'not fetched yet';
  }

  /* ------------------------------------------------------------------ modal */
  var overlay = $('modal-overlay');
  var lastFocus = null;
  var modalHideTimer = null;
  var modalTransitionEnd = null;

  function clearModalHideWait() {
    if (modalHideTimer !== null) {
      clearTimeout(modalHideTimer);
      modalHideTimer = null;
    }
    if (modalTransitionEnd) {
      overlay.removeEventListener('transitionend', modalTransitionEnd);
      modalTransitionEnd = null;
    }
  }

  function finishModalClose() {
    clearModalHideWait();
    if (!overlay.classList.contains('open')) overlay.hidden = true;
  }

  function jsonHighlight(obj) {
    var json = JSON.stringify(obj, null, 2);
    return esc(json)
      .replace(/&quot;([^&]+?)&quot;:/g, '<span class="k">&quot;$1&quot;</span>:')
      .replace(/: &quot;([^]*?)&quot;/g, ': <span class="s">&quot;$1&quot;</span>')
      .replace(/: (-?\d[\d.]*)/g, ': <span class="n">$1</span>');
  }

  function curlFor(card) {
    // location.origin keeps the scheme and port, so the copied command reads
    // correctly behind a TLS-terminating proxy and on loopback. Deriving the
    // host by hand and hardcoding http:// sent a TLS deployment's copy to the
    // plaintext port instead.
    var origin = location.origin;
    var kind = kindMeta(kindOf(card));
    var imageEdit = supportsImageEdit(card) && endpointOf(card) === '/v1/images/edits';
    if (kind.multipart || imageEdit) {
      // -F makes curl build the multipart body and set its own boundary, so
      // the command works as it stands; @ prefixes a file part.
      var fields = imageEdit
        ? { model: card.id, prompt: 'Replace the background with a quiet studio wall', image: '@image.png' }
        : bodyOf(card);
      var flags = Object.keys(fields).map(function (k) {
        return '  -F ' + k + '="' + fields[k] + '"';
      });
      return 'curl ' + origin + endpointOf(card) + ' \\\n' + flags.join(' \\\n');
    }
    var body = JSON.stringify(bodyOf(card), null, 2)
      .split('\n').map(function (line) { return '    ' + line; }).join('\n');
    return 'curl ' + origin + endpointOf(card) + ' \\\n' +
      '  -H "Content-Type: application/json" \\\n' +
      '  -d \'' + body + '\'';
  }

  // A Responses-native model is reachable through the chat endpoint too, so the
  // modal says so rather than leaving a client to wonder. The note is keyed on
  // the specific path being in the card's accepts list, not on accepts being
  // non-empty: an OCR or moderation card may name its own endpoint there, and
  // claiming it also answers /v1/responses would be a lie.
  function surfaceNote(card) {
    var acc = card && card.accepts;
    var other = card.surface === 'responses' ? '/v1/chat/completions' : '/v1/responses';
    if (!acc || acc.indexOf(other) === -1) return '';
    return '<div class="modal-note">Also answers on <code>' + esc(other) +
      '</code>. The router translates between the two OpenAI shapes.</div>';
  }

  function modalFact(label, value) {
    if (value == null || value === '') return '';
    return '<div class="modal-fact"><dt>' + esc(label) + '</dt><dd>' + esc(value) + '</dd></div>';
  }

  function imageEditNote(card) {
    if (!supportsImageEdit(card)) return '';
    return '<div class="modal-note">Supports image edits at <code>/v1/images/edits</code> with multipart fields <code>model</code>, <code>prompt</code>, <code>image</code>, and optional <code>mask</code>.</div>';
  }

  function setBackgroundInert(value) {
    Array.prototype.forEach.call(document.body.children, function (node) {
      if (node === overlay) return;
      if (value) node.setAttribute('inert', '');
      else node.removeAttribute('inert');
    });
  }

  function modalFocusable() {
    return Array.prototype.filter.call(overlay.querySelectorAll(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
    ), function (el) {
      return !el.hidden && el.offsetParent !== null;
    });
  }

  function openModal(card) {
    lastFocus = document.activeElement;
    var kind = kindOf(card);
    var lim = ctxOf(card);
    var modalities = modalitiesOf(card);
    var capabilities = capabilitiesOf(card);
    $('modal-id').textContent = card.id;
    $('modal-title').textContent = card.name || card.id;
    $('modal-description').textContent = card.description || 'Router metadata and a ready-to-copy request for this model.';
    $('modal-facts').innerHTML =
      modalFact('Provider', card.owned_by || 'unknown') +
      modalFact('Context', fmtTokens(lim.context)) +
      (lim.output > 0 ? modalFact('Max output', fmtTokens(lim.output)) : '') +
      modalFact('Input', modalities.input.join(', ')) +
      modalFact('Output', modalities.output.join(', ')) +
      modalFact('Capabilities', capabilities.map(function (x) { return kindMeta(x).label; }).join(', ')) +
      modalFact('Image edits', supportsImageEdit(card) ? 'supported' : '');
    $('modal-endpoint').textContent = endpointOf(card);
    $('modal-kind').innerHTML = kindMeta(kind).icon + ' ' + esc(kindMeta(kind).label);
    $('modal-kind').className = 'kind kind-' + kind;
    $('modal-json').innerHTML = jsonHighlight(card);
    $('modal-curl').textContent = curlFor(card);
    $('modal-surface').innerHTML = surfaceNote(card) + imageEditNote(card);
    $('modal-copy-id').onclick = function () { copy(card.id, $('modal-copy-id')); };
    $('modal-copy-json').onclick = function () { copy(JSON.stringify(card, null, 2), $('modal-copy-json')); };
    clearModalHideWait();
    overlay.hidden = false;
    overlay.removeAttribute('aria-hidden');
    document.body.classList.add('modal-open');
    setBackgroundInert(true);
    // Reveal the base state before adding .open so the opacity transition has
    // a rendered starting point.
    void overlay.offsetWidth;
    overlay.classList.add('open');
    $('modal-close').focus();
  }
  function closeModal() {
    overlay.classList.remove('open');
    overlay.setAttribute('aria-hidden', 'true');
    document.body.classList.remove('modal-open');
    setBackgroundInert(false);
    if (lastFocus && lastFocus.focus) lastFocus.focus();

    clearModalHideWait();
    modalTransitionEnd = function (e) {
      if (e.target === overlay && e.propertyName === 'opacity') finishModalClose();
    };
    overlay.addEventListener('transitionend', modalTransitionEnd);
    // Keep hidden as a fallback for browsers that do not dispatch transitionend.
    modalHideTimer = setTimeout(finishModalClose, 220);
  }
  $('modal-close').addEventListener('click', closeModal);
  overlay.addEventListener('click', function (e) { if (e.target === overlay) closeModal(); });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && overlay.classList.contains('open')) closeModal();
    if (e.key !== 'Tab' || !overlay.classList.contains('open')) return;
    var focusable = modalFocusable();
    if (!focusable.length) return;
    var first = focusable[0], last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  });

  /* ---------------------------------------------------------------- controls */
  var debounce = null;
  $('q').addEventListener('input', function (e) {
    clearTimeout(debounce);
    debounce = setTimeout(function () {
      state.query = e.target.value;
      renderFilters();
      renderGrid();
    }, 120);
  });
  $('sort').addEventListener('change', function (e) {
    state.sort = e.target.value;
    renderGrid();
  });
  $('refresh-top').addEventListener('click', function () {
    var btn = $('refresh-top');
    btn.classList.add('spin');
    btn.disabled = true;
    load();
    setTimeout(function () { btn.classList.remove('spin'); btn.disabled = false; }, 1200);
  });
  // "/" focuses the search box, the way docs sites do, unless the user is
  // already typing somewhere.
  document.addEventListener('keydown', function (e) {
    if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
    var tag = (document.activeElement && document.activeElement.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    e.preventDefault();
    $('q').focus();
  });
  setInterval(function () { if (state.fetchedAt) renderFetchedAt(); }, 30000);

  /* -------------------------------------------------------------------- go */
  // The base_url an OpenAI-compatible client is pointed at: this origin plus
  // /v1. location.origin keeps the scheme and port, so it reads correctly on
  // loopback (http://127.0.0.1:32256/v1) and behind TLS
  // (https://llm.example.com/v1).
  (function initBaseURL() {
    var base = location.origin + '/v1';
    $('base-url').textContent = base;
    $('base-url-copy').onclick = function () { copy(base, $('base-url-copy')); };
  })();

  load();
})();
