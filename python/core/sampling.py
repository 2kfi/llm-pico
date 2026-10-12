from __future__ import annotations

import hashlib
import random
import time

from core.db import get_db


async def maybe_sample(
    request_id: str,
    model: str,
    prompt: str,
    response: str,
    sampling_rate: float = 0.0,
) -> None:
    if sampling_rate <= 0 or random.random() > sampling_rate:
        return

    prompt_hash = hashlib.sha256(prompt.encode()).hexdigest()[:16]
    prompt_preview = prompt[:200]
    response_preview = response[:200]
    created_at = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())

    async with get_db() as db:
        await db.execute(
            """INSERT INTO request_samples
               (request_id, model, prompt_hash, prompt_preview, response_preview, created_at)
               VALUES (?, ?, ?, ?, ?, ?)""",
            (request_id, model, prompt_hash, prompt_preview, response_preview, created_at),
        )
        await db.commit()
