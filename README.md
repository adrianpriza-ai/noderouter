<div align=center>

![noderouter](docs/banner.png)

# noderouter

</div>

A small Go router that sits between your LLM client and one or more providers. It serves an OpenAI-compatible `/v1` endpoint on a local port, picks the provider from the model name in the request, and sends every request through a rotating pool of free proxies that refreshes on a schedule (midnight by default).

No dependencies outside the standard library.

```text
client ──▶ noderouter :32256 ──▶ http://proxy-N:port ──▶ provider /v1
              │  reads model from the body,
              │  rewrites the model id, injects the api key
              └── reads ~/.noderouter/ports.txt, refreshed on a cron schedule
```

## Features

- **Local OpenAI-compatible endpoint.** One port exposes `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, etc., so existing clients and SDKs keep working.
- **Model-based provider routing.** Resolves model names to the right provider, including renamed ids, namespaced ids, and round-robin across a unified model group.
- **Endpoint pools.** Providers can list several upstream `baseURLs` spread by `endpointStrategy` (`rotate | primary | first | random`), sharing load and retrying on failure.
- **Endpoint health.** Failed endpoints are benched; a background probe returns them to the pool when they recover.
- **Rotating proxy pool.** Requests rotate through tested free proxies with retries across proxies and backends.
- **Scheduled proxy refresh.** Proxy list fetched, tested, and saved on a cron schedule; on-demand refresh from `POST /noderouter/refresh`.
- **Chat ↔ Responses translation.** Providers with `surface: "responses"` are reachable from either endpoint; request/response rewriting between `/v1/chat/completions` and `/v1/responses`, streaming included.
- **Chat → Anthropic translation.** Providers with `api: "anthropic"` are reachable from `/v1/chat/completions`: the request is rewritten onto `/v1/messages` (system, tools, JSON mode, thinking budgets) and the answer comes back as a chat completion, streaming included.
- **Per-provider proxy control.** Global `proxy.enabled` is master; only providers with `"re-proxy": true` use the proxy pool.
- **Body budget and memory limits.** Soft GC ceiling plus hard body-budget backstop keep buffered request bytes from blowing up the process.
- **Standard-library only.** No external runtime dependencies; built from the Go toolchain alone.

## Quick start

```bash
go build -o noderouter .

./noderouter init                 # writes ~/.noderouter/models.json
./noderouter convert              # fetch + test proxies -> ~/.noderouter/ports.txt
./noderouter keygen               # mint a client api key (auth.apiKeys)
./noderouter                      # serve on 127.0.0.1:32256
```

Point your client at the router:

```bash
export OPENAI_BASE_URL=http://127.0.0.1:32256/v1
export OPENAI_API_KEY=whatever        # the router swaps in the real key
```

or with the OpenAI Python SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:32256/v1", api_key="anything")
response = client.chat.completions.create(
    model="llm/minimax-m3",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)
```

The listen port is **32256**, deliberately off the usual 3000/5173/8000/8080 rails. Override it with `server.port` in the config or `-port` on the command line (`-port 0` picks any free port and prints it).

Whatever your provider exposes works: `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, etc. Streaming is passed through chunk by chunk; the router adds `X-Noderouter-Proxy` to response headers so you can inspect which hop served it.

**Provider-specific endpoints.** The router forwards requests path-through; an endpoint only works when the upstream provider exposes it. `/v1/moderations` works with Mistral and OpenAI. `/v1/ocr` works with Mistral only. `/v1/videos/generations` is what OpenAI-compatible gateways use — OpenAI answers at `/v1/videos`. `/v1/audio/transcriptions` takes whisper-style `multipart/form-data` uploads; the model id rides in a form field, so routing and renames work on uploads the same as on JSON bodies.

Every path passes through untouched except the translated pairs. The pair `/v1/chat/completions` ↔ `/v1/responses` is rewritten when a provider declares [`surface`](docs/configuration.md#two-openai-surfaces); the pair `/v1/chat/completions` → `/v1/messages` is rewritten for a provider that declares `api: "anthropic"`. In both cases the router rewrites the request, the path and the answer (streaming included), so clients and providers that speak different dialects can seamlessly meet.

The pool is maintained without you: the `proxy.refresh` cron refills it from the sources, and every `proxy.test.reValidateAfter` (6h by default) the router re-checks what is already on disk — the same pass `noderouter check` runs — so proxies that died since the last refresh drop out. Both schedules are printed at startup and reported on `GET /noderouter/status`. See [the proxy list](docs/proxies.md#re-checking-the-pool-default-every-6h).

## Documentation

| Guide | Description |
|-|-|
| [Configuration](docs/configuration.md) | Every key in `~/.noderouter/models.json`: providers, API keys, server limits, model routing, and proxy testing. |
| [The proxy list](docs/proxies.md) | Where the free proxies come from, how they are tested and refreshed, TLS interception, selection, and retries. |
| [Operations](docs/operations.md) | CLI commands, running behind reverse proxies (like nginx), and the architecture layout. |

## Endpoints

| Route | Description |
|-|-|
| `GET /` | Quick index |
| `GET /healthz` | Liveness check |
| `GET /v1/models`, `GET /models` | Configured models in OpenAI shape with limits and modalities. IDs are unique (renamed with `as`, or auto-namespaced). The models website in a browser, JSON to API clients. |
| `GET /v1/models/{id}` | Single model details by any reaching ID |
| `POST /v1/images/edits` | Multipart image editing requests. Configure an edit-capable model with `kind: "image_edit"`, or add `"image_edit"` to its `kinds`. |
| `GET /status`, `GET /v1/status`, `GET /v1/status/` | Public status: request outcomes for the last 7 days, per hour, per model and provider. Only the exact versioned paths `GET /v1/status` and `GET /v1/status/` are local public status routes; deeper `/v1/status/*` paths use normal upstream forwarding and can consume provider quota. The local status routes render the status website in a browser and return the JSON snapshot to API clients; `?format=json` forces the feed. |
| `GET /noderouter/status` | Uptime, counters, pool state, next scheduled refresh and re-check |
| `GET /noderouter/routes` | Resolved model → provider routing table |
| `POST /noderouter/refresh` | Fetch, test, and save proxies immediately |
| `POST /noderouter/reload` | Re-read the proxy list file |
| `/*` (everything else) | Forwarded upstream according to model resolution |

Set `auth.apiKeys` (or the single-key `auth.token`) to require a client api key on forwarded requests - `noderouter keygen` mints one, writes `~/.noderouter/client-keys.txt` and prints the snippet to hand over: accepted as `Authorization: Bearer …`, `x-api-key`, or `X-Noderouter-Token`, with any key from the pool. With neither set, no key is checked - any bearer, or no header at all, is accepted. Set `auth.adminToken` to require a token on the `/noderouter/*` routes - the api keys are accepted there too, so one credential can cover both. Do this if anything beyond loopback can reach the port.

Independently of the key gate, a cross-origin request from a browser is refused with a 403 so no web page can drive an unauthenticated router into spending a provider key. The check compares `Origin` against `Host`, which browsers keep under their own control; a front proxy must pass the original host through (`proxy_set_header Host $host;` in the shipped [nginx example](nginx.conf.example) does exactly that).

DNS rebinding defeats that by making the attacker's own domain resolve to the router, so the page's origin *is* the router's host and the request passes as same-origin. So forwarded requests are also answered only for hosts the router knows: loopback, the listen host, and the names in `server.allowedHosts`. A deployment reached by a public name lists it once in `server.allowedHosts`; loopback clients, curl and SDKs need nothing.

## Note

Free proxies are slow, occasionally log traffic, and may drop connections without warning. The router exists to make that trade-off survivable and automated, not invisible. Treat the proxy pool as best-effort.
