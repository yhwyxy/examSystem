"""build_scoring_service 测试: DB 评分设置 -> 服务构造 (不发起真实请求)."""
from __future__ import annotations

import pytest

import scoring_worker.grader_bridge as gb


def test_build_scoring_service_local_default(monkeypatch):
    monkeypatch.delenv("RERANKER_MODEL", raising=False)
    monkeypatch.delenv("RERANK_USE_REMOTE", raising=False)
    monkeypatch.setattr(gb, "_service_singleton", None)
    monkeypatch.setattr(gb, "_remote_reranker", None)
    # local 模式: allow_model_load=True 且 text_model/code_model 指向默认
    svc = gb.build_scoring_service({"method": "local"})
    assert svc is not None
    assert svc.allow_model_load is True


def _strip_proxy(monkeypatch):
    """清除代理环境变量: httpx 构造时若检测到 SOCKS proxy 会要求 socksio, 测试环境不应依赖."""
    for k in ("ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy",
              "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"):
        monkeypatch.delenv(k, raising=False)


def test_build_scoring_service_llm_config(monkeypatch):
    """method=llm + 完整 API 配置 -> 构造 LLM judge 服务 (不触发请求)."""
    _strip_proxy(monkeypatch)
    import subjective_scoring
    from subjective_scoring import LLMJudgeScorer, ScoringMode

    real_init = subjective_scoring.SubjectiveScoringService.__init__
    captured = {}

    def spy_init(self, *a, **kw):
        captured.update(kw)
        return real_init(self, *a, **kw)

    monkeypatch.setattr(subjective_scoring.SubjectiveScoringService, "__init__", spy_init)
    svc = gb.build_scoring_service({
        "method": "llm",
        "llm_api_url": "https://llm.example/v1",
        "llm_api_key": "sk-secret",
        "llm_model": "gpt-x",
    })
    assert captured.get("llm_judge") is not None
    assert captured.get("allow_model_load") is False
    assert captured.get("judge_fallback") is True
    # 四类题型全部注册 LLMJudgeScorer
    for mode, scorer in svc._scorers.items():
        assert isinstance(scorer, LLMJudgeScorer), f"{mode} 应注册 LLMJudgeScorer"
    assert set(svc._scorers) == set(ScoringMode)


def test_build_scoring_service_llm_missing_fields_raises():
    with pytest.raises(RuntimeError, match="大模型 API 配置不完整"):
        gb.build_scoring_service({"method": "llm", "llm_api_url": "http://x", "llm_api_key": ""})


def test_build_scoring_service_remote_missing_fields_raises():
    with pytest.raises(RuntimeError, match="远程 reranker 配置不完整"):
        gb.build_scoring_service({"method": "remote_reranker", "rerank_api_url": "http://x"})


def test_build_scoring_service_falls_back_to_env(monkeypatch):
    """字段缺省时回退环境变量."""
    _strip_proxy(monkeypatch)
    monkeypatch.setenv("LLM_API_URL", "https://env-llm/v1")
    monkeypatch.setenv("LLM_API_KEY", "env-key")
    monkeypatch.setenv("LLM_MODEL", "env-model")
    import subjective_scoring
    real_init = subjective_scoring.SubjectiveScoringService.__init__
    captured = {}

    def spy_init(self, *a, **kw):
        captured.update(kw)
        return real_init(self, *a, **kw)

    monkeypatch.setattr(subjective_scoring.SubjectiveScoringService, "__init__", spy_init)
    # method=llm 但 DB 缺字段 -> 回退 env LLM_*
    svc = gb.build_scoring_service({"method": "llm", "llm_api_url": ""})
    assert svc is not None
    jc = captured["llm_judge"]
    assert jc.url == "https://env-llm/v1"
    assert jc.model == "env-model"


def test_reset_service_clears_singletons():
    gb._service_singleton = object()
    gb._remote_reranker = object()
    gb.reset_service()
    assert gb._service_singleton is None
    assert gb._remote_reranker is None
