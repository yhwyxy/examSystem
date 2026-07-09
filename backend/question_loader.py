"""题库加载、结构校验与员工端脱敏。

题库文件：data/questions.json
设计要求：
- 启动期校验题库结构，避免考试中途才暴露配置错误。
- 员工端接口必须移除 answer / scoring_rubric 等敏感字段。
"""
from __future__ import annotations

import json
import threading
from functools import lru_cache
from pathlib import Path
from typing import Any

from fastapi import HTTPException

PROJECT_ROOT = Path(__file__).resolve().parent.parent
QUESTIONS_PATH = PROJECT_ROOT / "data" / "questions.json"

ALLOWED_TYPES = {"single_choice", "multiple_choice", "true_false", "short_answer", "essay"}
OBJECTIVE_TYPES = {"single_choice", "multiple_choice", "true_false"}
SUBJECTIVE_TYPES = {"short_answer", "essay"}
SENSITIVE_FIELDS = {"answer", "scoring_rubric"}


def is_objective(question: dict[str, Any]) -> bool:
    """判断题目是否为客观题。"""
    return question.get("type") in OBJECTIVE_TYPES

_reload_lock = threading.Lock()


def _error(message: str, code: str = "INVALID_QUESTION_FILE") -> None:
    raise HTTPException(status_code=500, detail={"code": code, "message": message})


def _validate_option_list(q: dict[str, Any]) -> None:
    options = q.get("options")
    if not isinstance(options, list) or not options:
        _error(f"题目 {q.get('id')} 缺少 options")
    keys: set[str] = set()
    for opt in options:
        if not isinstance(opt, dict) or "key" not in opt or "text" not in opt:
            _error(f"题目 {q.get('id')} options 格式错误")
        key = str(opt["key"])
        if key in keys:
            _error(f"题目 {q.get('id')} options key 重复: {key}")
        keys.add(key)


def validate_questions(data: dict[str, Any]) -> None:
    info = data.get("exam_info")
    if not isinstance(info, dict):
        _error("exam_info 必须是对象")
    if not info.get("title"):
        _error("exam_info.title 必须存在")

    questions = data.get("questions")
    if not isinstance(questions, list) or not questions:
        _error("questions 必须是非空数组")

    ids: set[str] = set()
    total = 0.0
    for q in questions:
        if not isinstance(q, dict):
            _error("questions 中每一项必须是对象")
        qid = q.get("id")
        if not qid:
            _error("每道题必须存在 id")
        if qid in ids:
            _error(f"题目 ID 重复: {qid}")
        ids.add(str(qid))

        qtype = q.get("type")
        if qtype not in ALLOWED_TYPES:
            _error(f"题目 {qid} 类型非法: {qtype}")

        if not q.get("question"):
            _error(f"题目 {qid} 缺少 question")

        score = q.get("score")
        if not isinstance(score, (int, float)) or float(score) <= 0:
            _error(f"题目 {qid} score 必须是大于 0 的数字")
        total += float(score)

        if qtype in OBJECTIVE_TYPES:
            if qtype in {"single_choice", "multiple_choice"}:
                _validate_option_list(q)
            if "answer" not in q:
                _error(f"客观题 {qid} 缺少 answer")
            if qtype == "multiple_choice":
                ans = q.get("answer")
                if not isinstance(ans, list) or not ans:
                    _error(f"多选题 {qid} answer 必须是非空数组")
        else:
            if not q.get("answer"):
                _error(f"主观题 {qid} 缺少参考答案 answer")

    declared_total = info.get("total_score")
    if isinstance(declared_total, (int, float)) and declared_total > 0:
        if abs(float(declared_total) - total) > 0.001:
            _error(f"题目分数总和 {total} 与 exam_info.total_score {declared_total} 不一致")


def clear_question_cache() -> None:
    load_questions.cache_clear()


def reload_questions() -> dict[str, Any]:
    """线程安全的题库重载：清除缓存后立即重新加载，避免并发读取到空状态。"""
    with _reload_lock:
        clear_question_cache()
        return load_questions()


@lru_cache(maxsize=1)
def load_questions() -> dict[str, Any]:
    if not QUESTIONS_PATH.exists():
        _error(f"题库文件不存在: {QUESTIONS_PATH}", "QUESTION_FILE_NOT_FOUND")
    try:
        with QUESTIONS_PATH.open("r", encoding="utf-8") as f:
            data = json.load(f)
    except json.JSONDecodeError as e:
        _error(f"题库 JSON 解析失败: {e}")
    if not isinstance(data, dict):
        _error("题库根节点必须是对象")
    validate_questions(data)
    return data


def get_exam_info() -> dict[str, Any]:
    return dict(load_questions().get("exam_info", {}))


def get_question_list() -> list[dict[str, Any]]:
    return list(load_questions().get("questions", []))


def get_question_map() -> dict[str, dict[str, Any]]:
    return {str(q["id"]): q for q in get_question_list()}


def sanitize_for_student(questions: list[dict[str, Any]]) -> list[dict[str, Any]]:
    sanitized: list[dict[str, Any]] = []
    for q in questions:
        sanitized.append({k: v for k, v in q.items() if k not in SENSITIVE_FIELDS})
    return sanitized


def public_exam_payload() -> dict[str, Any]:
    data = load_questions()
    return {
        "exam_info": data.get("exam_info", {}),
        "questions": sanitize_for_student(data.get("questions", [])),
    }
