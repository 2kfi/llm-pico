import asyncio
import logging
import os
from pathlib import Path

import click
import uvicorn


def _run_server(host: str, port: int, db: str, verbose: bool):
    level = logging.DEBUG if verbose else logging.INFO
    logging.basicConfig(
        level=level,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    )
    logging.getLogger("httpx").setLevel(logging.WARNING)
    logging.getLogger("aiosqlite").setLevel(logging.WARNING)
    logging.getLogger("uvicorn.access").setLevel(logging.WARNING)

    log = logging.getLogger("llm-pico")
    db_path = Path(db).resolve()
    log.info("database: %s", db_path)

    from core.db import init_db
    from core.config import load_config_from_db

    loop = asyncio.new_event_loop()
    loop.run_until_complete(init_db(str(db_path)))
    cfg = loop.run_until_complete(load_config_from_db())
    log.info("loaded %d models from database", len(cfg.model_list))

    app_state = {
        "config": cfg,
        "db_path": str(db_path),
        "log": log,
        "verbose": verbose,
    }

    from api.server import create_app
    app = create_app(app_state)

    log.info("llm-pico v0.1.0 starting on http://%s:%s", host, port)
    uvicorn.run(
        app,
        host=host,
        port=port,
        log_level="debug" if verbose else "info",
        lifespan="on",
    )


@click.command()
@click.option("--host", default="0.0.0.0", show_default=True, help="Listen host")
@click.option("--port", default=4000, show_default=True, help="Listen port", type=int)
@click.option("--db", default="llm-pico.db", show_default=True, help="SQLite database path", type=click.Path(dir_okay=False))
@click.option("--verbose", "-v", is_flag=True, help="Enable verbose logging")
@click.option("--workers", default=1, show_default=True, help="Number of worker processes", type=int)
@click.version_option(version="0.1.0", prog_name="llm-pico")
def main(host: str, port: int, db: str, verbose: bool, workers: int):
    """llm-pico — lightweight LLM proxy"""
    log = logging.getLogger("llm-pico")
    if workers > 1:
        log.info("workers flag set to %d (uvicorn single-process mode)", workers)
    _run_server(host, port, db, verbose)


if __name__ == "__main__":
    main()
