# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""LangChain's ChatAnthropic on the rotation-aware httpx2 clients, behind
kaalm.langchain_chat_anthropic.

ChatAnthropic takes no http_client or http_async_client argument: it builds
its Anthropic SDK clients in the private cached properties _client and
_async_client, from the parameters in _client_params. So the
kaalm.http_client factories cannot reach it by argument. The subclass here
replaces those two properties with ones that build the SDK clients on the
kaalm httpx2 clients, which carry the Pod's mTLS identity and follow
certificate rotation.

The image does not bundle LangChain: langchain_anthropic is imported on the
first call, and a release that no longer builds its clients in those
properties raises instead of returning a model whose clients would bypass the
Pod's identity.
"""

from __future__ import annotations

import functools
from typing import Any

# A proxy would mount proxy transports around the identity-bearing transport,
# so the factory owns it. ANTHROPIC_PROXY in the environment is ignored for
# the same reason the http client factories set trust_env=False.
_RESERVED_KWARGS = ("anthropic_proxy",)

# The ChatAnthropic cached properties the subclass replaces or reads.
_REQUIRED_PROPERTIES = ("_client", "_async_client", "_client_params")

PLACEHOLDER_API_KEY = "managed-by-kaalm"


@functools.cache
def _model_class(http_client: Any, http_async_client: Any) -> type:
    """The ChatAnthropic subclass for these client factories, built on first use.

    The factories reach the subclass through this closure, not as class
    attributes: pydantic turns an underscore class attribute on a model into
    a private attribute and copies its default into every instance.
    """
    try:
        import langchain_anthropic
    except ModuleNotFoundError as exc:
        if exc.name != "langchain_anthropic":
            raise
        raise ImportError(
            "kaalm.langchain_chat_anthropic needs the langchain-anthropic package; "
            "the kaalm-agent-python image does not bundle it, install it in your "
            "FROM build (pip install langchain-anthropic)"
        ) from exc
    import anthropic

    base = langchain_anthropic.ChatAnthropic
    for name in _REQUIRED_PROPERTIES:
        if not isinstance(vars(base).get(name), functools.cached_property):
            raise RuntimeError(
                f"langchain-anthropic {getattr(langchain_anthropic, '__version__', '(unknown version)')} "
                f"has no ChatAnthropic.{name} cached property, which "
                "kaalm.langchain_chat_anthropic relies on to build the Anthropic clients; "
                "no model is returned, because its clients would not carry the Pod's identity"
            )

    class KaalmChatAnthropic(base):
        """ChatAnthropic whose Anthropic SDK clients use the kaalm httpx2 clients."""

        @functools.cached_property
        def _client(self) -> Any:
            return anthropic.Client(**self._client_params, http_client=http_client(package="httpx2"))

        @functools.cached_property
        def _async_client(self) -> Any:
            return anthropic.AsyncClient(
                **self._client_params, http_client=http_async_client(package="httpx2")
            )

    return KaalmChatAnthropic


def make_chat_anthropic(http_client: Any, http_async_client: Any, gateway_url: str, /, **kwargs: Any) -> Any:
    """A ChatAnthropic whose sync and async clients carry the Pod's identity.

    base_url defaults to the gateway endpoint (the Anthropic SDK appends
    /v1/messages) and api_key to a placeholder. Either
    spelling ChatAnthropic accepts (base_url or anthropic_api_url, api_key or
    anthropic_api_key) counts as given. Remaining keyword arguments pass
    through to ChatAnthropic; anthropic_proxy is rejected.
    """
    for name in _RESERVED_KWARGS:
        if name in kwargs:
            raise TypeError(
                f"{name!r} is owned by kaalm.langchain_chat_anthropic: the model's "
                "clients come from the runtime's identity-bearing http client factories"
            )
    if "base_url" not in kwargs and "anthropic_api_url" not in kwargs:
        kwargs["base_url"] = gateway_url
    if "api_key" not in kwargs and "anthropic_api_key" not in kwargs:
        kwargs["api_key"] = PLACEHOLDER_API_KEY
    return _model_class(http_client, http_async_client)(**kwargs)
