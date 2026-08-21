"""ASGI middleware publishing the active model's declared capabilities."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Awaitable, Callable


_SCHEMA_VERSION = 1
_ALLOWED_LEVELS = ("minimal", "low", "medium", "high", "xhigh", "max")
_MAX_QUANTIZATION_LENGTH = 32


def _fail(message: str) -> ValueError:
    return ValueError(f"invalid model capability profile: {message}")


def load_capabilities(path: Path, *, model: str, context_window: int) -> dict[str, Any]:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise _fail(f"cannot read {path}") from error
    if not isinstance(raw, dict) or set(raw) != {
        "schema_version",
        "api",
        "input",
        "max_output_tokens",
        "tools",
        "quantization",
        "reasoning",
    }:
        raise _fail("unexpected top-level fields")
    if raw["schema_version"] != _SCHEMA_VERSION:
        raise _fail("unsupported schema_version")
    if raw["api"] != "openai-completions":
        raise _fail("api must be openai-completions")
    if raw["input"] not in (["text"], ["text", "image"]):
        raise _fail("input must be text or text+image")
    if not isinstance(raw["max_output_tokens"], int) or raw["max_output_tokens"] < 1:
        raise _fail("max_output_tokens must be positive")
    quantization = raw["quantization"]
    if not isinstance(quantization, dict) or set(quantization) != {"weights"}:
        raise _fail("quantization fields are invalid")
    weight_quantization = quantization["weights"]
    if (
        not isinstance(weight_quantization, str)
        or not weight_quantization
        or len(weight_quantization) > _MAX_QUANTIZATION_LENGTH
        or not all(character.isalnum() or character in "._+-" for character in weight_quantization)
    ):
        raise _fail("weight quantization is invalid")


    tools = raw["tools"]
    if not isinstance(tools, dict) or set(tools) != {"supported", "parallel"}:
        raise _fail("tools fields are invalid")
    if not isinstance(tools["supported"], bool) or not isinstance(tools["parallel"], bool):
        raise _fail("tools values must be boolean")
    if tools["parallel"] and not tools["supported"]:
        raise _fail("parallel tools require tool support")

    reasoning = raw["reasoning"]
    expected_reasoning = {
        "supported",
        "can_disable",
        "levels",
        "default",
        "request_format",
        "response_field",
    }
    if not isinstance(reasoning, dict) or set(reasoning) != expected_reasoning:
        raise _fail("reasoning fields are invalid")
    if not isinstance(reasoning["supported"], bool) or not isinstance(reasoning["can_disable"], bool):
        raise _fail("reasoning flags must be boolean")
    levels = reasoning["levels"]
    if not isinstance(levels, list) or len(levels) != len(set(levels)):
        raise _fail("reasoning levels must be a unique list")
    if any(level not in _ALLOWED_LEVELS for level in levels):
        raise _fail("reasoning level is unsupported by the schema")
    if levels != [level for level in _ALLOWED_LEVELS if level in levels]:
        raise _fail("reasoning levels must use canonical order")
    if reasoning["supported"] != bool(levels):
        raise _fail("reasoning support must agree with levels")
    if reasoning["default"] not in levels and reasoning["default"] is not None:
        raise _fail("reasoning default must be a published level")
    if reasoning["request_format"] not in (None, "qwen-chat-template"):
        raise _fail("reasoning request_format is unsupported")
    if reasoning["response_field"] not in (None, "reasoning_content", "reasoning"):
        raise _fail("reasoning response_field is unsupported")
    if not reasoning["supported"] and any(
        value is not None for value in (reasoning["default"], reasoning["request_format"], reasoning["response_field"])
    ):
        raise _fail("disabled reasoning cannot publish protocol fields")

    if not model or context_window < 1:
        raise _fail("runtime model identity is invalid")
    return {
        "object": "model.capabilities",
        "schema_version": _SCHEMA_VERSION,
        "model": model,
        "api": raw["api"],
        "input": raw["input"],
        "context_window": context_window,
        "max_output_tokens": raw["max_output_tokens"],
        "quantization": quantization,
        "tools": tools,
        "reasoning": reasoning,
    }


class ModelCapabilitiesMiddleware:
    """Serve a fail-closed capability document without changing inference routes."""

    def __init__(self, app: Callable[..., Awaitable[None]]) -> None:
        self.app = app
        profile = os.environ.get("DGX_MODEL_CAPABILITIES_PATH")
        model = os.environ.get("SERVED")
        context_window = os.environ.get("MAX_MODEL_LEN")
        if not profile or not model or not context_window:
            raise _fail("required environment is missing")
        try:
            parsed_context_window = int(context_window)
        except ValueError as error:
            raise _fail("MAX_MODEL_LEN must be an integer") from error
        self.document = load_capabilities(
            Path(profile),
            model=model,
            context_window=parsed_context_window,
        )
        self.body = json.dumps(self.document, separators=(",", ":"), sort_keys=True).encode()

    async def __call__(
        self,
        scope: dict[str, Any],
        receive: Callable[..., Awaitable[dict[str, Any]]],
        send: Callable[[dict[str, Any]], Awaitable[None]],
    ) -> None:
        if scope.get("type") == "http" and scope.get("path") == "/v1/model-capabilities":
            if scope.get("method") != "GET":
                await send({"type": "http.response.start", "status": 405, "headers": [(b"allow", b"GET")]})
                await send({"type": "http.response.body", "body": b""})
                return
            await send(
                {
                    "type": "http.response.start",
                    "status": 200,
                    "headers": [
                        (b"content-type", b"application/json"),
                        (b"content-length", str(len(self.body)).encode()),
                        (b"cache-control", b"no-store"),
                    ],
                }
            )
            await send({"type": "http.response.body", "body": self.body})
            return
        await self.app(scope, receive, send)
