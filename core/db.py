from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from typing import AsyncIterator

import aiosqlite

_db_connection: aiosqlite.Connection | None = None
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
    tpd_limit       INTEGER
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

CREATE TABLE IF NOT EXISTS request_cache (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    cache_key     TEXT    NOT NULL UNIQUE,
    response_body BLOB   NOT NULL,
    content_type  TEXT    NOT NULL DEFAULT 'application/json',
    created_at    TEXT    NOT NULL,
    expires_at    TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    action      TEXT    NOT NULL,
    actor_hash  TEXT    NOT NULL,
    details     TEXT,
    created_at  TEXT    NOT NULL
);
"""


async def init_db(db_path: str) -> None:
    global _db_connection
    _db_connection = await aiosqlite.connect(db_path)
    _db_connection.row_factory = aiosqlite.Row
    await _db_connection.execute("PRAGMA journal_mode=WAL")
    await _db_connection.execute("PRAGMA busy_timeout=5000")
    await _db_connection.execute("PRAGMA synchronous=NORMAL")
    await _db_connection.executescript(SCHEMA_SQL)
    await _db_connection.commit()
    _log.info("database initialized at %s", db_path)


async def close_db() -> None:
    global _db_connection
    if _db_connection:
        await _db_connection.close()
        _db_connection = None
        _log.info("database closed")


@asynccontextmanager
async def get_db() -> AsyncIterator[aiosqlite.Connection]:
    if _db_connection is None:
        raise RuntimeError("database not initialized")
    yield _db_connection
