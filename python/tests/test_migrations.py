from __future__ import annotations

import asyncio
import os
import tempfile

import pytest


@pytest.fixture
def tmp_db(tmp_path):
    db_path = str(tmp_path / "test.db")
    os.environ["LLM_PICO_DB"] = db_path
    yield db_path
    os.environ.pop("LLM_PICO_DB", None)


@pytest.mark.asyncio
async def test_schema_version_table_exists(tmp_db):
    from core.db import init_db, get_db, close_db

    await init_db(tmp_db)
    try:
        async with get_db() as db:
            cursor = await db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='schema_version'")
            row = await cursor.fetchone()
            assert row is not None, "schema_version table should exist"
    finally:
        await close_db()


@pytest.mark.asyncio
async def test_get_schema_version_empty(tmp_db):
    from core.db import init_db, close_db
    from core.migrations import get_schema_version

    await init_db(tmp_db)
    try:
        version = await get_schema_version()
        assert version == 0
    finally:
        await close_db()


@pytest.mark.asyncio
async def test_run_migrations_applies(tmp_db):
    from core.db import init_db, get_db, close_db
    from core.migrations import run_migrations, get_schema_version

    await init_db(tmp_db)
    try:
        applied = await run_migrations()
        assert applied >= 1
        version = await get_schema_version()
        assert version >= 1
    finally:
        await close_db()


@pytest.mark.asyncio
async def test_run_migrations_idempotent(tmp_db):
    from core.db import init_db, close_db
    from core.migrations import run_migrations

    await init_db(tmp_db)
    try:
        first = await run_migrations()
        second = await run_migrations()
        assert second == 0, "second run should apply nothing"
    finally:
        await close_db()
