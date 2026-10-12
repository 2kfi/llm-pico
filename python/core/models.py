from __future__ import annotations

from datetime import datetime
from typing import Any, Literal

from pydantic import BaseModel, Field


class ChatCompletionRequest(BaseModel):
    model: str
    messages: list[dict[str, Any]]
    stream: bool | None = False
    max_tokens: int | None = 4096
    temperature: float | None = None
    top_p: float | None = None
    stop: str | list[str] | None = None
    frequency_penalty: float | None = None
    presence_penalty: float | None = None
    seed: int | None = None
    tools: list[dict[str, Any]] | None = None
    tool_choice: str | dict[str, Any] | None = None
    user: str | None = None
    extra_body: dict[str, Any] | None = None


class DeltaMessage(BaseModel):
    role: str | None = None
    content: str | None = None
    tool_calls: list[dict[str, Any]] | None = None


class Choice(BaseModel):
    index: int = 0
    delta: DeltaMessage | None = None
    message: dict[str, Any] | None = None
    finish_reason: str | None = None
    logprobs: dict[str, Any] | None = None


class Usage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0


class ChatCompletionResponse(BaseModel):
    id: str
    object: str = "chat.completion"
    created: int
    model: str
    choices: list[Choice]
    usage: Usage | None = None
    system_fingerprint: str | None = None


class CompletionRequest(BaseModel):
    model: str
    prompt: str | list[str] | list[int] | list[list[int]]
    stream: bool | None = False
    max_tokens: int | None = 256
    temperature: float | None = None
    top_p: float | None = None
    stop: str | list[str] | None = None


class CompletionResponse(BaseModel):
    id: str
    object: str = "text_completion"
    created: int
    model: str
    choices: list[dict[str, Any]]
    usage: Usage | None = None


class EmbeddingRequest(BaseModel):
    model: str
    input: str | list[str] | list[int] | list[list[int]]
    encoding_format: str | None = "float"
    user: str | None = None


class EmbeddingResponse(BaseModel):
    object: str = "list"
    data: list[dict[str, Any]]
    model: str
    usage: Usage


class ModelObject(BaseModel):
    id: str
    object: str = "model"
    created: int
    owned_by: str = "llm-pico"


class ModelList(BaseModel):
    object: str = "list"
    data: list[ModelObject]


class ErrorResponse(BaseModel):
    error: dict[str, Any]


class UserKeyCreate(BaseModel):
    label: str | None = None
    models: list[str] | None = None
    rpm_limit: int | None = None
    rpd_limit: int | None = None
    tpm_limit: int | None = None
    tpd_limit: int | None = None


class UserKeyResponse(BaseModel):
    key_prefix: str
    label: str | None
    is_active: bool
    created_at: str
    expires_at: str | None
    model_allowlist: list[str] | None
    rpm_limit: int | None
    rpd_limit: int | None
    tpm_limit: int | None
    tpd_limit: int | None


class KeyList(BaseModel):
    keys: list[UserKeyResponse]
    total: int


class UsageStats(BaseModel):
    total_prompt_tokens: int = 0
    total_completion_tokens: int = 0
    total_tokens: int = 0
    total_requests: int = 0
    total_cost_usd: float = 0.0


class UsageSummary(BaseModel):
    summary: UsageStats
    per_key: list[dict[str, Any]] = []
    per_model: list[dict[str, Any]] = []
