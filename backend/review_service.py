"""人工复核与成绩管理服务。"""
from __future__ import annotations

import asyncio
import json
from typing import Any

from . import database
from .grader import aggregate_review_status, grade_submission
from .question_loader import SUBJECTIVE_TYPES


def _parse_detail(submission: dict[str, Any]) -> list[dict[str, Any]]:
    return json.loads(submission.get("grading_detail_json") or "[]")


def _parse_answers(submission: dict[str, Any]) -> dict[str, Any]:
    return json.loads(submission.get("answers_json") or "{}")


def get_submission_detail(submission_id: int) -> dict[str, Any] | None:
    sub = database.get_submission(submission_id)
    if not sub:
        return None
    sub["answers"] = _parse_answers(sub)
    sub["grading_detail"] = _parse_detail(sub)
    sub["review_logs"] = database.list_review_logs(submission_id)
    return sub


def list_submission_summaries(
    keyword: str | None = None,
    review_status: str | None = None,
    sort_by: str = "submitted_at",
    order: str = "desc",
) -> list[dict[str, Any]]:
    rows = database.list_submissions(keyword=keyword, review_status=review_status, sort_by=sort_by, order=order)
    for r in rows:
        r.pop("answers_json", None)
        r.pop("grading_detail_json", None)
    return rows


def review_question(submission_id: int, question_id: str, new_score: float, note: str | None = None) -> dict[str, Any]:
    submission = database.get_submission(submission_id)
    if not submission:
        return {"success": False, "code": "SUBMISSION_NOT_FOUND", "message": "提交记录不存在"}

    details = _parse_detail(submission)
    target = next((d for d in details if d.get("question_id") == question_id), None)
    if target is None:
        return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "题目不存在"}
    if target.get("type") not in SUBJECTIVE_TYPES:
        return {"success": False, "code": "OBJECTIVE_REVIEW_FORBIDDEN", "message": "客观题不支持人工改分"}

    max_score = float(target.get("max_score", 0))
    if new_score < 0 or new_score > max_score:
        return {"success": False, "code": "REVIEW_SCORE_INVALID", "message": "复核分数非法"}

    old_score = float(target.get("final_score", target.get("machine_score", 0.0)))
    target["final_score"] = float(new_score)
    target["manually_reviewed"] = True
    target["review_status"] = "reviewed"
    target["reviewer_note"] = note

    subjective_final = sum(float(d.get("final_score", 0.0)) for d in details if d.get("type") in SUBJECTIVE_TYPES)
    total_score = round(float(submission["objective_score"]) + subjective_final, 6)
    review_status = aggregate_review_status(details)

    database.update_submission_after_review(
        submission_id=submission_id,
        grading_detail=details,
        subjective_score_final=subjective_final,
        total_score=total_score,
        review_status=review_status,
        reviewer_note=note,
    )
    database.insert_review_log(
        submission_id=submission_id,
        question_id=question_id,
        old_score=old_score,
        new_score=float(new_score),
        note=note,
    )
    return {"success": True, "total_score": total_score, "review_status": review_status}


def _is_manually_reviewed(detail: dict[str, Any]) -> bool:
    return bool(detail.get("reviewed_by") or detail.get("manually_reviewed"))


def _preserve_manual_final(
    new_detail: dict[str, Any], old_detail: dict[str, Any]
) -> None:
    """保留人工最终分和复核元数据，不覆盖新的机器评分字段。"""
    new_detail["final_score"] = float(
        old_detail.get("final_score", old_detail.get("score", 0)) or 0
    )
    new_detail["review_status"] = "reviewed"
    for field in (
        "reviewed_by", "review_note", "manually_reviewed", "reviewer_note"
    ):
        if field in old_detail:
            new_detail[field] = old_detail[field]


def _merge_manual_reviews(
    new_details: list[dict[str, Any]], old_details: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    old_by_qid = {str(d.get("question_id")): d for d in old_details}
    for detail in new_details:
        old = old_by_qid.get(str(detail.get("question_id")))
        if not old:
            continue

        if detail.get("is_composite") and isinstance(detail.get("sub_results"), list):
            old_subs = {
                str(sub.get("sub_question_id")): sub
                for sub in old.get("sub_results", [])
                if isinstance(sub, dict)
            }
            for sub in detail["sub_results"]:
                old_sub = old_subs.get(str(sub.get("sub_question_id")))
                if old_sub and _is_manually_reviewed(old_sub):
                    _preserve_manual_final(sub, old_sub)

            detail["machine_score"] = sum(
                float(sub.get("machine_score", sub.get("score", 0)) or 0)
                for sub in detail["sub_results"]
            )
            detail["score"] = detail["machine_score"]
            detail["final_score"] = sum(
                float(sub.get("final_score", sub.get("score", 0)) or 0)
                for sub in detail["sub_results"]
            )
            detail["review_status"] = aggregate_review_status(detail["sub_results"])
            detail["low_confidence"] = detail["review_status"] != "reviewed"
            detail["need_manual_review"] = detail["review_status"] != "reviewed"
            detail["reason"] = "; ".join(
                f"{sub.get('sub_question_id')}={sub.get('final_score', sub.get('score'))}/{sub.get('max_score')}"
                for sub in detail["sub_results"]
            )
        elif _is_manually_reviewed(old):
            _preserve_manual_final(detail, old)
    return new_details


def regrade_submission(submission_id: int) -> dict[str, Any]:
    """重新机器判分。已人工复核的主观题保留人工分。"""
    submission = database.get_submission(submission_id)
    if not submission:
        return {"success": False, "code": "SUBMISSION_NOT_FOUND", "message": "提交记录不存在"}

    old_details = _parse_detail(submission)
    answers = _parse_answers(submission)
    loop = asyncio.new_event_loop()
    try:
        new_result = loop.run_until_complete(
            grade_submission(answers, paper_id=submission.get("paper_id") or "default")
        )
    finally:
        loop.close()
    new_details = _merge_manual_reviews(new_result.grading_detail, old_details)

    objective_score = sum(float(d.get("final_score", 0.0)) for d in new_details if d.get("type") not in SUBJECTIVE_TYPES)
    subjective_machine = sum(float(d.get("machine_score", 0.0)) for d in new_details if d.get("type") in SUBJECTIVE_TYPES)
    subjective_final = sum(float(d.get("final_score", 0.0)) for d in new_details if d.get("type") in SUBJECTIVE_TYPES)
    total_score = round(objective_score + subjective_final, 6)
    review_status = aggregate_review_status(new_details)

    database.save_grading_result(submission_id, {
        "objective_score": objective_score,
        "subjective_score_machine": subjective_machine,
        "subjective_score_final": subjective_final,
        "total_score": total_score,
        "review_status": review_status,
        "grading_detail": new_details,
    })
    return {"success": True, "total_score": total_score, "review_status": review_status}
