"""主观题打分 bridge: subjective-scoring 0.1.7 适配 (plan Task 7 Step 4 完整版).

不 import root.backend (plan 边界); 但与 backend/grader.py 评分结果保持
结构 parity (细节字段对齐 admin 读取 / XLSX 导出).

依赖 subjective-scoring git tag v0.1.7 @ yhwyxy/subjective-scoring:
    SubjectiveScoringService / ScoringRequest / ScoringResult / ScoringMode /
    ReviewLevel / CohereRerankerPairScorer
"""
from __future__ import annotations

import json
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
        rc = validate_remote_reranker_config()
        if rc is not None:
            from subjective_scoring import (  # type: ignore
                CohereRerankerPairScorer, SubjectiveScoringService,
            )
            reranker = CohereRerankerPairScorer(
                url=rc["RERANK_API_URL"], api_key=rc["RERANK_API_KEY"],
                model=rc["RERANK_MODEL"],
            )
            try:
                svc = SubjectiveScoringService(
                    allow_model_load=False,
                    text_pair_scorer=reranker, code_pair_scorer=reranker,
                )
            except Exception:
                reranker.close()
                raise
            _remote_reranker = reranker
            _service_singleton = svc
            return svc
        svc = build_scoring_service(None)  # env 默认 (local)
        _service_singleton = svc
        return svc


def build_scoring_service(scoring_cfg: dict | None = None) -> Any:
    """按评分配置构造评分服务 (DB app_settings['scoring'] / 环境变量).

    scoring_cfg: {"method": "local"|"remote_reranker"|"llm", 及各 API 字段}.
      字段缺省时回退对应环境变量 (RERANK_API_URL/KEY/MODEL, LLM_API_URL/KEY/MODEL,
      RERANKER_MODEL); scoring_cfg=None 完全走环境变量 (原 get_subjective_service 语义).

    - method=remote_reranker: Cohere 兼容远程 reranker (需 url/api_key/model).
    - method=llm:            大模型判分 (OpenAI 兼容 chat/completions), 失败自动
                             回退经典引擎 (judge_fallback=True).
    - 其余:                  本地语义模型 (RERANKER_MODEL / 默认 bge).
    """
    from subjective_scoring import (  # type: ignore
        CohereRerankerPairScorer, LLMJudgeConfig, SubjectiveScoringService,
    )
    cfg = dict(scoring_cfg or {})
    method = str(cfg.get("method") or "local").strip().lower()

    if method == "remote_reranker":
        url = str(cfg.get("rerank_api_url") or os.environ.get("RERANK_API_URL", "")).strip()
        key = str(cfg.get("rerank_api_key") or os.environ.get("RERANK_API_KEY", "")).strip()
        model = str(cfg.get("rerank_model") or os.environ.get("RERANK_MODEL", "")).strip()
        missing = [n for n, v in (("url", url), ("api_key", key), ("model", model)) if not v]
        if missing:
            raise RuntimeError("远程 reranker 配置不完整, 缺: " + ", ".join(missing))
        reranker = CohereRerankerPairScorer(url=url, api_key=key, model=model)
        try:
            return SubjectiveScoringService(
                allow_model_load=False,
                text_pair_scorer=reranker, code_pair_scorer=reranker,
            )
        except Exception:
            reranker.close()
            raise

    if method == "llm":
        url = str(cfg.get("llm_api_url") or os.environ.get("LLM_API_URL", "")).strip()
        key = str(cfg.get("llm_api_key") or os.environ.get("LLM_API_KEY", "")).strip()
        model = str(cfg.get("llm_model") or os.environ.get("LLM_MODEL", "")).strip()
        missing = [n for n, v in (("url", url), ("api_key", key), ("model", model)) if not v]
        if missing:
            raise RuntimeError("大模型 API 配置不完整, 缺: " + ", ".join(missing))
        judge = LLMJudgeConfig(url=url, api_key=key, model=model)
        return SubjectiveScoringService(
            allow_model_load=False, llm_judge=judge, judge_fallback=True,
        )

    # local (默认): 本地语义模型
    model_name = str(cfg.get("rerank_model") or os.environ.get("RERANKER_MODEL", "")).strip()
    if model_name:
        model_path = Path(model_name)
        if not model_path.is_absolute():
            model_path = (_project_root / model_name).resolve()
        if model_path.exists():
            model_name = str(model_path)
    return SubjectiveScoringService(
        allow_model_load=True,
        text_model=model_name or None, code_model=model_name or None,
    )


def get_subjective_service_for_config(scoring_cfg: dict | None) -> Any:
    """按 DB 评分配置构造服务 (不走单例; poll loop 检测到配置变化时重建)."""
    return build_scoring_service(scoring_cfg)


def _close_service_scorers(svc: Any) -> None:
    """关闭服务持有的远程客户端 (LLM judge httpx / reranker), 切换方式时释放."""
    try:
        for scorer in (getattr(svc, "_scorers", {}) or {}).values():
            close = getattr(scorer, "close", None)
            if close is None:
                continue
            try:
                close()
            except Exception:
                logger.debug("scorer close failed", exc_info=True)
    except Exception:
        logger.debug("close scorers failed", exc_info=True)


def reset_service() -> None:
    """清除服务单例 (评分方式切换时由 poll loop 调用), 释放远程连接."""
    global _remote_reranker, _service_singleton
    if _service_singleton is not None:
        _close_service_scorers(_service_singleton)
    if _remote_reranker is not None:
        try:
            _remote_reranker.close()
        except Exception:
            logger.exception("remote reranker close")
    _remote_reranker = None
    _service_singleton = None


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


def _to_scoring_points(points: list[tuple[str, float]] | None) -> list[Any]:
    """解析出的 (desc, score) tuple 列表 -> subjective_scoring.ScoringPoint 列表.

    ScoringPoint 字段 (0.1.7): id/description/max_score.
    """
    if not points:
        return []
    from subjective_scoring import ScoringPoint  # type: ignore
    out = []
    for idx, (desc, sc) in enumerate(points):
        out.append(ScoringPoint(
            id=f"p{idx + 1}",
            text=str(desc),
            score=float(sc),
        ))
    return out


def build_scoring_request(question: dict[str, Any], student_answer: str) -> Any:
    """question --> ScoringRequest (subjective_scoring 0.1.7 API).

    字段对齐 ScoringRequest 真实 schema (preflight 2026-07-26 校验):
        question (非 question_text) / question_type / code_language /
        scoring_points: list[ScoringPoint] (非 tuple) / scoring_config.
    scoring_mode 映射: text->TEXT, code/sql/code_completion->CODE.
    """
    from subjective_scoring import ScoringMode  # type: ignore
    qtype = str(question.get("type", "") or "")
    mode_raw = str(question.get("scoring_mode") or "").strip().lower()
    has_lang = bool(question.get("allowed_languages")
                    or question.get("code_language"))
    if mode_raw == "code" or (mode_raw == "" and has_lang):
        mode = ScoringMode.CODE
    elif mode_raw == "calculation" and isinstance(question.get("calculation"), dict):
        mode = ScoringMode.CALCULATION
    else:
        mode = ScoringMode.TEXT
    scoring_points = _structured_scoring_points(question)
    if scoring_points is None:
        # 没有显式评分点时才走 rubric 解析；评分点已配为空数组则不进入
        scoring_points = parse_scoring_rubric(
            question.get("scoring_rubric"),
            question.get("score"),
        )
    sp_list = _to_scoring_points(scoring_points)
    code_lang = question.get("code_language")
    if not code_lang:
        allowed = question.get("allowed_languages")
        if isinstance(allowed, list) and allowed:
            code_lang = allowed[0]
    # 语言随评分模式走: scoring_mode=code 的题不论 type (short_answer/essay
    # 也可能是代码题) 都必须透传, 否则引擎默认按 python 解析 -> AST 全灭.
    pass_lang = mode == ScoringMode.CODE or qtype in (
        "sql", "sql_completion", "code_completion", "code")
    request_kwargs: dict[str, Any] = {
        "question_id": str(question.get("id", "?")),
        "question_type": qtype or "subjective",
        "question": str(question.get("question", "")),
        "reference_answer": str(question.get("answer", "")),
        "student_answer": student_answer,
        "code_language": code_lang if pass_lang else None,
        "code_scoring_profile": question.get("code_scoring_profile") or None,
        "course_type": question.get("course_type"),
        "max_score": float(question.get("score", 0) or 0),
        "scoring_mode": mode,
        "scoring_points": sp_list,
        "scoring_config": _build_scoring_options(question),
    }
    return _build_scoring_request_kwargs(request_kwargs)


def _build_scoring_options(question: dict[str, Any]) -> Any | None:
    """题目带 calculation 配置或业务同义组时构造请求级 ScoringOptions。"""
    calc = question.get("calculation")
    eqs = question.get("extra_equivalences")
    if not isinstance(calc, dict) and not eqs:
        return None
    try:
        from subjective_scoring import ScoringOptions
    except ImportError:
        from subjective_scoring.models.schemas import ScoringOptions  # type: ignore
    payload: dict[str, Any] = {}
    if isinstance(calc, dict):
        payload["calculation"] = calc
    if isinstance(eqs, list) and eqs:
        payload["text_bounded_corrections"] = {
            "extra_equivalences": [
                tuple(str(x) for x in g)
                for g in eqs
                if isinstance(g, list) and len(g) >= 2
            ],
        }
    return ScoringOptions.model_validate(payload)


def _structured_scoring_points(
    question: dict[str, Any],
) -> list[tuple[str, float]] | None:
    """题目自带结构化 scoring_points ([{text, score, ...}]) 时直接采用.

    空列表 [] 也返回 [] — 代表「显式配置了无评分点」, 不走 rubric 兜底.
    试卷数据或快照中 `scoring_points: []` 就是要求题目走强制人工复核.
    """
    raw = question.get("scoring_points")
    if not isinstance(raw, list):
        return None
    if not raw:
        return []
    points: list[tuple[str, float]] = []
    for p in raw:
        if not isinstance(p, dict):
            continue
        text = str(p.get("text") or "").strip()
        try:
            w = float(p.get("score", 0) or 0)
        except (TypeError, ValueError):
            continue
        if text and w > 0:
            points.append((text, w))
    return points or None


def _build_scoring_request_kwargs(kwargs: dict[str, Any]) -> Any:
    """调 SubjectiveScoringService.score(request) 实际 ScoringRequest 构造.

    Plan 行 1113: 一种兼容构造路径是与 subjective_scoring 0.1.7 parity. 跳过
    keyword-only 严格区分, 转发到 ScoringRequest(**kwargs), 跳过 None 字段.
    """
    from subjective_scoring import ScoringRequest  # type: ignore
    valid_keys = {"question_id", "question_type", "question",
                  "reference_answer", "student_answer", "code_language",
                  "code_scoring_profile",
                  "course_type", "max_score", "scoring_mode",
                  "scoring_points", "scoring_config"}
    cleaned = {k: v for k, v in kwargs.items()
               if k in valid_keys and v is not None}
    return ScoringRequest(**cleaned)


def _is_open_ended_question(question: dict[str, Any]) -> bool:
    """scoring_points 显式配置为空列表 → 开放题，不走机器评分，强制人工。

    与 _structured_scoring_points 的返回约定保持一致：
    - scoring_points=[] 代表「显式配置了无评分点」
    - 区别于 scoring_points 字段缺失（走 rubric 兜底）
    """
    sp = question.get("scoring_points")
    return isinstance(sp, list) and not sp


def _legacy_review_status(result: Any) -> str:
    review = getattr(result, "review_level", None)
    if review is None:
        return "low_confidence"
    val = review.value if hasattr(review, "value") else str(review)
    val = val.lower()
    if val in ("high", "auto_pass"):
        return "high_confidence"
    if val in ("medium", "suggested_review"):
        return "reviewed"
    if val in ("low",):
        return "low_confidence"
    if val in ("manual", "manual_required"):
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
    # 开放题（scoring_points=[] 显式配置）覆盖 review_status
    if _is_open_ended_question(question):
        review_status = "open_ended"
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
    entry.setdefault("manually_reviewed", False)
    if not preserve:
        return entry
    # 仅人工复核过的旧条目才回填 (final_score 拒绝被 regrade 的 machine_score
    # 覆盖); 非人工旧条目一律采用新机器结果 (旧实现无条件回填旧值, regrade 形同虚设).
    if not preserve.get("manually_reviewed"):
        return entry
    for f in ("final_score", "review_status", "reviewer_id",
              "reviewed_at", "manually_reviewed",
              "review_comment", "review_action"):
        v = preserve.get(f)
        if v is not None:
            entry[f] = v
    return entry


def _entry_flag(e: dict[str, Any], key: str) -> bool:
    """条目旗标兼容读: 顶层优先, 缺则看嵌套 detail (worker 旧格式只写 detail 层,
    曾致 aggregate 全部漏检 -> 整卷 need_review 被误标 'reviewed')."""
    if e.get(key):
        return True
    d = e.get("detail")
    return bool(isinstance(d, dict) and d.get(key))


def aggregate_review_status(entries: list[dict[str, Any]]) -> str:
    """整卷级别 review_status plass规则:

    1. low_confidence 标志任一 -> 'need_review' (Worker 上抛人工)
    2. 任一 manually_reviewed=True -> 'partial_manual'
    3. 任一 need_manual_review=True -> 'need_review'
    4. open_ended 标记的题 -> 整卷 'need_review'（开放题需人工审核）
    5. 否则根据 machine_score 与 max 比例 -> high_confidence/reviewed 链.

    与 backend/grader.py parity 名字. 旗标读取兼容顶层 / detail 嵌套层, 并把
    单题 review_status 字符串 (low_confidence/need_review/unanswered/open_ended)
    一并算作需人工信号.
    """
    if not entries:
        return "need_review"
    score = 0.0
    max_score = 0.0
    found_low = False
    found_manual = False
    found_need_manual = False
    found_unanswered = False
    found_open_ended = False

    for e in entries:
        s = float(e.get("machine_score", 0) or 0)
        m = float(e.get("max_score", 0) or 0)
        score += s
        max_score += m
        rs = str(e.get("review_status") or "")
        if _entry_flag(e, "low_confidence") or rs == "low_confidence":
            found_low = True
        if e.get("manually_reviewed"):
            found_manual = True
        if _entry_flag(e, "need_manual_review") or rs == "need_review":
            found_need_manual = True
        if rs == "unanswered":
            found_unanswered = True
        if rs == "open_ended":
            found_open_ended = True

    if found_unanswered or found_need_manual or found_low or found_open_ended:
        return "need_review"
    if found_manual:
        return "partial_manual"
    if max_score <= 0:
        return "reviewed"
    ratio = score / max_score
    if ratio >= 0.8:
        return "high_confidence"
    return "reviewed"


def _is_blank_answer(student_answer: Any) -> bool:
    """空答案判定: None / 空串 / 空 dict / 空 list."""
    if student_answer is None:
        return True
    if isinstance(student_answer, str):
        return student_answer.strip() == ""
    if isinstance(student_answer, (dict, list)):
        return len(student_answer) == 0
    return False


def grade_subjective(
    question: dict[str, Any], student_answer: Any, *,
    preserve: dict[str, Any] | None = None,
) -> tuple[float, float, str, dict[str, Any]]:
    """主观题打分入口 (grading.py 调用).

    返回 (final_score, machine_score, review_status, detail_entry).
    composite 题答案是 dict {sub_id: {"answer": str} | str}, 必须先于字符串
    强转分流 (曾把整个 dict 的 repr 串喂给每个子题评分).
    """
    if str(question.get("type")) == "composite":
        if _is_blank_answer(student_answer):
            entry = _empty_subjective_detail(question, preserve=preserve)
            return (0.0, 0.0, entry["review_status"], entry)
        ssvc = get_subjective_service()
        return _grade_composite(ssvc, question, student_answer, preserve)

    if not isinstance(student_answer, str):
        student_answer = "" if student_answer is None else str(student_answer)
    if student_answer.strip() == "":
        entry = _empty_subjective_detail(question, preserve=preserve)
        return (0.0, 0.0, entry["review_status"], entry)

    # 精确评分模式分派 — 命中则直接返回，不经过 text reranker
    scoring_mode = question.get("scoring_mode") or ""
    if scoring_mode:
        result = _grade_by_exact_mode(scoring_mode, question, student_answer, preserve)
        if result is not None:
            return result

    ssvc = get_subjective_service()
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
    ssvc: Any, question: dict[str, Any], student_answer: Any,
    preserve: dict[str, Any] | None,
) -> tuple[float, float, str, dict[str, Any]]:
    """Composite 题: 沿 subquestions 各自打分; final = sum(sub_final).

    student_answer: dict {sub_id: {"answer": str} | str} 或其 JSON 字符串.
    条目输出 sub_results (前端 detail.js / Go review 契约: sub_question_id +
    score/final_score/review_status), 需人工旗标聚合到题级.
    """
    subs = (question.get("subquestions") or question.get("sub_questions")
            or question.get("sub_qs") or [])
    ans_map = student_answer
    if isinstance(ans_map, str):
        try:
            ans_map = json.loads(ans_map)
        except ValueError:
            ans_map = {}
    if not isinstance(ans_map, dict):
        ans_map = {}
    # 子题级 preserve: 旧条目 sub_results[] 中人工复核过的子项按 sub_id 建索引.
    sub_preserve: dict[str, dict] = {}
    for s in ((preserve or {}).get("sub_results") or []):
        if isinstance(s, dict) and s.get("manually_reviewed"):
            key = s.get("sub_question_id") or s.get("question_id") or s.get("id")
            if key:
                sub_preserve[str(key)] = s

    final_total = 0.0
    machine_total = 0.0
    sub_results: list[dict] = []
    for sub in subs:
        sub_id = str(sub.get("id"))
        raw = ans_map.get(sub_id, "")
        if isinstance(raw, dict):
            raw = raw.get("answer", "")
        f, m, r, e = grade_subjective(sub, raw,
                                      preserve=sub_preserve.get(sub_id))
        final_total += f
        machine_total += m
        sub_results.append({
            "sub_question_id": sub_id,
            "question_id": sub_id,
            "type": sub.get("type"),
            "question": sub.get("question", ""),
            "student_answer": raw,
            "reference_answer": sub.get("answer", ""),
            "max_score": float(sub.get("score", 0) or 0),
            "score": m,
            "machine_score": m,
            "final_score": f,
            "review_status": e.get("review_status", r),
            "manually_reviewed": bool(e.get("manually_reviewed", False)),
            "low_confidence": bool(e.get("low_confidence", False)),
            "need_manual_review": bool(e.get("need_manual_review", False)),
            "reason": e.get("reason"),
            "detail": e,
        })
    reasons = [s["reason"] for s in sub_results if s.get("reason")]
    entry = {
        "sub_results": sub_results,
        "machine_score": round(machine_total, 6),
        "final_score": round(final_total, 6),
        "low_confidence": any(s["low_confidence"] for s in sub_results),
        "need_manual_review": any(s["need_manual_review"] for s in sub_results),
        "manually_reviewed": any(s["manually_reviewed"] for s in sub_results),
        "confidence": round(min(
            (float(s.get("detail", {}).get("confidence", 0.0) or 0.0)
             for s in sub_results), default=0.0), 4),
        "grading_method": "subjective",
        "reason": "; ".join(reasons) if reasons else None,
    }
    overall = aggregate_review_status(sub_results)
    return (round(final_total, 6), round(machine_total, 6), overall, entry)


_EXACT_SCORER_MAP: dict[str, str] = {
    "enumeration": "score_enumeration",
    "translation": "score_translation",
    "ledger": "score_ledger",
    "table": "score_table",
    "case_analysis": "score_case_analysis",
}


def _grade_by_exact_mode(
    mode: str, question: dict, student_answer: str,
    preserve: dict | None = None,
) -> tuple[float, float, str, dict] | None:
    """精确评分模式分派；返回 None 表示走 text reranker 兜底。"""
    fn_name = _EXACT_SCORER_MAP.get(mode)
    if not fn_name:
        return None
    if fn_name == "score_case_analysis":
        from .case_analysis_scorer import score_case_analysis as fn
    else:
        from . import exact_scorers
        fn = getattr(exact_scorers, fn_name)
    result = fn(question, student_answer)
    entry = _exact_result_to_detail(question, student_answer, result, preserve)
    return (round(float(entry["final_score"]), 6),
            round(float(entry["machine_score"]), 6),
            entry["review_status"], entry)


def _exact_result_to_detail(
    question: dict, student_answer: str, result, preserve: dict | None = None,
) -> dict:
    """ExactScoreResult -> 与 detail_from_scoring_result 同结构的 entry。"""
    max_score = float(question.get("score", 0) or 0)
    machine = max(0.0, min(max_score, float(result.score)))
    manual = bool((preserve or {}).get("manually_reviewed"))
    final = float((preserve or {}).get("final_score", machine)) if manual else machine
    qid = question.get("id")
    review_status = _exact_review_status(result.review_level)
    need_manual = result.review_level == "manual_required"
    confidence = 1.0 if result.review_level == "auto_pass" else 0.5
    inner = {
        "track": f"ExactScorer({question.get('scoring_mode')})",
        "reason": result.detail.get("reason"),
        "warnings": result.detail.get("warnings", []),
        "max_score": max_score, "confidence": confidence,
        "final_score": machine, "machine_score": machine,
        "question_id": qid, "review_level": result.review_level,
        "scoring_mode": question.get("scoring_mode"),
        "matched_points": result.detail.get("matched_points", []),
        "missed_points": result.detail.get("missed_points", []),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "manually_reviewed": manual, "need_manual_review": need_manual,
    }
    return {
        "id": qid, "question_id": qid, "type": question.get("type"),
        "question": question.get("question", ""),
        "reference_answer": question.get("answer", ""),
        "student_answer": student_answer,
        "max_score": max_score,
        "machine_score": machine, "score": machine, "final_score": final,
        "confidence": confidence,
        "grading_method": f"exact:{question.get('scoring_mode')}",
        "reason": result.detail.get("reason"),
        "review_status": review_status,
        "manually_reviewed": manual,
        "low_confidence": need_manual,
        "need_manual_review": need_manual,
        "lowest_confidence_flagged": need_manual,
        "detail": inner,
    }


def _exact_review_status(level: str) -> str:
    """与 _legacy_review_status 同语义：级别 -> 单题 review_status。"""
    if level == "auto_pass":
        return "high_confidence"
    if level == "suggested_review":
        return "reviewed"
    if level == "manual_required":
        return "need_review"
    return "low_confidence"


def _empty_subjective_detail(question: dict[str, Any],
                             *, preserve: dict[str, Any] | None = None) -> dict[str, Any]:
    is_open = _is_open_ended_question(question)
    reason = "open_ended" if is_open else "unanswered"
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
        "reason": reason,
        "review_status": reason,
        "matched_points": [], "missed_points": [], "warnings": [reason],
        "low_confidence": False, "need_manual_review": is_open,
        "manually_reviewed": False,
    }
    return _merge_preserved_review(entry, preserve)


def close_service() -> None:
    """Worker exit 时释放评分服务 (remote reranker / LLM client)."""
    global _remote_reranker, _service_singleton
    reset_service()
    _remote_reranker = None
    _service_singleton = None
