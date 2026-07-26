"""主观题打分 bridge: subjective-scoring 0.1.7 适配 (plan Task 7 Step 4 完整版).

不 import root.backend (plan 边界); 但与 backend/grader.py 评分结果保持
结构 parity (细节字段对齐 admin 读取 / XLSX 导出).

依赖 subjective-scoring git tag v0.1.7 @ yhwyxy/subjective-scoring:
    SubjectiveScoringService / ScoringRequest / ScoringResult / ScoringMode /
    ReviewLevel / CohereRerankerPairScorer
"""
from __future__ import annotations

import logging
import os
import re
import threading
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


# 评分标准解析正则 (与 backend/grader.py parity).
_RUBRIC_SPLIT_RE = re.compile(r"[；;。\n]+")
_RUBRIC_POINT_RE = re.compile(r"^\s*(.+?)\s*[:：]?\s*(\d+(?:\.\d+)?)\s*分?\s*$")
_RUBRIC_POINT_RE_ALT = re.compile(r"^\s*(.+?)\s+(\d+(?:\.\d+)?)\s*分\s*$")

# 支持的主观题题型 (8 种, 与 backend SUBJECTIVE_TYPES parity).
SUBJECTIVE_TYPES = {
    "short_answer", "essay", "sql", "code", "composite",
    "code_completion", "sql_completion", "calculation",
}


_service_lock = threading.RLock()
_service_singleton: Any | None = None  # SubjectiveScoringService 实例
_remote_reranker: Any | None = None    # CohereRerankerPairScorer 实例
_project_root = Path(__file__).resolve().parent.parent


def validate_remote_reranker_config() -> dict[str, str] | None:
    """读 RERANK_USE_REMOTE 环境变量. 与 backend 完全 parity."""
    raw_on = os.environ.get("RERANK_USE_REMOTE", "").strip().lower()
    if raw_on in {"", "false"}:
        return None
    if raw_on != "true":
        raise RuntimeError("RERANK_USE_REMOTE 只能设置为 true 或 false")
    cfg = {
        "RERANK_API_URL": os.environ.get("RERANK_API_URL", "").strip(),
        "RERANK_API_KEY": os.environ.get("RERANK_API_KEY", "").strip(),
        "RERANK_MODEL": os.environ.get("RERANK_MODEL", "").strip(),
    }
    missing = [k for k, v in cfg.items() if not v]
    if missing:
        raise RuntimeError("云端 Reranker 配置不完整, 缺: " + ", ".join(missing))
    return cfg


def get_subjective_service() -> Any:
    """懒加载 SubjectiveScoringService 单例 (remote / local 双模式).

    local 模式: 读 RERANKER_MODEL 环境变量, 相对路径按 _project_root 解析.
    """
    global _remote_reranker, _service_singleton
    if _service_singleton is not None:
        return _service_singleton
    with _service_lock:
        if _service_singleton is not None:
            return _service_singleton
        from subjective_scoring import (  # type: ignore
            CohereRerankerPairScorer, SubjectiveScoringService,
        )
        rc = validate_remote_reranker_config()
        if rc is not None:
            reranker = CohereRerankerPairScorer(
                url=rc["RERANK_API_URL"], api_key=rc["RERANK_API_KEY"],
                model=rc["RERANK_MODEL"],
            )
            try:
                _service_singleton = SubjectiveScoringService(
                    allow_model_load=False,
                    text_pair_scorer=reranker, code_pair_scorer=reranker,
                )
            except Exception:
                reranker.close()
                raise
            _remote_reranker = reranker
            return _service_singleton
        model_name = os.environ.get("RERANKER_MODEL", "").strip()
        if model_name:
            model_path = Path(model_name)
            if not model_path.is_absolute():
                model_path = (_project_root / model_name).resolve()
            if model_path.exists():
                model_name = str(model_path)
        _service_singleton = SubjectiveScoringService(
            allow_model_load=True,
            text_model=model_name or None, code_model=model_name or None,
        )
        return _service_singleton


def parse_scoring_rubric(
    scoring_rubric: str | None, max_score: float | int | None
) -> list[tuple[str, float]] | None:
    """build scoring_points: 解析 scoring_rubric 文本.

    1. 单赛道式: 每行一个点 ("点 X 分" 或 "点: 描述 N") -> regex 抽取 + 标准化.
    2. 无 scoring_rubric 且有 max_score -> 整卷作为单一得分点 (max).
    3. 分数总和不等于 max_score: 按比例缩放 (max_score 归一).
    4. 完全无 max_score + 无 rubric -> None (走 judge 没多评分点).
    """
    if not scoring_rubric and max_score in {None, 0}:
        return None
    if not scoring_rubric:
        return [(str(max_score), float(max_score))]
    points: list[tuple[str, float]] = []
    for line in _RUBRIC_SPLIT_RE.split(scoring_rubric):
        line = line.strip()
        if not line:
            continue
        m = _RUBRIC_POINT_RE.match(line) or _RUBRIC_POINT_RE_ALT.match(line)
        if not m:
            continue
        text = m.group(1).strip()
        w = float(m.group(2))
        if text and w >= 0:
            points.append((text, w))
    if not points:
        return [(str(max_score), float(max_score))] if max_score else None
    if max_score:
        total = sum(p[1] for p in points)
        if total > 0 and abs(total - float(max_score)) > 1e-6:
            scale = float(max_score) / total
            points = [(t, round(w * scale, 6)) for t, w in points]
    return points


def build_scoring_request(question: dict[str, Any], student_answer: str) -> Any:
    """question --> ScoringRequest (subjective_scoring 0.1.7 API).

    scoring_mode 映射:
        text -> text 普通短答
        code -> code/sql (code_completion/sql_completion 复用 code mode)
    scoring_points 由 parse_scoring_rubric 给出 (None -> 空列表走 judge 自由打分).
    """
    from subjective_scoring import ScoringMode  # type: ignore
    qtype = str(question.get("type", ""))
    mode_raw = str(question.get("scoring_mode") or "").strip().lower()
    has_lang = bool(question.get("allowed_languages") or question.get("code_language"))
    if mode_raw == "code" or (mode_raw == "" and has_lang):
        mode = ScoringMode.CODE
    else:
        mode = ScoringMode.TEXT
    scoring_points = parse_scoring_rubric(
        question.get("scoring_rubric"),
        question.get("score"),
    )
    request_kwargs: dict[str, Any] = {
        "question_id": str(question.get("id", "?")),
        "question_text": str(question.get("question", "")),
        "reference_answer": str(question.get("answer", "")),
        "student_answer": student_answer,
        "answer_language": "sql" if qtype in ("sql", "sql_completion", "code_completion") else None,
        "answer_code_language": (question.get("code_language") or
                                 (str(question.get("allowed_languages") or [None])[0] if isinstance(question.get("allowed_languages"), list) and question.get("allowed_languages") else None)),
        "max_score": float(question.get("score", 0) or 0),
        "scoring_mode": mode,
        "scoring_points": scoring_points or [],
        "banned_languages_raw": question.get("banned_languages"),
    }
    return _build_scoring_request_kwargs(request_kwargs)


def _build_scoring_request_kwargs(kwargs: dict[str, Any]) -> Any:
    """调 SubjectiveScoringService.score(request) 实际 ScoringRequest 构造.

    Plan 行 1113: 一种兼容构造路径是与 subjective_scoring 0.1.7 parity. 跳过
    keyword-only 严格区分, 转发到 ScoringRequest(**kwargs), 跳过 None 字段.
    """
    from subjective_scoring import ScoringRequest  # type: ignore
    valid_keys = {"question_id", "question_text", "reference_answer",
                  "student_answer", "answer_language", "answer_code_language",
                  "max_score", "scoring_mode", "scoring_points",
                  "banned_languages_raw"}
    cleaned = {k: v for k, v in kwargs.items() if k in valid_keys and v is not None}
    return ScoringRequest(**cleaned)


def _legacy_review_status(result: Any) -> str:
    review = getattr(result, "review_level", None)
    if review is None:
        return "low_confidence"
    val = review.value if hasattr(review, "value") else str(review)
    val = val.lower()
    if val == "high":
        return "high_confidence"
    if val == "medium":
        return "reviewed"
    if val == "low":
        return "low_confidence"
    if val == "manual":
        return "need_review"
    return "low_confidence"


def detail_from_scoring_result(
    question: dict[str, Any],
    student_answer: str,
    result: Any,
    *,
    preserve: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """ScoringResult -> grading_detail 条目 (完整字段对齐 backend/grader.py)."""
    max_score = float(question.get("score", 0) or 0)
    score = float(result.score)
    review_status = _legacy_review_status(result)
    low = review_status == "low_confidence" or (
        getattr(result, "need_manual_review", False) and
        float(getattr(result, "confidence", 0.0)) < 0.5)
    matched = [
        {
            "point_id": getattr(p, "point_id", None),
            "score": getattr(p, "score", 0.0),
            "max_score": getattr(p, "max_score", 0.0),
            "similarity": getattr(p, "similarity", 0.0),
            "evidence": getattr(p, "evidence", None),
            "reason": getattr(p, "reason", None),
        } for p in (getattr(result, "matched_points", None) or [])
    ]
    missed = [
        {
            "point_id": getattr(p, "point_id", None),
            "score": getattr(p, "score", 0.0),
            "max_score": getattr(p, "max_score", 0.0),
            "reason": getattr(p, "reason", None),
        } for p in (getattr(result, "missed_points", None) or [])
    ]
    smode = getattr(result, "scoring_mode", None)
    rvlevel = getattr(result, "review_level", None)
    entry: dict[str, Any] = {
        "question_id": question.get("id"),
        "type": question.get("type"),
        "question": question.get("question", ""),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "scoring_rubric": question.get("scoring_rubric"),
        "machine_score": score,
        "final_score": score,
        "max_score": max_score,
        "grading_method": f"subjective_scoring:{getattr(result, 'track', 'judge')}",
        "similarity": round(float(getattr(result, "confidence", 0.0)), 4),
        "confidence": round(float(getattr(result, "confidence", 0.0)), 4),
        "reason": "; ".join(result.warnings) if getattr(result, "warnings", None) else None,
        "fallback_reason": None,
        "review_status": review_status,
        "low_confidence": low,
        "scoring_mode": smode.value if smode and hasattr(smode, "value") else None,
        "track": getattr(result, "track", None),
        "need_manual_review": bool(getattr(result, "need_manual_review", False)),
        "review_level": rvlevel.value if rvlevel and hasattr(rvlevel, "value") else None,
        "matched_points": matched,
        "missed_points": missed,
        "warnings": list(getattr(result, "warnings", []) or []),
    }
    return _merge_preserved_review(entry, preserve)


# 保留 reviewer 改过分后的手动评分; machine_score 由新计算覆盖, 但 final +
# review_* + manually_reviewed 若已有 manual 结果则保留 (类似 backend
# preserve_manual_reviews 行为, plan Step 4 substep 9).
_PRESERVED_FIELDS = {
    "final_score", "review_status", "reviewer_id",
    "reviewed_at", "manually_reviewed",
    "review_comment", "review_action",
}


def _merge_preserved_review(entry: dict, preserve: dict | None) -> dict:
    if not preserve:
        return entry
    is_manual = bool(preserve.get("manually_reviewed"))
    for f in ("final_score", "review_status", "reviewer_id",
              "reviewed_at", "manually_reviewed",
              "review_comment", "review_action"):
        v = preserve.get(f)
        if v is not None and is_manual:
            entry[f] = v
        elif v is not None:
            entry[f] = v
    # 关键: 若 manual, final_score 拒绝被 worker 重置 -> 走新 machine_score 不替换 manual.
    # 若 admin 完成 hands-on review 后 worker regrade generation+1, 旧 final_score 不应
    # 被自动 = machine_score 覆盖.
    entry.setdefault("manually_reviewed", False)
    return entry


def aggregate_review_status(entries: list[dict[str, Any]]) -> str:
    """整卷级别 review_status plass规则:

    1. low_confidence 标志任一 -> 'need_review' (Worker 上抛人工)
    2. 任一 manually_reviewed=True -> 'partial_manual'
    3. 任一 need_manual_review=True -> 'need_review'
    4. 否则根据 machine_score 与 max 比例 -> high_confidence/reviewed 链.

    与 backend/grader.py parity 名字.
    """
    if not entries:
        return "need_review"
    score = 0.0
    max_score = 0.0
    found_low = False
    found_manual = False
    found_need_manual = False

    for e in entries:
        s = float(e.get("machine_score", 0) or 0)
        m = float(e.get("max_score", 0) or 0)
        score += s
        max_score += m
        if e.get("low_confidence"):
            found_low = True
        if e.get("manually_reviewed"):
            found_manual = True
        if e.get("need_manual_review"):
            found_need_manual = True

    found_unanswered = False
    for e in entries:
        if e.get("review_status") == "unanswered":
            found_unanswered = True
    if found_unanswered or found_need_manual or found_low:
        return "need_review"
    if found_manual:
        return "partial_manual"
    if max_score <= 0:
        return "reviewed"
    ratio = score / max_score
    if ratio >= 0.8:
        return "high_confidence"
    return "reviewed"


def grade_subjective(
    question: dict[str, Any], student_answer: Any, *,
    preserve: dict[str, Any] | None = None,
) -> tuple[float, float, str, dict[str, Any]]:
    """主观题打分入口 (grading.py 调用).

    返回 (final_score, machine_score, review_status, detail_entry).
    student_answer: str / dict (dict -> json dict 解出整体? quote_priority)

    composite 题:  按 subquestions 递归 (各自打分后求和)
    """
    if not isinstance(student_answer, str):
        student_answer = "" if student_answer is None else str(student_answer)
    if student_answer.strip() == "":
        return (0.0, 0.0, "unanswered", _empty_subjective_detail(
            question, preserve=preserve))

    ssvc = get_subjective_service()
    if str(question.get("type")) == "composite":
        return _grade_composite(ssvc, question, student_answer, preserve)

    request = build_scoring_request(question, student_answer)
    result = ssvc.score(request)
    entry = detail_from_scoring_result(question, student_answer,
                                       result, preserve=preserve)
    return (
        round(float(entry["final_score"]), 6),
        round(float(entry["machine_score"]), 6),
        entry["review_status"],
        entry,
    )


def _grade_composite(
    ssvc: Any, question: dict[str, Any], student_answer: str,
    preserve: dict[str, Any] | None,
) -> tuple[float, float, str, dict[str, Any]]:
    """Composite 题: 沿 subquestions 各自打分; final = sum(sub_final).

    student_answer 期望是 dict{"sub_q_id": "answer"} 或 json 解析失败 fallback whole str.
    """
    subs = question.get("subquestions") or []
    subs_map = question.get("sub_questions") or question.get("sub_qs") or []
    if not subs_map and not subs:
        subs = []
    final_total = 0.0
    machine_total = 0.0
    reviews: list[str] = []
    sub_details: list[dict] = []
    for sub in (subs_map or subs):
        sub_id = sub.get("id")
        ans = student_answer if isinstance(student_answer, dict) else student_answer
        if isinstance(ans, dict):
            ans = ans.get(sub_id, "")
        f, m, r, e = grade_subjective(sub, ans)
        final_total += f
        machine_total += m
        reviews.append(r)
        sub_details.append(e)
    return (round(final_total, 6), round(machine_total, 6),
            aggregate_review_status(sub_details),
            {"subquestion_results": sub_details})


def _empty_subjective_detail(question: dict[str, Any],
                             *, preserve: dict[str, Any] | None = None) -> dict[str, Any]:
    entry = {
        "question_id": question.get("id"),
        "type": question.get("type"),
        "question": question.get("question", ""),
        "student_answer": "",
        "reference_answer": question.get("answer", ""),
        "machine_score": 0.0, "final_score": 0.0,
        "max_score": float(question.get("score", 0) or 0),
        "grading_method": "subjective",
        "similarity": 0.0, "confidence": 0.0,
        "reason": "unanswered",
        "review_status": "unanswered",
        "matched_points": [], "missed_points": [], "warnings": ["unanswered"],
        "low_confidence": False, "need_manual_review": False,
        "manually_reviewed": False,
    }
    return _merge_preserved_review(entry, preserve)


def close_service() -> None:
    """Worker exit 时 close remote reranker (local 由 GC 回收)."""
    global _remote_reranker, _service_singleton
    if _remote_reranker is not None:
        try:
            _remote_reranker.close()
        except Exception:
            logger.exception("remote reranker close")
    _remote_reranker = None
    _service_singleton = None
