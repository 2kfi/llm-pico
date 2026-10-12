from __future__ import annotations

import asyncio
import copy
import ctypes
import ctypes.util
import gc
import json
import logging
import os
import random
import time
from array import array
from dataclasses import dataclass, field
from typing import Any

from core.auth import hash_key, verify_api_key
from core.config import Config, GeneralSettings, ModelEntry, ModelParams, RouterSettings
from core.db import close_db, get_db, init_db
from core.ratelimit import get_limiter
from core.router import Router

_log = logging.getLogger("llm-pico.stress")

_MOCK_MESSAGES: list[list[dict[str, Any]]] = [
    [{"role": "user", "content": "Explain quantum computing in simple terms."}],
    [{"role": "user", "content": "Write a Python function to sort a list."}],
    [{"role": "user", "content": "What is the capital of France?"}],
    [{"role": "user", "content": "Summarize the theory of relativity."}],
    [{"role": "user", "content": "How do I bake a chocolate cake?"}],
    [{"role": "user", "content": "Translate hello to Spanish."}],
    [{"role": "user", "content": "What is machine learning?"}],
    [{"role": "user", "content": "Write a haiku about programming."}],
    [{"role": "user", "content": "List three benefits of exercise."}],
    [{"role": "user", "content": "Explain how HTTP works."}],
    [{"role": "user", "content": "What is the difference between TCP and UDP?"}],
    [{"role": "user", "content": "Write a bash script to backup a directory."}],
    [{"role": "user", "content": "Describe the water cycle."}],
    [{"role": "user", "content": "What is an API?"}],
    [{"role": "user", "content": "How does DNS work?"}],
]

_MOCK_PROMPT_TOKENS: list[int] = []
for msgs in _MOCK_MESSAGES:
    text = ""
    for msg in msgs:
        c = msg.get("content", "")
        if isinstance(c, str):
            text += c
    _MOCK_PROMPT_TOKENS.append(max(1, len(text) // 4))

_PROVIDER_SLUGS = ["openai", "anthropic", "gemini", "groq", "openrouter", "nvidia_nim", "cloudflare"]
_NUM_CPUS = os.cpu_count() or 1

_libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=False)
_M_PURGE = -3


def _trim_memory() -> None:
    _libc.mallopt(_M_PURGE, 0)
    _libc.malloc_trim(0)


def _read_rss_mb() -> float:
    try:
        with open("/proc/self/status") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) / 1024
    except (OSError, ValueError, IndexError):
        pass
    return 0.0


_last_cpu_ticks: int | None = None
_last_cpu_time: float = 0.0


def _read_cpu_percent() -> float:
    global _last_cpu_ticks, _last_cpu_time
    try:
        with open("/proc/self/stat") as f:
            parts = f.read().split()
            utime = int(parts[13])
            stime = int(parts[14])
            total = utime + stime
        now = time.monotonic()
        if _last_cpu_ticks is None:
            _last_cpu_ticks = total
            _last_cpu_time = now
            return 0.0
        delta_ticks = total - _last_cpu_ticks
        delta_time = now - _last_cpu_time
        _last_cpu_ticks = total
        _last_cpu_time = now
        try:
            clk_tck = os.sysconf(os.sysconf_names["SC_CLK_TCK"])
        except (AttributeError, ValueError, KeyError):
            clk_tck = 100
        cpu_pct = (delta_ticks / clk_tck) / delta_time * 100 / _NUM_CPUS
        return min(cpu_pct, 100.0)
    except (OSError, ValueError, IndexError, AttributeError):
        return 0.0


def parse_token_count(s: str) -> int:
    s = s.strip().upper()
    if s.endswith("B"):
        return int(float(s[:-1]) * 1_000_000_000)
    if s.endswith("M"):
        return int(float(s[:-1]) * 1_000_000)
    if s.endswith("K"):
        return int(float(s[:-1]) * 1_000)
    return int(s)


def parse_duration(s: str) -> int:
    s = s.strip().upper()
    if s.endswith("H"):
        return int(float(s[:-1]) * 3600)
    if s.endswith("M"):
        return int(float(s[:-1]) * 60)
    if s.endswith("S"):
        return int(float(s[:-1]))
    return int(s)


@dataclass
class StressConfig:
    tokens_per_minute: int = 0
    duration_seconds: int = 600
    num_providers: int = 10
    num_connections: int = 50
    is_stress_test: bool = False


@dataclass(slots=True)
class ResourceSample:
    timestamp: float
    rss_mb: float
    cpu_percent: float


_RESERVOIR_SIZE = 10_000


@dataclass
class StressResult:
    total_requests: int = 0
    total_prompt_tokens: int = 0
    total_completion_tokens: int = 0
    total_latency_ms: float = 0.0
    errors: int = 0
    errors_by_type: dict[str, int] = field(default_factory=dict)
    latencies: array = field(default_factory=lambda: array("d"))
    _latency_seen: int = 0
    resources: list[ResourceSample] = field(default_factory=list)
    start_time: float = 0.0
    end_time: float = 0.0
    max_connections_reached: int = 0
    bottleneck: str = ""

    def record_latency(self, ms: float) -> None:
        self._latency_seen += 1
        if self._latency_seen <= _RESERVOIR_SIZE:
            self.latencies.append(ms)
        else:
            r = random.randrange(self._latency_seen)
            if r < _RESERVOIR_SIZE:
                self.latencies[r] = ms

    @property
    def duration(self) -> float:
        return self.end_time - self.start_time

    @property
    def requests_per_second(self) -> float:
        return self.total_requests / self.duration if self.duration else 0.0

    @property
    def tokens_per_second(self) -> float:
        return (self.total_prompt_tokens + self.total_completion_tokens) / self.duration if self.duration else 0.0

    @property
    def tokens_per_minute(self) -> float:
        return self.tokens_per_second * 60

    @property
    def avg_latency(self) -> float:
        return self.total_latency_ms / self.total_requests if self.total_requests else 0.0

    @property
    def p50_latency(self) -> float:
        if not self.latencies:
            return 0.0
        s = sorted(self.latencies)
        return s[len(s) // 2]

    @property
    def p95_latency(self) -> float:
        if not self.latencies:
            return 0.0
        s = sorted(self.latencies)
        return s[int(len(s) * 0.95)]

    @property
    def p99_latency(self) -> float:
        if not self.latencies:
            return 0.0
        s = sorted(self.latencies)
        return s[int(len(s) * 0.99)]

    @property
    def min_ram(self) -> float:
        return min((r.rss_mb for r in self.resources), default=0.0)

    @property
    def max_ram(self) -> float:
        return max((r.rss_mb for r in self.resources), default=0.0)

    @property
    def avg_ram(self) -> float:
        if not self.resources:
            return 0.0
        return sum(r.rss_mb for r in self.resources) / len(self.resources)

    @property
    def min_cpu(self) -> float:
        cpus = [r.cpu_percent for r in self.resources if r.cpu_percent > 0.0]
        return min(cpus, default=0.0)

    @property
    def max_cpu(self) -> float:
        return max((r.cpu_percent for r in self.resources), default=0.0)

    @property
    def avg_cpu(self) -> float:
        cpus = [r.cpu_percent for r in self.resources if r.cpu_percent > 0.0]
        if not cpus:
            return 0.0
        return sum(cpus) / len(cpus)

    @property
    def error_rate(self) -> float:
        total = self.total_requests + self.errors
        return (self.errors / total * 100) if total else 0.0


_MOCK_RESPONSE_TEMPLATE: dict[str, Any] = {
    "id": "",
    "object": "chat.completion",
    "created": 0,
    "model": "",
    "choices": [{
        "index": 0,
        "message": {"role": "assistant", "content": "Mock response for stress testing."},
        "finish_reason": "stop",
    }],
    "usage": {
        "prompt_tokens": 0,
        "completion_tokens": 0,
        "total_tokens": 0,
    },
}


class MockAdapter:
    def __init__(self) -> None:
        self._prompt_tokens_list = _MOCK_PROMPT_TOKENS
        self._num_templates = len(self._prompt_tokens_list)

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> dict[str, Any]:
        idx = random.randrange(self._num_templates)
        prompt_tokens = self._prompt_tokens_list[idx]
        completion_tokens = random.randint(50, 500)

        await asyncio.sleep(0.0001)

        resp = copy.deepcopy(_MOCK_RESPONSE_TEMPLATE)
        resp["id"] = f"chatcmpl-{os.urandom(8).hex()}"
        resp["created"] = int(time.time())
        resp["model"] = model_string
        resp["usage"]["prompt_tokens"] = prompt_tokens
        resp["usage"]["completion_tokens"] = completion_tokens
        resp["usage"]["total_tokens"] = prompt_tokens + completion_tokens
        return resp

    async def close(self) -> None:
        pass


async def _flush_usage(stop_event: asyncio.Event, buffer_refs: list[list[dict[str, Any]]]) -> None:
    while not stop_event.is_set():
        await asyncio.sleep(0.5)
        await _do_flush(buffer_refs)


async def _do_flush(buffer_refs: list[list[dict[str, Any]]]) -> None:
    batch: list[dict[str, Any]] = []
    for buf in buffer_refs:
        if buf:
            batch.extend(buf)
    if not batch:
        return
    try:
        async with get_db() as db:
            await db.executemany(
                """INSERT INTO usage_log
                   (key_hash, key_prefix, model_name, provider, request_id,
                    prompt_tokens, completion_tokens, total_tokens,
                    latency_ms, status_code, error, cost_usd, created_at)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    (
                        e["key_hash"], e["key_prefix"], e["model_name"],
                        e["provider"], e["request_id"],
                        e["prompt_tokens"], e["completion_tokens"],
                        e["total_tokens"], e["latency_ms"],
                        e["status_code"], e["error"], e["cost_usd"],
                        e["created_at"],
                    )
                    for e in batch
                ),
            )
            await db.commit()
        for buf in buffer_refs:
            buf.clear()
    except Exception:
        _log.warning("flush_usage failed, will retry", exc_info=True)


async def _resource_monitor(result: StressResult, stop_event: asyncio.Event) -> None:
    gc_counter = 0
    gen2_counter = 0
    while not stop_event.is_set():
        result.resources.append(ResourceSample(
            timestamp=time.time(),
            rss_mb=_read_rss_mb(),
            cpu_percent=_read_cpu_percent(),
        ))
        gc_counter += 1
        if gc_counter % 10 == 0:
            gc.collect(0)
            _trim_memory()
        gen2_counter += 1
        if gen2_counter % 60 == 0:
            gc.collect(2)
            _trim_memory()
        await asyncio.sleep(0.5)


async def _worker(
    worker_id: int,
    stress_cfg: StressConfig,
    result: StressResult,
    user_keys: list[str],
    router: Router,
    model_names: list[str],
    master_key: str,
    stop_event: asyncio.Event,
    worker_buffer: list[dict[str, Any]],
) -> None:
    cached_auth: dict[str, dict[str, Any] | None] = {}
    adapter = MockAdapter()
    request_bodies: list[bytes] = []
    for messages in _MOCK_MESSAGES:
        body = json.dumps({
            "model": "",
            "messages": messages,
            "stream": False,
            "max_tokens": 4096,
        })
        request_bodies.append(body.encode())

    _id_salt = os.urandom(8).hex()
    _ts = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    _ts_next = time.time() + 1.0
    _req_counter = 0

    while not stop_event.is_set():
        if time.time() >= _ts_next:
            _ts = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
            _ts_next = time.time() + 1.0

        api_key = user_keys[random.randrange(len(user_keys))]
        model_name = model_names[random.randrange(len(model_names))]
        body_template = request_bodies[random.randrange(len(request_bodies))]

        body_bytes = body_template.replace(b'"model": ""', b'"model": "' + model_name.encode() + b'"')

        t0 = time.monotonic()
        try:
            key_data = cached_auth.get(api_key)
            if key_data is None:
                key_data = await verify_api_key(api_key, master_key)
                cached_auth[api_key] = key_data
            if not key_data:
                result.errors += 1
                result.errors_by_type["auth_none"] = result.errors_by_type.get("auth_none", 0) + 1
                continue

            resolved = router.resolve(model_name)
            if resolved is None:
                result.errors += 1
                result.errors_by_type["resolve_none"] = result.errors_by_type.get("resolve_none", 0) + 1
                continue

            provider_group, key_state, model_entry = resolved

            resp = await adapter.proxy_request(body_bytes, model_entry.model_params.model)

            latency = (time.monotonic() - t0) * 1000

            usage = resp.get("usage", {})
            pt = usage.get("prompt_tokens", 0)
            ct = usage.get("completion_tokens", 0)

            _req_counter += 1
            entry = {
                "key_hash": key_data.get("key_hash", ""),
                "key_prefix": key_data.get("key_prefix", ""),
                "model_name": model_name,
                "provider": provider_group.provider_slug,
                "request_id": _id_salt + str(_req_counter),
                "prompt_tokens": pt,
                "completion_tokens": ct,
                "total_tokens": pt + ct,
                "latency_ms": int(latency),
                "status_code": 200,
                "error": None,
                "cost_usd": 0.0,
                "created_at": _ts,
            }
            worker_buffer.append(entry)

            result.total_requests += 1
            result.total_prompt_tokens += pt
            result.total_completion_tokens += ct
            result.total_latency_ms += latency
            result.record_latency(latency)

        except Exception as exc:
            result.errors += 1
            t = type(exc).__name__
            result.errors_by_type[t] = result.errors_by_type.get(t, 0) + 1


def _build_mock_config(master_key: str, num_providers: int) -> Config:
    config = Config(
        general_settings=GeneralSettings(master_key=master_key),
        router_settings=RouterSettings(num_retries=0),
    )

    for i in range(num_providers):
        slug = _PROVIDER_SLUGS[i % len(_PROVIDER_SLUGS)]
        config.model_list.append(ModelEntry(
            model_name=f"stress-model-{i}",
            model_params=ModelParams(
                model=f"{slug}/stress-model-{i}",
                api_key=f"sk-stress-key-{i}",
            ),
            rpm=10000,
            rpd=1000000,
            tpm=10000000,
            tpd=100000000,
        ))

    return config


async def _seed_users(num_connections: int) -> list[str]:
    user_keys: list[str] = []
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    params = []
    for i in range(num_connections):
        raw_key = "sk-pico-stress-" + os.urandom(16).hex()
        user_keys.append(raw_key)
        key_hash = hash_key(raw_key)
        prefix = raw_key[:16] + "..."
        params.append((key_hash, prefix, f"stress-user-{i}", now))
    async with get_db() as db:
        await db.executemany(
            "INSERT OR IGNORE INTO user_keys (key_hash, key_prefix, label, is_active, created_at) VALUES (?, ?, ?, 1, ?)",
            params,
        )
        await db.commit()
    return user_keys


def _print_results(result: StressResult, cfg: StressConfig) -> None:
    log = logging.getLogger("llm-pico.stress")

    log.info("=" * 72)
    log.info("  STRESS TEST RESULTS")
    log.info("=" * 72)
    log.info("  Configuration:")
    log.info("    Mode:                 %s", "AUTO-RAMP (MAX)" if cfg.is_stress_test else "FIXED-LOAD")
    if not cfg.is_stress_test:
        log.info("    Target throughput:    %s tokens/min", _fmt_num(cfg.tokens_per_minute))
    actual = result.duration
    log.info("    Duration:             %.0f s (%s)", cfg.duration_seconds, _fmt_duration(int(cfg.duration_seconds)))
    log.info("    Actual wall time:     %.0f s (%s)", actual, _fmt_duration(int(actual)))
    log.info("    Providers:            %d", cfg.num_providers)
    log.info("    Connections:          %d", result.max_connections_reached)
    log.info("")
    log.info("  Throughput:")
    log.info("    Wall time:            %.1f s", result.duration)
    log.info("    Total requests:       %d", result.total_requests)
    log.info("    Total tokens:         %d", result.total_prompt_tokens + result.total_completion_tokens)
    log.info("    Prompt tokens:        %d", result.total_prompt_tokens)
    log.info("    Completion tokens:    %d", result.total_completion_tokens)
    log.info("    Errors:               %d (%s%%)", result.errors, _fmt_float(result.error_rate, 2))
    if result.errors_by_type:
        for err_type, count in sorted(result.errors_by_type.items(), key=lambda x: -x[1]):
            log.info("      %s: %d", err_type, count)
    log.info("    Throughput:           %s req/s", _fmt_num(result.requests_per_second))
    log.info("    Token throughput:     %s tok/s", _fmt_num(result.tokens_per_second))
    log.info("    Token throughput:     %s tok/min", _fmt_num(result.tokens_per_minute))
    log.info("")
    log.info("  Latency:")
    log.info("    Average:              %s ms", _fmt_float(result.avg_latency, 2))
    log.info("    p50:                  %s ms", _fmt_float(result.p50_latency, 2))
    log.info("    p95:                  %s ms", _fmt_float(result.p95_latency, 2))
    log.info("    p99:                  %s ms", _fmt_float(result.p99_latency, 2))
    log.info("")
    log.info("  Resources:")
    log.info("    CPU  (min / avg / max): %s%% / %s%% / %s%%",
             _fmt_float(result.min_cpu, 1), _fmt_float(result.avg_cpu, 1), _fmt_float(result.max_cpu, 1))
    log.info("    RAM  (min / avg / max): %s MB / %s MB / %s MB",
             _fmt_float(result.min_ram, 1), _fmt_float(result.avg_ram, 1), _fmt_float(result.max_ram, 1))
    log.info("")
    if cfg.is_stress_test:
        log.info("  Bottleneck:")
        log.info("    %s", result.bottleneck)
        log.info("    Max sustainable connections: %d", result.max_connections_reached)
    log.info("=" * 72)


def _fmt_num(n: float) -> str:
    if n >= 1_000_000:
        return f"{n / 1_000_000:.2f}M"
    if n >= 1_000:
        return f"{n / 1_000:.1f}K"
    return f"{n:.0f}"


def _fmt_float(n: float, d: int) -> str:
    return f"{n:.{d}f}"


def _fmt_duration(seconds: int) -> str:
    if seconds >= 3600:
        return f"{seconds // 3600}h{(seconds % 3600) // 60}m"
    if seconds >= 60:
        return f"{seconds // 60}m{seconds % 60}s"
    return f"{seconds}s"


async def _run_phase(
    num_connections: int,
    duration: float,
    stress_cfg: StressConfig,
    router: Router,
    limiter: Any,
    model_names: list[str],
    master_key: str,
    user_keys: list[str],
) -> StressResult:
    result = StressResult(start_time=time.time())
    stop_event = asyncio.Event()

    monitor_task = asyncio.create_task(_resource_monitor(result, stop_event))

    worker_buffers: list[list[dict[str, Any]]] = [[] for _ in range(num_connections)]
    buffer_refs: list[list[dict[str, Any]]] = worker_buffers
    flush_task = asyncio.create_task(_flush_usage(stop_event, buffer_refs))

    workers = [
        asyncio.create_task(_worker(
            i, stress_cfg, result, user_keys, router,
            model_names, master_key, stop_event, worker_buffers[i],
        ))
        for i in range(num_connections)
    ]

    await asyncio.sleep(duration)

    result.end_time = time.time()
    stop_event.set()
    await asyncio.gather(*workers, return_exceptions=True)
    await flush_task
    await _do_flush(buffer_refs)
    await monitor_task

    result.max_connections_reached = num_connections
    return result


async def _bootstrap(master_key: str, num_providers: int, num_connections: int) -> tuple[str, Router, Any, list[str], list[str]]:
    db_path = "/tmp/llm-pico-stress-" + os.urandom(4).hex() + ".db"
    pool_size = max(4, num_connections // 10)
    await init_db(db_path, memory_db=True, pool_size=pool_size)

    gc.freeze()

    config = _build_mock_config(master_key, num_providers)
    model_names = [m.model_name for m in config.model_list]

    router = Router(config)
    limiter = get_limiter()
    limiter.start()

    user_keys = await _seed_users(num_connections)

    return db_path, router, limiter, model_names, user_keys


async def _teardown(db_path: str, router: Router, limiter: Any) -> None:
    await limiter.stop()
    await close_db()
    try:
        os.unlink(db_path)
    except OSError:
        pass


async def run_stress(cfg: StressConfig) -> None:
    master_key = "sk-pico-stress-master"
    db_path, router, limiter, model_names, user_keys = await _bootstrap(
        master_key, cfg.num_providers, cfg.num_connections,
    )

    result = await _run_phase(
        num_connections=cfg.num_connections,
        duration=float(cfg.duration_seconds),
        stress_cfg=cfg,
        router=router,
        limiter=limiter,
        model_names=model_names,
        master_key=master_key,
        user_keys=user_keys,
    )

    result.max_connections_reached = cfg.num_connections
    _print_results(result, cfg)
    await _teardown(db_path, router, limiter)


async def run_stress_test(cfg: StressConfig) -> None:
    master_key = "sk-pico-stress-master"
    db_path, router, limiter, model_names, user_keys = await _bootstrap(
        master_key, cfg.num_providers, max(cfg.num_connections, 2000),
    )

    total_budget = cfg.duration_seconds
    max_connections = 2000
    current_connections = 10
    best_tokens_per_sec = 0.0
    best_result: StressResult | None = None
    ramp_results: list[tuple[int, float, float, float]] = []
    ramp_start = time.time()

    num_phases = min(8, max(4, total_budget // 15))
    phase_duration = max(15.0, total_budget / num_phases)
    phase_duration = min(phase_duration, total_budget)

    log = logging.getLogger("llm-pico.stress")

    while current_connections <= max_connections:
        elapsed = time.time() - ramp_start
        remaining = total_budget - elapsed
        if remaining <= phase_duration:
            remaining = phase_duration

        phase_time = min(phase_duration, remaining)
        if phase_time <= 1:
            break

        log.info("RAMP phase: %d connections for %.0f s ...", current_connections, phase_time)

        result = await _run_phase(
            num_connections=current_connections,
            duration=phase_time,
            stress_cfg=cfg,
            router=router,
            limiter=limiter,
            model_names=model_names,
            master_key=master_key,
            user_keys=user_keys,
        )

        tps = result.tokens_per_second
        avg_cpu = result.avg_cpu
        avg_ram = result.avg_ram

        log.info("  ramp %d conn: %s tok/s, CPU %s%%, RAM %s MB",
                 current_connections,
                 _fmt_num(tps),
                 _fmt_float(avg_cpu, 1),
                 _fmt_float(avg_ram, 1))

        ramp_results.append((current_connections, tps, avg_cpu, avg_ram))

        if tps > best_tokens_per_sec:
            best_tokens_per_sec = tps
            best_result = result

        bottleneck_found = False
        if avg_cpu > 90:
            log.info("  CPU saturated (%.1f%%) at %d connections", avg_cpu, current_connections)
            bottleneck_found = True

        if not bottleneck_found and len(ramp_results) >= 2:
            prev_tps = ramp_results[-2][1]
            if prev_tps > 0 and (tps / prev_tps) < 1.05:
                log.info("  throughput plateaued at %d connections (<5%% gain)", current_connections)
                bottleneck_found = True

        if bottleneck_found:
            remaining = total_budget - (time.time() - ramp_start)
            if remaining > 5:
                log.info("  Sustained benchmark at %d connections for %.0f s …", current_connections, remaining)
                sustain = await _run_phase(
                    num_connections=current_connections,
                    duration=remaining,
                    stress_cfg=cfg,
                    router=router,
                    limiter=limiter,
                    model_names=model_names,
                    master_key=master_key,
                    user_keys=user_keys,
                )
                if sustain.tokens_per_second > best_tokens_per_sec:
                    best_tokens_per_sec = sustain.tokens_per_second
                    best_result = sustain
            break

        current_connections = min(current_connections * 2, max_connections)

        if time.time() - ramp_start >= total_budget:
            break

    last_cpu = ramp_results[-1][2] if ramp_results else 0
    scaling_eff = 1.0
    if len(ramp_results) >= 2:
        initial = ramp_results[0]
        final = ramp_results[-1]
        if initial[1] > 0 and initial[0] > 0 and final[0] > 0:
            scaling_eff = (final[1] / final[0]) / (initial[1] / initial[0])

    if last_cpu > 90:
        bottleneck = "CPU-bound (single-core saturated at {:.1f}%)".format(last_cpu)
    elif scaling_eff < 0.5:
        bottleneck = "Lock contention (scaling efficiency {:.0f}%)".format(scaling_eff * 100)
    elif current_connections >= max_connections:
        bottleneck = "Reached connection cap ({} — may handle more)".format(max_connections)
    else:
        bottleneck = "Unknown — further analysis needed (iostat, perf)"

    if best_result:
        best_result.bottleneck = bottleneck
        best_result.max_connections_reached = current_connections
        best_result.end_time = time.time()

    _print_results(best_result or StressResult(), cfg)
    await _teardown(db_path, router, limiter)
