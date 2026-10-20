# Browser end-to-end harness for the audit's F2 findings

These scripts close the two browser-level gaps the security audit left open
(`handoff.md`, "Needs validation"): the no-cors/simple-request mechanism (item
2) and its DNS-rebinding variant (item 3). They drive a **real headless
Firefox** at an attack page and check whether a provider key reaches a mock
upstream. Everything runs loopback-only inside a network namespace — no live
providers, dummy secrets only.

Nothing here runs under `go test`; this is a manual harness.

## Requirements

- `firefox` (any recent version; verified with 157), `python3`, `curl`,
  `unshare`/`util-linux` with user+net namespaces available
  (`unshare -rn true` must work), `bash`.
- A built router binary: `go build -o /tmp/nr .`

## Scenario A — cross-origin no-cors simple request (item 2)

```bash
unshare -rn tests/e2e/scenarioA.sh /tmp/nr <tag>
```

A page on `http://127.0.0.1:18081` fires
`fetch("http://127.0.0.1:18082/v1/...", {mode:"no-cors", headers:{"Content-Type":"text/plain"}, body:<JSON>})`
— the "simple request" a hostile page sends, delivered without a CORS
preflight. Expected:

- **pre-fix build** (any binary without `crossSiteRefusal`, e.g. built from
  the audit's base commit): the mock upstream logs
  `Authorization: Bearer <provider key>` and the attacker's body; the page
  sees an opaque response.
- **current build**: the router answers 403 at the gate
  (`request refused: cross-site` in its log); the upstream logs nothing.

## Scenario B — DNS-rebinding variant (item 3)

```bash
unshare -rnm tests/e2e/scenarioB.sh /tmp/nr <tag>
```

Inside a user+mount namespace, `evil.test` resolves to `127.0.0.1` through a
bind-mounted `/etc/hosts` — the shape a rebound DNS produces. The attack page
is served at `http://evil.test/` and fetches `/v1/chat/completions` on its own
origin; the collector relays raw bytes to the router so `Host: evil.test`
reaches it verbatim. Expected:

- **pre-fix build**: forwarded — the upstream logs the key **and** the page
  reads the completion response (200), which is worse than scenario A's
  opaque response: a rebind makes the reply readable, not just spendable.
- **current build**: `Origin` matches `Host` (as in a true rebind), so the
  refusal comes from the host allowlist: 403 naming the host, upstream
  untouched.

## Status page render check (real browser)

```bash
# 1. a router with history to show (any config with a status.file works)
/tmp/nr serve -config /tmp/status-demo.json &

# 2. a headless Firefox with its WebDriver BiDi endpoint open
firefox --headless --no-remote --profile /tmp/ffprof --remote-debugging-port=9222 about:blank &

# 3. read the rendered page back, with two screenshots
node tests/e2e/status_render.mjs http://127.0.0.1:32256/status \
  /tmp/status.png /tmp/status-dialog.png
```

`status_render.mjs` drives that browser over WebDriver BiDi: it loads `/status`,
waits for the page's own fetch of `/v1/status` to land, and then reads the
**rendered** DOM back - the seven day blocks and the colour each is filled in,
the provider accordions and their model rows with 24 hour blocks per model, and
no placeholder text anywhere. It then clicks a day open in the real DOM and
clicks hour 13, checking the dialog's title, its 24 segments and the readout
that spells the selected hour out, and takes a screenshot before and after (the
second one dimmed by the modal overlay, which is why its colours are not the
page's fills).

The page has to be showing traffic for the band fills to exist: seed a week of
history into the `status.file` the config names, or let the router run and make
requests for a while. `BIDI_ENDPOINT` overrides
the default `ws://127.0.0.1:9222/session`.

What this does **not** do is look at the screenshots: it asserts the DOM and the
computed fills, and leaves the PNGs for a person to open. Nothing here runs
under `go test` either.

## Artifacts

Each run writes to `$E2E_WORKDIR` (default `/tmp/noderouter-e2e`):
`upstream.log` (what the provider saw), `results.log` (what the page saw),
`relay.log` (scenario B wire), `router.log`, `firefox.log`.
