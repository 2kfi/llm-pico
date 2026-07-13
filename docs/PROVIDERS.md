# Providers

llm-pico supports 10 built-in providers and any OpenAI-compatible endpoint via passthrough.

## Built-in Providers

| Provider | Slug | Base URL | Env Key | Features |
|----------|------|----------|---------|----------|
| OpenAI | `openai` | `https://api.openai.com/v1` | `OPENAI_API_KEY` | Images, Embeddings |
| Gemini | `gemini` | `https://generativelanguage.googleapis.com/v1beta` | `GEMINI_API_KEY` | Images, Embeddings |
| Anthropic | `anthropic` | `https://api.anthropic.com/v1` | `ANTHROPIC_API_KEY` | Images |
| OpenRouter | `openrouter` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` | Passthrough |
| OpenCode Zen | `opencode_zen` | `https://opencode.ai/api/v1` | `OPENCODE_ZEN_API_KEY` | Passthrough |
| Groq | `groq` | `https://api.groq.com/openai/v1` | `GROQ_API_KEY` | Passthrough |
| NVIDIA NIM | `nvidia_nim` | `https://integrate.api.nvidia.com/v1` | `NVIDIA_NIM_API_KEY` | Passthrough |
| Cloudflare | `cloudflare` | `https://api.cloudflare.com/client/v4/accounts/{ID}/ai/v1` | `CLOUDFLARE_API_KEY` | Embeddings |
| Cohere | `cohere` | `https://api.cohere.ai/compatibility/v1` | `COHERE_API_KEY` | Embeddings |
| ZAI (GLM) | `zai` | `https://open.bigmodel.cn/api/paas/v4` | `ZAI_API_KEY` | Passthrough |

## Adapter Capabilities

| Adapter | Images | Embeddings | STT | TTS | Custom Streaming |
|---------|--------|------------|-----|-----|-----------------|
| OpenAI | Yes | Yes | Yes | Yes | No |
| Gemini | Yes | Yes | No | No | Yes |
| Anthropic | Yes | No | No | No | Yes |
| Cloudflare | No | Yes | No | No | No |
| Cohere | No | Yes | No | No | No |
| Fallback (OpenAI) | Config-based | Config-based | Config-based | Config-based | No |

## Custom Providers

For any OpenAI-compatible, Anthropic-compatible, or Gemini-compatible endpoint not in the built-in list.

### Custom OpenAI-Compatible

```yaml
model_list:
  - model_name: my-model
    model_params:
      model: my-model
      api_key: "KEYS/MY_PROVIDER_KEY"
      api_base: "https://my-provider.com/v1"
```

### Custom Anthropic-Compatible

```yaml
model_list:
  - model_name: my-claude
    model_params:
      model: my-claude
      api_key: "KEYS/MY_PROVIDER_KEY"
      api_base: "https://my-provider.com/v1"
```

### Custom Gemini-Compatible

```yaml
model_list:
  - model_name: my-gemini
    model_params:
      model: my-gemini
      api_key: "KEYS/MY_PROVIDER_KEY"
      api_base: "https://my-provider.com/v1"
```

## Provider-Specific Quirks

### OpenAI

- Newer models (`gpt-5*`, `o3*`, `o4*`) require `max_completion_tokens` instead of `max_tokens` — the adapter converts automatically
- Think tags (`<think>...</think>`) are stripped from responses

### Anthropic

- System messages are extracted and sent as the `system` parameter
- Image URLs with `data:` URIs are converted to base64
- Response format is translated: `stop_reason` mapped to `finish_reason`
- Custom streaming reads Anthropic SSE events and converts to OpenAI format

### Gemini

- API key is passed as URL query parameter (`?key=...`)
- System messages become `systemInstruction`
- Image `data:` URIs become `inline_data`
- `thought=true` parts are filtered from output
- Streaming uses `streamGenerateContent` endpoint
- Logs are sanitized to redact API keys

### Cloudflare

- Base URL is built dynamically from `CLOUDFLARE_ACCOUNT_ID` env var
- Model strings have `cloudflare/` prefix stripped before forwarding

### Cohere

- Embeddings include `input_type: "search_document"` if not provided
- Provider prefix is stripped from model strings

## Unsupported Provider Fallback

Any unrecognized provider slug falls back to `OpenAIAdapter`. This means any OpenAI-compatible provider works out of the box:

```yaml
model_list:
  - model_name: deepseek-chat
    model_params:
      model: deepseek-chat
      api_key: "sk-..."
      api_base: "https://api.deepseek.com/v1"
```

The request is forwarded as-is to the upstream API.

## Model String Format

The `model` field uses the format `<provider>/<upstream-model-name>`:

```
openai/gpt-4
anthropic/claude-3-sonnet-20240229
gemini/gemini-3-flash-preview
cloudflare/@cf/meta/llama-3.1-8b-instruct
```

For custom providers, the provider prefix is the `model` field value itself:

```
my-model
custom-model-name
```

## Fetching Models

During `llm-pico init`, models are fetched live from each provider's `/v1/models` endpoint:

1. Request sent with the provider's API key
2. If 401/403 → authentication failed, skip provider
3. If 404 → provider doesn't support model listing, skip
4. If other error → retry once with fresh key
5. If success → parse model list and present for selection

This ensures you only see models your keys actually have access to.
