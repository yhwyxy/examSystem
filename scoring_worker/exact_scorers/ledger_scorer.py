"""账目题精确评分器：按借贷分录匹配评分点。

评分策略
--------
- 每个 ledger.entries[i] 有 keywords (字符串列表) 和 numbers (浮点数列表)，
  以及 score。学生答案必须包含 ALL numbers（在 tolerance 内）且至少包含一个
  keyword，该 entry 才得满分。
- 每个 ledger.treatment_points[i] 有 text、可选 synonyms、score。命中逻辑
  复用 enumeration_scorer 的 _item_hit()。
- 总分 = min(Σ(entries + treatments), max_score)。
- 零匹配且作答非空 → manual_required；否则 suggested_review（金额正确性仍需
  人眼确认）。
"""
from __future__ import annotations

import re
import unicodedata

from ._base import ExactScoreResult
from .enumeration_scorer import _item_hit, _norm as _enum_norm

_STRIP_RE = re.compile(r"[\s,，。;；:：、！!？?（）()\[\]【】\"'“”‘’\-—–/\\]+")


def _norm(text: str) -> str:
    return _STRIP_RE.sub("", unicodedata.normalize("NFKC", str(text or "")).casefold())


def _extract_numbers(text: str) -> list[float]:
    """从归一化文本中提取所有数字。"""
    tokens = re.findall(r"\d+(?:\.\d+)?", text)
    return [float(t) for t in tokens]


def _numbers_match(
    expected: list[float], student_norm: str, tolerance: float = 0.005
) -> bool:
    """检查学生答案中是否包含所有期望数字（在 tolerance 内）。"""
    student_numbers = _extract_numbers(student_norm)
    for exp in expected:
        found = any(abs(s - exp) <= tolerance for s in student_numbers)
        if not found:
            return False
    return True


def _keywords_match(keywords: list[str], student_norm: str) -> bool:
    """检查学生答案中是否包含至少一个 keyword。"""
    for kw in keywords:
        nkw = _norm(kw)
        if nkw and nkw in student_norm:
            return True
    return False


def score_ledger(question: dict, student_answer: str) -> ExactScoreResult:
    ledger = question.get("ledger") or {}
    entries = ledger.get("entries") or []
    treatment_points = ledger.get("treatment_points") or []
    max_score = float(question.get("score", 0) or 0)
    student_norm = _norm(student_answer)
    tolerance = float(ledger.get("tolerance", 0.005))

    matched_entries: list[dict] = []
    missed_entries: list[dict] = []
    total = 0.0

    # 评分 entries
    for i, entry in enumerate(entries):
        w = float(entry.get("score", 0) or 0)
        keywords = entry.get("keywords") or []
        numbers = entry.get("numbers") or []
        eid = f"entry{i + 1}"

        num_ok = _numbers_match(numbers, student_norm, tolerance)
        kw_ok = _keywords_match(keywords, student_norm)

        if num_ok and kw_ok:
            total += w
            matched_entries.append({
                "point_id": eid, "score": w, "max_score": w,
                "evidence": f"数字+关键词命中",
                "reason": "分录条目命中（数字+关键词）",
            })
        else:
            reasons = []
            if not num_ok:
                reasons.append(f"数字未匹配：期望 {numbers}")
            if not kw_ok:
                reasons.append(f"关键词未匹配：{keywords}")
            missed_entries.append({
                "point_id": eid, "score": 0.0, "max_score": w,
                "reason": "；".join(reasons),
            })

    # 评分 treatment_points
    matched_treatments: list[dict] = []
    missed_treatments: list[dict] = []
    for i, tp in enumerate(treatment_points):
        w = float(tp.get("score", 0) or 0)
        tid = f"tp{i + 1}"
        ev = _item_hit(tp, student_norm)
        if ev is not None:
            total += w
            matched_treatments.append({
                "point_id": tid, "score": w, "max_score": w,
                "evidence": ev, "reason": "处理意见命中（精确/同义词）",
            })
        else:
            missed_treatments.append({
                "point_id": tid, "score": 0.0, "max_score": w,
                "reason": f"未命中处理意见：{tp.get('text')}",
            })

    total = round(min(total, max_score), 4)
    detail = {
        "matched_entries": matched_entries,
        "missed_entries": missed_entries,
        "matched_treatment_points": matched_treatments,
        "missed_treatment_points": missed_treatments,
    }

    has_any_match = bool(matched_entries or matched_treatments)
    if not has_any_match and student_norm:
        detail["reason"] = "账目零匹配：疑似意译或错答，转人工确认"
        return ExactScoreResult(0.0, detail, "manual_required")

    detail["reason"] = "账目评分完成，金额正确性仍需人眼确认"
    return ExactScoreResult(total, detail, "suggested_review")