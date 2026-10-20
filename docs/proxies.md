# The proxy list

`proxy.file` (default `~/.noderouter/ports.txt`) is a plain text file, one proxy per line, fastest first. All of these parse:

```
http://1.2.3.4:8080
socks5://user:pass@5.6.7.8:1080
ip:port
ip:port:user:pass
```

Blank lines, `#` comments, and duplicates are ignored. A sidecar `ports.meta.txt` (the list name with `.meta` in the middle) holds each proxy's latency and exit IP. The list file wins if they disagree.

`http`, `https`, and `socks5` proxies are supported (socks5 dialling is built in). socks4 lines are parsed and skipped with a note.

## Where the proxies come from

`proxy.sources` is a list, and **every entry is fetched on each refresh, merged into one deduplicated pool, tested together, and written to a single `ports.txt`**. Nothing is kept separate per source, so one dead source does not cost you the others.

| entry | |
|-|-|
| `proxyscrape` | the ProxyScrape free list, filtered by response time |
| `geonode` | the Geonode list, sorted by response time |
| `chillyproxy` | the [ChillyProxy](https://chillyproxy.com) free list, one request per protocol, since its plain-text output omits the scheme |
| `freeproxydb` | the [FreeProxyDB](https://freeproxydb.com) search API, filtered by the requested protocols |
| `roundproxies` | the [RoundProxies](https://roundproxies.com) free list, most-recently-checked first, low-uptime slice |
| `iplocate` | [iplocate/free-proxy-list](https://github.com/iplocate/free-proxy-list) `all-proxies.txt`, a large mixed http/socks5 list |
| `file` | re-test the local `proxy.file` |
| `private` | load your own proxies from `proxy.privateFile` (see below) |
| any `https://…` URL | any raw list of one-proxy-per-line; parsed with the same rules |

Each row is a self-contained provider. `proxy/provider_*.go` registers the names it answers to and the code that fetches it. A refresh resolves `proxy.sources` through that registry, not a hardcoded list. A new upstream is one new file — the fetch loop and the "unknown source" hint pick it up with no other change.

So a refresh of the default config pulls from all five and tests the union. One real run of `convert -protocol all` merged 150 + 150 + 1654 candidates, dropped 36 cross-source duplicates, and discarded 74 undialable socks4 entries. It kept the 254 that answered, 153 of them socks5. That is the main reason the iplocate list is worth including.

Each refresh **replaces** the file rather than appending to it, so proxies that died since last time drop out. `limit` caps the two API sources only; raw lists are taken whole. Use `test.keep` to save just the N fastest.

A source that fails is logged and skipped; the rest of the refresh still runs.

`proxy.protocol` filters every source. `http` keeps `http`/`https` entries. `all` (the default above) also takes `socks5`. socks4 is parsed but never dialled by this build, so those lines are dropped.

### Your own proxies

`proxy.privateFile` (default `~/.noderouter/ports-private.txt`) holds your own proxies, one per line, same format as the main list — for example `http://user:pass@1.2.3.4:8000` or `socks5://user:pass@5.6.7.8:1080`. Add `private` to `proxy.sources`, or run `noderouter convert -private`, and the entries are fetched, **tested with the same URLs and timeouts**, and merged into `ports.txt` next to the free proxies.

A private proxy that fails the test stays in `ports-private.txt` untouched — it is just not promoted into `ports.txt`. Private entries beat a free proxy on the same host:port and are never cut by `test.keep`. Since your own proxies carry credentials, the written `ports.txt` is tightened to `0600`: treat it as a secret.

## Refreshing

Proxies are refreshed on the `proxy.refresh` cron expression (`0 0 * * *`, i.e. **00:00 every day**), unless you change it. Also supported: `*/30 * * * *`, `@daily`, `@midnight`, `@hourly`, `@weekly`, `@monthly`, `@yearly`, and `@every 2h` for testing.

A refresh fetches from the sources, tests them all concurrently, writes the list, and swaps it in without a restart. If the fetch fails, the old list stays in place.

**Starting the router never fetches or tests.** It only reads the list file. To fill or refresh the pool, run `noderouter convert`, wait for the `00:00` refresh, or `POST /noderouter/refresh`.

### Re-checking the pool (default: every 6h)

Between two midnight refreshes the pool ages: free proxies die by the minute, and a list that was 254 strong at `00:00` can be half dead by lunch. So the router also runs `noderouter check` for you in the background, every `proxy.test.reValidateAfter` (**6h** by default):

```
11:43:07 level=INFO msg="re-checking proxies" due=2 fresh=0 window=6h0m0s
11:43:07 level=INFO msg="testing proxies" count=2 workers=50 timeout=2s
11:43:07 level=INFO msg="proxy check" done=50/50 alive=17
11:43:07 level=INFO msg="proxy pool re-checked" alive=131 dropped=23 tested=154 took=1.2s
```

A re-check fetches **nothing**. The list on disk is both the input and the output, so the ranking, `test.keep`, and your private proxies survive it untouched; only the entries whose last successful check is older than the window are dialled, the ones that answer get a fresh `checkedAt` and latency, and the ones that do not are dropped. A pass where every entry failed keeps the previous list rather than writing an empty file — an empty `ports.txt` reads as "no proxies" and the router would quietly go direct.

The schedule is printed at startup and reported as `nextRecheck` on `GET /noderouter/status`:

```
  refresh  0 0 * * * (next 00:00:00)
  recheck  every 6h (next 11:43:07)
```

Shorten the window and the schedule follows: `"reValidateAfter": "90m"` re-checks every 90 minutes, and `"0"` re-tests everything on each pass. The re-check is skipped while `proxy.enabled` or `proxy.test.enabled` is off, and it never overlaps a refresh.

Independently of the schedule, the file is re-read every `watchSeconds` (30) when its mtime or size changes. Running `noderouter convert` by hand — or editing the file — takes effect immediately.

## Proxies that intercept TLS

A large share of free proxies man-in-the-middle `https` and present their own certificate. Go refuses those, correctly, and so does `curl`: a request through one fails with `x509: certificate relies on legacy Common Name field`.

If you want the router to use them anyway:

```json
"provider": { "P": { "options": { "insecureSkipVerify": true } } },
"proxy": { "test": { "insecureSkipVerify": true } }
```

The second one is effectively required. The tester verifies TLS too, so without it those proxies are thrown away as dead and never reach the pool. The router logs a loud warning on every request while this is on.

With it on, the proxy can read your API key, your prompts, and your completions. It is a reasonable setting for a throwaway provider key and a bad idea for a real one. Off unless you say so, per provider.

## Selection, retries and provider opt-in

**Selection** (`proxy.mode`):

- `rotate` (default): round robin, retries walk to the next proxy.
- `first`: always start with the fastest entry, fall forward on failure.
- `random`: a random entry per request.

A proxy that fails 3 times is benched for 5 minutes. A failed request is retried on the next proxy, and a provider that answers with a 429 or a 5xx is retried too. The budget for both is [`retry.maxAttempts`](configuration.md#retries) — 10. The pool is only bypassed for a direct connection when it is empty.

Only forwarded `/v1/` requests may use the pool, and a provider must explicitly opt in. Non-`/v1/` passthroughs and local `/v1/models` routes always connect locally or directly:

```json
"provider": {
  "Zen": { "options": { "re-proxy": true } }
}
```

Omitting `re-proxy` or setting it to `false` keeps that provider on a direct connection.

## Testing options

The `proxy.test` keys (`enabled`, `workers`, `timeoutMs`, `urls`, `keep`, `insecureSkipVerify`, `reValidateAfter`) are listed in [Configuration](configuration.md#proxy-test-options).
