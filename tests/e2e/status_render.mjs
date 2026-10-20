// The status page, rendered by a real browser.
//
// The page's structure, text and wiring are pinned by the stub-DOM harness the
// Go tests carry; what that harness cannot see is a real engine: a real fetch of
// /v1/status, real layout, the fills that end up on screen, and a click
// travelling through the page's own handlers. This drives a headless Firefox
// over WebDriver BiDi, reads the rendered DOM back after the page's script has
// run, clicks a day open and an hour segment, and can drop two screenshots.
//
// Usage (see README.md for the full recipe):
//   firefox --headless --no-remote --profile /tmp/ffprof --remote-debugging-port=9222 about:blank &
//   node tests/e2e/status_render.mjs http://127.0.0.1:32256/status /tmp/status.png /tmp/status-dialog.png
//
// Two screenshots because they show different things: the first is the page as
// it lands, the one whose fills can be compared against the page's own palette;
// the second is taken with the day dialog open, where the modal dims the page
// behind it, so its colours are the palette at the overlay's alpha.
import { writeFileSync } from 'node:fs';

const [url, shot, shotDialog] = process.argv.slice(2);
if (!url) {
  console.error('usage: node status_render.mjs <status-url> [page.png] [dialog.png]');
  process.exit(2);
}

const endpoint = process.env.BIDI_ENDPOINT || 'ws://127.0.0.1:9222/session';
const ws = new WebSocket(endpoint);
const pending = new Map();
let nextId = 1;

function send(method, params = {}) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject, method });
    ws.send(JSON.stringify({ id, method, params }));
  });
}

ws.addEventListener('message', (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id === undefined) return; // an event, not a reply
  const p = pending.get(msg.id);
  if (!p) return;
  pending.delete(msg.id);
  if (msg.type === 'error') p.reject(new Error(`${p.method}: ${msg.error} ${msg.message || ''}`));
  else p.resolve(msg.result);
});

function check(ok, what, detail) {
  if (!ok) throw new Error('FAIL ' + what + (detail === undefined ? '' : ': ' + detail));
  console.log('  ok  ' + what);
}

await new Promise((resolve) => ws.addEventListener('open', resolve, { once: true }));
console.log('browser ready:', JSON.stringify(await send('session.status')));
await send('session.new', { capabilities: {} });

const tree = await send('browsingContext.getTree');
const context = tree.contexts[0].context;
await send('browsingContext.setViewport', {
  context,
  viewport: { width: 1280, height: 1024 },
  devicePixelRatio: 1,
});

await send('browsingContext.navigate', { context, url, wait: 'complete' });

async function evaluate(expression) {
  const res = await send('script.evaluate', {
    expression,
    target: { context },
    awaitPromise: true,
    resultOwnership: 'none',
  });
  if (res.type === 'exception') throw new Error('page script threw: ' + JSON.stringify(res.exceptionDetails));
  return res.result.value;
}

// The page fetches /v1/status on load and renders from it; wait until what it
// fetched is on screen rather than guessing at a delay.
let ready = false;
for (let i = 0; i < 60 && !ready; i++) {
  ready = await evaluate(`!!document.querySelector('button.seg[data-day]')`);
  if (!ready) await new Promise((r) => setTimeout(r, 250));
}
if (!ready) {
  console.error('the page never rendered its week; body text follows');
  console.error(await evaluate('document.body.innerText'));
  process.exit(1);
}

const report = JSON.parse(
  await evaluate(`(() => {
    const css = (el) => getComputedStyle(el).backgroundColor;
    const days = [...document.querySelectorAll('button.seg[data-day]')].map((b) => ({
      day: b.dataset.day,
      cls: b.className.replace(/^seg\\s*/, ''),
      bar: css(b.querySelector('.seg-bar')),
      label: b.querySelector('.seg-cap').innerText.replace(/\\s+/g, ' ').trim(),
      title: b.getAttribute('title'),
    }));
    const providers = [...document.querySelectorAll('.acc')].map((a) => ({
      key: a.dataset.key,
      open: a.open,
      heading: a.querySelector('summary') ? a.querySelector('summary').innerText.replace(/\\s+/g, ' ').trim() : '',
      rows: [...a.querySelectorAll('button.mrow-name')].map((r) => {
        const line = r.parentElement.querySelector('.mline') || r.parentElement;
        const blocks = [...line.querySelectorAll('[data-hour]')];
        return {
          model: r.dataset.model,
          provider: r.dataset.provider,
          hours: blocks.map((h) => h.dataset.hour),
          colours: [...new Set(blocks.map((h) => css(h.querySelector('.mseg-bar'))))],
          row: r.parentElement.innerText.replace(/\\s+/g, ' ').trim().slice(0, 90),
        };
      }),
    }));
    return JSON.stringify({
      title: document.title,
      text: document.body.innerText,
      days,
      weekCount: (document.querySelector('#week-count') || {}).textContent,
      providers,
      theme: document.documentElement.dataset.theme,
    });
  })()`)
);

console.log('theme:', report.theme, '| title:', report.title, '|', report.weekCount);
check(report.days.length === 7, 'the week renders seven day blocks', report.days.length);
const bands = new Set(report.days.map((d) => d.cls));
check(
  ['ok', 'degraded', 'down'].every((b) => bands.has(b)),
  'the week shows green, amber and red days',
  [...bands].join(',')
);

// The fills, against the page's own palette for the theme it chose.
const palette = {
  light: { ok: 'rgb(21, 128, 61)', degraded: 'rgb(180, 83, 9)', down: 'rgb(220, 38, 38)' },
  dark: { ok: 'rgb(34, 197, 94)', degraded: 'rgb(245, 158, 11)', down: 'rgb(239, 68, 68)' },
}[report.theme] || {};
for (const d of report.days) {
  if (palette[d.cls]) {
    check(d.bar === palette[d.cls], `day ${d.day} (${d.cls}) is filled in its band colour`, `${d.bar} / ${d.title}`);
  }
}
check(/uptime/i.test(report.text), 'the page text carries the uptime it is built around');
check(
  !/\b(undefined|NaN|\[object Object\])\b/.test(report.text),
  'no placeholder text in the rendered page',
  report.text.match(/\b(undefined|NaN|\[object Object\])\b/)
);

check(report.providers.length >= 1, 'the providers section is rendered', report.providers.map((p) => p.key).join(','));
for (const p of report.providers) {
  check(p.rows.length >= 1, `provider ${p.key} lists the models it served`, JSON.stringify(p.heading));
  for (const r of p.rows) {
    check(r.hours.length === 24, `model ${r.model} via ${p.key} shows that day's 24 hours`, r.hours.join(','));
    check(r.hours[0] === '0' && r.hours[23] === '23', `model ${r.model} hour blocks run 0..23 in order`);
  }
}

const shoot = async (path) => {
  if (!path) return;
  const bytes = Buffer.from((await send('browsingContext.captureScreenshot', { context })).data, 'base64');
  writeFileSync(path, bytes);
  console.log('screenshot:', path, bytes.length, 'bytes');
};
await shoot(shot);

// Drive the dialog in the real DOM: open a day, then click an hour.
const dialog = JSON.parse(
  await evaluate(`(() => {
    document.querySelector('button.seg[data-day]').click();
    const modal = document.querySelector('#day-modal');
    const hours = [...document.querySelectorAll('#dd-hours [data-hour]')];
    if (hours.length > 12) hours[13].click();
    return JSON.stringify({
      hidden: modal.getAttribute('aria-hidden'),
      open: modal.classList.contains('open'),
      title: document.querySelector('#dd-title').textContent,
      sub: document.querySelector('#dd-sub').textContent,
      hourCount: hours.length,
      hourIds: hours.map((h) => h.dataset.hour),
      readoutAfterClick: document.querySelector('#dd-readout').textContent,
    });
  })()`)
);
check(dialog.hidden === 'false', 'clicking a day opens the dialog', dialog.hidden);
check(dialog.hourCount === 24, 'the dialog has 24 hour blocks', dialog.hourCount);
check(dialog.hourIds[0] === '0' && dialog.hourIds[23] === '23', 'the hour blocks run 0..23 in order');
check(dialog.title.length > 0 && /\d/.test(dialog.title), 'the dialog is titled with the day', dialog.title);
check(
  /\b13\b/.test(dialog.readoutAfterClick),
  'clicking hour 13 is spelled out in the readout',
  dialog.readoutAfterClick
);
check(!/\b(undefined|NaN)\b/.test(dialog.readoutAfterClick + dialog.sub), 'no placeholder text in the dialog', dialog.sub);

await shoot(shotDialog);

console.log('render check clean: real Firefox, page rendered from /v1/status');
ws.close();
process.exit(0);
