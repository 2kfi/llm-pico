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
