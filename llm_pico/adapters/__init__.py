from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .base import BaseAdapter

_registry: dict[str, type["BaseAdapter"]] = {}


def register(provider: str):
    def decorator(cls: type["BaseAdapter"]):
        _registry[provider] = cls
        return cls
    return decorator


def get_adapter(provider: str) -> type["BaseAdapter"] | None:
    return _registry.get(provider)


def list_adapters() -> list[str]:
    return list(_registry.keys())


from . import anthropic  # noqa: F811, E402
from . import gemini  # noqa: F811, E402
from . import cloudflare  # noqa: F811, E402
