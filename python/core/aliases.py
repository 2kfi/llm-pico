from __future__ import annotations

import difflib
from typing import Any

from core.db import get_db

MODEL_ALIASES: dict[str, str] = {
    "gpt4": "gpt-4",
    "gpt4o": "gpt-4o",
    "gpt-4-turbo": "gpt-4-turbo-2024-04-09",
    "claude3": "claude-3-sonnet-20240229",
    "claude3haiku": "claude-3-haiku-20240307",
    "claude3opus": "claude-3-opus-20240229",
    "gemini-pro": "gemini-1.5-pro",
    "gemini-flash": "gemini-1.5-flash",
    "llama3": "llama-3-70b-versatile",
}


def fuzzy_match_model(query: str, available: list[str]) -> str | None:
    if query in available:
        return query

    alias_target = MODEL_ALIASES.get(query.lower())
    if alias_target and alias_target in available:
        return alias_target

    matches = difflib.get_close_matches(query, available, n=1, cutoff=0.6)
    return matches[0] if matches else None


async def resolve_alias(alias: str, available_models: list[str]) -> str | None:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT model_name, priority FROM model_aliases WHERE alias = ? ORDER BY priority DESC",
            (alias,),
        )
        row = await cursor.fetchone()
        if row:
            return row["model_name"]

    return fuzzy_match_model(alias, available_models)


async def add_alias(alias: str, model_name: str, priority: int = 0) -> None:
    async with get_db() as db:
        await db.execute(
            "INSERT OR REPLACE INTO model_aliases (alias, model_name, priority) VALUES (?, ?, ?)",
            (alias.lower(), model_name, priority),
        )
        await db.commit()


async def remove_alias(alias: str) -> bool:
    async with get_db() as db:
        cursor = await db.execute("DELETE FROM model_aliases WHERE alias = ?", (alias.lower(),))
        await db.commit()
        return cursor.rowcount > 0


async def list_aliases() -> list[dict[str, Any]]:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT alias, model_name, priority FROM model_aliases ORDER BY priority DESC, alias"
        )
        rows = await cursor.fetchall()
    return [dict(r) for r in rows]
