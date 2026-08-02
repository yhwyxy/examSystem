"""案例分析题精确评分器：按评分点匹配，需回调 grader_bridge 做结构化输出。

评分策略
--------
两种评分点类型：
1. Phrase conclusion points（match="phrase"）：通过 _item_hit() 精确匹配，
   确定性结果。
2. Reason points：将 reason points 作为评分点构造文本评分请求，调用 text
   reranker 评分。
总分 = phrase points 得分 + reason points 得分。
Review level = 两种组件中较低的 review_level。
"""
from __future__ import annotations

import copy

from typing import Any

from scoring_worker.exact_scorers._base import ExactScoreResult
from scoring_worker.exact_scorers.enumeration_scorer import _item_hit, _norm as _enum_norm


def _get_phrase_points(question: dict) -> list[dict]:
    """获取 match='phrase' 的评分点。"""
    points = question.get("scoring_points") or []
    return [p for p in points if p.get("match") == "phrase"]


def _get_reason_points(question: dict) -> list[dict]:
    """获取没有 match='phrase' 标签的评分点（reason points）。"""
    points = question.get("scoring_points") or []
    return [p for p in points if p.get("match") != "phrase"]


def _score_phrase_points(
    phrase_points: list[dict], student_norm: str, max_score: float,
) -> tuple[float, list[dict], list[dict], str]:
    """精确匹配 phrase 评分点，返回 (score, matched, missed, review_level)。"""
    matched: list[dict] = []
    missed: list[dict] = []
    total = 0.0

    for i, p in enumerate(phrase_points):
        w = float(p.get("score", 0) or 0)
        pid = p.get("id", f"phrase{i + 1}")
        ev = _item_hit(p, student_norm)
        if ev is not None:
            total += w
            matched.append({
                "point_id": pid, "score": w, "max_score": w,
                "evidence": ev, "reason": "结论短语命中（精确/同义词）",
            })
        else:
            missed.append({
                "point_id": pid, "score": 0.0, "max_score": w,
                "reason": f"未命中结论短语：{p.get('text')}",
            })

    total = round(min(total, max_score), 4)
    if not matched and student_norm:
        return total, matched, missed, "manual_required"
    return total, matched, missed, "auto_pass"


def _score_reason_points(
    question: dict, reason_points: list[dict], student_answer: str,
) -> tuple[float, str, dict[str, Any]]:
    """调用 text reranker 评分 reason points。

    构造一个只有 reason points 的子 question，通过 grader_bridge 评分。
    返回 (score, review_level, detail_section)。
    """
    from scoring_worker.grader_bridge import (
        build_scoring_request,
        detail_from_scoring_result,
        get_subjective_service,
    )

    if not reason_points:
        return 0.0, "auto_pass", {}

    # 浅拷贝 question，只保留 reason points 和 scoring_mode="text"
    sub_q = copy.copy(question)
    sub_q["scoring_points"] = reason_points
    sub_q["scoring_mode"] = "text"

    request = build_scoring_request(sub_q, student_answer)
    svc = get_subjective_service()
    result = svc.score(request)
    entry = detail_from_scoring_result(sub_q, student_answer, result)

    matched = entry.get("matched_points", [])
    missed = entry.get("missed_points", [])
    score = float(entry.get("machine_score", 0.0))
    max_score = float(question.get("score", 0) or 0)
    score = round(min(score, max_score), 4)

    review_level = "auto_pass"
    rs = entry.get("review_status", "")
    if rs in ("need_review", "low_confidence", "open_ended"):
        review_level = "manual_required"
    elif rs == "reviewed":
        review_level = "suggested_review"

    return score, review_level, {
        "reason_matched_points": matched,
        "reason_missed_points": missed,
    }


def score_case_analysis(question: dict, student_answer: str) -> ExactScoreResult:
    max_score = float(question.get("score", 0) or 0)

    # 1) 评分 phrase 结论点
    phrase_points = _get_phrase_points(question)
    student_norm = _enum_norm(student_answer)
    phrase_score, phrase_matched, phrase_missed, phrase_level = (
        _score_phrase_points(phrase_points, student_norm, max_score)
    )

    # 2) 评分 reasoning 点
    reason_points = _get_reason_points(question)
    reason_score, reason_level, reason_detail = (
        _score_reason_points(question, reason_points, student_answer)
    )

    # 3) 总分 = phrase + reason
    total = round(min(phrase_score + reason_score, max_score), 4)

    # 4) review_level = min of both components
    level_rank = {"auto_pass": 0, "suggested_review": 1, "manual_required": 2}
    final_level = phrase_level
    if level_rank.get(reason_level, 0) > level_rank.get(final_level, 0):
        final_level = reason_level

    # 如果任何组件是 manual_required 则整体 manual_required
    if phrase_level == "manual_required" or reason_level == "manual_required":
        final_level = "manual_required"

    detail = {
        "phrase_matched_points": phrase_matched,
        "phrase_missed_points": phrase_missed,
        "reason_score": reason_score,
        "reason_review_level": reason_level,
        **reason_detail,
    }

    if final_level == "manual_required":
        detail["reason"] = "案例结论短语或推理未命中，转人工确认"
    else:
        detail["reason"] = "案例分析评分完成"

    return ExactScoreResult(total, detail, final_level)  # type: ignore[arg-type]