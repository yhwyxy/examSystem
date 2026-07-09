"""LLM 主观题判分。

当前实现面向 Ollama /api/generate，失败或输出不合法时返回 None，由上层触发 Embedding 回退。
"""
from __future__ import annotations

import json
import logging
from typing import Any

import httpx

from .config import get_config

logger = logging.getLogger(__name__)

PROMPT_TEMPLATE = """你是严格、公正的企业内部考试阅卷老师。请根据题目、参考答案和评分标准，对学生答案进行评分。

题目：{question}
满分：{max_score}
参考答案：{reference_answer}
评分标准：{scoring_rubric}
学生答案：{student_answer}

要求：
1. score 必须是 0 到 {max_score} 之间的数字。
2. confidence 必须是 0 到 1 之间的小数，表示你对评分的把握。
3. reason 使用一句中文说明得分和扣分原因。
4. 只输出 JSON，不要输出 Markdown，不要输出额外解释。

输出格式：
{{"score": 0, "confidence": 0.0, "reason": ""}}
"""


def _strip_json_fence(text: str) -> str:
    s = text.strip()
    if s.startswith("```"):
        s = s.strip("`").strip()
        if s.lower().startswith("json"):
            s = s[4:].strip()
    start = s.find("{")
    end = s.rfind("}")
    if start >= 0 and end >= start:
        return s[start:end + 1]
    return s


def parse_llm_output(content: str, max_score: float) -> dict[str, Any] | None:
    try:
        obj = json.loads(_strip_json_fence(content))
    except Exception:
        return None

    score = obj.get("score")
    confidence = obj.get("confidence")
    reason = obj.get("reason")

    if not isinstance(score, (int, float)):
        return None
    if float(score) < 0 or float(score) > max_score:
        return None
    if not isinstance(confidence, (int, float)):
        return None
    if float(confidence) < 0 or float(confidence) > 1:
        return None
    if not isinstance(reason, str) or not reason.strip():
        return None

    return {
        "machine_score": round(float(score), 6),
        "confidence": round(float(confidence), 4),
        "reason": reason.strip(),
        "llm_raw_output": content,
    }


def grade_with_llm(question: dict[str, Any], student_answer: str, cfg: Any = None) -> dict[str, Any] | None:
    """使用 Ollama LLM 对主观题进行判分。返回 None 表示不可用或失败。

    cfg 可选，未传入时读取全局配置（向后兼容）。
    """
    if cfg is None:
        cfg = get_config()
    if not cfg.grading.use_llm:
        return None
    llm = cfg.grading.llm
    if llm.provider.lower() != "ollama":
        logger.warning("暂不支持的 LLM provider: %s", llm.provider)
        return None

    max_score = float(question["score"])
    prompt = PROMPT_TEMPLATE.format(
        question=question.get("question", ""),
        max_score=max_score,
        reference_answer=question.get("answer", ""),
        scoring_rubric=question.get("scoring_rubric", "无"),
        student_answer=student_answer or "",
    )

    url = f"{llm.endpoint.rstrip('/')}/api/generate"
    payload = {"model": llm.model, "prompt": prompt, "stream": False}

    for attempt in range(llm.retry_times + 1):
        try:
            with httpx.Client(timeout=llm.timeout_seconds, trust_env=False) as client:
                resp = client.post(url, json=payload)
                resp.raise_for_status()
                data = resp.json()
            parsed = parse_llm_output(str(data.get("response", "")), max_score)
            if parsed is None:
                logger.warning("LLM 输出不合法，attempt=%s", attempt)
                continue
            parsed["grading_method"] = "llm"
            parsed["fallback_reason"] = None
            return parsed
        except Exception as e:
            logger.warning("LLM 判分失败，attempt=%s, err=%s", attempt, e)
    return None


def grade_subjective(question: dict[str, Any], student_answer: str, cfg: Any = None) -> dict[str, Any] | None:
    """兼容接口：grader.py 调用时会传入已加载的 cfg，避免重复读配置 + 便于测试 mock。

    若未传入 cfg 则降级到 get_config()（向后兼容）。
    """
    if cfg is None:
        cfg = get_config()
    return grade_with_llm(question, student_answer, cfg)
