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


def test_build_scoring_request_passes_calculation_config():
    from backend.grader import build_scoring_request

    req = build_scoring_request(
        {
            "id": "calc-1",
            "type": "short_answer",
            "question": "计算 1+1",
            "answer": "2",
            "score": 10,
            "scoring_mode": "calculation",
            "calculation": {
                "steps": [],
                "final_answers": [
                    {
                        "id": "result",
                        "description": "最终结果",
                        "expected": 2,
                        "score": 10,
                    }
                ],
            },
        },
        "2",
    )

    assert req.scoring_mode.value == "calculation"
    assert len(req.scoring_config.calculation.final_answers) == 1
    assert req.scoring_config.calculation.final_answers[0].expected == 2


def test_calculation_mode_without_config_falls_back_to_text():
    from backend.grader import build_scoring_request

    req = build_scoring_request(
        {
            "id": "calc-unconfigured",
            "type": "essay",
            "question": "待补充计算评分项",
            "answer": "参考答案",
            "score": 10,
            "scoring_mode": "calculation",
        },
        "学生答案",
    )

    assert req.scoring_mode.value == "text"



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


def test_build_scoring_request_passes_code_language():
    from backend.grader import build_scoring_request

    q = {
        "id": "q1",
        "type": "short_answer",
        "question": "写 python",
        "answer": "print(1)",
        "score": 5,
        "scoring_mode": "code",
        "code_language": "Python",
    }
    req = build_scoring_request(q, "print(1)")
    assert req.code_language == "python"
    mode = req.scoring_mode
    assert getattr(mode, "value", mode) == "code" or str(mode).endswith("code") or str(mode) == "code"


def test_grade_composite_sums_sub_scores(monkeypatch):
    import asyncio
    from backend import grader

    parent = {
        "id": "c1",
        "type": "short_answer",
        "question": "父",
        "score": 10,
        "sub_questions": [
            {"id": "s1", "question": "子1", "answer": "A", "score": 4, "scoring_mode": "text"},
            {"id": "s2", "question": "子2", "answer": "B", "score": 6, "scoring_mode": "text"},
        ],
    }

    async def fake_run(question, student_answer: str):
        sid = str(question.get("id") or "")
        score = 2.0 if sid.endswith("s1") else 5.0
        return {
            "question_id": question.get("id"),
            "type": question.get("type"),
            "question": question.get("question"),
            "student_answer": student_answer,
            "reference_answer": question.get("answer"),
            "max_score": float(question.get("score") or 0),
            "machine_score": score,
            "score": score,
            "final_score": score,
            "grading_method": "subjective_scoring:text",
            "review_status": "high_confidence",
            "reason": "ok",
            "confidence": 0.95,
            "low_confidence": False,
        }

    monkeypatch.setattr(grader, "_run_subjective_grading", fake_run)
    detail = asyncio.run(grader.grade_composite_question(parent, {"s1": "a", "s2": "b"}))
    assert detail["is_composite"] is True
    assert detail["score"] == 7.0
    assert detail["final_score"] == 7.0
    assert len(detail["sub_results"]) == 2
    assert detail["sub_results"][0]["sub_question_id"] == "s1"
    assert detail["student_answer"] == {"s1": "a", "s2": "b"}


def test_grade_composite_passes_selected_language_sequentially(monkeypatch):
    import asyncio
    from backend import grader

    parent = {
        "id": "c1",
        "type": "composite",
        "question": "父",
        "score": 7,
        "subquestions": [
            {
                "id": "s1",
                "question": "代码",
                "answer": "print(1)",
                "score": 5,
                "scoring_mode": "code",
                "allowed_languages": ["python", "javascript"],
            },
            {
                "id": "s2",
                "question": "解释",
                "answer": "A",
                "score": 2,
                "scoring_mode": "text",
            },
        ],
    }
    seen = []

    async def fake_run(question, student_answer):
        seen.append((question["id"], question.get("code_language"), student_answer))
        return {
            "machine_score": question["score"],
            "final_score": question["score"],
            "score": question["score"],
            "max_score": question["score"],
            "review_status": "high_confidence",
            "low_confidence": False,
        }

    monkeypatch.setattr(grader, "_run_subjective_grading", fake_run)
    detail = asyncio.run(
        grader.grade_composite_question(
            parent,
            {
                "s1": {"answer": "console.log(1)", "language": "javascript"},
                "s2": {"answer": "说明"},
            },
        )
    )

    assert seen == [
        ("c1:s1", "javascript", "console.log(1)"),
        ("c1:s2", None, "说明"),
    ]
    assert detail["sub_results"][0]["selected_language"] == "javascript"
    assert detail["sub_results"][1]["selected_language"] is None


def test_grade_composite_legacy_code_answer_uses_first_allowed_language(monkeypatch):
    import asyncio
    from backend import grader

    parent = {
        "id": "c1",
        "type": "composite",
        "question": "父",
        "score": 5,
        "subquestions": [
            {
                "id": "s1",
                "question": "代码",
                "answer": "print(1)",
                "score": 5,
                "scoring_mode": "code",
                "allowed_languages": ["python", "javascript"],
            }
        ],
    }
    seen = {}

    async def fake_run(question, student_answer):
        seen.update(question)
        return {
            "machine_score": 5,
            "final_score": 5,
            "score": 5,
            "max_score": 5,
            "review_status": "high_confidence",
            "low_confidence": False,
        }

    monkeypatch.setattr(grader, "_run_subjective_grading", fake_run)
    detail = asyncio.run(grader.grade_composite_question(parent, {"s1": "print(1)"}))

    assert seen["code_language"] == "python"
    assert detail["sub_results"][0]["selected_language"] == "python"


def test_format_answer_for_export_composite():
    from backend.exporter import format_student_answer_for_export, format_score_note_for_export

    d = {
        "is_composite": True,
        "sub_results": [
            {"sub_question_id": "s1", "student_answer": "答1", "final_score": 2, "max_score": 4},
            {"sub_question_id": "s2", "student_answer": "答2", "final_score": 6, "max_score": 6},
        ],
        "student_answer": {"s1": "答1", "s2": "答2"},
    }
    text = format_student_answer_for_export(d)
    assert "s1" in text and "答1" in text
    assert "s2" in text
    note = format_score_note_for_export(d)
    assert "复合题" in note
    assert "s1=2/4" in note


def test_apply_review_sub_question(tmp_path, monkeypatch):
    from backend import database
    import json

    db_path = tmp_path / "t.db"
    monkeypatch.setattr(database, "DB_PATH", db_path)
    monkeypatch.setattr(database, "_initialized", False)
    database.init_db()

    detail = [{
        "question_id": "c1",
        "type": "short_answer",
        "is_composite": True,
        "max_score": 10,
        "score": 5,
        "final_score": 5,
        "review_status": "need_review",
        "sub_results": [
            {"sub_question_id": "s1", "max_score": 4, "score": 2, "final_score": 2, "review_status": "need_review"},
            {"sub_question_id": "s2", "max_score": 6, "score": 3, "final_score": 3, "review_status": "high_confidence"},
        ],
    }]
    sid = database.insert_submission(
        name="张三",
        employee_id="E1",
        paper_id="p1",
        paper_name="P1",
        department="D",
        answers={"c1": {"s1": "a", "s2": "b"}},
        grading_detail=detail,
        scores={
            "objective_score": 0,
            "subjective_score_machine": 5,
            "subjective_score_final": 5,
            "total_score": 5,
        },
        review_status="need_review",
        started_at=None,
        client_ip=None,
        user_agent=None,
    )

    r = database.apply_review(
        submission_id=sid,
        question_id="c1",
        new_score=4,
        note="满分口径",
        sub_question_id="s1",
    )
    assert r["success"] is True, r
    with database.db_cursor() as conn:
        row = conn.execute(
            "SELECT grading_detail_json, subjective_score_final FROM submissions WHERE id=?",
            (sid,),
        ).fetchone()
        details = json.loads(row["grading_detail_json"])
        target = details[0]
        s1 = next(s for s in target["sub_results"] if s["sub_question_id"] == "s1")
        assert s1["final_score"] == 4
        assert target["final_score"] == 7
        assert float(row["subjective_score_final"]) == 7
