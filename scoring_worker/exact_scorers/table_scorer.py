"""表格补全评分器：单元格期望值精确匹配，可要求行标签出现在值的邻域。"""
from __future__ import annotations

import re
import unicodedata

from ._base import ExactScoreResult

# 保留 ASCII 句点（IP/掩码等含点值），只清空白与标点
_STRIP_RE = re.compile(r"[\s,，。;；:：、！!？?（）()\[\]【】\"'“”‘’\-—–/\\]+")
_CTX_WINDOW = 24


def _norm(text: str) -> str:
    return _STRIP_RE.sub("", unicodedata.normalize("NFKC", str(text or "")).casefold())


def score_table(question: dict, student_answer: str) -> ExactScoreResult:
    cells = (question.get("table") or {}).get("cells") or []
    max_score = float(question.get("score", 0) or 0)
    student_norm = _norm(student_answer)
    matched, missed, total = [], [], 0.0
    for i, cell in enumerate(cells):
        w = float(cell.get("score", 0) or 0)
        label = _norm(cell.get("label") or "")
        hit = None
        for v in (cell.get("expected") or []):
            nv = _norm(v)
            idx = student_norm.find(nv) if nv else -1
            if idx < 0:
                continue
            if cell.get("require_label_context") and label:
                ctx = student_norm[max(0, idx - _CTX_WINDOW): idx]
                if label not in ctx:
                    continue
            hit = v
            break
        cid = f"cell{i + 1}"
        if hit is not None:
            total += w
            matched.append({"point_id": cid, "score": w, "max_score": w,
                            "evidence": hit, "reason": "单元格值命中"})
        else:
            missed.append({"point_id": cid, "score": 0.0, "max_score": w,
                           "reason": f"未命中单元格 {cell.get('label')}：{cell.get('expected')}"})
    total = round(min(total, max_score), 4)
    detail = {"matched_points": matched, "missed_points": missed}
    if len(matched) < len(cells):
        detail["reason"] = "表格未完整填写，转人工核对"
        return ExactScoreResult(total, detail, "manual_required")
    return ExactScoreResult(total, detail, "auto_pass")