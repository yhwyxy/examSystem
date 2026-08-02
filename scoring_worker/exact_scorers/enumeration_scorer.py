"""列举题精确评分器：评分点=条目集合，命中任一同义词即得该条目满分。

评分策略
--------
- 命中 = 条目 text 或任一 synonym 归一化后是学生答案子串；长度 >3 的变体
  额外允许 bigram 覆盖率 >= 0.75。
- 命中条目得该条满分，一条最多计一次；总分 min(Σ命中分, 题目满分)；不倒扣。
- 命中数 >= 1 -> auto_pass（确定性结果）；零命中且作答非空 -> manual_required
  （意译保护，交人工）；空答案不会进入（上游已拦）。
"""
from __future__ import annotations

import re
import unicodedata

from ._base import ExactScoreResult

_STRIP_RE = re.compile(r"[\s,，。.;；:：、！!？?（）()\[\]【】\"'“”‘’\-—–/\\]+")


def _norm(text: str) -> str:
    return _STRIP_RE.sub("", unicodedata.normalize("NFKC", str(text or "")).casefold())


def _bigram_coverage(term: str, student: str) -> float:
    grams = [term[i:i + 2] for i in range(len(term) - 1)]
    if not grams:
        return 1.0 if term and term in student else 0.0
    return sum(1 for g in grams if g in student) / len(grams)


def _item_hit(point: dict, student_norm: str) -> str | None:
    """命中返回证据变体原文，未命中返回 None。"""
    variants = [str(point.get("text") or "")] + [str(s) for s in (point.get("synonyms") or [])]
    for v in variants:
        nv = _norm(v)
        if not nv:
            continue
        if len(nv) <= 3:
            if nv in student_norm:
                return v
        elif nv in student_norm or _bigram_coverage(nv, student_norm) >= 0.75:
            return v
    return None


def score_enumeration(question: dict, student_answer: str) -> ExactScoreResult:
    points = question.get("scoring_points") or []
    max_score = float(question.get("score", 0) or 0)
    student_norm = _norm(student_answer)
    matched, missed, total = [], [], 0.0
    for p in points:
        w = float(p.get("score", 0) or 0)
        ev = _item_hit(p, student_norm)
        if ev is not None:
            total += w
            matched.append({"point_id": p.get("id"), "score": w, "max_score": w,
                            "evidence": ev, "reason": "条目命中（精确/同义词）"})
        else:
            missed.append({"point_id": p.get("id"), "score": 0.0, "max_score": w,
                           "reason": f"未命中条目：{p.get('text')}"})
    total = round(min(total, max_score), 4)
    detail = {"matched_points": matched, "missed_points": missed}
    if not matched and student_norm:
        detail["reason"] = "枚举零命中：疑似意译或错答，转人工确认"
        return ExactScoreResult(0.0, detail, "manual_required")
    return ExactScoreResult(total, detail, "auto_pass")