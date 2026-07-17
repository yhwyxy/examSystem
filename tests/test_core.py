"""Tests for the core grading logic and the full submission lifecycle.

This file tests the integrated behavior of grade_submission() across all
question types and grader modules (objective + subjective via subjective-scoring).
"""
from __future__ import annotations

import asyncio
import json
from types import SimpleNamespace

import pytest

import backend.grader as grader
from backend.grader import grade_question, grade_submission

import backend.grader as grader_mod


def _fake_scoring_result(score: float, confidence: float, review_level: str = "auto_pass"):
    """Build a ScoringResult-like object for grader integration tests."""
    from subjective_scoring import ReviewLevel, ScoringMode, ScoringResult

    level = ReviewLevel(review_level)
    return ScoringResult(
        question_id="q4",
        score=score,
        max_score=10,
        scoring_mode=ScoringMode.TEXT,
        track="TextRerankerScorer",
        confidence=confidence,
        need_manual_review=level is not ReviewLevel.AUTO_PASS,
        review_level=level,
        matched_points=[],
        missed_points=[],
        warnings=[],
    )


class _FakeSubjectiveService:
    def __init__(self, result_factory):
        self._factory = result_factory

    def score(self, request, **kwargs):
        return self._factory(request)



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
        lambda paper_id=None: {"questions": _FAKE_QUESTIONS, "paper_id": paper_id or "default"},
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
class TestSubjectiveScoringIntegration:
    def setup_method(self):
        grader_mod.set_subjective_service(None)

    def teardown_method(self):
        grader_mod.set_subjective_service(None)

    def test_subjective_service_grades_short_answer(self, monkeypatch):
        svc = _FakeSubjectiveService(
            lambda req: _fake_scoring_result(score=8.5, confidence=0.85, review_level="auto_pass")
        )
        grader_mod.set_subjective_service(svc)
        answers = {"q1": "B", "q2": ["A", "C", "D"], "q3": False, "q4": "RESTful APIs use resources and HTTP methods."}
        result = asyncio.get_event_loop().run_until_complete(grade_submission(answers))
        assert result.subjective_score_machine == 8.5
        assert result.subjective_score_final == result.subjective_score_machine
        assert result.total_score == result.objective_score + result.subjective_score_final
        detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail_q4["grading_method"].startswith("subjective_scoring:")
        assert detail_q4["review_status"] == "high_confidence"

    def test_review_status_is_propagated(self, monkeypatch):
        svc = _FakeSubjectiveService(
            lambda req: _fake_scoring_result(score=6.0, confidence=0.72, review_level="suggested_review")
        )
        grader_mod.set_subjective_service(svc)
        result = asyncio.get_event_loop().run_until_complete(grade_submission({"q4": "partial answer"}))
        detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail_q4["review_status"] == "need_review"
        assert result.review_status == "pending"

    def test_service_error_marks_need_review(self, monkeypatch):
        class Boom:
            def score(self, request, **kwargs):
                raise RuntimeError("boom")

        grader_mod.set_subjective_service(Boom())
        result = asyncio.get_event_loop().run_until_complete(grade_submission({"q4": "resources and HTTP methods"}))
        detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail_q4["grading_method"] == "subjective_scoring:error"
        assert detail_q4["machine_score"] == 0
        assert detail_q4["review_status"] == "need_review"


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

    @pytest.mark.parametrize("student_answer", [None, "", "   ", "\n\t"])
    def test_grade_true_false_unanswered_does_not_match_false_reference(self, student_answer):
        from backend.objective_grader import grade_true_false

        result = grade_true_false(student_answer, False, 2.0)

        assert result["score"] == 0
        assert result["is_correct"] is False

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
        papers = tmp_path / "papers"
        papers.mkdir()
        (papers / "t1.json").write_text(json.dumps({**valid, "paper_id": "t1", "name": "T"}, ensure_ascii=False))
        (papers / "index.json").write_text(json.dumps({
            "papers": [{"slug": "t1", "name": "T", "status": "closed", "question_count": 2, "total_score": 10}]
        }), encoding="utf-8")
        monkeypatch.setattr(question_loader, "PAPERS_DIR", papers)
        monkeypatch.setattr(question_loader, "INDEX_PATH", papers / "index.json")
        question_loader.clear_question_cache()
        data = question_loader.load_questions("t1")
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
        from backend.question_loader import public_exam_payload, ensure_papers_layout, list_papers
        ensure_papers_layout()
        papers = list_papers()
        assert papers, "expected migrated default paper"
        payload = public_exam_payload(papers[0]["slug"])
        for q in payload["questions"]:
            assert "answer" not in q
            assert "scoring_rubric" not in q
            assert "scoring_points" not in q


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
    grader_mod.set_subjective_service(
        _FakeSubjectiveService(
            lambda req: _fake_scoring_result(score=5.0, confidence=0.5, review_level="suggested_review")
        )
    )
    try:
        answers = {"q1": "B", "q2": ["A", "C", "D"], "q3": False, "q4": "some answer"}
        result = asyncio.get_event_loop().run_until_complete(grade_submission(answers))
        assert abs(result.total_score - (result.objective_score + result.subjective_score_final)) < 0.01
    finally:
        grader_mod.set_subjective_service(None)



@pytest.mark.usefixtures("_fake_question_loader")
def test_grade_submission_marks_low_confidence_for_review(monkeypatch):
    """Low-confidence subjective detail should propagate to pending review_status."""
    grader_mod.set_subjective_service(
        _FakeSubjectiveService(
            lambda req: _fake_scoring_result(score=3.0, confidence=0.3, review_level="manual_required")
        )
    )
    try:
        result = asyncio.get_event_loop().run_until_complete(grade_submission({"q4": "partial answer"}))
        detail_q4 = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail_q4["review_status"] == "low_confidence"
        assert result.review_status == "pending"
    finally:
        grader_mod.set_subjective_service(None)


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


# ---------------------------------------------------------------------------
# grader ↔ subjective-scoring 映射
# ---------------------------------------------------------------------------

def test_parse_scoring_rubric_splits_points():
    from backend.grader import parse_scoring_rubric
    points = parse_scoring_rubric(
        "资源导向 3 分；HTTP 方法 2 分；无状态 2 分；统一接口和状态码 2 分；缓存 1 分。",
        max_score=10,
    )
    assert len(points) == 5
    assert points[0]["text"] == "资源导向"
    assert points[0]["score"] == 3
    assert abs(sum(p["score"] for p in points) - 10) < 1e-6


def test_build_scoring_request_from_question():
    from backend.grader import build_scoring_request
    q = {
        "id": "q4",
        "type": "short_answer",
        "question": "Explain REST",
        "answer": "resources and HTTP methods",
        "score": 10,
        "scoring_rubric": "资源导向 5 分；HTTP 方法 5 分。",
    }
    req = build_scoring_request(q, "student text")
    assert req.question_id == "q4"
    assert req.scoring_mode.value == "text"
    assert len(req.scoring_points) == 2
    assert req.student_answer == "student text"



@pytest.mark.usefixtures("_fake_question_loader")
def test_real_subjective_service_smoke():
    """真实 SubjectiveScoringService（无模型）可跑通 short_answer。"""
    from subjective_scoring import SubjectiveScoringService
    grader_mod.set_subjective_service(SubjectiveScoringService(allow_model_load=False))
    try:
        result = asyncio.get_event_loop().run_until_complete(
            grade_submission({"q4": "资源导向，使用 HTTP 方法，无状态，统一接口，支持缓存"})
        )
        detail = next(d for d in result.grading_detail if d["question_id"] == "q4")
        assert detail["grading_method"].startswith("subjective_scoring:")
        assert detail["max_score"] == 10
        assert "matched_points" in detail
        assert detail["machine_score"] >= 0
    finally:
        grader_mod.set_subjective_service(None)
