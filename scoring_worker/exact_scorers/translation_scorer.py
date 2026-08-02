"""翻译题精确评分器：先校验作答语言与目标语言一致，再按短语条目命中。"""
from __future__ import annotations

import re

from ._base import ExactScoreResult
from .enumeration_scorer import score_enumeration

_CJK_RE = re.compile(r"[一-鿿]")


def _cjk_ratio(text: str) -> float:
    chars = [c for c in str(text or "") if not c.isspace()]
    if not chars:
        return 0.0
    return sum(1 for c in chars if _CJK_RE.match(c)) / len(chars)


def score_translation(question: dict, student_answer: str) -> ExactScoreResult:
    cfg = question.get("translation") or {}
    target = str(cfg.get("target_lang") or "en").lower()
    ratio = _cjk_ratio(student_answer)
    if target == "en" and ratio > 0.5:
        return ExactScoreResult(0.0, {
            "reason": f"作答语言与目标语言不符（要求英文，中文占比 {ratio:.0%}）",
            "matched_points": [], "missed_points": [],
        }, "manual_required")
    if target == "zh" and ratio < 0.2:
        return ExactScoreResult(0.0, {
            "reason": f"作答语言与目标语言不符（要求中文，中文占比 {ratio:.0%}）",
            "matched_points": [], "missed_points": [],
        }, "manual_required")
    return score_enumeration(question, student_answer)