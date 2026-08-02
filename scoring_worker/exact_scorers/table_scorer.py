"""表格题精确评分器：按单元格/行列结构匹配评分点。"""
from ._base import ExactScoreResult


def score_table(question: dict, student_answer: str) -> ExactScoreResult:
    return ExactScoreResult(0.0, {"reason": "not implemented"}, "manual_required")