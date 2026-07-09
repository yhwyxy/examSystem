"""判分主入口。

返回结构统一使用 GradingResult dataclass，避免裸 tuple 导致可读性差、易出错。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from fastapi.concurrency import run_in_threadpool

from . import database, llm_grader, objective_grader
from .config import get_config
from .embedding_grader import grade_with_embedding
from .question_loader import get_question_map, is_objective


@dataclass(frozen=True, slots=True)
class GradingResult:
    """单条判题结果。"""
    question_id: str
    question_type: str
    max_score: float
    score: float
    is_correct: bool | None = None
    grading_method: str = "objective"
    reason: str = ""
    confidence: float | None = None
    low_confidence: bool = False
    raw_scores: dict[str, float] | None = None


@dataclass
class SubmissionGradingResult:
    """整份试卷的判分结果。"""
    objective_score: float = 0.0
    subjective_score_machine: float = 0.0
    subjective_score_final: float = 0.0
    total_score: float = 0.0
    review_status: str = "auto_scored"
    grading_detail: list[dict[str, Any]] = field(default_factory=list)


def _score_value(detail: dict[str, Any], *keys: str) -> float:
    for key in keys:
        value = detail.get(key)
        if value is not None:
            try:
                return float(value)
            except (TypeError, ValueError):
                continue
    return 0.0


def _review_status_from_confidence(confidence: Any, cfg: Any) -> str:
    try:
        value = float(confidence)
    except (TypeError, ValueError):
        return "need_review"
    # 三阈值四段语义：
    #   >= high_confidence_threshold      → auto_scored（高置信，自动采信）
    #   >= need_review_threshold          → need_review（中等，进人工复核队列）
    #   >= low_confidence_threshold       → low_confidence（低置信，标记重点关注）
    #   <  low_confidence_threshold       → low_confidence
    if value >= cfg.review.high_confidence_threshold:
        return "auto_scored"
    if value >= cfg.review.need_review_threshold:
        return "need_review"
    return "low_confidence"


def _subjective_detail_from_llm(
    question: dict[str, Any],
    student_answer: str,
    llm_result: dict[str, Any],
    cfg: Any,
) -> dict[str, Any]:
    score_raw = llm_result.get("score", llm_result.get("machine_score"))
    try:
        score = round(float(score_raw), 2)
    except (TypeError, ValueError):
        score = 0.0

    max_score = float(question.get("score", 0))
    if score < 0:
        score = 0.0
    if score > max_score:
        score = max_score
    confidence = llm_result.get("confidence", 0.0)
    review_status = _review_status_from_confidence(confidence, cfg)

    return {
        "question_id": question["id"],
        "type": question.get("type", "unknown"),
        "max_score": max_score,
        "score": score,
        "machine_score": score,
        "final_score": score,
        "question": question.get("question", question.get("text", "")),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "grading_method": "llm",
        "reason": llm_result.get("reason", ""),
        "confidence": confidence,
        "low_confidence": review_status in {"need_review", "low_confidence"},
        "review_status": review_status,
        "grading_status": "pending" if review_status in {"need_review", "low_confidence"} else "auto_scored",
        "raw_scores": llm_result.get("raw_scores"),
    }


def _subjective_detail_from_embedding(
    question: dict[str, Any],
    student_answer: str,
    embedding_result: dict[str, Any],
) -> dict[str, Any]:
    max_score = float(question.get("score", 0))
    safe_score = max(0.0, min(embedding_result.get("final_score", 0), max_score))
    review_status = embedding_result.get("review_status") or "need_review"
    low_confidence = review_status in {"need_review", "low_confidence", "pending"}
    return {
        "question_id": question["id"],
        "type": question.get("type", "unknown"),
        "max_score": max_score,
        "score": safe_score,
        "machine_score": max(0.0, min(embedding_result.get("machine_score", safe_score), max_score)),
        "final_score": safe_score,
        "question": question.get("question", question.get("text", "")),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "grading_method": "embedding",
        "reason": embedding_result.get("reason", ""),
        "confidence": embedding_result.get("confidence"),
        "low_confidence": low_confidence,
        "review_status": "need_review" if review_status == "pending" else review_status,
        "grading_status": "pending" if low_confidence else "auto_scored",
        "raw_scores": None,
    }


async def _run_subjective_grading(
    question: dict[str, Any],
    student_answer: Any,
    cfg: Any,
) -> dict[str, Any]:
    text_answer = student_answer if isinstance(student_answer, str) else str(student_answer or "")

    llm_result = await run_in_threadpool(
        llm_grader.grade_subjective, question, text_answer, cfg
    ) if cfg.grading.use_llm else None

    if llm_result is not None:
        detail = _subjective_detail_from_llm(question, text_answer, llm_result, cfg)
    else:
        fallback_reason = "llm_unavailable_or_invalid" if cfg.grading.use_llm else "llm_disabled"
        embedding_result = grade_with_embedding(question, text_answer, fallback_reason)
        detail = _subjective_detail_from_embedding(question, text_answer, embedding_result)
        if fallback_reason == "llm_unavailable_or_invalid":
            detail["reason"] = f"{detail['reason']}（LLM 不可用，已回退到嵌入评分）".strip()

    return detail


async def grade_submission(answers: dict[str, Any]) -> SubmissionGradingResult:
    cfg = get_config()
    questions = get_question_map()

    details: list[dict[str, Any]] = []
    objective_score = 0.0
    subjective_score_machine = 0.0
    pending_subjective_ids: list[str] = []
    low_confidence_count = 0

    for qid, question in questions.items():
        student_answer = answers.get(qid)

        if is_objective(question):
            detail = objective_grader.grade_objective(question, student_answer)
            details.append(detail)
            objective_score += _score_value(detail, "final_score", "score", "machine_score")
        else:
            detail = await _run_subjective_grading(question, student_answer, cfg)
            details.append(detail)
            subjective_score_machine += _score_value(detail, "machine_score", "score", "final_score")
            review_status = detail.get("review_status")
            if review_status == "low_confidence":
                low_confidence_count += 1
                pending_subjective_ids.append(qid)
            elif review_status in {"need_review", "pending"} or detail.get("low_confidence"):
                pending_subjective_ids.append(qid)

    subjective_score_final = sum(
        _score_value(d, "final_score", "score", "machine_score")
        for d in details
        if not is_objective({"type": d.get("type")})
    )
    if pending_subjective_ids:
        review_status = "low_confidence" if low_confidence_count else "need_review"
    elif low_confidence_count > 0:
        review_status = "low_confidence"
    else:
        review_status = "auto_scored"

    total_score = objective_score + subjective_score_final
    return SubmissionGradingResult(
        objective_score=objective_score,
        subjective_score_machine=subjective_score_machine,
        subjective_score_final=subjective_score_final,
        total_score=total_score,
        review_status=review_status,
        grading_detail=details,
    )
