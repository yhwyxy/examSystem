"""判分总入口：编排客观题和主观题。"""
from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any

from . import embedding_grader, objective_grader
from .question_loader import SUBJECTIVE_TYPES

logger = logging.getLogger(__name__)


@dataclass
class GradingResult:
    """判分结果数据结构。"""
    objective_score: float = 0.0
    subjective_score_machine: float = 0.0
    subjective_score_final: float = 0.0
    total_score: float = 0.0
    review_status: str = "pending"
    grading_detail: list[dict[str, Any]] = field(default_factory=list)


# ---------------------------------------------------------------------------
# 主观题判分（Embedding 为主，关键词回退）
# ---------------------------------------------------------------------------

async def _run_subjective_grading(
    question: dict[str, Any],
    student_answer: str,
) -> dict[str, Any]:
    """对单道主观题判分，返回 grading_detail 条目。"""

    max_score = float(question.get("score", 0))
    qid = question.get("id", "?")

    emb = embedding_grader.grade_with_embedding(question, student_answer)

    if emb["status"] == "embedding_ok":
        return _subjective_detail_from_embedding(question, student_answer, emb)

    # Embedding 不可用，回退关键词相似度
    logger.info("Embedding 不可用，回退关键词判分: %s (reason=%s)", qid, emb.get("fallback_reason"))
    from .utils import keyword_similarity

    ref = question.get("answer", "")
    sim = keyword_similarity(student_answer, ref)
    score = round(sim * max_score, 2)
    return {
        "question_id": qid,
        "type": question.get("type"),
        "question": question.get("question", ""),
        "student_answer": student_answer,
        "reference_answer": ref,
        "scoring_rubric": question.get("scoring_rubric"),
        "machine_score": score,
        "final_score": score,
        "max_score": max_score,
        "grading_method": "keyword",
        "similarity": round(sim, 4),
        "confidence": None,
        "reason": None,
        "fallback_reason": f"Embedding 不可用: {emb.get('fallback_reason', '')}",
        "review_status": "need_review",
        "low_confidence": False,
    }


def _subjective_detail_from_embedding(
    question: dict[str, Any],
    student_answer: str,
    embedding_result: dict[str, Any],
) -> dict[str, Any]:
    max_score = float(question.get("score", 0))
    sim = float(embedding_result.get("similarity", 0.0))
    raw = sim * max_score
    from .utils import safe_score
    score = safe_score(raw, max_score)
    low = embedding_result.get("review_status") == "low_confidence"
    return {
        "question_id": question.get("id"),
        "type": question.get("type"),
        "question": question.get("question", ""),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "scoring_rubric": question.get("scoring_rubric"),
        "machine_score": score,
        "final_score": score,
        "max_score": max_score,
        "grading_method": "embedding",
        "similarity": round(sim, 4),
        "confidence": round(sim, 4),
        "reason": None,
        "fallback_reason": embedding_result.get("fallback_reason"),
        "review_status": embedding_result.get("review_status", "pending"),
        "low_confidence": low,
    }


# ---------------------------------------------------------------------------
# 逐题判分
# ---------------------------------------------------------------------------

async def grade_question(
    question: dict[str, Any],
    student_answer: Any,
) -> dict[str, Any]:
    """对单道题判分，返回该题的 grading_detail 条目。"""
    qtype = question.get("type")
    if qtype in SUBJECTIVE_TYPES:
        return await _run_subjective_grading(question, str(student_answer or ""))
    return objective_grader.grade_objective(question, student_answer)


# ---------------------------------------------------------------------------
# 汇总评分
# ---------------------------------------------------------------------------

def aggregate_review_status(details: list[dict[str, Any]]) -> str:
    """汇总复核状态（兼容新旧数据）。"""
    if not details:
        return "pending"
    for d in details:
        rs = d.get("review_status", "")
        if rs != "reviewed":
            return "pending"
    return "reviewed"


async def grade_submission(answers: dict[str, Any]) -> GradingResult:
    """完整判分入口，返回 GradingResult。"""
    from .question_loader import load_questions

    data = load_questions()
    questions = data.get("questions", [])
    q_map = {q["id"]: q for q in questions}

    details: list[dict[str, Any]] = []
    obj_score = 0.0
    subj_machine = 0.0
    subj_final = 0.0

    for qid, q in q_map.items():
        student_answer = answers.get(qid)
        detail = await grade_question(q, student_answer)
        details.append(detail)

        if q["type"] not in SUBJECTIVE_TYPES:
            obj_score += float(detail.get("final_score", 0))
        else:
            subj_machine += float(detail.get("machine_score", 0))
            subj_final += float(detail.get("final_score", 0))

    review_status = aggregate_review_status(details)

    return GradingResult(
        objective_score=round(obj_score, 6),
        subjective_score_machine=round(subj_machine, 6),
        subjective_score_final=round(subj_final, 6),
        total_score=round(obj_score + subj_final, 6),
        review_status=review_status,
        grading_detail=details,
    )
