# llm-pico

A lightweight LLM proxy that exposes an OpenAI-compatible API and routes each
request to a configured upstream provider. No database, no dashboard: the whole
instance is defined by four files.

Routes:

- `POST /v1/chat/completions` (streaming supported)
- `POST /v1/embeddings`
- `POST /v1/audio/speech` (TTS), `POST /v1/audio/transcriptions` (STT)
- `GET /v1/models`, `GET /v1/models/{id}` (canonical names only)
- `GET /health`

## Config files

Four YAML/dotenv files in the config directory (default `.`):
`keys.env`, `provider.yaml`, `models.yaml`, `aliases.yaml`. `aliases.yaml` is
optional; the other three are required. Anything invalid aborts startup with an
error naming file and line.

### `keys.env`

```dotenv
PROXY_KEYS=sk-house-1,sk-house-2        # client bearer keys the proxy accepts
OPENAI_1_KEY=sk-...                     # upstream credentials, PROVIDER[_SLOT]_KEY
```

`PROXY_KEYS` is a comma list. The names of other variables are matched against
the `keys:` references in `provider.yaml`. See `examples/` for a complete set.

### `provider.yaml`

```yaml
providers:
  openai-1:
    adapter: openai             # openai | anthropic | gemini | openai-compat
    base_url: https://api.openai.com/v1   # optional for native adapters; openai-compat requires it
    keys: OPENAI_1_KEY, OPENAI_TEMP3_KEY  # first = primary, rest = rotation pool
  local-whisper:
    adapter: openai-compat
    base_url: http://127.0.0.1:8080/v1
    keys: WHISPER_PROX_1_KEY
    ctx: 8192                   # optional per-slot context override, beats models.yaml ctx
```

`openai-compat` speaks the OpenAI protocol against any compatible endpoint
(relay, vLLM, whisper.cpp).

### `models.yaml`

```yaml
gpt-4o-mini:
  kind: chat            # chat | embeddings | tts | stt
  providers: openai-1   # order = priority, implicit fallback
  ctx: 128000
  rpm: 300
  rpd: 10000
  tpm: 500000
  tpd: 10000000
```

A model's `kind` determines which endpoints serve it and which limit fields
apply:

| Field              | Applies to        | Counts |
|--------------------|-------------------|---------------------------------------------------|
| `rpm`              | chat, embeddings, tts, stt | requests per minute                     |
| `rpd`              | chat, embeddings, tts, stt | requests per day                        |
| `tpm` / `tpd`      | chat, embeddings  | tokens (est. body/4 + max_tokens; reconciled to provider-reported usage) |
| `cpm` / `cpd`      | tts               | characters of the request `input` (per minute/day) |
| `daily-audio-sec`  | tts, stt          | audio seconds per day (tts: `len(input)/15` estimate generated; stt: `~32 kB/s` estimate of the upload) |
| `audio-min`        | stt               | audio minutes per day, same estimate as above rounded up |

### `aliases.yaml`

```yaml
gpt-mini: gpt-4o-mini   # alias -> canonical model name; chains are rejected
```

Aliases resolve at load. `/v1/models` lists canonical names only.

## Semantics

**Context.** A request whose estimated input plus `max_tokens` exceeds a slot's
context budget skips that slot; `ctx` comes from models.yaml, and the
per-slot `ctx` in provider.yaml overrides it. If no slot's budget can hold the
request, the proxy answers 400 with the OpenAI-style
"This model's maximum context length is N tokens" message and nothing reaches
upstream.

**Failover.** Slots are tried in declared priority order. Within a slot, keys
rotate round-robin; failing keys enter cooldown, and a circuit breaker opens if
the slot's upstream keeps failing. An upstream 429, 5xx, or a fully cooled-down
slot moves the request to the next slot. Caller errors (bad request, unknown
field, kind mismatch) stop immediately.

**Quota.** Every window above is enforced per model **and** per provider slot
from one in-memory sliding-window limiter, so exhausting one slot's `rpd` never
drains another slot's allowance. A hit on the proxy's limiter returns 429 with
a `rate_limit_exceeded` body, `Retry-After` (window end), and
`X-RateLimit-*` headers with remaining values. A hit on the upstream itself is
failover, not an error.

**Auth.** Bearer keys are checked against the `PROXY_KEYS` hash set. There is
no admin surface, no per-key configuration, and no telemetry storage. Optional
HMAC-SHA256 request signing is implemented but no file setting enables it yet.

## Run

Download the binary from the [releases page](https://github.com/2kfi/llm-pico/releases)
or build it:

```bash
go build -o llm-pico ./cmd/llm-pico
```

First run: the binary writes itself a starter configuration (embedded, so it
needs no repository checkout). `-init` never overwrites existing files, prints
the generated client key once, and leaves one placeholder upstream key to
fill in:

```bash
./llm-pico -init myconfig       # starter config, fresh PROXY_KEYS
# edit myconfig/keys.env: put your OpenAI API key in OPENAI_MAIN_KEY
./llm-pico -config-dir ./myconfig -port 4000
```

Flags: `-config-dir` (default `.`), `-init <dir>`, `-host`, `-port`
(default 4000), `-verbose` (debug logs), `-version`.

Startup validates and loads all files first, then probes each configured slot
in the background: probe failures are logged, never fatal, and retried with
backoff. `SIGHUP` reloads all config files and swaps the routing table without
touching in-flight requests; a failed reload keeps the previous configuration.
On shutdown (SIGINT/SIGTERM) the listener closes and streaming responses drain
before exit.
