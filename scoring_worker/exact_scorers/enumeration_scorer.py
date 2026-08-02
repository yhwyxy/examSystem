"""列举题精确评分器：评分点=条目集合，命中任一同义词即得该条目满分。"""
from ._base import ExactScoreResult


def score_enumeration(question: dict, student_answer: str) -> ExactScoreResult:
    return ExactScoreResult(0.0, {"reason": "not implemented"}, "manual_required")