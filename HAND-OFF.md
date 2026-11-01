# HAND-OFF.md

**Project:** llm-pico — lightweight LLM proxy exposing an OpenAI-compatible API
**Repo:** https://github.com/2kfi/llm-pico (local: `/home/arkan/projects/llm-pico`, Go 1.27)
**Deployment:** `/home/arkan/llm-pico/` — binary `llm-pico-linux-amd64`, server running on port **4400**
**State as of:** Sun Nov 01 2026
**HEAD at writing:** `3ad6808` ("fix: /v1/embeddings and /v1/audio/transcriptions must send the routed model")
**⚠ Uncommitted work in the tree:** Gemini thinking controls + audio/video support + per-model `media:` capability flags (see §7 — it compiles and all tests pass, but it has **not been committed or pushed yet**).

---

## 1. What llm-pico is

A Go proxy that sits in front of many LLM providers (Google AI Studio / NVIDIA NIM /
OpenRouter / Groq / Ollama Cloud / OpenCode Zen) and exposes an OpenAI-shaped HTTP API
(`/v1/chat/completions`, `/v1/embeddings`, `/v1/audio/*`, `/v1/models`, `/health`).
It does: slotted failover, per-slot circuit breakers, round-robin key rotation keyed
by health, per-key rate-limit cooldowns, client-side 429 windows (RPM/RPD/TPM/TPD/CPM/
CPD/audio budgets), request/response streaming, graceful shutdown drain, and hot config
reload on SIGHUP.

Config is **3 required files** in the config dir:

- `keys.env` — `PROXY_KEYS=<client bearer keys>` + upstream keys (`<PROVIDER>_<SLOT>_KEY=<value>`)
- `provider.yaml` — top-level `providers:` map: slot name → `adapter` (`openai`, `anthropic`, `gemini`, `openai-compat`), optional `base_url` (required for openai-compat), `keys:` list of keys.env names, optional `ctx:` override
- `models.yaml` — **caller-facing key** → config; call out:

```yaml
gpt-mini:              # name API callers use /v1/models lists these keys
  kind: chat | embeddings | tts | stt
  model: gpt-4o-mini   # string sent upstream; OMIT to send the key itself
  providers: openai-1, groq   # order IS the failover priority
  ctx: 128000          # context budget; provider.yaml ctx: overrides per-slot
  # media gating (chat only):
  #   media: image, audio, video    # explicit allow-list
  #   media: ""                     # no media at all
  # limits per kind, all optional ints (enforced client-side by the proxy):
  #   chat, embeddings: rpm rpd tpm tpd
  #   tts:              rpm rpd cpm cpd daily-audio-sec   (cpm = chars/month)
  #   stt:              rpm rpd daily-audio-sec audio-min
```

**Vendor-prefixed IDs caveat:** adapters strip one leading `/`-segment from `model`
(OpenAI `owner/model` → `model`), so for upstreams that DO need the full
`vendor/model` id (NIM, OpenRouter, Groq), write `model: /vendor/model` (leading
slash makes the strip land on nothing useful → it strips the `/` prefix and sends
`vendor/model` intact). The `deepseek-ai/...` block in the deployment models.yaml
does exactly this.

`aliases.yaml` was **retired 393f856** — a leftover file makes startup hard-error.

Startup: `llm-pico --config-dir <dir> --host 0.0.0.0 --port 4000 [--verbose]`.
Flags also: `--init <dir>`, `--version`.

---

## 2. Bug-hunt history (Oct 29–30, 2026 — all fixed and pushed)

Commits: `107af4e` (hotfix batch), `393f856` (aliases retirement), `3ad6808`
(routed-model fix). Full detail in the commit messages.

### High severity (fixed)
1. **Model-failure cooldown dead code** — `proxy.Dispatch` fed `slot.Provider` into
   `RecordModelFailure` but `isCoolingDown`/`clearModelFailure` looked up by model
   name → cooldown never fired, maps leaked. Fixed by keying on the model.
2. **Streaming token accounting never reconciled** — `callStreaming` returned a
   `finalize` guarded by a `sync.Once`, and the buffered-dispatch path called it
   immediately with zero usage, so the copy-goroutine's later (correct) call was a
   no-op. Streams now settle once from inside the goroutine with real provider usage;
   the proxy-side reconcile path gets actual TPM.
3. **Router cooldown aborted the fallback walk** — when `attemptSlot` got
   `ErrAllKeysCoolingDown` it converted it to a plain `httpx.RateLimited`, which
   `Dispatch` treated as a client rejection and **broke out of the slot loop**.
   It's now wrapped in `quotaError` (the terminal-but-retry-someone-else shape).
4. **Gemini stream shape** — `streamGenerateContent` default is a JSON *array*, not
   SSE; nothing torn the `alt=sse` that flips it. Now the URL includes `?alt=sse`.

### Medium (fixed)
5. **Gemini concatenated all candidates** into one `chat.completion` choice — now
   only candidate index 0.
6. **Gemini finish_reason gap** — terminal chunks with `finishReason: STOP`
   produced no `finish_reason` field at all. Now every finish is forwarded,
   merged INTO the same text chunk if present, before `[DONE]`.
7. **Half-open breaker admitted unlimited probes** — added a single-probe seat
   (`probes` counter on `breaker`); a failed probe re-opens the breaker.
8. **Round-robin key uneven distribution** — `pickKey` was `nextKey % len(eligible)`
   with a counter, so keys entering/leaving cooldown re-shuffled weights. Now it
   aims at the key *after the previously handed-out one* in the eligible list
   (`g.lastKey` anchor in `group`).
9. **fallback_only semaphore token leak** — `release()` used the *current* mode;
   a request admitted under `fallback_only` whose mode flipped to `normal` never
   released its slot. `admit` now returns a `semOwned bool` threaded via
   `attemptCtx.semOwned` → `release(ac.semOwned)`.
10. **`RecordFailure` blamed `g.keys[0]`** (unknown key) — now forwards `nil`, and
    `recordFailure` handles the nil key case safely.

### Lower (fixed)
- Adapter-rewritten bodies forwarded the upstream's `Content-Length` (and other
  framing headers), so an HTTP/1.1 upstream that sets one made clients abort
  mid-response when the translated body came up short. Framing headers are now
  dropped in `server.copyResponseHeaders`.
- Gemini API key is `url.QueryEscape`d in the query string.
- `estimateTokens` was multiplied per returned embedding — computed once now.
- `/v1/chat/completions` no longer charges 4096 default tokens of TPM budget for
  requests that didn't set `max_tokens` (`server.go` peekInt default → 0).
- `getModel`‘s `listing["data"].([]map[string]any)[0]` type-assume is guarded.
- `probeUpstreams` had a confusing `Probe` on a `(bool, error)` — unchanged, left as is, it’s benign.

---

## 3. Round-robin / load balancing — status and live evidence

Per-credential rotation **inside a slot** is anchored to the last key handed out,
so as keys enter/leave cooldown the rotation stays even, not `prefix-shifted`.
That's `(*group) pickKey` in `internal/router/router.go` (fixture:
`TestPicksNextKeyOnCooldown`, `TestResolveCooldownErrorWhenAllKeysCooled`).
Breaker + health weights `internal/router`: a group is only “graded” (healthScore)
across requests; selection between slots treats `min(0.01, health)` as the weight
in a single-draw weighted comparison (`weightedBetter`). NOTE: the docstring says
selection is “health-weighted” across *groups*, but the production path is
`ResolveSlot` (strict priority order, failover, not weighted) — the weighted
`Resolve` exists but is only exercised by router tests.

Per-credential 429: escalating cooldown 10s → 30s (after 3 hits), 401/402/403 →
365-day quarantine (a “dead key”, plus breaker untouched), 5xx → counts against
the breaker by `failureThreshold` (3 by default), recovery after 30s via one
half-open probe. Model-level consecutive-failure cooldown after 2 losses:
60s (config `cooldownWindow`). All inside one `atomic.Pointer[router.Router]`
snapshot swap; readers never block, in-flight requests keep their snapshot.

**Live evidence (Nov 01):** Groq `openai/gpt-oss-20b` (3 keys) served multiple
sequential calls through `key-<n>` rotation and Ollama Cloud (3 keys) also served
through the same dispatch — no 429s observed, meaning rotation works. Nothing was
verified *quantitatively* (i.e., hit counter per key) but the unit tests cover it.

---

## 4. Gemini adapter — status

Gemini paths now all work live against Google AI Studio (Nov 01):

| Feature | Status |
|---|---|
| Buffered completion | ✅ `PONG` via `gemini-flash-lite-latest` + `gemini-3.5-flash-lite` |
| Streaming | ✅ deltas + a final `finish_reason: "stop"` chunk + normal close |
| Candidate 0 only | ✅ (no more concatenated alternatives) |
| `finish_reason` mapping | ✅ MAX_TOKENS→length, SAFETY/RECITATION/PROHIBITED_CONTENT→content_filter, else→stop |
| `alt=sse` framing | ✅ URL carries `?alt=sse` |
| API key escaping | ✅ `url.QueryEscape` on the `key` query param |
| System prompt | ✅ mapped to `systemInstruction` |
| Images (data URI on `image_url`) | ✅ live-tested, real answer about color |
| **Video** (data URI with `video/mp4` on `image_url`) | ✅ live-tested with a real 1s ffmpeg mp4 — Gemini watched it (“Red”) |

### Tools, JSON mode (NEW)

- **Tools.** OpenAI `tools[].function` → Gemini `functionDeclarations`, with the
  caller's JSON Schema forwarded **verbatim** through `parametersJsonSchema`
  (full JSON Schema; the legacy `parameters` field only takes the OpenAPI 3.0
  subset). Non-function tools are skipped. `tool_choice` maps
  `auto`→default, `none`→`NONE`, `required`→`ANY`, and a specific function→
  `ANY` + `allowedFunctionNames`.
- **Tool turns.** assistant `tool_calls` → `functionCall` parts (the argument
  string is parsed into an object; a non-object is wrapped as `{"value": …}`).
  `role:"tool"` → a user-role `functionResponse` keyed by the function name,
  resolved from the earlier `tool_call_id`; a JSON-object result passes through,
  anything else becomes `{"result": …}`. Adjacent same-role turns are merged
  (parallel tool results become one user turn) and empty content is dropped.
- **Responses.** `functionCall` parts → OpenAI `tool_calls`
  (`id: call_<hex>`, `arguments` as a JSON **string**), `finish_reason:
  tool_calls` when the reply only called tools and content is `null`; streamed
  calls arrive as `delta.tool_calls` with an `index`, and the terminal chunk
  carries `finish_reason: tool_calls`. `thought: true` parts are skipped so
  reasoning summaries never leak into content.
- **JSON mode.** `response_format: {"type":"json_object"}` →
  `responseMimeType: application/json`; `{"type":"json_schema", …}` also
  forwards the schema through `responseJsonSchema` (same verbatim policy as
  tools).

Not supported / not attempted (still true):
- Gemini `batchEmbedContents` sends **one** `requests[]` element (i.e., one
  embedding per proxy call) — the OpenAI `input` array semantics aren't batched.
- Gemini thought signatures are not echoed back, so multi-turn tool use on
  models that require them may need a follow-up.
- `parallel_tool_calls` and non-function tool types are ignored.

---

## 5. Thinking controls (NEW)

OpenAI clients can now control Gemini thinking from `/v1/chat/completions`
**without a models.yaml change**:

```jsonc
{ "reasoning_effort": "low" }        // mapped: low=1024, medium=8192, high=24576, none=0(disable)
{ "thinking_budget": 512 }            // explicit token budget; WINS over effort presets
```

Battery of behaviors (`internal/providers/gemini.go` → `thinkingBudgetFrom`,
`thinkingCfg`, `generationCfg.ThinkingConfig`):

- `reasoning_effort` OR `thinking_budget` → `generationConfig.thinkingConfig.thinkingBudget`
- Neither set → the field is **absent** entirely (Gemini runs its default)
- Budget beats preset (the literal is sent as-is, even if “low” was also present)
- Sending thinking to a **non-thinking model** is a map: the control is forwarded;
  if the upstream 400s (some do), the 400 *passes straight through* to the caller
  as a per-failure revert with the upstream message intact (`INVALID_ARGUMENT`,
  Model does not support thinkingConfig) — nothing is retried against sibling
  slots, the walk does not continue. **Live data point (Nov 01):** Google's
  `gemini-3.5-flash-lite` **accepts** `reasoning_effort: high` silently (200 OK),
  so this upstream is tolerant; stricter model families will surface their 400.

Live verified: budget 512 on `gemini-3.6-flash`, `none`/`low` on both thinking
models — all 200s returned against sensible completions, and per-tests the wire
payload carries exactly the mapped `thinkingBudget` int.

---

## 6. Audio / video support in chat (NEW)

Gemini adapter now maps the OpenAI `input_audio` content part:

```jsonc
{ "type": "input_audio", "input_audio": { "data": "<base64>", "format": "wav" } }
```

→ `inlineData` with `audio/wav` (`mp3` is canonicalized to `audio/mpeg`).
Previously the whole part was dropped and only the text survived, so audio was
silently lost. **Video** continues to work as a base64 `data:video/*;base64,<…>`
data URI on an `image_url` part (OpenAI has no dedicated video part type).

### Media capability flags (the “how do we clarify support” answer)

- `internal/config`: `ModelEntry` now carries `Audio bool` and `Video bool`
  alongside the existing `Images bool`.
- `models.yaml` can state support explicitly **per model**:

```yaml
gemini-flash:
  kind: chat
  providers: google-ai
  media: image, audio, video   # ← explicit allow-list

flash-local-gemma4:31b:
  kind: chat
  model: gemma4:31b
  providers: ollama
  media: ""                    # ← accepts no media at all
```

- **Default:** chat models accept every media kind **their adapter can translate**
  (`Capability.Images/Audio/Video`): Gemini takes image+audio+video, OpenAI takes
  image+audio, Anthropic takes images only, openai-compat advertises all three
  because it forwards the body verbatim. Embeddings/tts/stt never expect media
  parts. A `media:` list NARROWS the adapter default; it cannot widen it
  (Anthropic slots still refuse audio/video).
- The gate is enforced in `internal/proxy/proxy.go` `mediaSlots`, before the
  slot walk, via `providers.HasMediaInput` (was `HasImageInput`): image without
  `Images`, audio without `Audio`, video without `Video`. Each kind is checked
  independently (video data URI ≠ image input), so `media: video` works without
  granting images. A `media:` field on a non-chat kind is a config error.
- Incapable slots are **skipped, not fatal**: a model fronted by a text-only
  slot and a vision slot serves image requests through the vision slot no
  matter which one round-robin would have picked. Only when no slot accepts
  every kind the request carries is it a `400 BadRequest` naming the
  unsupported kind(s), still **before** any upstream spend. (This replaced the
  per-attempt 400, which aborted the failover walk and randomly rejected
  requests the model could serve.)

Work still open in this feature:
- No Anthropic-side audio (dropped deliberately, code says so at
  `anthropic.go ~line 237-243`), and no Anthropic `source:` → OpenAI re-encode
  for minimality.

---

## 7. Current deployment (Nov 01, morning)

```
~/llm-pico/
  llm-pico-linux-amd64    ← NEWEST build (has thinking+audio+video+media flags)
  keys.env                 (0600; populated with real keys — OPENAI, ANTHROPIC unused, GOOGLE AI, NVIDIA, OPENROUTER x3, GROQ x3, OLLAMA x3, OPENCODE)
  provider.yaml            slots: google-ai, nvidia-nim, openrouter, groq, ollama, opencode
  models.yaml              migrated format (see §1); includes caller-friendly blocks:
                           gemini-flash / gemini-flash-lite / gemini-embed,
                           nim-* (4), or-* (4) — each an entry with `model:` mapping
  try_key.py               user test script
  aliases.yaml.bak / models.yaml.bak    ← pre-migration backups (Nov 01)
  pico.log
```

Server: `llm-pico --port 4400`, running in the background (detached via `setsid nohup`).
Smoke matrix run Nov 01:

| Path | Result |
|---|---|
| `/health` | ✅ 200 |
| `/v1/models` | ✅ 43 ids incl. all caller-facing names |
| `chat gemini-flash` (thinking-3.6-flash model) | ✅ `PONG`, `X-` → provider reports `gemini-3.6-flash` upstream |
| `chat gemini-flash-lite-latest`, `gemini-3.5-flash-lite` | ✅ |
| `chat groq openai/gpt-oss-20b` | ✅ 200, usage QUEUED period seen, no 429 |
| `chat ollama nemotron-3-super` | ✅ 200 |
| `chat NIM deepseek-ai/deepseek-v4.1-flash` | ✅ upstream reached |
| `stream gemini-flash` | ✅ deltas + finish_reason + open close |
| `openai/embeddings gemini-embed` | previously-broken; verified post `3ad6808` |
| **thinking controllers live** | ✅ (§5) |
| **audio + video live** | ✅ (§4, §6) |

Timeout failure mode (verified): if an upstream hangs, the response shows:
```
{"error":{"message":"upstream nvidia-nim request failed: Post \"https://integrate.api.nvidia.com/v1/chat/completions\": http2: timeout awaiting response headers","type":"upstream_error","code":502}}
```
Note this is **502**, not 504, and the walk does not get a chance to retry because
the timeout left each attempt — the retry loop *does* apply within the same slot
(verify: NumRetries default 2 → 3 attempts).

---

## 8. Testing & tooling

- Full suite: `go test ./... -count=1` (-race clean as of the last run)
- `gofmt -w` on touched files; `go vet ./...` clean
- New tests: `routed_model_test.go` (embeddings/transcribe routed-model precedence),
  `gemini_thinking_test.go` (effort mapping, budget priority, audio part mapping,
  video data URI mapping, non-thinking 400 pass-through),
  `gemini_tools_test.go` (tool/turn mapping, tool_choice modes, JSON mode, tool
  calls in buffered and streamed replies, thought-part filtering),
  `HasMediaInput` cases in `providers_test.go`, media/thinking flags in
  `config/load_test.go`, capability listing and framing-header tests in
  `server/server_test.go`
- GitHub Actions: `.github/workflows/release.yml` builds releases; version stamping
  via `internal/version` reading build info — the binary is built as
  `go build -trimpath -ldflags "-s -w"` (version string v0.3.2-dirty-<hash> when
  a commit is ahead of the tag, proven correct in prod logs).

---

## 9. Known gaps / ideas (NOT urgent, opinionated)

- `/v1/models` now reports `supports_images` / `supports_audio` /
  `supports_video` / `supports_thinking`, unioned across a model's slots. The
  union is optimistic: dispatch gates each slot individually, so a model with a
  video slot and a text-only slot answers video only through the former. A
  request whose kind no slot accepts is a 400, so the listing never promises
  something the model cannot serve at all.
- Gemini: no response length cap beyond `max_tokens`; thought signatures from
  Gemini 3 tool calls are not preserved across turns.
- The deployment's `models.yaml` must add `thinking: true` to a model before
  `/v1/models` advertises thinking for it; the flag is opt-in by design.
- OpenAI 4xx vs proxy 4xx distinction is fuzzy — `upstreamError` currently
  canonicalizes upstream errors into `httpx.Error`, losing provider metadata
  headers (a cached “provider” field is in `EndpointError`, but it never
  survives past `upstreamError`).
- `prompt_tokens` estimates: the proxy's pre-flight estimator uses `len(body)/4`
  + `max_tokens`; real token counts later available as reconcile — no drift
  surface (verified in tests that TPM was reconciled from provider usage).
- WebUI in `anti-slop/` / `spotify-design/` etc. directories at repo root are
  leftover design-system experiments from other sessions — **do not delete**,
  they're the user's.
- `.freebuff/` directory appeared mid-session, untracked — unknown origin, left alone.

---

## 10. Where to look (map)

| Concern | File |
|---|---|
| Adapter interface, capability types | `internal/providers/providers.go` |
| OpenAI-family adapter (vendor-id strip, auth headers, think-block stripping on streams, `max_completion_tokens` rename for gpt-5/o3/o4) | `internal/providers/openai.go` |
| Gemini adapter (thinking, audio, video, images, alt=sse, key escape) | `internal/providers/gemini.go` |
| Anthropic adapter (no audio, source→base64, `think` tag strip) | `internal/providers/anthropic.go` |
| Registry / BaseURL defaults | `internal/providers/registry.go` |
| Provider-slot selection, cooldowns, breaker, key rotation, health | `internal/router/router.go` |
| Dispatch, failover walk, media gates, rate limit reservation/reconcile | `internal/proxy/proxy.go` |
| HTTP surface, auth, HMAC, body caps, model endpoints | `internal/server/server.go` |
| Client rate-limit windows (RPM/RPD/TPM/TPD/CPM/CPD/AUDIO) | `internal/ratelimit/ratelimit.go` |
| Cross-file error shapes, headers, model listing | `internal/httpx/` |
| Config snapshot types + `media:` flag parsing | `internal/config/config.go`, `internal/config/load.go` |
| Embedded defaults written by `--init` | `internal/config/defaults/{provider,models}.yaml` |
| Root config loading + legacy-file rejection | `internal/config/load.go::Load` |

## 11. If you are picking this up

1. Build: `go build -trimpath -ldflags "-s -w" -o ~/llm-pico/llm-pico-linux-amd64 ./cmd/llm-pico`
2. Reload config live: `kill -HUP <pid>` (no restart needed)
3. Restart cleanly: `pkill -f 'llm-pico-linu[x] --port'` then re-launch with
   `cd ~/llm-pico && setsid nohup ./llm-pico-linux-amd64 --port 4400 > pico.log 2>&1 < /dev/null &`
   (the `setsid` is deliberate — a bare `&` in some shells dies with the console).
4. Quick health: `curl -s :4400/health` (version stamp), `/v1/models` with a PROXY_KEY from `keys.env`.
5. Real completion smoke: `gemini-flash` + a 1-token budget question — see §7 table.
6. Before publishing: tag a release if a new user-facing knob lands — builds
   for linux/darwin/windows are produced by `.github/workflows/release.yml` on
   a `v*.*.*` tag (the version string is stamped from `git describe`).
7. Sneaky bits worth knowing about the code:
   - round-robin anchor lives in `group.lastKey` (string, the api key), not an int — don't “refactor” it back.
   - `HasMediaInput` returns 3 bools. A video data URI rides on an `image_url`
    part but reports only `video=true`, never `image=true`; `image_url` is read
    as raw JSON because responses-style `input_image` carries a bare string URL.
    The proxy checks all three independently.
  - The `key == "" ` fallback in the OpenAI transcribe model override keeps a
    `<missing req.Model>`-free path — don't remove without re-reading that test.
  - Gemini schemas use `parametersJsonSchema` / `responseJsonSchema` on purpose:
    the legacy `parameters` / `responseSchema` fields only accept the OpenAPI
    3.0 subset, while the JSON-Schema fields take what OpenAI clients send.
  - `geminiStream.toolIndex`/`sawToolCalls` exist so a finish-only terminal
    chunk can still report `finish_reason: tool_calls`.
   - `aliases.yaml.bak` / `models.yaml.bak` in the deployment dir are the
     user's rollback, keep them.
