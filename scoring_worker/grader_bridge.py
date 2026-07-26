"""Grader bridge: 主观题打分适配 (Task 7 Step 4).

TODO (Task 7 partial, 留给下轮 session):
- 实现 ScoringRequest / ScoringResult / CohereReranker / ScoringMode / ReviewLevel
  对齐 subjective_scoring 0.1.7 git tag v0.1.7 实际 API.
- 实现 parse_scoring_rubric: 从 scoring_rubric 文本 + max_score 自动生成 scoring_points.
- 实现 code language 白名单 (sql->sql, 其余->code) 与 answers_by_language 复用.
- 实现 composite 逐 subquestions 打分 + 父题汇总.
- 实现 preserve_manual_reviews: machine_score 用新分但已 reviewed 主观题 final_score
  + review_*  + manually_reviewed 保留 (不 import backend.review_service).
- 实现 review_status 汇总: low_confidence 优先 -> need_review -> reviewed -> high_confidence.
- Worker 启动时构造 RERANK_USE_REMOTE 真实模式 (use CohereRerankerPairScorer 或 local model).

grader_bridge 单测: 对 Task 0 fixture paper.json +主观题 parity 与旧 Python 输出对齐.
"""
from __future__ import annotations

# placeholder; 详见 module docstring TODO


class StubSubjectiveScorer:
    """Simple stub until full grader bridge implemented (Task 7 续)."""
    def score(self, student_answer, reference, prompt):
        return {"score": 0.0, "confidence": 0.0, "reason": "stub"}
