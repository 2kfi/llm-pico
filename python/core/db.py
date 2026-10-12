from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import aiosqlite

_db_pool: asyncio.Queue[aiosqlite.Connection] | None = None
_db_path: str | None = None
_POOL_SIZE = 2
_log = logging.getLogger("llm-pico.db")

SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS teams (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    description     TEXT,
    is_active       INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,
    model_allowlist TEXT,
    rpm_limit       INTEGER,
    rpd_limit       INTEGER,
    tpm_limit       INTEGER,
    tpd_limit       INTEGER,
    ash_limit       INTEGER,
    asd_limit       INTEGER,
    monthly_budget_usd REAL
);

CREATE TABLE IF NOT EXISTS users (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    team_id           INTEGER NOT NULL REFERENCES teams(id),
    email             TEXT    NOT NULL UNIQUE,
    name              TEXT    NOT NULL,
    is_active         INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT    NOT NULL,
    model_allowlist   TEXT,
    rpm_limit         INTEGER,
    rpd_limit         INTEGER,
    tpm_limit         INTEGER,
    tpd_limit         INTEGER,
    ash_limit         INTEGER,
    asd_limit         INTEGER,
    monthly_budget_usd REAL
);

CREATE TABLE IF NOT EXISTS user_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash        TEXT    NOT NULL UNIQUE,
    key_prefix      TEXT    NOT NULL,
    label           TEXT,
    is_active       INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,
    expires_at      TEXT,
    model_allowlist TEXT,
    rpm_limit       INTEGER,
    rpd_limit       INTEGER,
    tpm_limit       INTEGER,
    tpd_limit       INTEGER,
    ash_limit       INTEGER,
    asd_limit       INTEGER,
    user_id         INTEGER REFERENCES users(id),
    monthly_budget_usd REAL,
    hard_limit      INTEGER NOT NULL DEFAULT 1,
    ip_allowlist    TEXT
);

CREATE TABLE IF NOT EXISTS api_key_scopes (
    key_id  INTEGER REFERENCES user_keys(id) ON DELETE CASCADE,
    scope   TEXT NOT NULL,
    PRIMARY KEY (key_id, scope)
);

CREATE TABLE IF NOT EXISTS usage_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash          TEXT    NOT NULL,
    key_prefix        TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,
    provider          TEXT    NOT NULL,
    request_id        TEXT    NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    status_code       INTEGER NOT NULL,
    error             TEXT,
    cost_usd          REAL,
    created_at        TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_key   ON usage_log(key_hash);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_log(model_name);
CREATE INDEX IF NOT EXISTS idx_usage_time  ON usage_log(created_at);

CREATE TABLE IF NOT EXISTS rate_counters (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash      TEXT    NOT NULL,
    model_name    TEXT    NOT NULL,
    level         TEXT    NOT NULL CHECK(level IN ('user', 'model')),
    window_type   TEXT    NOT NULL CHECK(window_type IN ('rpd', 'tpd', 'asd')),
    window_start  TEXT    NOT NULL,
    count         INTEGER NOT NULL DEFAULT 0,
    UNIQUE(key_hash, model_name, level, window_type, window_start)
);

CREATE TABLE IF NOT EXISTS admin_log (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    action              TEXT    NOT NULL,
    actor_hash          TEXT    NOT NULL,
    details             TEXT,
    structured_details  TEXT,
    client_ip           TEXT,
    created_at          TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS models (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    model_name          TEXT    NOT NULL UNIQUE,
    model               TEXT    NOT NULL,
    api_base            TEXT,
    images              INTEGER NOT NULL DEFAULT 0,
    embeddings          INTEGER NOT NULL DEFAULT 0,
    stt                 INTEGER NOT NULL DEFAULT 0,
    tts                 INTEGER NOT NULL DEFAULT 0,
    failover_model      TEXT,
    can_cache           INTEGER NOT NULL DEFAULT 0,
    cost_per_1m_input   REAL,
    cost_per_1m_output  REAL,
    rpm                 INTEGER,
    rpd                 INTEGER,
    tpm                 INTEGER,
    tpd                 INTEGER,
    ash                 INTEGER,
    asd                 INTEGER,
    is_active           INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id    INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    api_key     TEXT    NOT NULL,
    priority    INTEGER NOT NULL DEFAULT 0,
    is_active   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_pk_model ON provider_keys(model_id);

CREATE TABLE IF NOT EXISTS model_capabilities (
    model_id        INTEGER REFERENCES models(id) ON DELETE CASCADE,
    supports_tools  INTEGER DEFAULT 0,
    supports_vision INTEGER DEFAULT 0,
    supports_json   INTEGER DEFAULT 0,
    supports_stream INTEGER DEFAULT 1,
    max_context     INTEGER,
    max_output      INTEGER,
    probed_at       TEXT,
    PRIMARY KEY (model_id)
);

CREATE TABLE IF NOT EXISTS model_aliases (
    alias       TEXT PRIMARY KEY,
    model_name  TEXT NOT NULL,
    priority    INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS request_samples (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    prompt_hash       TEXT    NOT NULL,
    prompt_preview    TEXT,
    response_preview  TEXT,
    created_at        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_model ON request_samples(model);
CREATE INDEX IF NOT EXISTS idx_samples_time  ON request_samples(created_at);

CREATE TABLE IF NOT EXISTS schema_version (
    version     INTEGER PRIMARY KEY,
    applied_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS request_traces (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id  TEXT    NOT NULL,
    span_index  INTEGER NOT NULL,
    label       TEXT    NOT NULL,
    start_ms    INTEGER NOT NULL,
    end_ms      INTEGER NOT NULL,
    model_name  TEXT,
    provider    TEXT,
    status      TEXT    NOT NULL DEFAULT 'ok',
    detail      TEXT
);
CREATE INDEX IF NOT EXISTS idx_trace_req ON request_traces(request_id);
"""


async def _configure_conn(conn: aiosqlite.Connection) -> None:
    conn.row_factory = aiosqlite.Row
    await conn.execute("PRAGMA journal_mode=WAL")
    await conn.execute("PRAGMA busy_timeout=2000")
    await conn.execute("PRAGMA synchronous=NORMAL")
    await conn.execute("PRAGMA mmap_size=67108864")
    await conn.execute("PRAGMA cache_size=-8000")
    await conn.execute("PRAGMA temp_store=MEMORY")
    await conn.execute("PRAGMA foreign_keys=ON")
    await conn.executescript(SCHEMA_SQL)
    # Migrate existing databases: add ash/asd columns if missing
    for table in ("teams", "users", "user_keys"):
        for col in ("ash_limit", "asd_limit"):
            try:
                await conn.execute(f"ALTER TABLE {table} ADD COLUMN {col} INTEGER")
            except Exception:
                pass  # column already exists
    try:
        await conn.execute("ALTER TABLE usage_log ADD COLUMN error_type TEXT")
    except Exception:
        pass  # column already exists
    for col in ("monthly_budget_usd", "hard_limit"):
        try:
            await conn.execute(f"ALTER TABLE user_keys ADD COLUMN {col} {'REAL' if col == 'monthly_budget_usd' else 'INTEGER NOT NULL DEFAULT 1'}")
        except Exception:
            pass
    try:
        await conn.execute("ALTER TABLE user_keys ADD COLUMN ip_allowlist TEXT")
    except Exception:
        pass
    await conn.commit()


async def init_db(db_path: str) -> None:
    global _db_pool, _db_path
    if _db_pool is not None:
        return
    _db_path = db_path
    pool: asyncio.Queue[aiosqlite.Connection] = asyncio.Queue(maxsize=_POOL_SIZE)
    for _ in range(_POOL_SIZE):
        conn = await aiosqlite.connect(db_path)
        await _configure_conn(conn)
        await pool.put(conn)
    _db_pool = pool
    _log.info("database pool initialized at %s (%d connections)", db_path, _POOL_SIZE)


@asynccontextmanager
async def get_db() -> AsyncIterator[aiosqlite.Connection]:
    if _db_pool is None:
        raise RuntimeError("database not initialized")
    conn = await _db_pool.get()
    try:
        yield conn
    except (Exception, asyncio.CancelledError):
        try:
            await conn.close()
        except Exception:
            pass
        if _db_path:
            new_conn = await aiosqlite.connect(_db_path)
            await _configure_conn(new_conn)
            await _db_pool.put(new_conn)
        raise
    else:
        await _db_pool.put(conn)


async def close_db() -> None:
    global _db_pool
    if _db_pool is None:
        return
    pool = _db_pool
    _db_pool = None
    while not pool.empty():
        conn = await pool.get()
        await conn.close()
    _log.info("database pool closed")


async def prune_logs(usage_days: int = 30, admin_days: int = 90) -> tuple[int, int]:
    """Delete usage_log and admin_log entries older than the specified days.

    Returns a tuple of (usage_deleted, admin_deleted).
    """
    usage_cutoff = time.time() - usage_days * 86400
    admin_cutoff = time.time() - admin_days * 86400
    async with get_db() as conn:
        cursor = await conn.execute(
            "DELETE FROM usage_log WHERE REPLACE(created_at, 'T', ' ') < datetime(?, 'unixepoch')",
            (usage_cutoff,),
        )
        usage_deleted = cursor.rowcount
        cursor = await conn.execute(
            "DELETE FROM admin_log WHERE REPLACE(created_at, 'T', ' ') < datetime(?, 'unixepoch')",
            (admin_cutoff,),
        )
        admin_deleted = cursor.rowcount
        await conn.commit()
    return usage_deleted, admin_deleted


async def get_capabilities(model_id: int) -> dict[str, Any] | None:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT * FROM model_capabilities WHERE model_id = ?", (model_id,)
        )
        row = await cursor.fetchone()
    return dict(row) if row else None


async def save_capabilities(model_id: int, caps: dict[str, Any]) -> None:
    async with get_db() as db:
        await db.execute(
            """INSERT OR REPLACE INTO model_capabilities
               (model_id, supports_tools, supports_vision, supports_json, supports_stream,
                max_context, max_output, probed_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                model_id,
                int(caps.get("supports_tools", False)),
                int(caps.get("supports_vision", False)),
                int(caps.get("supports_json", False)),
                int(caps.get("supports_stream", True)),
                caps.get("max_context"),
                caps.get("max_output"),
                caps.get("probed_at"),
            ),
        )
        await db.commit()


async def log_trace(request_id: str, span_index: int, label: str, start_ms: int, end_ms: int,
                    model_name: str | None = None, provider: str | None = None,
                    status: str = "ok", detail: str | None = None) -> None:
    async with get_db() as db:
        await db.execute(
            """INSERT INTO request_traces
               (request_id, span_index, label, start_ms, end_ms, model_name, provider, status, detail)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (request_id, span_index, label, start_ms, end_ms, model_name, provider, status, detail),
        )
        await db.commit()


async def get_traces(request_id: str) -> list[dict[str, Any]]:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT * FROM request_traces WHERE request_id = ? ORDER BY span_index",
            (request_id,),
        )
        return [dict(row) for row in await cursor.fetchall()]
