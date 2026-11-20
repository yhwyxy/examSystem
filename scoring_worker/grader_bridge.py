"""Adapt the pinned subjective-scoring contract for grading, review and export."""
from __future__ import annotations

import json
import hashlib
from difflib import SequenceMatcher
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


def _sandbox_runner_options() -> dict[str, Any]:
    url = os.environ.get("SANDBOX_API_URL", "").strip()
    token = os.environ.get("SANDBOX_AUTH_TOKEN", "").strip()
    build = os.environ.get("SANDBOX_RUNNER_BUILD", "").strip()
    if not any((url, token, build)):
        return {}
    if not all((url, token, build)):
        raise RuntimeError("sandbox 配置不完整，需 URL、专用 token 和 runner build")
    try:
        from subjective_scoring.sandbox_transport import HTTPExecutionClient, RemoteCodeRunner, RemoteSQLRunner
    except ImportError as exc:
        raise RuntimeError("评分库版本尚不支持远程 sandbox，请完成版本升级") from exc
    try:
        client = HTTPExecutionClient(url=url, token=token, expected_build=build)
    except ValueError as exc:
        raise RuntimeError("sandbox 配置无效，需 HTTPS、专用长 token 和固定 build") from exc
    return {"code_execution_runner": RemoteCodeRunner(client),
            "sql_execution_runner": RemoteSQLRunner(client)}


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
                    **_sandbox_runner_options(),
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
    sandbox_options = _sandbox_runner_options()

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
                **sandbox_options,
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
            **sandbox_options,
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
        **sandbox_options,
    )


def get_subjective_service_for_config(scoring_cfg: dict | None) -> Any:
    """按 DB 评分配置构造服务 (不走单例; poll loop 检测到配置变化时重建)."""
    svc = build_scoring_service(scoring_cfg)
    # Version the configuration without persisting credentials or endpoints.
    cfg = scoring_cfg or {}
    public_config = {k: cfg.get(k) for k in ("method", "llm_model", "rerank_model", "llm_api_url", "rerank_api_url")}
    for env in ("RERANK_MODEL", "RERANKER_MODEL", "LLM_MODEL", "RERANK_USE_REMOTE"):
        public_config[env] = os.environ.get(env)
    public_config["bridge_contract_version"] = "p0-v1"
    public_config["sandbox_runner_build"] = os.environ.get("SANDBOX_RUNNER_BUILD", "")
    public_config["sandbox_endpoint_fingerprint"] = hashlib.sha256(
        os.environ.get("SANDBOX_API_URL", "").encode()).hexdigest()
    svc._exam_requested_model = cfg.get("llm_model") if cfg.get("method") == "llm" else cfg.get("rerank_model")
    svc._exam_config_version = hashlib.sha256(
        json.dumps(public_config, sort_keys=True).encode()).hexdigest()
    return svc


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
    2. 无可解析标准 -> 无评分点，由评分库综合评分或转人工复核。
    3. 分数总和不等于 max_score: 按比例缩放 (max_score 归一).
    4. 完全无 max_score + 无 rubric -> None (走 judge 没多评分点).
    """
    if not scoring_rubric:
        return None
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
        return None
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
    显式模式优先；缺少模式时使用评分库的统一路由规则。
    """
    from subjective_scoring import ScoringMode  # type: ignore
    qtype = str(question.get("type", "") or "")
    mode_raw = str(question.get("scoring_mode") or "").strip().lower()
    mode = ScoringMode(mode_raw) if mode_raw else None
    sp_list = _structured_scoring_points(question)
    if sp_list is None:
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
    request_kwargs: dict[str, Any] = {
        "question_id": str(question.get("id", "?")),
        "question_type": qtype or "subjective",
        "question": str(question.get("question", "")),
        "reference_answer": str(question.get("answer", "")),
        "sql_dialect": question.get("sql_dialect"),
        "sql_test_datasets": question.get("sql_test_datasets") or [],
        "sql_execution": question.get("sql_execution", {}),
        "code_test_cases": question.get("code_test_cases") or [],
        "student_answer": student_answer,
        "code_language": code_lang or None,
        "code_scoring_profile": question.get("code_scoring_profile") or None,
        "course_type": question.get("course_type"),
        "max_score": float(question.get("score", 0) or 0),
        "scoring_mode": mode,
        "scoring_points": sp_list,
        "scoring_config": _build_scoring_options(question),
    }
    request = _build_scoring_request_kwargs(request_kwargs)
    if mode is None:
        from subjective_scoring import QuestionTypeRouter
        request = request.model_copy(update={
            "scoring_mode": QuestionTypeRouter().resolve(request).mode,
        })
    return request


def _build_scoring_options(question: dict[str, Any]) -> Any | None:
    """题目带 calculation 配置或业务同义组时构造请求级 ScoringOptions。"""
    calc = question.get("calculation")
    eqs = question.get("extra_equivalences")
    if not isinstance(calc, dict) and not eqs and not question.get("scoring_config"):
        return None
    try:
        from subjective_scoring import ScoringOptions
    except ImportError:
        from subjective_scoring.models.schemas import ScoringOptions  # type: ignore
    payload: dict[str, Any] = dict(question.get("scoring_config") or {})
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
) -> list[Any] | None:
    """题目自带结构化 scoring_points ([{text, score, ...}]) 时直接采用.

    空列表 [] 也返回 [] — 代表「显式配置了无评分点」, 不走 rubric 兜底.
    试卷数据或快照中 `scoring_points: []` 就是要求题目走强制人工复核.
    """
    raw = question.get("scoring_points")
    if not isinstance(raw, list):
        if "scoring_points" in question:
            raise ValueError("scoring_points must be a list")
        return None
    if not raw:
        return []
    from subjective_scoring import ScoringPoint
    points = []
    for idx, p in enumerate(raw):
        # Legacy points may lack IDs; all supplied attributes are validated,
        # rather than silently discarding critical/conflict configuration.
        payload = dict(p)
        payload.setdefault("id", f"p{idx + 1}")
        points.append(ScoringPoint.model_validate(payload))
    if len({p.id for p in points}) != len(points):
        raise ValueError("duplicate scoring point ID")
    return points


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
                  "scoring_points", "scoring_config", "sql_dialect",
                  "sql_test_datasets", "sql_execution", "code_test_cases"}
    model_fields = getattr(ScoringRequest, "model_fields", None)
    if isinstance(model_fields, dict):
        valid_keys.intersection_update(model_fields)
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
        return "need_review"
    if val in ("low",):
        return "low_confidence"
    if val in ("manual", "manual_required"):
        return "need_review"
    return "low_confidence"


def _original_evidence(answer: str, evidence: str | None) -> tuple[str | None, int | None, int | None]:
    """Map a normalized text quote back to the original answer, with verification."""
    if not evidence:
        return evidence, None, None
    start = answer.find(evidence)
    if start >= 0:
        return evidence, start, start + len(evidence)
    from subjective_scoring.components.normalizer import TextNormalizer
    normalizer = TextNormalizer()
    normalized = normalizer.normalize(answer)
    quote = normalizer.normalize(evidence)
    start = normalized.find(quote)
    if start < 0 or not quote:
        return None, None, None
    end = start + len(quote)
    spans = []
    for tag, a0, a1, b0, b1 in SequenceMatcher(None, normalized, answer, autojunk=False).get_opcodes():
        if a1 <= start or a0 >= end:
            continue
        if tag == "equal":
            spans.append((b0 + max(start, a0) - a0, b0 + min(end, a1) - a0))
        else:
            spans.append((b0, b1))
    if not spans:
        return None, None, None
    original_start, original_end = spans[0][0], spans[-1][1]
    original = answer[original_start:original_end]
    if normalizer.normalize(original) != quote:
        return None, None, None
    return original, original_start, original_end


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
    trace = getattr(result, "trace", None)
    intermediate = getattr(trace, "intermediate", None)
    metadata = getattr(intermediate, "metadata", {}) or {}
    provenance = {k: metadata[k] for k in (
        "model", "parser", "judge_backend", "llm_error", "route_reason",
        "route_mode", "decision_reason", "calibrator", "requested_model",
        "execution_validation", "execution_review_reason", "assessment_type",
    ) if k in metadata}
    fallback_reason = None
    if metadata.get("judge_backend") == "llm_fallback":
        fallback_reason = "llm_failure:" + str(metadata.get("llm_error", "unknown"))
    elif metadata.get("model") == "lexical_fallback":
        fallback_reason = "lexical_fallback"
    elif metadata.get("error"):
        fallback_reason = "engine_error:" + str(metadata["error"])
    configured_points = (
        _structured_scoring_points(question) or _to_scoring_points(
            parse_scoring_rubric(question.get("scoring_rubric"), max_score))
    )
    descriptions = {p.id: p.text for p in configured_points}
    descriptions["__full_reference__"] = "整题综合评分"
    calculation = question.get("calculation") or (question.get("scoring_config") or {}).get("calculation") or {}
    for item in calculation.get("steps", []) + calculation.get("final_answers", []):
        descriptions[item.get("id")] = item.get("description") or item.get("id")
    descriptions.update({"sql.select":"查询字段", "sql.from":"来源表", "sql.where":"筛选条件",
                         "sql.join":"关联条件", "sql.group_by":"分组", "sql.having":"分组筛选", "sql.order_by":"排序", "sql.limit":"结果数量"})

    def point_detail(p: Any, status: str) -> dict[str, Any]:
        pid = getattr(p, "point_id", None)
        relation = getattr(p, "relation", None)
        evidence = getattr(p, "evidence", None)
        original, start, end = _original_evidence(student_answer, evidence)
        return {
            "point_id": pid,
            "description": descriptions.get(pid, "整题综合评分" if pid == "whole" else str(pid or "结构验证")),
            "status": status,
            "score": getattr(p, "score", 0.0),
            "max_score": getattr(p, "max_score", 0.0),
            "similarity": getattr(p, "similarity", None),
            "evidence": original if original is not None else evidence,
            "evidence_start": start,
            "evidence_end": end,
            "evidence_located": start is not None,
            "reason": getattr(p, "reason", None),
            "relation": relation.value if hasattr(relation, "value") else relation,
            "relation_confidence": getattr(p, "relation_confidence", None),
            "provisional_score": getattr(p, "provisional_score", None),
        }
    matched = [point_detail(p, "matched") for p in (getattr(result, "matched_points", None) or [])]
    missed = [point_detail(p, "missed") for p in (getattr(result, "missed_points", None) or [])]
    execution = metadata.get("execution_validation")
    if isinstance(execution, dict) and execution.get("status") == "verified":
        records = execution.get("datasets") if metadata.get("route_mode") == "sql" else execution.get("cases")
        if not isinstance(records, list):
            records = execution.get("datasets") or execution.get("cases") or []
        for point in matched + missed:
            match = re.fullmatch(r"(?:sql\.dataset|code\.test)\.(\d+)", str(point["point_id"]))
            if match and 0 < int(match.group(1)) <= len(records):
                record = records[int(match.group(1)) - 1]
                if isinstance(record, dict) and type(record.get("passed")) is bool:
                    point["validation"] = {key: record[key] for key in (
                        "name", "visibility", "passed", "reason", "expected_rows", "actual_rows"
                    ) if key in record}
                    point["evidence_kind"] = "execution_record"
    if score > 0 and not matched and not missed and metadata.get("deterministic"):
        matched = [{
            "point_id":"__full_reference__", "description":"整题参考答案匹配",
            "status":"matched", "score":score, "max_score":max_score,
            "evidence":student_answer, "evidence_start":0, "evidence_end":len(student_answer),
            "evidence_located":True, "relation":"supported", "relation_confidence":1.0,
            "reason":str(metadata.get("decision_reason") or "deterministic_reference_match"),
        }]
    # Guard the pinned library contract as well as newer library releases.
    # This keeps critical conflict policy effective before a library upgrade.
    guards = []
    if score > 0 and not matched and not missed:
        guards.append("missing_score_evidence")
    for item in matched:
        if (float(item["score"] or 0) > 0 and not item["evidence"]
                and not (item.get("validation", {}).get("passed") is True)):
            guards.append("missing_score_evidence")
    point_config = {p.id: p for p in configured_points}
    seen = set()
    for item in matched + missed:
        pid = item["point_id"]
        if pid in seen:
            guards.append("duplicate_scoring_result_point")
            score = 0.0
        seen.add(pid)
        point = point_config.get(pid)
        if item["relation"] in {"contradicted", "unknown"} and float(item["score"] or 0) > 0:
            score = max(0.0, score - float(item["score"]))
            item["score"] = 0.0
            guards.append("unsupported_point_score")
        if point and item["relation"] == "contradicted":
            guards.append("scoring_point_conflict")
            if point.critical and float(item["relation_confidence"] or 0) >= float(
                (question.get("scoring_config") or {}).get("text_relation_thresholds", {}).get("conflict", 0.8)
            ):
                policy = getattr(point.conflict_policy, "value", point.conflict_policy)
                if policy == "zero_total":
                    score = 0.0
                elif policy == "cap_total":
                    score = min(score, max_score * point.conflict_score_cap_ratio)
    smode = getattr(result, "scoring_mode", None)
    rvlevel = getattr(result, "review_level", None)
    # 开放题（scoring_points=[] 显式配置）覆盖 review_status
    if _is_open_ended_question(question):
        review_status = "open_ended"
    requires_review = bool(getattr(result, "need_manual_review", False)) or bool(fallback_reason) or bool(guards) or _is_open_ended_question(question)
    if requires_review and review_status == "high_confidence":
        review_status = "need_review"
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
        "fallback_reason": fallback_reason,
        "scoring_backend": getattr(intermediate, "scorer", None) or getattr(result, "track", None),
        "scoring_model": metadata.get("model"),
        "scoring_metadata": provenance,
        "review_status": review_status,
        "low_confidence": low,
        "scoring_mode": smode.value if smode and hasattr(smode, "value") else None,
        "track": getattr(result, "track", None),
        "need_manual_review": requires_review,
        "decision": "manual_review" if requires_review else getattr(getattr(result, "decision", None), "value", None),
        "decision_reason": guards[0] if guards else "missing_scoring_standard" if _is_open_ended_question(question) else fallback_reason or getattr(result, "decision_reason", None),
        "provisional_score": getattr(result, "provisional_score", None),
        "review_level": "manual_required" if requires_review else rvlevel.value if rvlevel and hasattr(rvlevel, "value") else None,
        "matched_points": matched,
        "missed_points": missed,
        "warnings": list(getattr(result, "warnings", []) or []) + guards,
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
    """Summarize unresolved review requirements, then human review coverage."""
    leaves = []
    def collect(entry):
        if not isinstance(entry, dict):
            leaves.append({"need_manual_review": True})
            return
        if entry.get("type") in {"single_choice", "multiple_choice", "true_false"}:
            return
        if entry.get("manually_reviewed") and entry.get("review_action") == "whole_question":
            leaves.append(entry)
            return
        subs = entry.get("sub_results")
        if isinstance(subs, list) and subs:
            for sub in subs:
                collect(sub)
        else:
            leaves.append(entry)
    for entry in entries:
        collect(entry)
    pending_statuses = {"low_confidence", "need_review", "unanswered", "open_ended", "pending", "grading"}
    for entry in leaves:
        if entry.get("manually_reviewed"):
            continue
        if (_entry_flag(entry, "low_confidence") or _entry_flag(entry, "need_manual_review")
                or entry.get("review_status") in pending_statuses):
            return "need_review"
    manual_count = sum(bool(e.get("manually_reviewed")) for e in leaves)
    if manual_count and manual_count == len(leaves):
        return "reviewed"
    if manual_count:
        return "partial_manual"
    return "auto_scored"


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
    service: Any | None = None,
    multiple_choice_partial: bool = True,
) -> tuple[float, float, str, dict[str, Any]]:
    """主观题打分入口 (grading.py 调用).

    返回 (final_score, machine_score, review_status, detail_entry).
    composite 题答案是 dict {sub_id: {"answer": str} | str}, 必须先于字符串
    强转分流 (曾把整个 dict 的 repr 串喂给每个子题评分).
    """
    if str(question.get("type")) == "composite":
        if _is_blank_answer(student_answer):
            entry = _empty_subjective_detail(question, preserve=preserve)
            return (float(entry["final_score"]), 0.0, entry["review_status"], entry)
        return _grade_composite(service, question, student_answer, preserve, multiple_choice_partial)

    selected_language = None
    if isinstance(student_answer, dict):
        selected_language = student_answer.get("language")
        student_answer = student_answer.get("answer", "")
    if selected_language:
        selected_language = str(selected_language).strip().lower()
        allowed = question.get("allowed_languages") or [question.get("code_language")]
        if selected_language not in allowed:
            raise ValueError("answer language is not allowed")
        question = dict(question, code_language=selected_language)
        for source, target in (("answers_by_language", "answer"),
                               ("scoring_points_by_language", "scoring_points"),
                               ("scoring_config_by_language", "scoring_config"),
                               ("code_scoring_profile_by_language", "code_scoring_profile")):
            values = question.get(source)
            if isinstance(values, dict):
                if selected_language not in values:
                    raise ValueError(f"missing {source} for selected language")
                question[target] = values[selected_language]

    if not isinstance(student_answer, str):
        student_answer = "" if student_answer is None else str(student_answer)
    if student_answer.strip() == "":
        entry = _empty_subjective_detail(question, preserve=preserve)
        return (float(entry["final_score"]), 0.0, entry["review_status"], entry)

    # 精确评分模式分派 — 命中则直接返回，不经过 text reranker
    scoring_mode = question.get("scoring_mode") or ""
    if scoring_mode:
        result = _grade_by_exact_mode(scoring_mode, question, student_answer, preserve)
        if result is not None:
            return result

    ssvc = service if service is not None else get_subjective_service()
    request = build_scoring_request(question, student_answer)
    if hasattr(ssvc, "score_with_trace"):
        result = ssvc.score_with_trace(request)
    else:
        result = ssvc.score(request)
    result = _require_execution_validation_for_sql_and_code(result, request)
    entry = detail_from_scoring_result(question, student_answer,
                                       result, preserve=preserve)
    entry["selected_language"] = selected_language or request.code_language
    entry["requested_model"] = getattr(ssvc, "_exam_requested_model", None)
    config_payload = {
        "options": request.scoring_config.model_dump(mode="json"),
        "points": [p.model_dump(mode="json") for p in request.scoring_points],
        "mode": request.scoring_mode.value,
        "service": getattr(ssvc, "_exam_config_version", "environment"),
    }
    config_payload["reference_answer"] = request.reference_answer
    config_payload["code_language"] = request.code_language
    config_payload["code_scoring_profile"] = request.code_scoring_profile
    config_payload["sql_dialect"] = getattr(request, "sql_dialect", None)
    config_payload["sql_test_datasets"] = getattr(request, "sql_test_datasets", [])
    config_payload["sql_execution"] = getattr(request, "sql_execution", {})
    config_payload["code_test_cases"] = getattr(request, "code_test_cases", [])
    entry["scoring_config_version"] = hashlib.sha256(
        json.dumps(config_payload, sort_keys=True, ensure_ascii=False).encode()).hexdigest()
    return (
        round(float(entry["final_score"]), 6),
        round(float(entry["machine_score"]), 6),
        entry["review_status"],
        entry,
    )


def _require_execution_validation_for_sql_and_code(result: Any, request: Any) -> Any:
    """旧版锁定评分库缺少执行器时，也不能把静态估分当成已验证结果。"""
    mode = getattr(getattr(request, "scoring_mode", None), "value", None)
    if mode not in {"sql", "code"}:
        return result
    trace = getattr(result, "trace", None)
    intermediate = getattr(trace, "intermediate", None)
    metadata = getattr(intermediate, "metadata", None) if intermediate is not None else getattr(result, "metadata", None)
    metadata = dict(metadata) if isinstance(metadata, dict) else {}
    execution = metadata.get("execution_validation")
    if isinstance(execution, dict) and execution.get("status") == "verified":
        total, passed = execution.get("total"), execution.get("passed")
        records = execution.get("datasets" if mode == "sql" else "cases")
        expected = getattr(request, "sql_test_datasets" if mode == "sql" else "code_test_cases", [])
        if (type(total) is int and total > 0 and type(passed) is int and passed == total
                and isinstance(records, list) and len(records) == total
                and all(isinstance(record, dict) and record.get("passed") is True for record in records)
                and isinstance(expected, list) and len(expected) == total):
            return result
        reason = "execution_tests_incomplete"
    else:
        reason = "execution_validation_unavailable"
    warnings = list(getattr(result, "warnings", []) or [])
    warning = "SQL/代码缺少完整的隔离执行验证，机器分仅作参考并需人工复核"
    if warning not in warnings:
        warnings.append(warning)
    metadata["execution_review_reason"] = reason
    if trace is not None and intermediate is not None:
        from subjective_scoring import ReviewLevel, ScoringDecision
        trace.intermediate = intermediate.model_copy(update={
            "force_manual_review": True, "warnings": warnings, "metadata": metadata,
        })
        result.result = result.result.model_copy(update={
            "need_manual_review": True, "review_level": ReviewLevel.MANUAL_REQUIRED,
            "decision": ScoringDecision.MANUAL_REVIEW, "decision_reason": reason, "warnings": warnings,
        })
        return result
    if hasattr(result, "model_copy"):
        fields = getattr(type(result), "model_fields", {})
        return result.model_copy(update={
            "need_manual_review" if "need_manual_review" in fields else "force_manual_review": True,
            "warnings": warnings, "metadata": metadata,
        })
    result.force_manual_review = True
    result.warnings = warnings
    result.metadata = metadata
    return result


def _grade_composite(
    ssvc: Any, question: dict[str, Any], student_answer: Any,
    preserve: dict[str, Any] | None,
    multiple_choice_partial: bool = True,
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
        if sub.get("type") in {"single_choice", "multiple_choice", "true_false"}:
            from .grading import _grade_objective
            e = _grade_objective(sub, raw.get("answer") if isinstance(raw, dict) else raw,
                                 partial=multiple_choice_partial)
            e = _merge_preserved_review(e, sub_preserve.get(sub_id))
            f, m, r = e["final_score"], e["machine_score"], e["review_status"]
        else:
            f, m, r, e = grade_subjective(sub, raw,
                                          preserve=sub_preserve.get(sub_id), service=ssvc,
                                          multiple_choice_partial=multiple_choice_partial)
        final_total += f
        machine_total += m
        sub_results.append({
            "sub_question_id": sub_id,
            "question_id": sub_id,
            "type": sub.get("type"),
            "question": sub.get("question", ""),
            "student_answer": e.get("student_answer", raw),
            "reference_answer": e.get("reference_answer", sub.get("answer", "")),
            "selected_language": e.get("selected_language"),
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
        "manually_reviewed": bool(sub_results) and all(s["manually_reviewed"] for s in sub_results),
        "confidence": round(min(
            (float(s.get("detail", {}).get("confidence", 0.0) or 0.0)
             for s in sub_results), default=0.0), 4),
        "grading_method": "subjective",
        "reason": "; ".join(reasons) if reasons else None,
    }
    overall = aggregate_review_status(sub_results)
    if (preserve or {}).get("review_action") == "whole_question":
        entry = _merge_preserved_review(entry, preserve)
        final_total = float(entry["final_score"])
        overall = "reviewed"
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
        return "need_review"
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
