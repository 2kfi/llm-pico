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
    user_id         INTEGER REFERENCES users(id)
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
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    action      TEXT    NOT NULL,
    actor_hash  TEXT    NOT NULL,
    details     TEXT,
    created_at  TEXT    NOT NULL
);
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
