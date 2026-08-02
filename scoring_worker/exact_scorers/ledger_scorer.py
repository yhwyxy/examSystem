"""账目题精确评分器：按借贷分录匹配评分点。"""
from ._base import ExactScoreResult


def score_ledger(question: dict, student_answer: str) -> ExactScoreResult:
    return ExactScoreResult(0.0, {"reason": "not implemented"}, "manual_required")