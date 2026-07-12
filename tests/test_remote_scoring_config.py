from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
import time

import pytest

import backend.grader as grader
import backend.main as main


@pytest.fixture(autouse=True)
def reset_subjective_service(monkeypatch):
    grader.set_subjective_service(None)
    for name in (
        "RERANK_USE_REMOTE",
        "RERANK_API_URL",
        "RERANK_API_KEY",
        "RERANK_MODEL",
    ):
        monkeypatch.delenv(name, raising=False)
    yield
    grader.set_subjective_service(None)


def test_default_service_uses_local_model_when_remote_is_not_configured(monkeypatch):
    calls: list[dict] = []

    class FakeService:
        def __init__(self, **kwargs):
            calls.append(kwargs)

    monkeypatch.setattr(grader, "SubjectiveScoringService", FakeService)

    service = grader.get_subjective_service()

    assert isinstance(service, FakeService)
    assert calls == [{"allow_model_load": True}]


def test_remote_credentials_do_not_enable_cloud_without_switch(monkeypatch):
    calls: list[dict] = []

    class FakeService:
        def __init__(self, **kwargs):
            calls.append(kwargs)

    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "test-model")
    monkeypatch.setattr(grader, "SubjectiveScoringService", FakeService)

    service = grader.get_subjective_service()

    assert isinstance(service, FakeService)
    assert calls == [{"allow_model_load": True}]


def test_explicit_false_uses_local_model_with_remote_credentials(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "false")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "test-model")

    assert grader.validate_remote_reranker_config() is None


def test_default_service_uses_cloud_reranker_when_enabled(monkeypatch):
    calls: list[dict] = []
    reranker_args: list[dict] = []

    class FakeReranker:
        def __init__(self, **kwargs):
            reranker_args.append(kwargs)

        def close(self):
            pass

    class FakeService:
        def __init__(self, **kwargs):
            calls.append(kwargs)

    monkeypatch.setenv("RERANK_USE_REMOTE", "TrUe")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "Pro/BAAI/bge-reranker-v2-m3")
    monkeypatch.setattr(grader, "CohereRerankerPairScorer", FakeReranker)
    monkeypatch.setattr(grader, "SubjectiveScoringService", FakeService)

    service = grader.get_subjective_service()

    assert isinstance(service, FakeService)
    assert reranker_args == [
        {
            "url": "https://router.example.test/v1/rerank",
            "api_key": "secret-key",
            "model": "Pro/BAAI/bge-reranker-v2-m3",
        }
    ]
    reranker = calls[0]["text_pair_scorer"]
    assert isinstance(reranker, FakeReranker)
    assert calls == [
        {
            "allow_model_load": False,
            "text_pair_scorer": reranker,
            "code_pair_scorer": reranker,
        }
    ]


def test_partial_remote_configuration_fails_without_exposing_secret(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "true")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")

    with pytest.raises(RuntimeError, match="RERANK_MODEL") as caught:
        grader.get_subjective_service()

    assert "secret-key" not in str(caught.value)


def test_enabled_remote_requires_all_connection_settings(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "true")

    with pytest.raises(RuntimeError, match="RERANK_API_URL") as caught:
        grader.get_subjective_service()

    assert "RERANK_API_KEY, RERANK_MODEL" in str(caught.value)


def test_preflight_rejects_partial_remote_configuration(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "true")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")

    with pytest.raises(RuntimeError, match="RERANK_MODEL"):
        main._preflight_check()


def test_preflight_rejects_invalid_remote_switch(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "yes")

    with pytest.raises(RuntimeError, match="RERANK_USE_REMOTE"):
        main._preflight_check()


def test_replacing_service_closes_owned_remote_client(monkeypatch):
    closed: list[bool] = []

    class FakeReranker:
        def __init__(self, **kwargs):
            pass

        def close(self):
            closed.append(True)

    class FakeService:
        def __init__(self, **kwargs):
            pass

    monkeypatch.setenv("RERANK_USE_REMOTE", "true")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "test-model")
    monkeypatch.setattr(grader, "CohereRerankerPairScorer", FakeReranker)
    monkeypatch.setattr(grader, "SubjectiveScoringService", FakeService)

    grader.get_subjective_service()
    grader.set_subjective_service(None)

    assert closed == [True]


def test_concurrent_first_access_creates_one_remote_service(monkeypatch):
    reranker_count = 0
    service_count = 0

    class FakeReranker:
        def __init__(self, **kwargs):
            nonlocal reranker_count
            reranker_count += 1
            time.sleep(0.02)

        def close(self):
            pass

    class FakeService:
        def __init__(self, **kwargs):
            nonlocal service_count
            service_count += 1

    monkeypatch.setenv("RERANK_USE_REMOTE", "true")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "test-model")
    monkeypatch.setattr(grader, "CohereRerankerPairScorer", FakeReranker)
    monkeypatch.setattr(grader, "SubjectiveScoringService", FakeService)

    with ThreadPoolExecutor(max_workers=4) as executor:
        services = list(executor.map(lambda _: grader.get_subjective_service(), range(8)))

    assert len({id(service) for service in services}) == 1
    assert reranker_count == 1
    assert service_count == 1


def test_shutdown_waits_for_grading_before_closing_service(monkeypatch):
    events: list[str] = []

    class FakeExecutor:
        def shutdown(self, *, wait, cancel_futures):
            assert wait is True
            assert cancel_futures is False
            events.append("executor")

    monkeypatch.setattr(main, "_grading_executor", FakeExecutor())
    monkeypatch.setattr(
        grader, "close_subjective_service", lambda: events.append("service")
    )

    main._shutdown_runtime()

    assert events == ["executor", "service"]
