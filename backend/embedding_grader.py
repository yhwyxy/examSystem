"""基于 Embedding 的判分。

同时支持 Ollama 远程 embedding 和 sentence-transformers 本地 embedding。
优先使用 Ollama（endpoint 配置非空时），本地模型作为回退。
"""
from __future__ import annotations

import json
import logging
from functools import lru_cache
from typing import Any

import httpx

from .config import get_config
from .utils import keyword_similarity, review_status_by_confidence, safe_score, similarity

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Ollama Embedding
# ---------------------------------------------------------------------------

def _extract_embedding(payload: dict[str, Any]) -> list[float]:
    """兼容 Ollama /api/embeddings 与新版 /api/embed 的返回结构。"""
    if "embedding" in payload:
        return payload["embedding"]
    if "data" in payload and isinstance(payload["data"], list) and len(payload["data"]) > 0:
        item = payload["data"][0]
        if isinstance(item, dict) and "embedding" in item:
            return item["embedding"]
    raise ValueError(f"无法从 Ollama 响应中提取 embedding: {json.dumps(payload, ensure_ascii=False)[:300]}")


@lru_cache(maxsize=2048)
def _ollama_embedding(text: str) -> list[float]:
    """调用 Ollama Embedding 模型，返回向量。"""
    cfg = get_config()
    endpoint = cfg.grading.embedding.endpoint.rstrip("/")
    url = f"{endpoint}/api/embed"
    payload = {"model": cfg.grading.embedding.model, "input": text}
    with httpx.Client(
        timeout=cfg.grading.embedding.timeout_seconds,
        trust_env=False,
    ) as client:
        resp = client.post(url, json=payload)
        resp.raise_for_status()
        return _extract_embedding(resp.json())


# ---------------------------------------------------------------------------
# sentence-transformers 本地 embedding
# ---------------------------------------------------------------------------

@lru_cache(maxsize=1)
def _load_model():
    cfg = get_config()
    from sentence_transformers import SentenceTransformer
    return SentenceTransformer(cfg.grading.embedding.model, device=cfg.grading.embedding.device)


def _similarity_sentence_transformers(a: str, b: str) -> float:
    model = _load_model()
    va, vb = model.encode([a, b], normalize_embeddings=True)
    return float(va @ vb)


# ---------------------------------------------------------------------------
# similarity 主入口
# ---------------------------------------------------------------------------

def _cosine(a: list[float], b: list[float]) -> float:
    import math
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(x * x for x in b))
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


def _similarity_with_embedding(a: str, b: str) -> tuple[float, str]:
    """Embedding 相似度，返回 (score, method)。"""
    cfg = get_config()
    endpoint = cfg.grading.embedding.endpoint
    if endpoint:
        try:
            ea = _ollama_embedding(a)
            eb = _ollama_embedding(b)
            return _cosine(ea, eb), "ollama_embedding"
        except Exception as e:
            logger.warning("Ollama Embedding 失败，回退本地模型: %s", e)
    try:
        return _similarity_sentence_transformers(a, b), "local_embedding"
    except Exception as e:
        logger.warning("sentence-transformers 加载失败，回退关键词: %s", e)
        return keyword_similarity(a, b), "keyword"


# ---------------------------------------------------------------------------
# 判分主入口
# ---------------------------------------------------------------------------

def grade_with_embedding(
    question: dict[str, Any],
    student_answer: str,
    fallback_reason: str | None = None,
) -> dict[str, Any]:
    """Embedding 判分主入口。"""
    cfg = get_config()
    max_score = float(question.get("score", 0))
    ref = question.get("answer", "")
    try:
        sim, method = _similarity_with_embedding(student_answer, ref)
    except Exception as e:
        logger.warning("Embedding 判分异常，回退关键词: %s", e)
        sim = keyword_similarity(student_answer, ref)
        method = "keyword"
        fallback_reason = fallback_reason or f"Embedding 异常: {e}"

    score = round(sim * max_score, cfg.scoring.score_precision)
    status = review_status_by_confidence(sim, cfg)
    return {
        "status": "embedding_ok" if method != "keyword" else "unavailable",
        "score": safe_score(score, max_score),
        "similarity": round(sim, 4),
        "review_status": status,
        "fallback_reason": fallback_reason if method == "keyword" else None,
    }
