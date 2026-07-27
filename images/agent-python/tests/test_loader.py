# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The loader matrix from docs/src/runtime/base-images.md: absent env serves
echo, a loadable handler runs, and every flavor of broken fails fatally with a
message naming the failure. No silent fallback, ever."""

from __future__ import annotations

import logging

import pytest

from kaalm_agent import loader

ASYNC_HANDLER = """
async def handle_message(envelope):
    return {"content": "custom: " + envelope.get("content", "")}
"""

SYNC_HANDLER = """
def handle_message(envelope):
    return {"content": "sync: " + envelope.get("content", "")}
"""


async def test_no_env_serves_echo():
    handler, source = loader.resolve()
    assert "echo" in source
    reply = await handler({"content": "hi"})
    assert reply["content"] == "echo: hi"


async def test_empty_env_serves_echo(monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", "")
    handler, source = loader.resolve()
    assert "echo" in source


async def test_async_handler_loads(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir(ASYNC_HANDLER)))
    handler, source = loader.resolve()
    assert "handler.py" in source
    reply = await handler({"content": "hi"})
    assert reply["content"] == "custom: hi"


async def test_sync_handler_is_wrapped(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir(SYNC_HANDLER)))
    handler, _ = loader.resolve()
    reply = await handler({"content": "hi"})
    assert reply["content"] == "sync: hi"


async def test_sibling_modules_are_importable(handler_dir, monkeypatch):
    d = handler_dir(
        "import helper\n"
        "async def handle_message(envelope):\n"
        "    return {'content': helper.decorate(envelope.get('content', ''))}\n",
        helper="def decorate(text):\n    return '<<' + text + '>>'\n",
    )
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(d))
    handler, _ = loader.resolve()
    reply = await handler({"content": "hi"})
    assert reply["content"] == "<<hi>>"


def test_missing_directory_fails(monkeypatch, tmp_path):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(tmp_path / "absent"))
    with pytest.raises(loader.HandlerLoadError, match="does not exist"):
        loader.resolve()


def test_missing_handler_file_fails(monkeypatch, tmp_path):
    d = tmp_path / "empty-mount"
    d.mkdir()
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(d))
    with pytest.raises(loader.HandlerLoadError, match="handler.py"):
        loader.resolve()


def test_broken_import_fails_and_names_the_error(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir("import does_not_exist_anywhere\n")))
    with pytest.raises(loader.HandlerLoadError, match="does_not_exist_anywhere"):
        loader.resolve()


def test_syntax_error_fails(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir("def handle_message(:\n")))
    with pytest.raises(loader.HandlerLoadError, match="importing"):
        loader.resolve()


def test_missing_function_fails(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir("x = 1\n")))
    with pytest.raises(loader.HandlerLoadError, match="no callable handle_message"):
        loader.resolve()


def test_non_callable_fails(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir("handle_message = 42\n")))
    with pytest.raises(loader.HandlerLoadError, match="no callable"):
        loader.resolve()


def test_removed_two_arg_form_gets_migration_hint(handler_dir, monkeypatch):
    monkeypatch.setenv(
        "KAALM_HANDLER_PATH",
        str(handler_dir("async def handle_message(agent, envelope):\n    return {}\n")),
    )
    with pytest.raises(loader.HandlerLoadError, match="removed in v0.3.0"):
        loader.resolve()


def test_zero_arg_handler_fails(handler_dir, monkeypatch):
    monkeypatch.setenv(
        "KAALM_HANDLER_PATH", str(handler_dir("def handle_message():\n    return {}\n"))
    )
    with pytest.raises(loader.HandlerLoadError, match="exactly one required argument"):
        loader.resolve()


def test_defaulted_second_arg_is_accepted(handler_dir, monkeypatch):
    monkeypatch.setenv(
        "KAALM_HANDLER_PATH",
        str(handler_dir("def handle_message(envelope, extra=None):\n    return {'content': 'ok'}\n")),
    )
    handler, _ = loader.resolve()  # must not raise: one REQUIRED argument


def test_load_or_exit_exits_nonzero(handler_dir, monkeypatch):
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(handler_dir("x = 1\n")))
    with pytest.raises(SystemExit) as exc:
        loader.load_or_exit(logging.getLogger("test"))
    assert exc.value.code == 1


async def test_handler_can_import_kaalm_after_bind(handler_dir, monkeypatch):
    """The startup order contract: _bind happens before the handler import, so
    a top-level `import kaalm` plus attribute use inside the handler works."""
    import kaalm

    class FakeMemory:
        def get(self, key, default=None):
            return "remembered"

    kaalm._bind(gateway=object(), memory=FakeMemory())
    d = handler_dir(
        "import kaalm\n"
        "async def handle_message(envelope):\n"
        "    return {'content': kaalm.memory.get('x')}\n"
    )
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(d))
    handler, _ = loader.resolve()
    reply = await handler({})
    assert reply["content"] == "remembered"
