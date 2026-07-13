import logging
import sys
from pathlib import Path

import click
import uvicorn

from core.config import load_config, load_users
from api.server import create_app


def _run_server(host: str, port: int, config: str, users: str | None, db: str | None, verbose: bool):
    """Shared server startup logic."""
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
    config_path = Path(config).resolve()
    config_dir = config_path.parent

    if not config_path.exists():
        click.echo(f"Error: config file not found: {config}")
        click.echo("Run 'llm-pico init' to create one.")
        raise SystemExit(1)

    if users is None:
        users_path = config_dir / "users.yaml"
        if not users_path.exists():
            users_path = config_dir / "users.yml"
    else:
        users_path = Path(users).resolve()

    users_list = []
    if users_path.exists():
        users_list = load_users(str(users_path))
        log.info("loaded %d user keys from %s", len(users_list), users_path)
    else:
        log.info("no users file found at %s, admin API only", users_path)

    if db is None:
        db_path = config_dir / "llm-pico.db"
    else:
        db_path = Path(db).resolve()
    log.info("database: %s", db_path)

    cfg = load_config(str(config_path))
    log.info("config loaded: %d model entries", len(cfg.model_list))

    app_state = {
        "config": cfg,
        "users": users_list,
        "db_path": str(db_path),
        "log": log,
        "verbose": verbose,
    }

    app = create_app(app_state)

    log.info("llm-pico v0.1.0 starting on http://%s:%s", host, port)
    uvicorn.run(
        app,
        host=host,
        port=port,
        log_level="debug" if verbose else "info",
        lifespan="on",
    )


@click.group(invoke_without_command=True)
@click.option("--host", default="0.0.0.0", show_default=True, help="Listen host")
@click.option("--port", default=4000, show_default=True, help="Listen port", type=int)
@click.option("--config", default="config.yaml", show_default=True, help="Path to config.yaml", type=click.Path(dir_okay=False))
@click.option("--users", default=None, help="Path to users.yaml (auto-detected if not set)", type=click.Path(dir_okay=False))
@click.option("--db", default=None, help="Path to SQLite database (auto-detected if not set)", type=click.Path(dir_okay=False))
@click.option("--verbose", "-v", is_flag=True, help="Enable verbose logging")
@click.version_option(version="0.1.0", prog_name="llm-pico")
@click.pass_context
def main(ctx, host: str, port: int, config: str, users: str | None, db: str | None, verbose: bool):
    if ctx.invoked_subcommand is not None:
        return
    _run_server(host, port, config, users, db, verbose)


@main.command()
@click.option("--host", default="0.0.0.0", show_default=True, help="Listen host")
@click.option("--port", default=4000, show_default=True, help="Listen port", type=int)
@click.option("--config", default="config.yaml", show_default=True, help="Path to config.yaml", type=click.Path(exists=True, dir_okay=False))
@click.option("--users", default=None, help="Path to users.yaml (auto-detected if not set)", type=click.Path(dir_okay=False))
@click.option("--db", default=None, help="Path to SQLite database (auto-detected if not set)", type=click.Path(dir_okay=False))
@click.option("--verbose", "-v", is_flag=True, help="Enable verbose logging")
def serve(host: str, port: int, config: str, users: str | None, db: str | None, verbose: bool):
    """Start the llm-pico server."""
    _run_server(host, port, config, users, db, verbose)


from api.init_cmd import init_command

main.add_command(init_command, "init")


if __name__ == "__main__":
    main()
