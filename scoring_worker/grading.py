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
    """单主观题评分: 调 subjective-scoring 包打分; max_score 取 question[score].

    返回结构对齐 Python 旧版 grader 与 SQL submissions.grading_detail_json 列.
    ssvc: subjective_scoring.ScoreService 实例 (注入)
    """
    score = float(question.get("score", 0) or 0)
    if student_answer is None or student_answer == "":
        return {
            "id": question.get("id"), "type": question.get("type"),
            "prompt": question.get("prompt", ""),
            "reference_answer": question.get("answer", ""),
            "student_answer": "", "max_score": score,
            "machine_score": 0.0, "final_score": 0.0,
            "is_correct": False, "confidence": 0.0,
            "grading_method": "subjective",
            "review_status": "unanswered",
            "manually_reviewed": False,
            "detail": {"reason": "unanswered"},
        }
    ref = question.get("answer", "")
    result = ssvc.score(student_answer, reference=ref_or_list(ref), prompt=question.get("prompt", ""))
    m = float(result.get("score", 0) or 0)  # 0..1 normalized
    machine = round(m * score, 6)
    return {
        "id": question.get("id"), "type": question.get("type"),
        "prompt": question.get("prompt", ""),
        "reference_answer": ref,
        "student_answer": student_answer,
        "max_score": score,
        "machine_score": machine,
        "final_score": machine,
        "is_correct": machine >= 0.6 * score,
        "confidence": float(result.get("confidence", 0) or 0),
        "grading_method": "subjective",
        "review_status": "graded",
        "manually_reviewed": False,
        "detail": result,
    }


def ref_or_list(ref: Any) -> Any:
    """histol参考答案 unwrap: 单字符串保持; list 取首."""
    if isinstance(ref, list):
        return ref[0] if ref else ""
    return ref


def grade_submission(snapshot_doc: dict, answers_map: dict, ssvc: Any) -> dict:
    """整卷打分: 按快照 questions 顺序重建完整数组.

    返回: {
        detail: List[dict],       # 按 snapshot 顺序 (含客观+主观完整 detail)
        objective_score: float,
        subjective_score_machine: float,
        subjective_score_final: float,
    }
    """
    questions = snapshot_doc.get("questions", []) or []
    detail: List[dict] = []
    obj_score = 0.0
    subj_machine = 0.0
    subj_final = 0.0
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
    return {
        "detail": detail,
        "objective_score": round(obj_score, 6),
        "subjective_score_machine": round(subj_machine, 6),
        "subjective_score_final": round(subj_final, 6),
    }
