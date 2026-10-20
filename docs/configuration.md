# Configuration

`~/.noderouter/models.json`. The `provider` section is the format you already use; `server`, `proxy`, `auth`, `routing` and `routes` are noderouter's own and all optional.

**[`noderouter.jsonc`](../noderouter.jsonc) is the config to copy**: every key, the providers, and the models they expose.

```bash
cp noderouter.jsonc ~/.noderouter/models.json   # or: noderouter init
```

The example is JSONC as it stands: `//` and `/* */` comments and trailing commas are accepted in the config file, so the copy above works verbatim. Comment markers inside a string are left alone — a `baseURL` of `"https://example.com/v1//x"` keeps its slashes.

## Provider options

| key | meaning |
|-|-|
| `baseURL` | upstream root. A path here joins with the request path without duplicating a shared prefix (`…/v1` + `/v1/chat/completions` = `…/v1/chat/completions`). |
| `baseURLs` | optional pool of upstream endpoints for this provider; requests are spread across them and a retry moves to the next one. Takes every form `apiKeys` takes (array, one string, `file:`), and wins over `baseURL` when both are set. See [Several endpoints](#several-endpoints). |
| `endpointStrategy` | which endpoint of the pool a request uses: `rotate` (default), `primary`, `first`, or `random`. The first three walk to the next endpoint when an attempt fails; what differs is where a request starts. See [Choosing an endpoint from the pool](#choosing-an-endpoint-from-the-pool). |
| `apiKey` | sent as `Authorization: Bearer …`, replacing whatever the client sent. Three forms: a literal key (`"sk-…"`), `"file:/path/to/key"` (read from disk on every request, so rotating it needs no restart), or empty (`""`) for a keyless endpoint. A keyless endpoint takes no credential, so neither the client's `Authorization` nor an empty Bearer header is sent - a client that had to invent a key to satisfy its own SDK does not thereby get it judged by the provider. |
| `apiKeys` | optional pool; requests are spread across the keys. Takes any of the forms below. |
| `keyStrategy` | which key of the pool a request uses: `rotate` (default), `first`, or `random`, the same three `proxy.mode` offers, per provider. See [Several api keys](#several-api-keys). |
| `headers` | extra headers set on every upstream request. |
| `userAgent` | replaces the `User-Agent` sent to this provider. Some upstreams filter or refuse requests that carry no browser-shaped one - Go's own `Go-http-client/1.1`, or whatever SDK the client used - and `headers` is a clumsy spelling for one well-known field. Empty (the default) forwards the client's; a `User-Agent` entry in `headers` still wins. |
| `timeoutSecs` | per-provider override of the upstream timeout. |
| `re-proxy` | explicitly route this provider's forwarded `/v1/` requests through the proxy pool (`true`); omitted or `false` connects directly. Non-`/v1/` passthroughs and local `/v1/models` routes never use the pool. `proxy.enabled` remains the master switch. |
| `id` / `aliases` | short names used by the `"<alias>/<model>"` form. |
| `surface` | which of the two OpenAI wire formats the provider speaks: `"chat"` (default) or `"responses"`. Independent of `api`, which only decides how the provider is authenticated. See [Two OpenAI surfaces](#two-openai-surfaces). |
| `insecureSkipVerify` | skip TLS verification on the upstream hop. See [The proxy list](proxies.md#proxies-that-intercept-tls). Off by default. |

Per model, under `models`:

| key | meaning |
|-|-|
| `as` | rename the model id. It becomes the id `/v1/models` advertises and the id a request has to name to reach the model, while the config key stays what goes upstream unless `upstreamName` says otherwise. See below. |
| `upstreamName` | the id the provider expects, when it differs from the config key. |
| `kind` | the model's primary category: `chat` (default), `image`, `image_edit`, `systemone`, `embedding`, `audio`, `tts`, `stt`, `ocr`, `moderation`, `video`. Left out, it is inferred from the output modalities. The kind is what `/v1/models` advertises and the models site renders, and each specialist kind names the endpoint the model answers on: `/v1/ocr`, `/v1/moderations`, `/v1/images/generations`, `/v1/images/edits`, `/v1/videos/generations`, `/v1/audio/speech` (tts), `/v1/audio/transcriptions` (stt) and so on. Requests are still forwarded path-through, so the upstream decides what it accepts. `tts` and `stt` accept the usual spellings (`text-to-speech`, `whisper`, `transcription`, `speech-to-text`, `asr`); `audio` remains the umbrella kind, answering at `/v1/audio/speech`. |
| `kinds` | optional additional capabilities. Use `"kind": "image", "kinds": ["image_edit"]` for a model that supports both `/v1/images/generations` and `POST /v1/images/edits`. The primary `kind` stays first, and `/v1/models` reports the complete capability list plus an `accepts` list of endpoints. `image_edit` is media-bearing even without image modalities, so multipart image and mask uploads use `server.maxMediaBodyMb`. |
| `limit` | `{ "context": …, "output": … }`: the input window and the max output tokens, as advertised by `/v1/models` and shown on the models site (a non-chat card reads them as `input` and `max output`). |
| `modalities` | `{ "input": [...], "output": [...] }` from `text`, `image`, `audio`, `video`, `file`. An image/file input — or an `image_edit`, `ocr`, `video` or `stt` kind, whose payload is the media itself — is what opens the larger `server.maxMediaBodyMb` body ceiling for the model. |

## Several api keys

`apiKeys` is a pool: each request takes the next key, round robin, so a quota or a rate limit is shared instead of hit by every request. The first three below are the same two-key pool, and the fourth mixes literals with a file:

```json
"apiKeys": ["sk-…91", "sk-…92"]              // an array
"apiKeys": "  sk-…91   sk-…92  "             // one string, spaces or tabs
"apiKeys": "sk-…91\nsk-…92\n"                // one string, one per line
"apiKeys": ["sk-…91", "file:~/keys.txt"]     // literals mixed with files
```

Whitespace and newlines separate keys, surrounding space is trimmed, and blank lines plus `#` comments are dropped, so a block pasted straight out of a `.env` file or a key file works as it stands.

**Keys on disk.** Any entry may be `"file:<path>"` instead of a key, read from disk on **every** request, so adding, removing or rotating a key needs no restart:

```json
"apiKeys": "file:~/.noderouter/zen-keys.txt"
```

```bash
# ~/.noderouter/zen-keys.txt: one key per line, # comments allowed
sk-…91
sk-…92
```

A key file holds a **pool**, not a single key: every key in it joins the round robin alongside your literal ones. An unreadable or empty file contributes no keys rather than failing the request, so a pool that is missing one file still serves the rest, and a provider left with no keys at all degrades to keyless instead of erroring. Set `apiKey: ""` on purpose if that is what you want.

`apiKey` takes the same forms, so `"apiKey": "sk-…91 sk-…92"` is a pool too; `apiKeys` is just clearer. When both are set, `apiKeys` wins and `apiKey` is ignored.

### Choosing keys from the pool

**Strategy** (`keyStrategy`), the same choice [`proxy.mode`](proxies.md#selection-retries-and-opting-out) offers for proxies:

- `rotate` (default): round robin, one key per request, wrapping at the end.
- `first`: always the first key in the pool. Useful when one key is the primary and the rest are spares you only want to touch by hand.
- `random`: a random key per request, which spreads a burst better than round robin does.

```json
"options": {
  "baseURL": "http://example.com/v1",
  "apiKeys": "sk-…91 sk-…92",
  "keyStrategy": "rotate"
}
```

Per provider, so a gateway can rotate while a single-key endpoint stays on `first`.

One difference from proxies: a key that turns out to be spent or rejected does not fail forward on its own. The router retries hop failures, not a `401`, so with `rotate` the next request moves to the next key rather than the failed one retrying in place. The retry lands on whatever key the strategy picks next. Anything unrecognised falls back to `rotate` instead of failing the config, the same way `proxy.mode` does.

## Several endpoints

`baseURLs` is a pool of upstream endpoints for one provider, spread the way [`apiKeys`](#several-api-keys) spreads keys: each request takes the next endpoint, round robin, and a retry after a dead or rate-limited endpoint takes the one after that. Several mirrors of one backend then share the load instead of one taking every request, and one of them going down costs an attempt rather than the whole request.

It accepts the same shapes. The first two below are the same two-endpoint pool, and the third puts it on disk:

```json
"baseURLs": ["https://a.example.com/v1", "https://b.example.com/v1"]   // an array
"baseURLs": "https://a.example.com/v1\nhttps://b.example.com/v1"        // one string, one per line
"baseURLs": "file:~/.noderouter/zen-endpoints.txt"                      // endpoints on disk, one per line
```

Whitespace and newlines separate endpoints, and blank lines plus `#` comments are dropped, so a list pasted out of a file works as it stands. A `"file:<path>"` entry is reread whenever the file changes, so adding or retiring a mirror needs no restart — the same mtime-cached read the key pool uses.

`baseURL` alone remains the single-endpoint spelling and is untouched. When both are set, `baseURLs` wins, the way `apiKeys` wins over `apiKey` — with one exception: a pool that resolves to **nothing** (a missing or empty file) falls back to `baseURL` rather than leaving the provider with nowhere to send a request. An empty pool is the one failure the router cannot route around: the request is refused with `provider "…" has no baseURL`.

### Choosing an endpoint from the pool

**Strategy** (`endpointStrategy`), the first three being the choice [`keyStrategy`](#choosing-keys-from-the-pool) and [`proxy.mode`](proxies.md#selection-retries-and-opting-out) offer:

- `rotate` (default): round robin, one endpoint per request, wrapping at the end — and a retry takes the endpoint after this one, so a dead endpoint is left behind rather than retried in place.
- `primary`: always the first endpoint, with the rest spent only when the primary fails: the request starts at `baseURLs[0]`, and a retry walks to the next one. Use it when one endpoint is clearly the one you want and the others are there for when it breaks.
- `first`: always the first endpoint, retries included. Useful when one endpoint is the primary and the rest are spares you only want to touch by hand; unlike `primary`, a failing primary keeps the spares out of it.
- `random`: a random endpoint per attempt.

```json
"options": {
  "baseURLs": ["https://a.example.com/v1", "https://b.example.com/v1"],
  "endpointStrategy": "rotate",
  "apiKeys": "sk-…91 sk-…92"
}
```

The two pools are independent: a request picks its endpoint and its key from each, so a four-endpoint, three-key provider spreads requests over twelve combinations rather than pairing them off.

Anything unrecognised in `endpointStrategy` falls back to `rotate` instead of failing the config, the same way `keyStrategy` and `proxy.mode` settle theirs.

Two places report what is in force: `GET /noderouter/status` carries `baseURL` (the endpoint a request starts on) for every provider, plus `baseURLs` and `endpointStrategy` when there is more than one, and `noderouter routes` prints the first endpoint followed by `(+… more endpoints)`.

## Endpoint health

Under `endpoints`. What happens to an endpoint that stops answering: the rest of the pool keeps serving while it is out, and it goes back in the moment it returns. Providers on a single `baseURL` have nothing to rotate to and are left alone.

| key | default | meaning |
|-|-|-|
| `failThreshold` | 3 | consecutive failures that bench an endpoint. **Negative turns benching off.** |
| `benchSecs` | 60 | how long a benched endpoint stays out when no probe brings it back sooner. |
| `recheckSecs` | 60 | how often the benched endpoints are probed. **Negative turns the probes off**, leaving the bench to expire on its own. |

```json
"endpoints": {
  "failThreshold": 3,
  "benchSecs": 60,
  "recheckSecs": 60
}
```

**What counts as a failure** — only what is the endpoint's own fault: a connection that never reached it, with no proxy in the way. Two things deliberately do not count:

- **A provider's own answer.** A 429 or a 500 is an endpoint saying something; it is up even when the model is not. Any answer clears the count and lifts a bench at once, rather than leaving a healthy endpoint out for the rest of its cooldown.
- **A failure behind a proxy.** With `re-proxy`, the hop may have broken at either end, and the proxy pool benches its own side already; blaming the endpoint would empty the pool over one bad proxy.

**While an endpoint is benched** it is skipped when a request picks one — `first` and `primary` notice too, since a benched primary hands the request straight to the spare. When *every* endpoint of a provider is benched, the pool is used whole anyway: trying is the only way to learn that one came back, while refusing the request outright can never serve it at all.

**Probes** are one `GET <baseURL>/models` per benched endpoint per interval, sent **directly** — never through the proxy pool, so a flaky proxy cannot keep a healthy endpoint out of rotation. Any answer at all, a 401 or a 404 included, puts the endpoint back immediately; only silence keeps it benched. `POST /noderouter/refresh` runs the same pass on demand and reports `endpointsRestored`.

`GET /noderouter/status` reports the policy in force under `endpoints` — `failThreshold`, `benchSecs`, `recheckSecs`, and how many endpoints are out right now — and names the benched ones on their provider's row as `benchedEndpoints`.

## Two OpenAI surfaces

OpenAI has two request shapes: `/v1/chat/completions` and `/v1/responses`. They are not spellings of each other: `messages` versus `input`, `max_tokens` versus `max_output_tokens`, a single SSE event stream versus typed events. A provider serves one of them, and a client that only speaks the other one would otherwise get a 404 from the provider.

`surface` tells the router which one the provider speaks. Whichever endpoint the client uses, the model answers.

```json
"options": {
  "baseURL": "https://api.example.com/v1",
  "surface": "responses",
  "apiKey": "sk-…"
}
```

With that set, a chat-completions client reaches the provider on `/v1/responses` with its body rewritten, and the answer comes back as a chat completion. Streaming is translated too, chunk by chunk. The same works the other way: a plain chat provider (no `surface`, the default) reached from a Responses client is rewritten down to `/v1/chat/completions`.

`surface` is **not** the same thing as `api`. `api: "anthropic"` chooses the Messages API and the `x-api-key` header; `surface` chooses between the two *OpenAI* shapes. They are independent, and an `anthropic` provider is always on the chat surface because the Messages API has no Responses counterpart — though it is no longer *only* reachable by chat-shaped clients, since the router translates onto it (see below).

Anything unrecognised in `surface` falls back to `chat` rather than failing the config, the same way `api` and `proxy.mode` settle theirs.

**What it does not touch.** Every other endpoint passes through as it always has: `/v1/embeddings`, `/v1/images/generations`, `/v1/images/edits`, `/v1/videos/generations`, `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/messages`, `/v1/ocr`, `/v1/moderations`. So does `/v1/responses/{id}`, which looks up an answer the provider stored earlier and can only be answered by the surface that stored it.

### Provider-specific endpoints

The router forwards requests path-through, so an endpoint only works when the upstream provider actually exposes it. Several are worth naming:

- **`/v1/images/edits`** (`image_edit`): a multipart upload with a `model`, `prompt`, `image` file and optional `mask` file. Configure an edit-capable model with `kind: "image_edit"`, or add `"image_edit"` to `kinds` alongside `kind: "image"` when the same model supports generation too. The router preserves the multipart parts and forwards the path unchanged.
- **`/v1/moderations`**: works with **Mistral** and **OpenAI**. Both expose the same path and accept the same request shape (`{ "model": "…", "input": "…" }`), so a client can switch between them by changing the model name.
- **`/v1/ocr`**: works with **Mistral only**. No other major provider (OpenAI, Anthropic, Google, Groq, Together, Fireworks, DeepInfra) exposes a dedicated OCR endpoint. Those providers do OCR through their chat/vision endpoints with prompt engineering, not a standalone `/v1/ocr` path.
- **`/v1/videos/generations`**: the video spelling OpenAI-compatible gateways use, with the Sora body (`{ "model": "…", "prompt": "…", "seconds": "…", "size": "…" }`) — `seconds` is a string (`"4"`, `"8"`, `"12"`) and there is no `n`. **OpenAI itself answers at `/v1/videos`**, and Google's Veo on a predict path of its own, so check the path against a provider's docs: the router forwards what the client sent, and a card's `endpoint` says where one provider answers, not where every provider does.
- **`/v1/audio/transcriptions`** (whisper-style `stt`): the upload is `multipart/form-data` — `curl -F file=@audio.mp3 -F model=whisper-1` — with the model id in a form field, not a JSON body. The router reads the model out of the form, so model routing, `as` renames and `upstreamName` rewrites all work on uploads exactly as they do on JSON. A JSON body is accepted too, for gateways that take one. `stt` models clear the media body ceiling from their kind alone, so minutes of audio are not refused at `maxBodyMb`.
- **`/v1/audio/speech`** (`tts`): a JSON body (`{ "model": "…", "input": "…", "voice": "…", "response_format": "mp3" }`) answered with audio bytes, streamed back as they arrive.

**What cannot be translated is refused, never dropped.** A field with no counterpart on the other wire comes back as a 400 naming the field, because answering a question the caller did not ask is worse than not answering:

- **chat → responses** refuses `n`, `stop`, `presence_penalty`, `frequency_penalty`, `logprobs`, `top_logprobs`, `logit_bias`, `audio`, `modalities`, `web_search_options`, `prediction`, `seed`.
- **responses → chat** refuses `previous_response_id`, `conversation`, `background`, `prompt`, `include`, `context_management`, `safety_identifier`, `prompt_cache_key`, and `reasoning.summary` (the one nested field; `reasoning.effort` still crosses as `reasoning_effort`).

Everything else crosses: fields the two wires share by name (`temperature`, `top_p`, `tools`, `tool_choice`, `metadata`, `store`, `stream_options`, …) are copied as they are, the renamed ones are rebuilt under their other name, and a field belonging to neither wire (`top_k`, `enable_thinking`, a gateway's own knob) is passed through untouched, since by definition it is the provider's.

Media is passed through rather than rebuilt: an image, audio, video or file part can be tens of megabytes of base64, so a content type the router has no proven mapping for reaches the provider byte for byte.

An `anthropic` provider reached from a Responses client is a 400 rather than a mistargeted request, naming the endpoints that do work.

### An Anthropic provider, from a chat client

The Messages API is a third dialect, and it gets the same treatment the two OpenAI surfaces give each other. A provider declared with `api: "anthropic"` speaks only `/v1/messages`; a client that speaks chat completions reaches it anyway:

```json
"options": {
  "baseURL": "https://api.anthropic.com/v1",
  "api": "anthropic",
  "apiKey": "sk-ant-…"
}
```

A `POST /v1/chat/completions` for one of its models leaves for `POST /v1/messages` with the body rewritten into the Messages shape — system messages become the top-level `system` array, `tool_calls` and `tool` turns become `tool_use` and `tool_result` blocks, `response_format` becomes a forced JSON tool, `reasoning_effort` becomes a `thinking` budget, and a missing `max_tokens` is filled in with the default the Messages API requires. The answer comes back as a chat completion — text, tool calls, usage, and Anthropic's stop reasons mapped onto `stop`, `length` and `tool_calls` — and a streamed answer arrives as `chat.completion.chunk` frames ending in `[DONE]`, with the usage tail when `stream_options.include_usage` was set. `stream_options`, `include_usage` and the `reasoning_content` field some clients read for a model's thinking all behave as they do against any OpenAI-compatible provider.

The translation is the reference module from `openai-anthropic-endpoint-translation`, copied into the router under `translate/anthropic` with its stream converter written for this router.

Clients that already speak Anthropic are untouched: `POST /v1/messages` is still forwarded path-through, byte for byte, and gets the provider's own answer back. A Responses client against an `anthropic` provider remains a 400 — there is no translation from the Responses wire onto the Messages API.

`GET /noderouter/status` reports such a provider as `"translates": ["chat","anthropic"]`, and `GET /v1/models` lists both `/v1/messages` and `/v1/chat/completions` in the model's `accepts`.

Two places report what is in force, so you do not have to read the config back: `GET /v1/models` carries `"surface"`, `"kinds"` for multi-capability models, and an `"accepts"` list of every router endpoint the model answers on, including a single specialist endpoint. `GET /noderouter/status` lists each provider's `surface` plus `"translates": ["chat","responses"]` (or `["chat","anthropic"]`) when it is reached through translation.

## Server options

Under `server`.

| key | meaning |
|-|-|
| `host` / `port` | where to listen. |
| `allowedHosts` | extra hostnames forwarded requests are answered for, one bare hostname per entry (no port; compared without case). Besides these, only loopback and `host` above are served - anything else gets a 403. |
| `maxBodyMb` | largest accepted request body; the whole body is buffered so retries can replay it. |
| `requestTimeoutSecs` | upstream timeout for one attempt. |
| `memoryLimitMb` | **soft** memory control (default 100). Handed to the Go GC: it collects harder as the heap approaches the limit instead of hoarding freed pages. Never refuses work. An env `GOMEMLIMIT` overrides it. |
| `bodyBudgetMb` | **hard** backstop (default `memoryLimitMb`/3, floor 32MB; negative disables). Caps buffered request-body bytes in flight. A request that would exceed it gets 503 instead of pushing RSS past the limit; small chat requests are never refused. |

A buffered body costs roughly 3× its size in resident memory (the raw read, the model-patched copy, GC slack), so a budget of `memoryLimitMb/3` keeps the worst case under the soft limit. With the defaults, one ~33MB body (or several smaller ones summing to that) can be in flight at once; chat-sized traffic never comes close.

### The host allowlist

Forwarded requests are answered only for a host the router knows: loopback (`localhost`, `127.0.0.0/8`, `::1`, with or without a port), the configured listen `host` when it names an address rather than `0.0.0.0` or `::`, and every entry of `server.allowedHosts`. A request for any other host is refused with a 403 that names the host, and the upstream never sees it.

The list exists because of DNS rebinding. A page on the internet can point its own domain at the router, which makes the browser's `Origin` equal to `Host` and so satisfies the cross-origin gate on forwarded requests - the request then looks same-origin and the response is readable. What the page cannot do is write `Host` (browsers reserve that header) or write `server.allowedHosts`, so declaring the hosts the router answers for turns the rebound name into the one thing the request fails on. Reach the router through a public name - directly, or behind a front proxy that passes the original `Host` through - and that name belongs in the list, once:

```json
"server": {
  "host": "127.0.0.1",
  "allowedHosts": ["llm.example.com"]
}
```

Loopback deployments, curl, and the SDKs need nothing configured. The allowlist gates the forwarded routes, where the provider key is spent; `/healthz`, `/v1/models` and the website are public information and stay reachable from any host.

## Status history

Under `status`.

`GET /status` and the exact versioned paths `GET /v1/status` and `GET /v1/status/` report a rolling 7-day window of request outcomes: how many requests each hour, split per model and per provider, and how many of them failed. Only the two exact versioned paths are local public status routes. A deeper `/v1/status/*` path is forwarded upstream through normal authentication and model resolution, so it can consume provider quota. Only LLM API requests under `/v1/` are counted; the local `/v1/models`, `/v1/status`, and `/v1/status/` endpoints, website routes, health checks, and admin routes are excluded. The local status routes serve the status website to a browser and the raw JSON to API clients; append `?format=json` to force the feed. A request is counted once in the total and once under the model and provider that served it, so a forwarded request is one request everywhere on the page, not one in the total and one in its model's row.

The window is kept in RAM and written to a file, so restarting the router - an update, a config edit, a crash a supervisor picks up - continues the same window instead of starting the counters over.

| key | meaning |
|-|-|
| `enabled` | keep the history on disk (default `true`). `false` keeps it in RAM only, the way the router behaved before the file existed. |
| `file` | where it lives; default `~/.noderouter/status.json`. |
| `saveSeconds` | how often a changed history is rewritten; default 30. A negative value writes only at shutdown. |

```json
"status": { "file": "~/.noderouter/status.json", "saveSeconds": 30 }
```

The file holds counts, not traffic: an hour, a model or provider name, and three numbers - total, succeeded, failed. No prompts, no responses, no keys, no client addresses. It is rewritten only when something changed since the last write, through a temporary file and a rename, so a reader (or a crash mid-write) never sees half a document; a hard kill loses at most the interval since the last write, and an ordinary shutdown flushes first.

The window belongs to the running router, not to the file: retention is 7 days, hours older than that are dropped on save and ignored on load, and a file last written days ago cannot bring back traffic the window has already forgotten. Deleting the file resets the history, which is the way to clear it; `"enabled": false` leaves it alone entirely. The startup banner prints the path in use, next to `config` and `proxies`.

One file belongs to one router. Two instances pointed at the same path each write the whole document, so the second one to flush wins and the first one's traffic between flushes is lost — give each instance its own `status.file`, or leave the second with `"enabled": false`.

The file carries the counting version of what it holds. A file in the older format — written by a build whose global total counted a forwarded request twice rather than once under its model — is corrected once when it is read, and written back in the current format: the startup log says `status history repaired` and how many hours that touched. The correction is exact rather than a guess, because the excess is recoverable from the per-model, per-provider and model+provider counts that were never wrong. A file this build wrote is never repaired.

## Auth

Under `auth`. Everything here is optional.

| key | meaning |
|-|-|
| `apiKeys` | keys a client may present on a forwarded request. The same shapes a provider's [`apiKeys`](#several-api-keys) takes: an array, one whitespace- or newline-separated string, or `"file:/path"` - re-read when the file changes, so revoking one client's key needs no restart. Wins over `token` when both are set. |
| `token` | the single-key spelling of `apiKeys`, in the same forms. |
| `adminToken` | required on `/noderouter/*`, as `X-Noderouter-Token` or `Authorization: Bearer …`. Left unset, the router generates one at startup and prints it, so the management endpoints are never open by accident. Any key from `apiKeys` is accepted here as well, in all three spellings, so a config that only sets `apiKeys` needs no second credential to read `/noderouter/status`. |

A forwarded request carries the key in whichever header its client speaks. All three spell the same key, so each kind of client authenticates without a translation layer:

```text
Authorization: Bearer sk-…      # the OpenAI SDKs
x-api-key: sk-…                 # the Anthropic SDKs
X-Noderouter-Token: sk-…        # curl, and the header the admin routes use
```

```json
"auth": {
  "apiKeys": ["sk-router-…91", "sk-router-…92"],
  "adminToken": "noro_…"
}
```

`noderouter keygen` mints a key, appends it to `~/.noderouter/client-keys.txt` and prints the config line plus the snippet to hand a client - the file, the wiring and the first request in one command.

**With neither `token` nor `apiKeys` set, no key is checked**: any bearer, or no header at all, is accepted. That is the default, and what makes `OPENAI_API_KEY=anything` work against a router on `127.0.0.1`.

A key outside the pool gets 401 before the body is buffered, before anything is forwarded, and without a retry spent on it. The check covers forwarded requests only: `/healthz`, `/v1/models`, the models site and `/docs` stay open so a browser can browse them, and `/noderouter/*` belongs to `adminToken` or to any key from `apiKeys`.

The keys accepted here are for this hop only. The router strips them from the upstream request, so a keyless provider sees the client's own credential or nothing - never the key that opened the router. `X-Noderouter-Token` never leaves the router at all.

A `"file:"` entry that cannot be read contributes no keys, but does not switch the check off: a gate that was configured stays closed rather than failing open on a typo or a bad rotation.

## Retries

Under `retry`.

| key | default | meaning |
|-|-|-|
| `maxAttempts` | 10 | total tries per request, the first included: 10 means nine retries. `1` disables retrying. |
| `backoffMs` | 500 | wait after the first failure, doubling per further failure up to `maxBackoffMs`, plus jitter. Negative retries at once. |
| `maxBackoffMs` | 8000 | ceiling on one wait, and on a provider's `Retry-After`. |
| `statuses` | — | add provider statuses to the ones worth another try. |
| `skipStatuses` | — | take them away again. |
| `totalSecs` | 0 | cap on the wall clock the whole sequence may take. 0 means only the request's own context bounds it. |

```json
"retry": {
  "maxAttempts": 10,
  "backoffMs": 500,
  "maxBackoffMs": 8000
}
```

Every kind of failure draws on the same budget, because none of them has reached the client yet: a dead proxy, a 429, a 503. Each try walks to the next proxy, and to the next key in the provider's pool, so a retry after a rate limit is not the same request to the same exhausted key.

Retried by default: **408, 425, 429, 500, 502, 503, 504, 522, 524, 529**, plus any connection-level failure. Passed straight back: **400, 401, 403, 404, 422**: the same answer arrives nine more times, only slower, and every one of them spends the caller's timeout budget.

When the budget runs out, the client gets the provider's own last response (its status, body and headers) rather than a 502 the router invented. A try that never got an answer at all is a 502 naming the failure.

- **A wait is not wasted on a dead proxy.** A transport failure retries immediately, because nothing reached the provider to be overloaded. Only a provider that actually answered is worth waiting out.
- **`Retry-After` is honoured, but capped** at `maxBackoffMs`, so a `Retry-After: 3600` cannot pin a request open for an hour.

To retry a rejected key too, only worth it when the provider has a key pool for the retry to walk to:

```json
"retry": { "statuses": [401] }
```

`proxy.maxAttempts` is the older name for `maxAttempts` and still works: set it, and the retry budget takes that value. `GET /noderouter/status` reports the policy in force under `retry`, next to the `retries` counter.

## Model routing

Given a `"model"` in the request body, in order:

1. `routes`: an explicit `{"model": "Provider Name"}` entry.
2. An **advertised id**: what `/v1/models` lists. Usually the model key, or its `as` rename. Advertised ids are unique, so this step also decides the clashes below.
3. An exact key in a provider's `models`. That key is what goes upstream, unless the model sets `upstreamName`.
4. `"<alias>/<model>"`: the prefix picks the provider and the rest is the model, so `llm/minimax-m3` reaches provider `LLM Povider` as `minimax-m3`. Matching is on the normalized name, so `llm` matches `LLM Povider`.
5. `routing.defaultProvider`, or the only provider, when the model is unknown. With several providers and no default, an unknown model is a 400 that lists what is available.

A request with no model at all (a plain `GET /whatever`) goes to `routing.defaultProvider`. `noderouter routes` prints the resolved table.

### Re-identifying a model

`as` renames a model id: the rename is what `/v1/models` advertises and what a request must name, while the config key stays what goes upstream.

```json
"models": {
  "big-pickle": { "as": "zen-big-pickle", "name": "Big Pickle" }
}
```

That matters because two providers can expose the same model id, and a `/v1/models` list with `big-pickle` in it twice is a list no client can act on; both entries answer to one id, and a request naming it is ambiguous. Renaming one side is enough: `zen-big-pickle` routes to that provider as `big-pickle`, and the other provider keeps `big-pickle` for itself.

You do not have to rename anything by hand. An id claimed by more than one model is resolved automatically: the model that asked for it with `as` keeps it, and the others are advertised as `"<provider-alias>/<key>"` (`zen/big-pickle`), which resolves like any other id. The bare key then stays a 400 that names the ids that do work, and startup logs what it renamed and how to stop it:

```
model id: provider "Zen" model "big-pickle" is advertised as "zen/big-pickle" because
more than one provider claims that id; set "as" to choose the id yourself
```

## Proxy test options

Under `proxy.test`. These apply to `convert`, to the `00:00` refresh, to `noderouter check` and to the background re-check `serve` runs, never to the router just starting up.

| key | meaning |
|-|-|
| `enabled` | test candidates before saving them. `convert -no-test` saves them all unverified. |
| `workers` | how many proxies are tested at once (50). |
| `timeoutMs` | per-proxy test timeout in ms (2000). |
| `urls` | endpoints to test against. Defaults to two https "what is my IP" services, because the router's upstreams are https. The list is https-only: a proxy that only forwards plain http passes a mixed-url check but cannot tunnel to any provider. |
| `keep` | save only the N fastest that passed (0 = all). |
| `insecureSkipVerify` | also keep proxies that intercept TLS. See below. Off by default. |
| `reValidateAfter` | how long a successful check stays valid (default `6h`). `check` only re-tests proxies older than this, and the router re-checks the pool on this window in the background — see [Re-checking the pool](../proxies.md#re-checking-the-pool-default-every-6h). `0` re-tests everything on every pass. |

The pool these produce, and everything around them (sources, the refresh schedule, TLS interception, selection) is in [The proxy list](proxies.md).
