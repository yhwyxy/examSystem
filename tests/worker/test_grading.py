"""Grading + grader_bridge 单元测试 (不需 PG / 不加载 model).

解答答空走 _empty_subjective_detail, 不调 get_subjective_service (避免加载 model).
"""
import json
import os
import sys
import pathlib
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))


def test_grade_objective_single_choice_correct():
    from scoring_worker.grading import _grade_objective
    q = {"id": "q1", "type": "single_choice", "score": 10, "answer": "A"}
    r = _grade_objective(q, "A")
    assert r["is_correct"] is True
    assert r["score"] == 10.0


def test_grade_objective_single_choice_wrong():
    from scoring_worker.grading import _grade_objective
    q = {"id": "q1", "type": "single_choice", "score": 10, "answer": "A"}
    r = _grade_objective(q, "B")
    assert r["is_correct"] is False
    assert r["score"] == 0.0


def test_grade_objective_multiple_choice_partial():
    from scoring_worker.grading import _grade_objective
    q = {"id": "q2", "type": "multiple_choice", "score": 10, "answer": ["A", "B", "C"]}
    # 部分正确无错项 -> 按命中比例给分 (与 Go gradeMultipleChoice 一致)
    r = _grade_objective(q, ["A", "B"])
    assert r["is_correct"] is False
    assert r["score"] == round(10 * 2 / 3, 6)
    assert r["question_id"] == "q2"  # 前端/review 契约键
    # 含错项 -> 0 分
    r2 = _grade_objective(q, ["A", "D"])
    assert r2["score"] == 0.0
    # partial=False -> 非全对 0 分
    r3 = _grade_objective(q, ["A", "B"], partial=False)
    assert r3["score"] == 0.0


def test_grade_objective_multiple_choice_full_correct():
    from scoring_worker.grading import _grade_objective
    q = {"id": "q3", "type": "multiple_choice", "score": 10, "answer": ["A", "B"]}
    r = _grade_objective(q, ["A", "B"])
    assert r["is_correct"] is True
    assert r["score"] == 10.0


def test_grade_objective_true_false_case_insensitive():
    from scoring_worker.grading import _grade_objective
    q = {"id": "q4", "type": "true_false", "score": 5, "answer": "True"}
    r1 = _grade_objective(q, "true")
    assert r1["is_correct"] is True and r1["score"] == 5.0
    r2 = _grade_objective(q, "FALSE")
    assert r2["is_correct"] is False and r2["score"] == 0.0


def test_grade_submission_unanswered_subjective_no_model_load(monkeypatch):
    """所有主观题答案为空 -> _grade_subjective 走 _empty_subjective_detail, 不加载 model."""
    import scoring_worker.grader_bridge as gb
    # 标记 get_subjective_service 为 fail-fast: 任何加载都将让测试失败
    def _no_load(*a, **kw):
        raise AssertionError("不应加载 subjective service (答案为空)")
    monkeypatch.setattr(gb, "get_subjective_service", _no_load)
    snapshot = {
        "questions": [
            {"id": "q1", "type": "single_choice", "score": 10, "answer": "A"},
            {"id": "q2", "type": "short_answer", "score": 20, "answer": "sample",
             "question": "what?"},
        ]
    }
    from scoring_worker.grading import grade_submission
    result = grade_submission(snapshot, {"q1": "A", "q2": ""},
                              ssvc=None, preserve=None)
    assert result["objective_score"] == 10.0
    assert result["subjective_score_machine"] == 0.0
    assert result["subjective_score_final"] == 0.0
    # overall_review_status 应是 'need_review' (主观题 unanswered 触发)
    assert result["overall_review_status"] == "need_review"


def test_aggregate_review_status_reads_nested_flags():
    """②回归: 单题旗标在 detail 嵌套层 (worker 旧格式) 也必须触发 need_review.

    曾因 aggregate 只读顶层旗标, 全部漏检 -> 整卷需人工的卷被误标 'reviewed'."""
    from scoring_worker.grader_bridge import aggregate_review_status
    entries = [{
        "machine_score": 0.0, "max_score": 10.0,
        "review_status": "low_confidence",
        "detail": {"low_confidence": True, "need_manual_review": True},
    }]
    assert aggregate_review_status(entries) == "need_review"


def test_aggregate_review_status_clean_paper_high_confidence():
    from scoring_worker.grader_bridge import aggregate_review_status
    assert aggregate_review_status([
        {"machine_score": 9.0, "max_score": 10.0,
         "review_status": "high_confidence",
         "low_confidence": False, "need_manual_review": False},
    ]) == "high_confidence"


def test_grade_composite_extracts_sub_answers(monkeypatch):
    """④回归: composite 答案 {sub_id: {answer: str}} 必须逐子题拆开评分.

    曾被整体 str() 成 repr 串喂给每个子题 -> 评分输入全错."""
    import scoring_worker.grader_bridge as gb
    seen = {}

    def _fake_grade(question, student_answer, *, preserve=None):
        seen[question["id"]] = student_answer
        return (3.0, 3.0, "reviewed",
                {"review_status": "reviewed", "manually_reviewed": False,
                 "low_confidence": False, "need_manual_review": False,
                 "confidence": 0.9, "reason": None})

    monkeypatch.setattr(gb, "grade_subjective", _fake_grade)
    q = {"id": "q9", "type": "composite", "score": 10,
         "subquestions": [
             {"id": "q9-1", "question": "s1", "score": 5, "answer": "a"},
             {"id": "q9-2", "question": "s2", "score": 5, "answer": "b"}]}
    ans = {"q9-1": {"answer": "回答一"}, "q9-2": "回答二"}
    f, m, overall, entry = gb._grade_composite(None, q, ans, None)
    assert seen == {"q9-1": "回答一", "q9-2": "回答二"}
    assert f == 6.0 and m == 6.0
    assert entry["sub_results"][0]["sub_question_id"] == "q9-1"
    assert overall == "reviewed"  # 6/10 < 0.8, 无人工/低置信信号


def test_build_scoring_request_passes_code_language_for_code_mode():
    """回归: scoring_mode=code 的题不论 type (short_answer/essay) 都必须
    透传 code_language 与 code_scoring_profile.

    曾因只认 type in {sql, code, code_completion} 丢语言 -> 引擎默认按
    python 解析 JS 代码 -> 参考/学生 AST 双失败 -> 结构分 0 (q21 得 0 分)."""
    from scoring_worker.grader_bridge import build_scoring_request
    q = {"id": "q21", "type": "essay", "score": 15, "scoring_mode": "code",
         "code_language": "javascript", "question": "找下标",
         "answer": "function findIndex(a, x) { return a.indexOf(x); }",
         "code_scoring_profile": "find_index_static"}
    req = build_scoring_request(q, "function f(a, x) { return a.indexOf(x); }")
    assert req.code_language == "javascript"
    assert req.code_scoring_profile == "find_index_static"
    assert req.scoring_mode.value == "code"


def test_build_scoring_request_empty_scoring_points_without_answer():
    """scoring_points=[] + answer="" 的场景 (q43 开放题) 验证.

    - _structured_scoring_points([]) 应返回 [] (不走 rubric 兜底)
    - build_scoring_request 中 scoring_points 应为空列表
    - reference_answer 应透传空串
    """
    from scoring_worker.grader_bridge import build_scoring_request
    q: dict[str, Any] = {
        "id": "q43",
        "type": "essay",
        "scoring_mode": "text",
        "score": 20.0,
        "scoring_points": [],
        "answer": "",
        "question": "请阐述自己从事电气工程或自动化专业工作后的打算和想法",
    }
    req = build_scoring_request(q, "我想从事电气工程工作")
    assert len(req.scoring_points) == 0, "scoring_points 应为空列表"
    assert req.reference_answer == "", "reference_answer 应透传空串"
    assert req.question_id == "q43"
    assert req.max_score == 20.0


def test_grade_subjective_empty_scoring_points_empty_answer_triggers_manual_review(monkeypatch):
    """scoring_points=[] + 学生答案为空 → 不走 engine, 直接返回 unanswered."""
    import scoring_worker.grader_bridge as gb

    def _no_load(*a, **kw):
        raise AssertionError("不应加载 subjective service (空答案)")
    monkeypatch.setattr(gb, "get_subjective_service", _no_load)

    q: dict[str, Any] = {
        "id": "q43",
        "type": "essay",
        "scoring_mode": "text",
        "score": 20.0,
        "scoring_points": [],
        "answer": "",
        "question": "请阐述自己从事电气工程或自动化专业工作后的打算和想法",
    }
    f, m, rs, entry = gb.grade_subjective(q, "", preserve=None)
    assert f == 0.0
    assert m == 0.0
    assert rs == "open_ended", f"开放题空答案应标记 open_ended, 实际 {rs!r}"
    assert entry["review_status"] == "open_ended"
    assert entry["need_manual_review"] is True  # 开放题需要人工审核
    assert entry["reason"] == "open_ended"
    assert entry["student_answer"] == ""


def test_grade_subjective_empty_scoring_points_with_answer_forces_review(monkeypatch):
    """scoring_points=[] + 学生有答案 + reference_answer="" → engine 触发 force_review.

    绕过 engine 直接 mock service.score() 返回 ScoringResult 来验证
    detail 层对 need_manual_review=True 的正确映射。
    """
    import scoring_worker.grader_bridge as gb
    from subjective_scoring import ReviewLevel, ScoringMode, ScoringResult, ScoringRequest

    class FakeSvc:
        def score(self, request: ScoringRequest) -> ScoringResult:
            return ScoringResult(
                question_id=request.question_id,
                scoring_mode=ScoringMode.TEXT,
                track="TextRerankerScorer",
                score=0.0,
                max_score=request.max_score,
                confidence=0.0,
                need_manual_review=True,
                review_level=ReviewLevel.MANUAL_REQUIRED,
                matched_points=[],
                missed_points=[],
                warnings=["无评分点且无标准答案，文本评分无法进行"],
                decision="manual_review",
                decision_reason="no_scoring_points_no_reference",
            )

    monkeypatch.setattr(gb, "get_subjective_service", lambda: FakeSvc())

    q: dict[str, Any] = {
        "id": "q43",
        "type": "essay",
        "scoring_mode": "text",
        "score": 20.0,
        "scoring_points": [],
        "answer": "",
        "question": "请阐述自己从事电气工程或自动化专业工作后的打算和想法",
    }
    f, m, rs, entry = gb.grade_subjective(q, "我想从事电气工程工作", preserve=None)
    assert entry["need_manual_review"] is True, "开放题 → need_manual_review=True"
    assert rs == "open_ended", f"review_status 应为 open_ended, 实际 {rs!r}"
    assert entry["review_status"] == "open_ended"
    assert m == 0.0
    assert f == 0.0


def test_get_subjective_service_local_mode_missing_model(tmp_path, monkeypatch):
    """本地 mode (RERANK_USE_REMOTE 未设) + RERANKER_MODEL 指向不存在路径 -> raise 而非降级.

    plan Step 8 期望: 缺失 local extra / 模型 的测试证明本地模式会失败而不是降级.
    注: 不调用模型加载 (SubjectiveScoringService 构造本身在 model_path 不存在时
    会 fallback 到 HF cache / 默认 model 名 -> 我们测的部分仅校验路径 resolve 逻辑).
    """
    import scoring_worker.grader_bridge as gb
    monkeypatch.delenv("RERANK_USE_REMOTE", raising=False)
    monkeypatch.setenv("RERANKER_MODEL", "/nonexistent/path/model-x")
    # 重置单例 + 通过 monkeypatch 替换 SubjectiveScoringService 构造让测试可控:
    monkeypatch.setattr(gb, "_service_singleton", None)
    monkeypatch.setattr(gb, "_remote_reranker", None)
    # 模拟 subjective_scoring.SubjectiveScoringService 在 allow_model_load=True 时
    # 对不存在 model 路径应 raise (避免静默降级):
    import subjective_scoring  # type: ignore
    orig_init = subjective_scoring.SubjectiveScoringService.__init__
    def _strict_init(self, *args, **kwargs):
        if kwargs.get("allow_model_load", False):
            tm = kwargs.get("text_model") or ""
            if tm and "/nonexistent/" in str(tm):
                raise RuntimeError(f"model not found: {tm}")
        return orig_init(self, *args, **kwargs)
    monkeypatch.setattr(subjective_scoring.SubjectiveScoringService, "__init__", _strict_init)
    try:
        gb.get_subjective_service()
        pytest.fail("期望 RuntimeError 而非静默成功")
    except RuntimeError as e:
        assert "model not found" in str(e)
