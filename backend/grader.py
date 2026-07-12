"""判分总入口：编排客观题和主观题。

主观题通过独立库 subjective-scoring（SubjectiveScoringService）评分；
客观题仍由 objective_grader 处理。
"""
from __future__ import annotations

import logging
import os
import re
import threading
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any

from . import objective_grader
from .config import get_config
from .question_loader import SUBJECTIVE_TYPES

logger = logging.getLogger(__name__)

from subjective_scoring import (
    CohereRerankerPairScorer,
    ReviewLevel,
    ScoringMode,
    ScoringRequest,
    ScoringResult,
    SubjectiveScoringService,
)


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
# 主观题服务（懒加载单例，测试可替换）
# ---------------------------------------------------------------------------

_subjective_service: SubjectiveScoringService | None = None
_remote_reranker: CohereRerankerPairScorer | None = None
_subjective_service_lock = threading.RLock()


def validate_remote_reranker_config() -> dict[str, str] | None:
    """读取并校验云端 Reranker 环境变量。"""
    raw_enabled = os.environ.get("RERANK_USE_REMOTE", "").strip().lower()
    if raw_enabled in {"", "false"}:
        return None
    if raw_enabled != "true":
        raise RuntimeError("RERANK_USE_REMOTE 只能设置为 true 或 false")

    remote_config = {
        "RERANK_API_URL": os.environ.get("RERANK_API_URL", "").strip(),
        "RERANK_API_KEY": os.environ.get("RERANK_API_KEY", "").strip(),
        "RERANK_MODEL": os.environ.get("RERANK_MODEL", "").strip(),
    }
    configured = [name for name, value in remote_config.items() if value]
    if len(configured) != len(remote_config):
        missing = [name for name, value in remote_config.items() if not value]
        raise RuntimeError(
            "云端 Reranker 配置不完整，缺少环境变量: " + ", ".join(missing)
        )
    return remote_config


def get_subjective_service() -> SubjectiveScoringService:
    """获取主观题评分服务单例。"""
    global _remote_reranker, _subjective_service
    with _subjective_service_lock:
        if _subjective_service is None:
            remote_config = validate_remote_reranker_config()
            if remote_config is not None:
                reranker = CohereRerankerPairScorer(
                    url=remote_config["RERANK_API_URL"],
                    api_key=remote_config["RERANK_API_KEY"],
                    model=remote_config["RERANK_MODEL"],
                )
                try:
                    _subjective_service = SubjectiveScoringService(
                        allow_model_load=False,
                        text_pair_scorer=reranker,
                        code_pair_scorer=reranker,
                    )
                except Exception:
                    reranker.close()
                    raise
                _remote_reranker = reranker
            else:
                # 默认允许加载 CrossEncoder；无 semantic 依赖时库内回退词法相似度
                _subjective_service = SubjectiveScoringService(allow_model_load=True)
        return _subjective_service


def set_subjective_service(service: SubjectiveScoringService | None) -> None:
    """测试/运维注入自定义服务；传 None 恢复懒加载默认。"""
    global _remote_reranker, _subjective_service
    with _subjective_service_lock:
        if _remote_reranker is not None:
            _remote_reranker.close()
            _remote_reranker = None
        _subjective_service = service


def close_subjective_service() -> None:
    """关闭评分服务持有的远端连接池。"""
    set_subjective_service(None)


# ---------------------------------------------------------------------------
# 题库字段 → ScoringRequest
# ---------------------------------------------------------------------------

_RUBRIC_SPLIT_RE = re.compile(r"[；;。\n]+")
_RUBRIC_POINT_RE = re.compile(
    r"^\s*(.+?)\s*[:：]?\s*(\d+(?:\.\d+)?)\s*分?\s*$"
)
_RUBRIC_POINT_RE_ALT = re.compile(
    r"^\s*(.+?)\s+(\d+(?:\.\d+)?)\s*分\s*$"
)


def parse_scoring_rubric(rubric: str | None, max_score: float) -> list[dict[str, Any]]:
    """将中文评分标准字符串拆成 scoring_points。

    示例：
    ``资源导向 3 分；HTTP 方法 2 分；无状态 2 分``
    """
    if not rubric or not str(rubric).strip():
        return []
    points: list[dict[str, Any]] = []
    parts = [p.strip() for p in _RUBRIC_SPLIT_RE.split(str(rubric)) if p.strip()]
    for i, part in enumerate(parts):
        m = _RUBRIC_POINT_RE_ALT.match(part) or _RUBRIC_POINT_RE.match(part)
        if not m:
            continue
        text = m.group(1).strip().rstrip("：:")
        try:
            score = float(m.group(2))
        except ValueError:
            continue
        if score < 0:
            continue
        points.append(
            {
                "id": f"r{i + 1}",
                "text": text,
                "score": score,
                "required": i == 0,
            }
        )
    if not points:
        return []
    total = sum(p["score"] for p in points)
    if total > max_score + 1e-6 and total > 0:
        # 按比例缩放到题目满分，避免 ValidationError
        scale = max_score / total
        for p in points:
            p["score"] = round(p["score"] * scale, 4)
    return points


def _coerce_scoring_mode(raw: Any) -> ScoringMode | None:
    if raw is None or raw == "":
        return None
    if isinstance(raw, ScoringMode):
        return raw
    try:
        return ScoringMode(str(raw).strip().lower())
    except ValueError:
        return None


def build_scoring_request(
    question: dict[str, Any],
    student_answer: str,
) -> ScoringRequest:
    """把题库题目 + 学生作答映射为 ScoringRequest。"""
    max_score = float(question.get("score", 0) or 0)
    qtype = str(question.get("type") or "subjective")

    # scoring_points：显式配置优先，否则解析 scoring_rubric
    raw_points = question.get("scoring_points")
    if isinstance(raw_points, list) and raw_points:
        scoring_points = raw_points
    else:
        scoring_points = parse_scoring_rubric(
            question.get("scoring_rubric"), max_score
        )

    mode = _coerce_scoring_mode(question.get("scoring_mode"))
    # short_answer / essay 默认 text；sql/code 由 mode / code_language 决定
    if mode is None and qtype in SUBJECTIVE_TYPES:
        code_lang = question.get("code_language")
        if code_lang and str(code_lang).strip().lower() == "sql":
            mode = ScoringMode.SQL
        elif code_lang:
            mode = ScoringMode.CODE
        else:
            mode = ScoringMode.TEXT

    cfg = get_config()
    precision = int(getattr(cfg.scoring, "score_precision", 1))

    payload: dict[str, Any] = {
        "question_id": str(question.get("id", "?")),
        "paper_id": question.get("paper_id"),
        "question_type": qtype,
        "scoring_mode": mode,
        "code_language": question.get("code_language"),
        "course_type": question.get("course_type"),
        "max_score": max_score,
        "question": str(question.get("question") or ""),
        "reference_answer": str(question.get("answer") or ""),
        "scoring_points": scoring_points,
        "student_answer": student_answer,
        "scoring_config": {
            "score_precision": precision,
            "allow_auto_scoring_point_generation": False,
        },
    }
    return ScoringRequest.model_validate(payload)


def _legacy_review_status(result: ScoringResult) -> str:
    """映射到现有管理端使用的 review_status 字符串。"""
    if result.review_level is ReviewLevel.MANUAL_REQUIRED:
        return "low_confidence" if result.confidence < 0.5 else "need_review"
    if result.review_level is ReviewLevel.SUGGESTED_REVIEW:
        return "need_review"
    return "high_confidence"


def detail_from_scoring_result(
    question: dict[str, Any],
    student_answer: str,
    result: ScoringResult,
) -> dict[str, Any]:
    """ScoringResult → grading_detail 条目（兼容 admin / 导出）。"""
    max_score = float(question.get("score", 0) or 0)
    score = float(result.score)
    review_status = _legacy_review_status(result)
    low = review_status == "low_confidence" or result.need_manual_review and result.confidence < 0.5

    matched = [
        {
            "point_id": p.point_id,
            "score": p.score,
            "max_score": p.max_score,
            "similarity": p.similarity,
            "evidence": p.evidence,
            "reason": p.reason,
        }
        for p in result.matched_points
    ]
    missed = [
        {
            "point_id": p.point_id,
            "score": p.score,
            "max_score": p.max_score,
            "reason": p.reason,
        }
        for p in result.missed_points
    ]

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
        "grading_method": f"subjective_scoring:{result.track}",
        "similarity": round(float(result.confidence), 4),
        "confidence": round(float(result.confidence), 4),
        "reason": "; ".join(result.warnings) if result.warnings else None,
        "fallback_reason": None,
        "review_status": review_status,
        "low_confidence": low,
        "scoring_mode": result.scoring_mode.value if result.scoring_mode else None,
        "track": result.track,
        "need_manual_review": result.need_manual_review,
        "review_level": result.review_level.value if result.review_level else None,
        "matched_points": matched,
        "missed_points": missed,
        "warnings": list(result.warnings),
    }


# ---------------------------------------------------------------------------
# 主观题判分
# ---------------------------------------------------------------------------

async def _run_subjective_grading(
    question: dict[str, Any],
    student_answer: str,
) -> dict[str, Any]:
    """对单道主观题调用 SubjectiveScoringService，返回 grading_detail 条目。"""
    qid = question.get("id", "?")
    try:
        request = build_scoring_request(question, student_answer)
        service = get_subjective_service()
        result = service.score(request)
        if not isinstance(result, ScoringResult):
            # score_with_trace 包装
            result = result.result  # type: ignore[attr-defined]
        return detail_from_scoring_result(question, student_answer, result)
    except Exception:
        logger.exception("主观题评分失败 question_id=%s，回退 0 分待复核", qid)
        max_score = float(question.get("score", 0) or 0)
        return {
            "question_id": qid,
            "type": question.get("type"),
            "question": question.get("question", ""),
            "student_answer": student_answer,
            "reference_answer": question.get("answer", ""),
            "scoring_rubric": question.get("scoring_rubric"),
            "machine_score": 0.0,
            "final_score": 0.0,
            "max_score": max_score,
            "grading_method": "subjective_scoring:error",
            "similarity": 0.0,
            "confidence": 0.0,
            "reason": "主观题评分异常",
            "fallback_reason": "SubjectiveScoringService 异常",
            "review_status": "need_review",
            "low_confidence": True,
            "need_manual_review": True,
            "warnings": ["主观题评分异常，已记 0 分并标记复核"],
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


async def grade_submission(answers: dict[str, Any], paper_id: str | None = None) -> GradingResult:
    """完整判分入口，返回 GradingResult。须指定 paper_id。"""
    from .question_loader import load_questions

    data = load_questions(paper_id)
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
