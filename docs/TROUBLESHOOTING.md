# Troubleshooting

## Common Errors

### "Model not available" (404)

**Cause:** All keys for the model are on cooldown, or the model doesn't exist in config.

**Fix:**
1. Check `config.yaml` has the model in `model_list`
2. Check `keys.yaml` has valid keys for the provider
3. Wait for cooldown to expire (check `Retry-After` header)
4. Add backup keys to `keys.yaml` for rotation

### "Invalid API key" (401)

**Cause:** The Bearer token doesn't match any key in the database.

**Fix:**
1. Check the key in `users.yaml` matches what you're sending
2. Check the key hasn't been deactivated via admin API
3. Check the key hasn't expired (`expires_at` field)
4. If using master key, ensure it matches `general_settings.master_key`

### "Rate limit exceeded" (429)

**Cause:** Per-key or per-model rate limit reached.

**Fix:**
1. Check `X-RateLimit-*` headers in response for current limits
2. Increase limits in `users.yaml` or via admin API
3. Wait for window reset (check `X-RateLimit-{window}-Reset` header)
4. Add backup keys to `keys.yaml` for rotation

### "Budget exceeded" (429)

**Cause:** User's monthly spend exceeds their budget.

**Fix:**
1. Check current spend: `GET /admin/users/{user_id}/usage`
2. Increase budget: `PUT /admin/users/{user_id}/budget`
3. Or set to `null` for unlimited

### "Model not allowed for this key" (403)

**Cause:** The requested model is not in the key's allowlist.

**Fix:**
1. Check key's model allowlist: `GET /admin/keys`
2. Update allowlist: `PUT /admin/keys/{prefix}/models`
3. Set to `null` or `[]` for all models

### "Upstream request failed" (502)

**Cause:** The upstream provider returned an error or is unreachable.

**Fix:**
1. Check provider status page
2. Verify API key is valid with the provider
3. Check `api_base` URL is correct
4. Try again (circuit breaker may be open)

### "Upstream timeout" (504)

**Cause:** The upstream provider took too long to respond.

**Fix:**
1. Check provider status page
2. Try with a simpler request (fewer tokens)
3. Check network connectivity
4. Increase timeout in provider adapter (if custom)

### "UNSET placeholder in api_base"

**Cause:** Cloudflare provider is missing `CLOUDFLARE_ACCOUNT_ID`.

**Fix:**
1. Set `CLOUDFLARE_ACCOUNT_ID` in `.env` or environment
2. Or set `api_base` explicitly in `config.yaml`:
   ```yaml
   api_base: "https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/ai/v1"
   ```

### "All keys for model 'X' are rate-limited" (429)

**Cause:** All keys for the provider are on cooldown.

**Fix:**
1. Wait for `Retry-After` seconds (check response header)
2. Add backup keys to `keys.yaml`
3. Check if provider has rate limit issues (status page)

## Debug Mode

Start with verbose logging:

```bash
llm-pico --verbose
```

Or set `-v` flag:

```bash
llm-pico -v
```

This logs:
- All incoming requests
- Router decisions (which key selected)
- Upstream requests and responses
- Rate limit checks
- Error details

## Health Check

```bash
curl http://localhost:4000/health
# {"status":"ok","version":"0.1.0"}
```

If this fails, the server is not running or not responding.

## Live Logs

Stream real-time logs:

```bash
curl -N "http://localhost:4000/admin/logs/stream?token=sk-pico-master-..."
```

Or open the HTML dashboard:

```bash
open http://localhost:4000/admin/logs
```

## Database Issues

### Database Locked

**Cause:** SQLite concurrent access conflict.

**Fix:**
1. Check WAL mode is enabled (should be by default)
2. Reduce concurrent requests
3. Check for long-running transactions

### Database Corrupted

**Cause:** Unexpected shutdown or disk failure.

**Fix:**
1. Stop the proxy
2. Delete `llm-pico.db`
3. Restart the proxy (database recreated automatically)
4. Re-import users from `users.yaml` (automatic on startup)

## Performance Issues

### High Latency

**Check:**
1. Network latency to upstream providers
2. Rate limit headers (are you being throttled?)
3. Circuit breaker state (is it open?)
4. Database size (large `usage_log` can slow queries)

**Fix:**
1. Reduce `usage_log_retention_days`
2. Add backup keys for rotation
3. Use faster upstream providers
4. Scale horizontally with multiple instances

### High Memory Usage

**Check:**
1. Number of concurrent connections
2. Cache size (256 entries default)
3. Rate limit counter count

**Fix:**
1. Reduce `num_retries` (fewer concurrent attempts)
2. Reduce cache size (if modified)
3. Restart periodically (counters are in-memory)

## Common Mistakes

### Wrong Key Format

User keys must start with `sk-pico-`. Master key can be any string.

### Missing Keys.yaml

If `config.yaml` references `KEYS/XXX` but `keys.yaml` doesn't exist:

```
Key 'OPENAI_API_KEY' not found in keys.yaml. Add it with:
OPENAI_API_KEY:
  - "sk-your-key"
```

### Duplicate Model Names

Multiple `model_list` entries with the same `model_name` are merged into the same router group. This can be useful for key pooling but may cause confusion.

### Wrong Provider Prefix

The `model` field must use `<provider>/<model-id>` format:

```yaml
model: openai/gpt-4       # Correct
model: gpt-4              # Wrong (missing provider)
model: openai/gpt-4/v2    # Wrong (extra suffix)
```

## Getting Help

1. Check this troubleshooting guide
2. Review the [logs](#live-logs)
3. Check the [GitHub issues](https://github.com/your-org/llm-pico/issues)
4. Open a new issue with:
   - Error message
   - Config (redact keys)
   - Log output
   - Steps to reproduce
