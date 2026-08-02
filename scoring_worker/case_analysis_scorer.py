"""案例分析题精确评分器：按评分点匹配，需回调 grader_bridge 做结构化输出。"""
from scoring_worker.exact_scorers._base import ExactScoreResult


def score_case_analysis(question: dict, student_answer: str) -> ExactScoreResult:
    return ExactScoreResult(0.0, {"reason": "not implemented"}, "manual_required")