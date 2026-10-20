# Operations

## Commands

```
noderouter [serve] [flags]   run the router (default command)
noderouter init [flags]      write a starter config
noderouter convert [flags]   fetch + test proxies into the ports file
noderouter check [flags]     re-test the proxies already in the list
noderouter routes [flags]    print how each model is routed
noderouter keygen [flags]    mint a client api key and show how to use it
```

`convert` fetches every source, tests proxies, and writes the list:

```bash
noderouter convert                                    # all sources, merged and tested
noderouter convert -source proxyscrape -limit 200 -timeout 10000
noderouter convert -source iplocate -protocol all     # the big mixed list
noderouter convert -source https://example.com/list.txt   # any raw list
noderouter convert -source geonode -limit 100
noderouter convert -source file -in myproxies.txt     # never overwrites -in
noderouter convert -no-test -keep 50                  # fastest 50, no testing
noderouter convert -insecure                          # also keep tls-intercepting proxies
noderouter check -force                               # re-test all proxies
```

`check` only re-tests proxies whose last successful check is older than `test.reValidateAfter` (default `6h`); `-force` ignores the window. Fresh proxies keep their place; expired ones are replaced or dropped. A pass where nothing survives writes nothing, since an empty list would send every request direct. That is the same pass `serve` runs for you [in the background](../proxies.md#re-checking-the-pool-default-every-6h).

`keygen` mints a client api key, appends it to `~/.noderouter/client-keys.txt` (mode 0600) and prints the config line plus the `export` / curl snippet. `-n` mints several at once, `-label` tags each line, `-out` writes to another file. Keys live in the file rather than the config, so adding or revoking a client is a line edit with no restart — `auth.apiKeys` reads the file per request. See [Auth](configuration.md#auth).

`serve` flags: `-config`, `-host`, `-port`, `-no-proxy`, `-v`. The config path can also come from `$NODEROUTER_CONFIG`.

On startup `serve` prints its two schedules, the harvest refresh and the background re-check, and logs each pass it runs:

```
noderouter dev listening on http://127.0.0.1:32256
  config   ~/.noderouter/models.json
  proxies  131 proxies from ~/.noderouter/ports.txt (mode rotate, 0 benched)
  refresh  0 0 * * * (next 00:00:00)
  status   ~/.noderouter/status.json
  recheck  every 6h (next 06:00:00)
  point your client at: base_url = http://127.0.0.1:32256/v1
```

```
level=INFO msg="proxy re-check scheduled" every=6h0m0s next=...
level=INFO msg="re-checking proxies" due=154 fresh=0 window=6h0m0s
level=INFO msg="proxy pool re-checked" alive=131 dropped=23 tested=154 took=1.2s
```

`serve -v` adds the debug lines around it. The next run of either is on `GET /noderouter/status` as `nextRefresh` and `nextRecheck`.

The `/noderouter/*` HTTP routes (`status`, `routes`, `refresh`, `reload`) are listed in the [README](../README.md#endpoints).

The request history behind the public status page (`GET /status`, and the same numbers on `GET /v1/status`) is kept in `~/.noderouter/status.json`, so restarting the router keeps the 7-day window. It is written when it changes — every `status.saveSeconds` (30s by default) and once more at shutdown — and pruned to the window on every write. Deleting the file clears the history; `"status": { "enabled": false }` keeps it in RAM only. See [Status history](configuration.md#status-history).

**Upgrading from a build that counted wider.** The counting scope is now `/v1/*` minus the local `/v1/models` and `/v1/status` routes: website pages, `/healthz`, `/noderouter/*` and non-`/v1` forwards are no longer recorded. An existing `status.json` keeps the hours it already counted, on the older and wider basis, and nothing repairs that on load — unlike the double-count format (above), the older hours cannot be rescaled, because what they include is what the new scope deliberately drops. For the first 7 days after the upgrade the status page therefore shows old hours as much busier than new ones, by roughly your share of non-`/v1` traffic; the mixed window ages out as the 7-day window rolls. If you would rather have seven comparable days than a mixed one, stop the router and delete `status.json` before the first start of the new build — the cost is the history itself.

**Two request counts, on purpose.** `GET /noderouter/status` reports `requests`, and the public status page reports `total`; they are measured differently and will not match. `requests` counts every request the process forwarded on any path (`/v1/*`, non-`/v1` aliases), for the lifetime of the process, and it doubles as the pool seed, so it is taken before auth or routing can reject a request. `total` counts the 7-day health window on the narrower scope above — `/v1/*` minus the local `/v1/models` and `/v1/status` routes. `requests` being higher is expected: one is *how much work this process did*, the other is *how the LLM API behaved over the week*.

## Exposing it with nginx

Behind a reverse proxy, keep the router on its default `127.0.0.1:32256` and let nginx be the only thing on the network: nginx terminates TLS, and the API key stays behind a loopback hop. A ready config is in [`nginx.conf.example`](../nginx.conf.example). Copy it to `/etc/nginx/conf.d/noderouter.conf`, point the certificate and `server_name` at yours, and clients use `https://llm.example.com/v1`.

Nothing about the router is nginx-specific: it is a plain HTTP/1.1 server that keeps no state, so any reverse proxy (Caddy, Traefik, HAProxy, a cloud load balancer) works the same way.

Four things break if you copy a generic config; the example handles all four:

- **Buffering.** `proxy_buffering off` is required. With it on, a streamed completion is held until the model finishes, or the connection stalls: the router flushes each chunk, and nginx would not.
- **Timeouts.** nginx's 60s default read timeout cuts off a slow completion mid-answer. The router itself has no write timeout, so the ceiling is nginx's.
- **Body size.** nginx rejects anything over 1MB by default, which quietly breaks image and long-context requests before the router ever sees them. Keep it at or above `server.maxBodyMb`.
- **Headers.** The router appends its own hop to `X-Forwarded-*` rather than replacing them, so the chain `client, nginx, router` reaches the provider intact and it can still see the real client IP and the original `https` scheme.

The router answers forwarded requests only for hosts it knows - loopback, the listen `host`, and `server.allowedHosts` - which is what stops a page that resolves its own domain at the router (DNS rebinding) from borrowing the credentials behind it. nginx's `proxy_set_header Host $host;` passes the public name through untouched, so put that same name in `server.allowedHosts` once:

```json
"server": {
  "allowedHosts": ["llm.example.com"]
}
```

See [the host allowlist](configuration.md#the-host-allowlist) for the rest of the rule.

`server.host` can also be set to `0.0.0.0` to skip nginx entirely, but then the router serves plain HTTP on every interface with no TLS and no auth of its own. If you do that, the router logs a warning at startup and you should set `auth.apiKeys` (or the single-key `auth.token`); otherwise anyone who can reach the port can spend your key. See [Auth](configuration.md#auth).

To put it behind a different proxy, the same rules apply: turn off response buffering, raise the read timeout above your longest completion, allow request bodies at least as large as `server.maxBodyMb`, and pass `X-Forwarded-For` / `X-Forwarded-Proto` through instead of rewriting them.

## Development

```bash
go test ./...          # unit tests plus an end-to-end forward through a fake proxy
go test -race ./...
gofmt -l . && go vet ./...
```

Files: `config.go` (schema, defaults, model routing), `router.go` (the HTTP server), `pool.go` (the ports file and selection), `harvest.go` (the converter), `provider.go` with `provider_*.go` (one self-registering module per free-proxy source), `cron.go` (the refresh schedule), `dial.go` (transports, socks5), `main.go` (CLI).