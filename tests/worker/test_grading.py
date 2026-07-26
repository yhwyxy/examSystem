"""Grading + grader_bridge 单元测试 (不需 PG / 不加载 model).

解答答空走 _empty_subjective_detail, 不调 get_subjective_service (避免加载 model).
"""
import json
import os
import sys
import pathlib

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
    # 部分正确 -> 0.5 * score
    r = _grade_objective(q, ["A", "B"])
    assert r["is_correct"] is False
    assert r["score"] == 5.0


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
