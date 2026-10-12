-- llm-pico schema.
--
-- This file is the single source of truth for the database layout and MUST stay
-- byte-compatible with python/core/db.py SCHEMA_SQL. TestSchemaParity in
-- internal/store/schema_test.go asserts that the two agree.

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
