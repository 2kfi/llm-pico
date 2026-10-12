from __future__ import annotations

import importlib
import logging
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from providers.base import BaseAdapter

_log = logging.getLogger("llm-pico.providers")

_registry: dict[str, type["BaseAdapter"]] = {}

_KNOWN_PROVIDERS = {
    "openai": "providers.openai",
    "openai-compat": "providers.openai_compat",
    "anthropic": "providers.anthropic",
    "gemini": "providers.gemini",
    "cloudflare": "providers.cloudflare",
    "cohere": "providers.cohere",
}


def register(provider: str):
    def decorator(cls: type["BaseAdapter"]):
        _registry[provider] = cls
        _log.debug("registered adapter: %s -> %s", provider, cls.__name__)
        return cls
    return decorator


def get_adapter(provider: str) -> type["BaseAdapter"] | None:
    if provider in _registry:
        return _registry[provider]
    module_path = _KNOWN_PROVIDERS.get(provider)
    if module_path:
        try:
            importlib.import_module(module_path)
            return _registry.get(provider)
        except ImportError:
            pass
    return None


def list_adapters() -> list[str]:
    return list(_registry.keys())


def load_custom_providers(directory: str | None = None) -> int:
    """Scan providers/custom/ for adapter modules, register any found. Returns count loaded."""
    from pathlib import Path
    import importlib.util

    if directory is None:
        directory = str(Path(__file__).parent / "custom")
    custom_dir = Path(directory)
    if not custom_dir.is_dir():
        return 0

    loaded = 0
    for py_file in sorted(custom_dir.glob("*.py")):
        if py_file.name.startswith("_"):
            continue
        mod_name = f"providers.custom.{py_file.stem}"
        try:
            spec = importlib.util.spec_from_file_location(mod_name, str(py_file))
            if spec and spec.loader:
                mod = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(mod)
                loaded += 1
        except Exception as e:
            _log.warning("custom provider %s failed to load: %s", py_file.name, e)
    if loaded:
        _log.info("loaded %d custom provider(s) from %s", loaded, custom_dir)
    return loaded
