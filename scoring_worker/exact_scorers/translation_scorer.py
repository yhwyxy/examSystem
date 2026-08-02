"""翻译题精确评分器：译文段落级按评分点评分。"""
from ._base import ExactScoreResult


def score_translation(question: dict, student_answer: str) -> ExactScoreResult:
    return ExactScoreResult(0.0, {"reason": "not implemented"}, "manual_required")