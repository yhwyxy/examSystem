"""主观题 Embedding/关键词回退判分。
"""
from __future__ import annotations

import logging
import math
import re
from collections import Counter
from functools import lru_cache
from typing import Any

import httpx

from .config import get_config

logger = logging.getLogger(__name__)


def _cosine(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    return dot / (na * nb) if na and nb else 0.0


@lru_cache(maxsize=1)
def _load_model():
    cfg = get_config().grading.embedding
    try:
        from sentence_transformers import SentenceTransformer  # type: ignore
        return SentenceTransformer(cfg.model, device=cfg.device)
    except Exception as e:
        logger.warning("Embedding 模型不可用，降级关键词相似度: %s", e)
        return None


def _keyword_tokens(text: str) -> list[str]:
    # 对英文按单词，对中文按连续汉字片段；MVP 场景足够作为兜底。
    return re.findall(r"[A-Za-z0-9_]+|[\u4e00-\u9fa5]+", (text or "").lower())


def _keyword_similarity(a: str, b: str) -> float:
    ta = _keyword_tokens(a)
    tb = _keyword_tokens(b)
    if not ta or not tb:
        return 0.0
    ca = Counter(ta)
    cb = Counter(tb)
    keys = sorted(set(ca) | set(cb))
    va = [float(ca[k]) for k in keys]
    vb = [float(cb[k]) for k in keys]
    return _cosine(va, vb)


def _extract_embedding(payload: dict[str, Any]) -> list[float]:
    """兼容 Ollama /api/embeddings 与新版 /api/embed 的返回结构。"""
    emb = payload.get("embedding")
    if isinstance(emb, list):
        return [float(x) for x in emb]

    embeddings = payload.get("embeddings")
    if isinstance(embeddings, list) and embeddings:
        first = embeddings[0]
        if isinstance(first, list):
            return [float(x) for x in first]

    raise ValueError("Ollama embedding 响应中没有 embedding 向量")


def _ollama_embedding(text: str) -> list[float]:
    cfg = get_config()
    endpoint = cfg.grading.llm.endpoint.rstrip("/")
    url = f"{endpoint}/api/embeddings"
    payload = {"model": cfg.grading.embedding.model, "prompt": text or ""}
    with httpx.Client(timeout=cfg.grading.llm.timeout_seconds, trust_env=False) as client:
        resp = client.post(url, json=payload)
        resp.raise_for_status()
        return _extract_embedding(resp.json())


def _ollama_similarity(student_answer: str, reference_answer: str) -> float:
    a = _ollama_embedding(student_answer)
    b = _ollama_embedding(reference_answer)
    return _cosine(a, b)


def similarity(student_answer: str, reference_answer: str) -> tuple[float, str]:
    try:
        sim = _ollama_similarity(student_answer or "", reference_answer or "")
        return max(0.0, min(1.0, float(sim))), "ollama_embedding"
    except Exception as e:
        logger.warning("Ollama Embedding 不可用，尝试 sentence-transformers: %s", e)

    model = _load_model()
    if model is not None:
        try:
            emb = model.encode([student_answer or "", reference_answer or ""], normalize_embeddings=True)
            a = emb[0].tolist() if hasattr(emb[0], "tolist") else list(emb[0])
            b = emb[1].tolist() if hasattr(emb[1], "tolist") else list(emb[1])
            return max(0.0, min(1.0, float(_cosine(a, b)))), "embedding"
        except Exception as e:
            logger.warning("Embedding 相似度计算失败，降级关键词相似度: %s", e)
    return max(0.0, min(1.0, _keyword_similarity(student_answer, reference_answer))), "keyword_fallback"


def review_status_by_confidence(confidence: float) -> str:
    cfg = get_config().review
    # 与 grader._review_status_from_confidence 保持一致的三阈值四段语义
    if confidence >= cfg.high_confidence_threshold:
        return "auto_scored"
    if confidence >= cfg.need_review_threshold:
        return "need_review"
    return "low_confidence"


def grade_with_embedding(question: dict[str, Any], student_answer: str, fallback_reason: str | None = None) -> dict[str, Any]:
    cfg = get_config()
    max_score = float(question["score"])
    sim, method = similarity(student_answer or "", question.get("answer", ""))
    score = round(sim * max_score, cfg.scoring.score_precision)
    status = review_status_by_confidence(sim)
    return {
        "machine_score": float(score),
        "final_score": float(score),
        "max_score": max_score,
        "grading_method": method,
        "similarity": round(sim, 4),
        "confidence": round(sim, 4),
        "reason": f"语义相似度 {sim:.3f}，按比例给分",
        "llm_raw_output": None,
        "fallback_reason": fallback_reason or ("embedding_fallback" if method == "embedding" else "keyword_fallback"),
        "review_status": status,
        "manually_reviewed": False,
    }
