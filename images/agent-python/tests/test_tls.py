# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The gateway identity the /v1/message handler accepts (runtime contract
item 4): both Service DNS forms, in the operator namespace the controller
injects."""

from __future__ import annotations

from kaalm_agent.tls import gateway_sans


def test_default_namespace_covers_both_service_dns_forms(monkeypatch):
    monkeypatch.delenv("KAALM_OPERATOR_NAMESPACE", raising=False)
    assert gateway_sans() == {
        "kaalm-gateway.kaalm-system.svc.cluster.local",
        "kaalm-gateway.kaalm-system.svc",
    }


def test_injected_namespace_replaces_the_default(monkeypatch):
    monkeypatch.setenv("KAALM_OPERATOR_NAMESPACE", "kaalm-ops")
    sans = gateway_sans()
    assert sans == {
        "kaalm-gateway.kaalm-ops.svc.cluster.local",
        "kaalm-gateway.kaalm-ops.svc",
    }
    assert "kaalm-gateway.kaalm-system.svc.cluster.local" not in sans


def test_empty_namespace_falls_back_to_the_default(monkeypatch):
    monkeypatch.setenv("KAALM_OPERATOR_NAMESPACE", "")
    assert gateway_sans() == {
        "kaalm-gateway.kaalm-system.svc.cluster.local",
        "kaalm-gateway.kaalm-system.svc",
    }
