"""客观题判分。"""
from __future__ import annotations

from typing import Any

OBJECTIVE_TYPES = {"single_choice", "multiple_choice", "true_false"}


def _norm_choice(value: Any) -> str:
    return str(value).strip()


def _norm_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return bool(value)
    if isinstance(value, str):
        return value.strip().lower() in {"true", "1", "t", "yes", "y", "对", "正确", "是"}
    return bool(value)


def grade_single_choice(student: Any, reference: Any, max_score: float) -> dict[str, Any]:
    correct = _norm_choice(student) == _norm_choice(reference)
    return {"is_correct": correct, "score": max_score if correct else 0.0}


def grade_true_false(student: Any, reference: Any, max_score: float) -> dict[str, Any]:
    correct = _norm_bool(student) == _norm_bool(reference)
    return {"is_correct": correct, "score": max_score if correct else 0.0}


def grade_multiple_choice(student: Any, reference: list[Any], max_score: float, partial: bool = True) -> dict[str, Any]:
    if student is None:
        student_list: list[Any] = []
    elif isinstance(student, list):
        student_list = student
    else:
        student_list = [student]

    selected = {_norm_choice(x) for x in student_list if _norm_choice(x)}
    correct_set = {_norm_choice(x) for x in reference}
    wrong = selected - correct_set

    if wrong:
        return {
            "is_correct": False,
            "score": 0.0,
            "wrong_choices": sorted(wrong),
            "correct_count": len(selected & correct_set),
            "total_count": len(correct_set),
        }

    if not partial:
        ok = selected == correct_set
        return {"is_correct": ok, "score": max_score if ok else 0.0}

    hit = len(selected & correct_set)
    score = max_score * hit / len(correct_set) if correct_set else 0.0
    return {
        "is_correct": selected == correct_set,
        "score": float(score),
        "correct_count": hit,
        "total_count": len(correct_set),
    }


def grade_objective(question: dict[str, Any], student_answer: Any, partial: bool = True) -> dict[str, Any]:
    qtype = question["type"]
    max_score = float(question["score"])
    reference = question.get("answer")

    if qtype == "single_choice":
        raw = grade_single_choice(student_answer, reference, max_score)
    elif qtype == "multiple_choice":
        raw = grade_multiple_choice(student_answer, reference or [], max_score, partial=partial)
    elif qtype == "true_false":
        raw = grade_true_false(student_answer, reference, max_score)
    else:
        raise ValueError(f"非客观题类型: {qtype}")

    return {
        "question_id": question["id"],
        "type": qtype,
        "question": question.get("question"),
        "student_answer": student_answer,
        "reference_answer": reference,
        "score": round(float(raw["score"]), 6),
        "machine_score": round(float(raw["score"]), 6),
        "final_score": round(float(raw["score"]), 6),
        "max_score": max_score,
        "is_correct": bool(raw["is_correct"]),
        "grading_method": "objective_rule",
        "confidence": 1.0,
        "reason": "客观题规则判分",
        "review_status": "auto_scored",
        "manually_reviewed": False,
        "detail": {k: v for k, v in raw.items() if k not in {"score", "is_correct"}},
    }
