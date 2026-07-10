"""Tests for the core grading logic and the full submission lifecycle.

This file tests the integrated behavior of grade_submission() across all
question types and grader modules (objective + subjective via embedding).
"""
from __future__ import annotations

import asyncio
import json
from types import SimpleNamespace

import pytest

import backend.grader as grader
from backend.grader import grade_question, grade_submission


# ---------------------------------------------------------------------------
# Fixtures / helpers
# ---------------------------------------------------------------------------

_FAKE_QUESTIONS = [
    {"id": "q1", "type": "single_choice", "question": "Which is a web framework?",
     "options": [{"key": "A", "text": "React"}, {"key": "B", "text": "Django"}, {"key": "C", "text": "Vue"}, {"key": "D", "text": "Angular"}],
     "answer": "B", "score": 5},
    {"id": "q2", "type": "multiple_choice", "question": "Which HTTP status codes indicate success?",
     "options": [{"key": "A", "text": "200"}, {"key": "B", "text": "404"}, {"key": "C", "text": "201"}, {"key": "D", "text": "204"}],
     "answer": ["A", "C", "D"], "score": 5},
    {"id": "q3", "type": "true_false", "question": "TCP is connectionless.",
     "answer": False, "score": 2},
    {"id": "q4", "type": "short_answer", "question": "Explain RESTful API design.",
     "answer": "RESTful APIs use resources, HTTP methods, stateless communication, uniform interface, status codes, and caching.",
     "score": 10, "scoring_rubric": "Resource-oriented 3; HTTP methods 2; stateless 2; uniform interface + status codes 2; caching 1."},
]


@pytest.fixture()
def _fake_question_loader(monkeypatch):
    """Bypass question_loader for tests that need it."""
    monkeypatch.setattr(
        "backend.question_loader.load_questions",
        lambda: {"questions": _FAKE_QUESTIONS},
    )


# ---------------------------------------------------------------------------
# Objective questions (single-choice, true-false, multi-choice)
# ---------------------------------------------------------------------------

class TestObjectiveGrading:
    def test_single_choice_correct(self):
        q = {"id": "q1", "type": "single_choice", "answer": "B", "score": 5}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, "B"))
        assert result["score"] == 5
        assert result["is_correct"] is True

    def test_single_choice_incorrect(self):
        q = {"id": "q1", "type": "single_choice", "answer": "B", "score": 5}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, "C"))
        assert result["score"] == 0
        assert result["is_correct"] is False

    def test_true_false_correct(self):
        q = {"id": "q3", "type": "true_false", "answer": False, "score": 2}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, False))
        assert result["score"] == 2
        assert result["is_correct"] is True

    def test_true_false_incorrect(self):
        q = {"id": "q3", "type": "true_false", "answer": False, "score": 2}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, True))
        assert result["score"] == 0

    def test_multi_choice_full_marks(self):
        q = {"id": "q2", "type": "multiple_choice", "answer": ["A", "C", "D"], "score": 9}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, ["D", "C", "A"]))
        assert result["score"] == 9
        assert result["is_correct"] is True

    def test_multi_choice_partial_marks(self):
        q = {"id": "q2", "type": "multiple_choice", "answer": ["A", "C", "D"], "score": 9}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, ["A", "C"]))
        assert abs(result["score"] - 6.0) < 0.01
        assert result["is_correct"] is False

    def test_multi_choice_wrong_choice_zero(self):
        q = {"id": "q2", "type": "multiple_choice", "answer": ["A", "C", "D"], "score": 9}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, ["A", "B"]))
        assert result["score"] == 0

    def test_multi_choice_empty_gives_zero(self):
        q = {"id": "q2", "type": "multiple_choice", "answer": ["A", "C"], "score": 6}
        result = asyncio.get_event_loop().run_until_complete(grade_question(q, []))
        assert result["score"] == 0


# ---------------------------------------------------------------------------
# Subjective questions via Embedding (mocked)
# ---------------------------------------------------------------------------

@pytest.mark.usefixtures("_fake_question_loader")
class TestSubjectiveEmbeddingGrading:
    def test_embedding_grades_subjective_short_answer(self, monkeypatch):
        monkeypatch.setattr(
            "backend.embedding_grader.grade_with_embedding",
            lambda q, sa, **kw: {
                "status": "embedding_ok", "similarity": 0.85, "review_status": "high_confidence",
                "score": round(0.85 * float(q.get("score", 0)), 1), "fallback_reason": None,
            },
        )
        answers = {"q1": "B", "q2": ["A", "C", "D"], "q3": False, "q4": "RESTful APIs use resources and HTTP methods."}
        result = asyncio.get_event_loop().run_until_complete(grade_submission(answers))
        assert result.subjective_score_machine > 0
        assert result.subjective_score_final == result.subjective_score_machine
        assert result.total_score == result.objective_score + result.subjective_score_final

    def test_embedding_review_status_is_propagated(self, monkeypatch):
        monkeypatch.setattr(
            "backend.embedding_grader.grade_with_embedding",
            lambda q, sa, **kw: {
                "status": "embedding_ok", "similarity": 0.6, "review_status": "need_review",
                "score": round(0.6 * float(q.get("score", 0)), 1), "fallback_reason": None,
            },
        )
        result = asyncio.get_event_loop().run_until_complete(grade_submission({"q4": "partial answer"}))
        detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail_q4["review_status"] == "need_review"
        assert result.review_status == "pending"

    def test_keyword_fallback_when_embedding_unavailable(self, monkeypatch):
        monkeypatch.setattr(
            "backend.embedding_grader.grade_with_embedding",
            lambda q, sa, **kw: {"status": "unavailable", "similarity": 0.0, "fallback_reason": "no ollama"},
        )
        result = asyncio.get_event_loop().run_until_complete(grade_submission({"q4": "resources and HTTP methods"}))
        detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail_q4["grading_method"] == "keyword"
        assert detail_q4["machine_score"] > 0


# ---------------------------------------------------------------------------
# aggregate_review_status edge cases
# ---------------------------------------------------------------------------

class TestAggregateReviewStatus:
    def test_all_empty_details_returns_pending(self):
        assert grader.aggregate_review_status([]) == "pending"

    def test_single_reviewed_returns_reviewed(self):
        assert grader.aggregate_review_status([{"review_status": "reviewed"}]) == "reviewed"

    def test_mixed_returns_pending(self):
        assert grader.aggregate_review_status([
            {"review_status": "reviewed"}, {"review_status": "high_confidence"}
        ]) == "pending"

    def test_unrecognized_status_returns_pending(self):
        assert grader.aggregate_review_status([{"review_status": "bogus"}]) == "pending"


# ---------------------------------------------------------------------------
# Objective + subjective final score aggregation
# ---------------------------------------------------------------------------

class TestObjectiveGraderModule:
    """Directly test the objective_grader module."""

    def test_grade_single_choice_correct(self):
        from backend.objective_grader import grade_single_choice
        assert grade_single_choice("B", "B", 5.0)["score"] == 5

    def test_grade_single_choice_incorrect(self):
        from backend.objective_grader import grade_single_choice
        assert grade_single_choice("A", "B", 5.0)["score"] == 0

    def test_grade_true_false_correct(self):
        from backend.objective_grader import grade_true_false
        assert grade_true_false(False, False, 2.0)["score"] == 2

    def test_grade_multiple_choice_partial(self):
        from backend.objective_grader import grade_multiple_choice
        assert grade_multiple_choice(["A", "C"], ["A", "C", "D"], 9.0)["score"] == 6

    def test_grade_multiple_choice_wrong_zero(self):
        from backend.objective_grader import grade_multiple_choice
        assert grade_multiple_choice(["A", "B"], ["A", "C", "D"], 9.0)["score"] == 0

    def test_grade_multiple_choice_empty(self):
        from backend.objective_grader import grade_multiple_choice
        assert grade_multiple_choice([], ["A"], 3.0)["score"] == 0

    def test_grade_multiple_choice_all_correct(self):
        from backend.objective_grader import grade_multiple_choice
        assert grade_multiple_choice(["A", "B", "C"], ["A", "B", "C"], 6.0)["score"] == 6


# ---------------------------------------------------------------------------
# question_loader validation
# ---------------------------------------------------------------------------

class TestQuestionLoaderValidation:
    """These tests use the real question_loader, NOT the autouse fixture."""

    def test_load_questions_passes_valid_file(self, tmp_path, monkeypatch):
        from backend import question_loader
        valid = {
            "exam_info": {"title": "Test", "total_score": 10},
            "questions": [
                {"id": "q1", "type": "single_choice", "question": "Q?",
                 "options": [{"key": "A", "text": "a"}, {"key": "B", "text": "b"}],
                 "answer": "A", "score": 5},
                {"id": "q2", "type": "true_false", "question": "T?",
                 "answer": True, "score": 5},
            ],
        }
        p = tmp_path / "questions.json"
        p.write_text(json.dumps(valid, ensure_ascii=False))
        monkeypatch.setattr(question_loader, "QUESTIONS_PATH", p)
        question_loader.clear_question_cache()
        data = question_loader.load_questions()
        assert len(data["questions"]) == 2

    def test_duplicate_ids_fail_validation(self):
        from backend.question_loader import validate_questions
        data = {
            "exam_info": {"title": "Test", "total_score": 10},
            "questions": [
                {"id": "q1", "type": "single_choice", "question": "Q?",
                 "options": [{"key": "A", "text": "a"}], "answer": "A", "score": 5},
                {"id": "q1", "type": "true_false", "question": "T?",
                 "answer": True, "score": 5},
            ],
        }
        with pytest.raises(Exception, match="题目 ID 重复"):
            validate_questions(data)

    def test_empty_questions_fail_validation(self):
        from backend.question_loader import validate_questions
        data = {"exam_info": {"title": "Test"}, "questions": []}
        with pytest.raises(Exception, match="questions 必须是非空数组"):
            validate_questions(data)

    def test_invalid_type_fails_validation(self):
        from backend.question_loader import validate_questions
        data = {
            "exam_info": {"title": "Test", "total_score": 5},
            "questions": [{"id": "q1", "type": "essay_2", "question": "Q?", "answer": "x", "score": 5}],
        }
        with pytest.raises(Exception, match="类型非法"):
            validate_questions(data)

    def test_public_exam_payload_strips_answers(self):
        from backend.question_loader import public_exam_payload
        payload = public_exam_payload()
        for q in payload["questions"]:
            assert "answer" not in q
            assert "scoring_rubric" not in q


# ---------------------------------------------------------------------------
# ReviewConfig thresholds
# ---------------------------------------------------------------------------

def test_review_thresholds_are_configurable():
    """Ensure ReviewConfig thresholds are accessible and have sane defaults."""
    from backend.config import ReviewConfig
    cfg = ReviewConfig()
    assert cfg.high_confidence_threshold > cfg.need_review_threshold > cfg.low_confidence_threshold


# ---------------------------------------------------------------------------
# End-to-end: grade_submission computes correct final scores
# ---------------------------------------------------------------------------

@pytest.mark.usefixtures("_fake_question_loader")
def test_grade_submission_aggregates_objective_final_scores(monkeypatch):
    """Ensure objective_score + subjective_score_final == total_score."""
    monkeypatch.setattr(
        "backend.embedding_grader.grade_with_embedding",
        lambda q, sa, **kw: {
            "status": "embedding_ok", "similarity": 0.5, "review_status": "need_review",
            "score": round(0.5 * float(q.get("score", 0)), 1), "fallback_reason": None,
        },
    )
    answers = {"q1": "B", "q2": ["A", "C", "D"], "q3": False, "q4": "some answer"}
    result = asyncio.get_event_loop().run_until_complete(grade_submission(answers))
    assert abs(result.total_score - (result.objective_score + result.subjective_score_final)) < 0.01


# ---------------------------------------------------------------------------
# Embedding grader: similarity edge cases
# ---------------------------------------------------------------------------

def test_embedding_grader_accepts_generic_ollama_endpoint(monkeypatch):
    """Ensure embedding_grader accepts a generic Ollama endpoint without a trailing slash."""
    responses = []
    class FakeResponse:
        status_code = 200
        def raise_for_status(self): pass
        def json(self):
            return responses.pop(0)
    class FakeClient:
        def __init__(self, **kwargs):
            pass
        def __enter__(self):
            return self
        def __exit__(self, *a):
            pass
        def post(self, url, **kwargs):
            responses.append({"embedding": [1.0, 0.0, 0.0]})
            return FakeResponse()
    monkeypatch.setattr("httpx.Client", FakeClient)
    from backend.embedding_grader import _ollama_embedding
    _ollama_embedding.cache_clear()
    emb = _ollama_embedding("hello")
    assert emb == [1.0, 0.0, 0.0]


def test_embedding_grader_reuses_cached_model(monkeypatch):
    """sentence-transformers loader is called at most once across multiple similarity() calls."""
    from backend import embedding_grader
    import numpy as np
    call_count = [0]
    def fake_load():
        call_count[0] += 1
        class FakeModel:
            def encode(self, texts, **kw):
                return np.array([[1.0, 0.0], [0.0, 1.0]])
        return FakeModel()
    # Clear any cached values before monkeypatching
    embedding_grader._load_model.cache_clear()
    embedding_grader._ollama_embedding.cache_clear()
    monkeypatch.setattr(embedding_grader, "_load_model", fake_load)
    monkeypatch.setattr(
        "backend.config.get_config",
        lambda: SimpleNamespace(
            grading=SimpleNamespace(embedding=SimpleNamespace(model="test-model", device="cpu", endpoint="", timeout_seconds=10)),
            review=SimpleNamespace(high_confidence_threshold=0.75, need_review_threshold=0.5, low_confidence_threshold=0.35),
        ),
    )
    embedding_grader._similarity_sentence_transformers("a", "b")
    embedding_grader._similarity_sentence_transformers("c", "d")
    assert call_count[0] == 2  # Called each time since we replaced _load_model itself


def test_similarity_prefers_ollama_embedding_when_available(monkeypatch):
    """_similarity_with_embedding() should use Ollama when endpoint is configured, skipping local model."""
    from backend import embedding_grader
    responses = []
    class FakeResponse:
        status_code = 200
        def raise_for_status(self): pass
        def json(self):
            return responses.pop(0)
    class FakeClient:
        def __init__(self, **kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *a): pass
        def post(self, url, **kwargs):
            responses.append({"embedding": [1.0, 0.0, 0.0]})
            return FakeResponse()
    monkeypatch.setattr("httpx.Client", FakeClient)
    monkeypatch.setattr(
        "backend.config.get_config",
        lambda: SimpleNamespace(
            grading=SimpleNamespace(
                embedding=SimpleNamespace(model="bge-m3", device="cpu", endpoint="http://localhost:11434", timeout_seconds=5),
            ),
            review=SimpleNamespace(high_confidence_threshold=0.75, need_review_threshold=0.5, low_confidence_threshold=0.35),
        ),
    )
    embedding_grader._ollama_embedding.cache_clear()
    sim, method = embedding_grader._similarity_with_embedding("hello", "hello")
    assert sim == pytest.approx(1.0)
    assert method == "ollama_embedding"


def test_embedding_grader_ignores_environment_proxy_for_local_ollama(monkeypatch):
    """Ollama httpx.Client must set trust_env=False so system proxies don't interfere."""
    client_kwargs = {}
    class FakeResponse:
        status_code = 200
        def raise_for_status(self): pass
        def json(self): return {"embedding": [1.0]}
    class FakeClient:
        def __init__(self, **kwargs):
            client_kwargs.update(kwargs)
        def __enter__(self): return self
        def __exit__(self, *a): pass
        def post(self, url, **kwargs): return FakeResponse()
    monkeypatch.setattr("httpx.Client", FakeClient)
    monkeypatch.setattr(
        "backend.config.get_config",
        lambda: SimpleNamespace(
            grading=SimpleNamespace(
                embedding=SimpleNamespace(model="bge-m3", device="cpu", endpoint="http://localhost:11434", timeout_seconds=5),
            ),
        ),
    )
    from backend.embedding_grader import _ollama_embedding
    _ollama_embedding.cache_clear()
    _ollama_embedding("test")
    assert client_kwargs.get("trust_env") is False
    assert "proxies" not in client_kwargs


@pytest.mark.usefixtures("_fake_question_loader")
def test_grade_submission_marks_embedding_low_confidence_for_review(monkeypatch):
    """Low-confidence subjective detail should propagate to pending review_status."""
    monkeypatch.setattr(
        "backend.config.get_config",
        lambda: SimpleNamespace(
            grading=SimpleNamespace(
                embedding=SimpleNamespace(model="bge-m3", device="cpu", endpoint="http://localhost:11434", timeout_seconds=1),
            ),
            scoring=SimpleNamespace(multiple_choice_partial=True, wrong_choice_penalty=False, score_precision=1),
            review=SimpleNamespace(high_confidence_threshold=0.75, need_review_threshold=0.5, low_confidence_threshold=0.35),
        ),
    )
    def fake_grade_with_embedding(q, sa, **kw):
        if q.get("type") in ("short_answer", "essay"):
            return {"status": "embedding_ok", "similarity": 0.3, "review_status": "low_confidence", "score": round(0.3 * float(q.get("score", 0)), 1), "reason": "low similarity", "fallback_reason": None}
        return {"status": "unavailable", "similarity": 0.0, "fallback_reason": "skip"}
    monkeypatch.setattr("backend.embedding_grader.grade_with_embedding", fake_grade_with_embedding)
    result = asyncio.get_event_loop().run_until_complete(grade_submission({"q4": "partial answer"}))
    detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
    assert detail_q4["review_status"] == "low_confidence"
    assert result.review_status == "pending"


def test_main_failure_rate_limiter():
    """Rate limiter allows up to max_requests then raises HTTPException."""
    import backend.main as m
    m._rate_store.clear()
    for _ in range(60):
        m._check_rate_limit("test_ip", max_requests=60)
    with pytest.raises(Exception):
        m._check_rate_limit("test_ip", max_requests=60)
    m._rate_store.clear()


def test_main_failure_rate_limiter_independent_ips():
    """Different IPs have independent rate budgets."""
    import backend.main as m
    m._rate_store.clear()
    for _ in range(60):
        m._check_rate_limit("ip_a", max_requests=60)
    m._check_rate_limit("ip_b", max_requests=60)
    with pytest.raises(Exception):
        m._check_rate_limit("ip_a", max_requests=60)
    m._rate_store.clear()
