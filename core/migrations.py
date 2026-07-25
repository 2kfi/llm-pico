from __future__ import annotations

import logging
from datetime import datetime, timezone

from core.db import get_db

_log = logging.getLogger("llm-pico.migrations")

MIGRATIONS = [
    {
        "version": 1,
        "sql": """
CREATE TABLE IF NOT EXISTS schema_version (
    version     INTEGER PRIMARY KEY,
    applied_at  TEXT NOT NULL
);
""",
    },
    {
        "version": 2,
        "sql": """
ALTER TABLE models ADD COLUMN fallback_chain TEXT;
""",
    },
    {
        "version": 3,
        "sql": """
ALTER TABLE teams ADD COLUMN model_chain TEXT;
ALTER TABLE teams ADD COLUMN chain_rewrites_response TEXT;
ALTER TABLE models ADD COLUMN chain_budget_usd REAL;
""",
    },
]


async def get_schema_version() -> int:
    async with get_db() as db:
        try:
            cursor = await db.execute("SELECT MAX(version) FROM schema_version")
            row = await cursor.fetchone()
            return row[0] if row and row[0] else 0
        except Exception:
            return 0


async def run_migrations() -> int:
    current = await get_schema_version()
    applied = 0
    for m in MIGRATIONS:
        if m["version"] > current:
            _log.info("applying migration v%d", m["version"])
            async with get_db() as db:
                for stmt in m["sql"].split(";"):
                    stmt = stmt.strip()
                    if stmt:
                        try:
                            await db.execute(stmt)
                        except Exception as e:
                            if "duplicate column" in str(e).lower():
                                _log.debug("column already exists, skipping")
                            else:
                                raise
                await db.execute(
                    "INSERT OR REPLACE INTO schema_version (version, applied_at) VALUES (?, ?)",
                    (m["version"], datetime.now(timezone.utc).isoformat()),
                )
                await db.commit()
            applied += 1
    return applied
