"""整卷打分核心.py 复刻 backend/grader.py 的逻辑; 不 import backend (包独立).

调用前 worker 要预先拉起主观题打分 session (一次加载模型):
  - ScoreService 借得 HorseSubjective / similar_row_table
  - populate_semantic_index(student_answer vs similar answers: 逐
    answer build a vector index once; 然后用 subjective_score 对单题 SECTION=(
    semantic_similarity, llm_similarity)
"""
from __future__ import annotations

import json
from typing import Any, List


def _grade_objective(question: dict, student_answer: Any) -> dict:
    """单客观题评分: {single_choice, multiple_choice, true_false}."""
    qt = question.get("type", "")
    if qt not in {"single_choice", "multiple_choice", "true_false"}:
        return None
    score = float(question.get("score", 0))
    correct = question.get("answer", "")
    if qt == "single_choice":
        is_correct = (student_answer == correct)
        fs = score if is_correct else 0.0
    elif qt == "true_false":
        is_correct = (str(student_answer).lower() == str(correct).lower())
        fs = score if is_correct else 0.0
    else:  # multiple_choice
        correct_set = set(correct) if isinstance(correct, (list, str)) else set()
        try:
            student_set = set(student_answer) if student_answer is not None else set()
        except TypeError:
            student_set = {student_answer} if student_answer is not None else set()
        if student_set == correct_set:
            fs = score
        elif student_set and student_set.issubset(correct_set):
            fs = score * 0.5  # 部分 (binary 容许 partial=False 是终)
        else:
            fs = 0.0
        is_correct = (student_set == correct_set)
    return {
        "id": question.get("id"), "type": qt, "student_answer": student_answer,
        "correct_answer": correct, "max_score": score, "score": fs,
        "is_correct": is_correct,
    }


def _grade_subjective(question: dict, student_answer: Any, ssvc: Any) -> dict:
    """单主观题评分: 调 subjective-scoring 0.1.7 通过 grader_bridge 完整打分.

    ssvc 参数保持位置兼容 (grade_submission 调用方仍传), 但实际桥接 +
    lazy load 由 grader_bridge 内部完成; preserve=None (worker 'blue-sky').
    """
    from .grader_bridge import grade_subjective as _grade
    preserve = None  # Worker grading 时 preserve=None; regrade preserve 由调用方传
    final, machine, review_status, entry = _grade(question, student_answer,
                                                  preserve=preserve)
    return {
        "id": question.get("id"), "type": question.get("type"),
        "question": question.get("question", ""),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "max_score": float(question.get("score", 0) or 0),
        "machine_score": machine,
        "final_score": final,
        "confidence": float(entry.get("confidence", 0.0) or 0.0),
        "grading_method": entry.get("grading_method", "subjective"),
        "review_status": review_status,
        "manually_reviewed": bool(entry.get("manually_reviewed", False)),
        "lowest_confidence_flagged": bool(entry.get("low_confidence", False)),
        "detail": entry,
    }


def grade_submission(snapshot_doc: dict, answers_map: dict, ssvc: Any,
                       preserve: dict | None = None) -> dict:
    """整卷打分: 按快照 questions 顺序重建完整数组.

    preserve: qid -> dict 已手工 reviewed 条目 (final_score 设 manual 分;
    regrade 不允许覆盖). Worker 默认 None.

    返回: {
        detail: List[dict],       # 按 snapshot 顺序 (含客观+主观完整 detail)
        objective_score: float,
        subjective_score_machine: float,
        subjective_score_final: float,
        overall_review_status: str,  # 整卷级别 review_status
    }
    """
    from .grader_bridge import aggregate_review_status
    questions = snapshot_doc.get("questions", []) or []
    detail: List[dict] = []
    obj_score = 0.0
    subj_machine = 0.0
    subj_final = 0.0
    subjective_entries: List[dict] = []
    for q in questions:
        qid = q.get("id")
        student_answer = answers_map.get(qid)
        qt = q.get("type", "")
        if qt in {"single_choice", "multiple_choice", "true_false"}:
            r = _grade_objective(q, student_answer)
            if r is not None:
                detail.append(r)
                obj_score += float(r["score"])
        else:
            r = _grade_subjective(q, student_answer, ssvc)
            subj_machine += r["machine_score"]
            subj_final += r["final_score"]
            detail.append(r)
            subjective_entries.append(r)
    return {
        "detail": detail,
        "objective_score": round(obj_score, 6),
        "subjective_score_machine": round(subj_machine, 6),
        "subjective_score_final": round(subj_final, 6),
        "overall_review_status": aggregate_review_status(subjective_entries),
    }
