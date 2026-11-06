#!/usr/bin/env bash
# Agent-shaped smoke test for a running llm-pico instance.
#
# Usage:
#   PICO_BASE_URL=http://localhost:4000 \
#   PICO_KEY=sk-... PICO_MODEL=gpt-4o-mini \
#   tools/smoke-agent.sh
#
# Optional:
#   PICO_TOOL_MODEL=<model>   model to use for the tool-definition check
#                             (defaults to PICO_MODEL)
#   PICO_LONG_STREAM=1        ask for a long answer and require the stream to
#                             stay open and terminate cleanly
#   PICO_CONTEXT_BYTES=<n>    size of the large-context probe (default ~800 KiB,
#                             roughly 200k tokens of ASCII)
#
# Checks, per adapter/model:
#   1. the instance is healthy
#   2. the model is listed and advertises tool support
#   3. ten consecutive streaming turns each complete with data: [DONE]
#   4. a request carrying tool definitions is accepted
#   5. a ~200k-token body is accepted or refused with 413, never a 5xx
#   6. responses carry X-Pico-Provider attribution
#
# Cross-slot 429 failover needs an upstream that returns 429, which this script
# cannot induce from the client side; run it against a deployment with a
# fault-injecting provider (or exercise TestUpstream429FallsOverToNextSlot).
set -uo pipefail

BASE_URL="${PICO_BASE_URL:-http://localhost:4000}"
KEY="${PICO_KEY:-}"
MODEL="${PICO_MODEL:-}"
TOOL_MODEL="${PICO_TOOL_MODEL:-$MODEL}"
CONTEXT_BYTES="${PICO_CONTEXT_BYTES:-800000}"

failures=0
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; failures=$((failures + 1)); }
skip() { printf 'SKIP  %s\n' "$1"; }

if [[ -z "$KEY" || -z "$MODEL" ]]; then
	echo "usage: PICO_KEY=<key> PICO_MODEL=<model> [PICO_BASE_URL=...] $0" >&2
	exit 2
fi

# post <path> <body-file> <header-file> <out-file> -> HTTP status
post() {
	curl -sS -o "$4" -D "$3" -w '%{http_code}' \
		-X POST "$BASE_URL$1" \
		-H "Authorization: Bearer $KEY" \
		-H 'Content-Type: application/json' \
		--data-binary @"$2"
}

headers="${tmpdir}/headers"
body="${tmpdir}/body"

# 1. health
code="$(curl -sS -o "$body" -w '%{http_code}' "$BASE_URL/health" || echo 000)"
if [[ "$code" == "200" ]]; then
	pass "health"
else
	fail "health (status $code)"
fi

# 2. model listing and agent-readiness
code="$(curl -sS -o "$body" -w '%{http_code}' "$BASE_URL/v1/models" -H "Authorization: Bearer $KEY" || echo 000)"
if [[ "$code" != "200" ]]; then
	fail "GET /v1/models (status $code)"
elif ! grep -q "\"id\":\"$MODEL\"" "$body"; then
	fail "model $MODEL is not listed"
elif grep -q '"supports_tools":true' "$body"; then
	pass "model listing advertises tool support"
else
	skip "model listing reports supports_tools:false for $MODEL (check models.yaml)"
fi

# 3. ten consecutive streaming turns
messages='[{"role":"user","content":"reply with the single word: turn 1"}]'
turns_ok=1
for turn in $(seq 1 10); do
	cat >"${tmpdir}/req.json" <<EOF
{"model":"$MODEL","messages":$messages,"stream":true,"max_tokens":32}
EOF
	code="$(post /v1/chat/completions "${tmpdir}/req.json" "$headers" "${tmpdir}/stream.txt")"
	if [[ "$code" != "200" ]] || ! grep -q 'data: \[DONE\]' "${tmpdir}/stream.txt"; then
		turns_ok=0
		fail "streaming turn $turn (status $code)"
		break
	fi
	messages="$(printf '%s' "$messages" | sed 's/]$//'),{\"role\":\"assistant\",\"content\":\"ok\"},{\"role\":\"user\",\"content\":\"turn $((turn + 1))\"}]"
done
if [[ "$turns_ok" == "1" ]]; then
	pass "10 streaming turns each terminated with [DONE]"
fi

# 4. tool definitions are accepted (and surfaced back when the model calls one)
cat >"${tmpdir}/req.json" <<EOF
{"model":"$TOOL_MODEL","messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],
 "tools":[{"type":"function","function":{"name":"get_weather","description":"current weather",
   "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],
   "additionalProperties":false}}}],
 "tool_choice":"auto","max_tokens":256}
EOF
code="$(post /v1/chat/completions "${tmpdir}/req.json" "${tmpdir}/tool-headers" "${tmpdir}/tools.txt")"
if [[ "$code" == "200" ]]; then
	pass "request with tool definitions accepted"
	if grep -q '"tool_calls"' "${tmpdir}/tools.txt"; then
		pass "model returned tool_calls (replay them in your agent loop)"
	else
		skip "model answered without calling the tool"
	fi
else
	fail "tool-definition request (status $code): $(head -c 300 "${tmpdir}/tools.txt")"
fi

# 5. large context: accepted, or a 413 with a clear message, never a 5xx
awk -v n="$CONTEXT_BYTES" -v m="$MODEL" 'BEGIN {
	printf "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"", m;
	for (i = 0; i < n; i++) printf "a";
	printf "\"}],\"max_tokens\":16}";
}' >"${tmpdir}/req.json"
code="$(post /v1/chat/completions "${tmpdir}/req.json" "$headers" "${tmpdir}/ctx.txt")"
case "$code" in
200) pass "large context accepted" ;;
413) pass "large context refused with 413 (retry-with-smaller-context signal)" ;;
5*) fail "large context returned a server error ($code)" ;;
*) fail "large context returned $code: $(head -c 300 "${tmpdir}/ctx.txt")" ;;
esac

# 6. provider attribution, read from the tool call that reached a provider
if grep -qi '^X-Pico-Provider:' "${tmpdir}/tool-headers"; then
	pass "X-Pico-Provider present"
else
	fail "X-Pico-Provider header missing"
fi

# Optional: a long stream that must stay open and finish cleanly.
if [[ "${PICO_LONG_STREAM:-0}" == "1" ]]; then
	cat >"${tmpdir}/req.json" <<EOF
{"model":"$MODEL","messages":[{"role":"user","content":"Write a detailed 3000 word essay about stream processing."}],
 "stream":true,"max_tokens":4096}
EOF
	started=$(date +%s)
	code="$(post /v1/chat/completions "${tmpdir}/req.json" "$headers" "${tmpdir}/long.txt")"
	elapsed=$(( $(date +%s) - started ))
	if [[ "$code" == "200" ]] && grep -q 'data: \[DONE\]' "${tmpdir}/long.txt"; then
		pass "long stream completed after ${elapsed}s"
	else
		fail "long stream (status $code after ${elapsed}s)"
	fi
else
	skip "long stream (set PICO_LONG_STREAM=1 to run; needs a provider that streams for minutes)"
fi

skip "cross-slot 429 failover (needs a fault-injecting upstream; see proxy_test.go)"

echo
if [[ "$failures" -gt 0 ]]; then
	printf '%d check(s) failed\n' "$failures"
	exit 1
fi
echo "all checks passed"
