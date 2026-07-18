"""多专业试卷加载、结构校验与员工端脱敏。

存储：
- data/papers/index.json  专业索引
- data/papers/{slug}.json 各专业当前卷

兼容：若 papers 为空且存在 data/questions.json，启动时迁移为 default 卷。
"""
from __future__ import annotations

import json
import re
import threading
from pathlib import Path
from typing import Any

from fastapi import HTTPException

PROJECT_ROOT = Path(__file__).resolve().parent.parent
DATA_DIR = PROJECT_ROOT / "data"
PAPERS_DIR = DATA_DIR / "papers"
INDEX_PATH = PAPERS_DIR / "index.json"
LEGACY_QUESTIONS_PATH = DATA_DIR / "questions.json"
BACKUPS_DIR = DATA_DIR / "backups" / "papers"

ALLOWED_TYPES = {"single_choice", "multiple_choice", "true_false", "short_answer", "essay", "composite"}
OBJECTIVE_TYPES = {"single_choice", "multiple_choice", "true_false"}
SUBJECTIVE_TYPES = {"short_answer", "essay"}
SENSITIVE_FIELDS = {"answer", "scoring_rubric", "scoring_points", "calculation"}
ALLOWED_SCORING_MODES = {"text", "sql", "code", "calculation"}
ALLOWED_CODE_LANGUAGES = frozenset({
    "python", "java", "javascript", "typescript", "go",
    "c", "cpp", "csharp", "sql", "bash", "shell",
})
SLUG_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,31}$")
PAPER_STATUS_OPEN = "open"
PAPER_STATUS_CLOSED = "closed"

_reload_lock = threading.Lock()
_paper_cache: dict[str, dict[str, Any]] = {}
_index_cache: dict[str, Any] | None = None


def is_objective(question: dict[str, Any]) -> bool:
    return question.get("type") in OBJECTIVE_TYPES


def is_composite_question(q: dict[str, Any]) -> bool:
    return bool(get_subquestions(q))


def _normalize_code_language(raw: Any, *, qid: str) -> str | None:
    if raw is None or raw == "":
        return None
    lang = str(raw).strip().lower()
    if lang not in ALLOWED_CODE_LANGUAGES:
        _error(f"题目 {qid} 不支持的 code_language: {raw}")
    return lang


def normalize_composite_question(question: dict[str, Any]) -> dict[str, Any]:
    """将旧复合题字段原地转换为规范字段。"""
    if "subquestions" not in question and isinstance(question.get("sub_questions"), list):
        question["subquestions"] = question.pop("sub_questions")
    subs = question.get("subquestions")
    if isinstance(subs, list) and subs and question.get("type") in SUBJECTIVE_TYPES:
        question["type"] = "composite"
    for sub in subs if isinstance(subs, list) else []:
        if not isinstance(sub, dict):
            continue
        legacy = sub.get("code_language")
        if str(sub.get("scoring_mode") or "text").strip().lower() == "code":
            sub.pop("code_language", None)
            languages = sub.get("allowed_languages") or ([legacy] if legacy else [])
            if isinstance(languages, list):
                normalized = [
                    _normalize_code_language(value, qid=str(sub.get("id") or "?"))
                    for value in languages
                ]
                sub["allowed_languages"] = [
                    language for language in normalized if language
                ]
    return question


def get_subquestions(question: dict[str, Any]) -> list[dict[str, Any]]:
    normalize_composite_question(question)
    value = question.get("subquestions")
    return value if isinstance(value, list) else []


def normalize_submitted_subanswer(
    subquestion: dict[str, Any], raw: Any, *, allow_legacy: bool = False
) -> tuple[str, str | None]:
    """校验并规范化一个小问的考生答案。"""
    mode = str(subquestion.get("scoring_mode") or "text").strip().lower()
    if isinstance(raw, str):
        if mode == "code" and not allow_legacy:
            raise ValueError("INVALID_ANSWER_SHAPE: 代码子题答案必须包含 language")
        answer = raw
        language = (subquestion.get("allowed_languages") or [None])[0]
    elif isinstance(raw, dict) and isinstance(raw.get("answer", ""), str):
        answer = raw.get("answer", "")
        language = raw.get("language")
    else:
        raise ValueError("INVALID_ANSWER_SHAPE: 子题答案必须包含字符串 answer")

    if mode == "code":
        normalized = str(language or "").strip().lower()
        if normalized not in subquestion.get("allowed_languages", []):
            raise ValueError("INVALID_CODE_LANGUAGE: 代码语言不在允许范围内")
        return answer, normalized
    return answer, None


def validate_answer_shape(question: dict[str, Any], answer: Any) -> None:
    """校验提交答案形状。非法时 raise ValueError('INVALID_ANSWER_SHAPE: ...')。"""
    if is_composite_question(question):
        if not isinstance(answer, dict):
            raise ValueError("INVALID_ANSWER_SHAPE: 复合题答案必须为对象 map")
        subs = get_subquestions(question)
        expected = {str(s.get("id")) for s in subs if s.get("id")}
        got = {str(k) for k in answer.keys()}
        if got != expected:
            raise ValueError(
                f"INVALID_ANSWER_SHAPE: 复合题答案键必须为 {sorted(expected)}，实际 {sorted(got)}"
            )
        submap = {str(s.get("id")): s for s in subs}
        for k, v in answer.items():
            normalize_submitted_subanswer(submap[str(k)], v)
        return
    if isinstance(answer, dict):
        raise ValueError("INVALID_ANSWER_SHAPE: 非复合题答案不能为对象")


def _error(message: str, code: str = "INVALID_QUESTION_FILE", status: int = 500) -> None:
    raise HTTPException(status_code=status, detail={"code": code, "message": message})


def validate_slug(slug: str) -> str:
    s = (slug or "").strip().lower()
    if not SLUG_RE.match(s):
        _error(
            "专业编码仅允许小写字母、数字、下划线、短横线，且以字母或数字开头（最长 32）",
            "INVALID_PAPER_SLUG",
            400,
        )
    if ".." in s or "/" in s or "\\" in s:
        _error("非法专业编码", "INVALID_PAPER_SLUG", 400)
    return s


def paper_path(slug: str) -> Path:
    slug = validate_slug(slug)
    return PAPERS_DIR / f"{slug}.json"


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
    qtype = q.get("type")
    ans = q.get("answer")
    if qtype == "single_choice":
        if str(ans) not in keys:
            _error(f"单选题 {q.get('id')} answer 不在 options 中")
    elif qtype == "multiple_choice":
        if not isinstance(ans, list) or not ans:
            _error(f"多选题 {q.get('id')} answer 必须是非空数组")
        for a in ans:
            if str(a) not in keys:
                _error(f"多选题 {q.get('id')} answer 含非法选项 {a}")


def _validate_scoring_points(q: dict[str, Any]) -> None:
    points = q.get("scoring_points")
    if points is None:
        return
    if not isinstance(points, list):
        _error(f"题目 {q.get('id')} scoring_points 必须是数组")
    max_score = float(q.get("score") or 0)
    total = 0.0
    for i, p in enumerate(points):
        if not isinstance(p, dict):
            _error(f"题目 {q.get('id')} scoring_points[{i}] 必须是对象")
        text = p.get("text")
        if not text or not str(text).strip():
            _error(f"题目 {q.get('id')} scoring_points[{i}] 缺少 text")
        score = p.get("score")
        if not isinstance(score, (int, float)) or float(score) < 0:
            _error(f"题目 {q.get('id')} scoring_points[{i}] score 非法")
        total += float(score)
    if total > max_score + 1e-6:
        _error(f"题目 {q.get('id')} 评分点合计 {total} 超过题目满分 {max_score}")


def _validate_subjective_mode_and_language(q: dict[str, Any], *, label: str | None = None) -> None:
    """规范化 scoring_mode / code_language，并做 code 语言必填等校验。"""
    qid = label or str(q.get("id") or "?")
    mode_raw = q.get("scoring_mode")
    mode = str(mode_raw or "text").strip().lower() if mode_raw is not None else "text"
    if mode_raw is not None and str(mode_raw).strip() and mode not in ALLOWED_SCORING_MODES:
        _error(f"题目 {qid} scoring_mode 非法: {mode_raw}")
    if mode_raw is not None and str(mode_raw).strip():
        q["scoring_mode"] = mode
    elif mode_raw is None or not str(mode_raw).strip():
        # 子题/单题未写 mode 时默认 text，便于下游透传
        q.setdefault("scoring_mode", "text")
        mode = str(q.get("scoring_mode") or "text").strip().lower()

    lang = _normalize_code_language(q.get("code_language"), qid=qid)
    allowed_languages = q.get("allowed_languages")
    if mode == "code" and not lang and not allowed_languages:
        _error(f"题目 {qid} 的 scoring_mode=code 时必须提供 code_language")
    if mode == "sql":
        if lang is None:
            lang = "sql"
        elif lang != "sql":
            _error(f"题目 {qid} 的 scoring_mode=sql 时 code_language 必须为 sql")
    if lang is not None:
        q["code_language"] = lang
    elif "code_language" in q and (q.get("code_language") is None or q.get("code_language") == ""):
        q.pop("code_language", None)

    _validate_scoring_points(q)
    if mode == "calculation":
        _validate_calculation(q)


def _validate_sub_questions(parent: dict[str, Any]) -> None:
    parent_qid = str(parent.get("id") or "?")
    subs = get_subquestions(parent)
    if not isinstance(subs, list) or not subs:
        _error(f"题目 {parent_qid} 的 subquestions 必须为非空数组")
    if parent.get("type") != "composite":
        _error(f"题目 {parent_qid} 配置 subquestions 时类型必须为 composite")

    parent_score = float(parent.get("score") or 0)
    seen: set[str] = set()
    total = 0.0
    for j, sub in enumerate(subs):
        if not isinstance(sub, dict):
            _error(f"题目 {parent_qid} subquestions[{j}] 必须是对象")
        sid = sub.get("id")
        if not sid or not str(sid).strip():
            _error(f"题目 {parent_qid} subquestions[{j}] 缺少 id")
        sid = str(sid).strip()
        if sid in seen:
            _error(f"题目 {parent_qid} 子题 id 重复: {sid}")
        seen.add(sid)
        sub["id"] = sid
        if not sub.get("question"):
            _error(f"题目 {parent_qid} 子题 {sid} 缺少 question")
        if not sub.get("answer"):
            _error(f"题目 {parent_qid} 子题 {sid} 缺少参考答案 answer")
        score = sub.get("score")
        if not isinstance(score, (int, float)) or float(score) <= 0:
            _error(f"题目 {parent_qid} 子题 {sid} score 必须是大于 0 的数字")
        total += float(score)
        _validate_subjective_mode_and_language(sub, label=f"{parent_qid}.{sid}")
        mode = sub.get("scoring_mode", "text")
        languages = sub.get("allowed_languages")
        if mode == "code":
            if not isinstance(languages, list) or not languages:
                _error(f"题目 {parent_qid} 子题 {sid} 的 allowed_languages 必须为非空数组")
            if len(languages) != len(set(languages)):
                _error(f"题目 {parent_qid} 子题 {sid} 的 allowed_languages 不能重复")
        elif languages is not None:
            _error(f"题目 {parent_qid} 子题 {sid} 仅代码模式可配置 allowed_languages")

    if abs(total - parent_score) > 1e-6:
        _error(
            f"题目 {parent_qid} 的 subquestions 分值之和 ({total}) 不等于题目分值 ({parent_score})"
        )


def _validate_calculation(q: dict[str, Any]) -> None:
    config = q.get("calculation")
    if config is None:
        # 转换器可以先标记计算题，待人工补齐数值评分项；运行时会回退到 text。
        return
    if not isinstance(config, dict):
        _error(f"计算题 {q.get('id')} calculation 必须是对象")
    if config.get("strategy", "static_values") != "static_values":
        _error(f"计算题 {q.get('id')} 仅支持 static_values 策略")

    items: list[dict[str, Any]] = []
    for key in ("steps", "final_answers"):
        values = config.get(key, [])
        if not isinstance(values, list):
            _error(f"计算题 {q.get('id')} calculation.{key} 必须是数组")
        items.extend(values)
    if not items:
        _error(f"计算题 {q.get('id')} 至少需要一个计算步骤或最终答案")

    ids: set[str] = set()
    total = 0.0
    for item in items:
        if not isinstance(item, dict):
            _error(f"计算题 {q.get('id')} calculation 项必须是对象")
        item_id = str(item.get("id") or "")
        if not item_id or item_id in ids:
            _error(f"计算题 {q.get('id')} calculation id 缺失或重复")
        ids.add(item_id)
        if not str(item.get("description") or "").strip():
            _error(f"计算题 {q.get('id')} calculation.description 缺失")
        if not isinstance(item.get("expected"), (int, float)):
            _error(f"计算题 {q.get('id')} calculation.expected 必须是数字")
        score = item.get("score")
        if not isinstance(score, (int, float)) or float(score) < 0:
            _error(f"计算题 {q.get('id')} calculation.score 非法")
        tolerance = item.get("tolerance", 0)
        if not isinstance(tolerance, (int, float)) or float(tolerance) < 0:
            _error(f"计算题 {q.get('id')} calculation.tolerance 非法")
        keywords = item.get("keywords", [])
        if not isinstance(keywords, list) or any(not isinstance(k, str) for k in keywords):
            _error(f"计算题 {q.get('id')} calculation.keywords 必须是字符串数组")
        total += float(score)
    if total > float(q.get("score") or 0) + 1e-6:
        _error(f"计算题 {q.get('id')} calculation 分值合计超过题目满分")
    cap = config.get("final_only_score_cap")
    if cap is not None and (not isinstance(cap, (int, float)) or float(cap) < 0 or float(cap) > total + 1e-6):
        _error(f"计算题 {q.get('id')} final_only_score_cap 非法")


def validate_questions(data: dict[str, Any]) -> None:
    """校验整卷结构（exam_info + questions）。"""
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
        normalize_composite_question(q)
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
            if is_composite_question(q):
                _error(f"客观题 {qid} 不能配置 sub_questions")
            if qtype in {"single_choice", "multiple_choice"}:
                _validate_option_list(q)
            if "answer" not in q:
                _error(f"客观题 {qid} 缺少 answer")
            if qtype == "true_false" and not isinstance(q.get("answer"), bool):
                _error(f"判断题 {qid} answer 必须是布尔值")
            if qtype == "multiple_choice":
                ans = q.get("answer")
                if not isinstance(ans, list) or not ans:
                    _error(f"多选题 {qid} answer 必须是非空数组")
        else:
            subs = q.get("subquestions")
            if subs is not None:
                if isinstance(subs, list) and len(subs) == 0:
                    _error(f"题目 {qid} 的 subquestions 不能为空数组，请省略该字段")
                if is_composite_question(q):
                    _validate_sub_questions(q)
                    continue

            if not q.get("answer"):
                _error(f"主观题 {qid} 缺少参考答案 answer")
            _validate_subjective_mode_and_language(q)

    # 服务端以题目分和为准时仍检查一致性（写路径会覆盖 total_score）
    declared_total = info.get("total_score")
    if isinstance(declared_total, (int, float)) and declared_total > 0:
        if abs(float(declared_total) - total) > 0.001:
            _error(f"题目分数总和 {total} 与 exam_info.total_score {declared_total} 不一致")

    passing = info.get("passing_score")
    if isinstance(passing, (int, float)) and float(passing) < 0:
        _error("exam_info.passing_score 不能为负")
    if (
        isinstance(passing, (int, float))
        and isinstance(declared_total, (int, float))
        and declared_total > 0
        and float(passing) > float(declared_total) + 1e-6
    ):
        _error("exam_info.passing_score 不能大于 total_score")


def recompute_total_score(data: dict[str, Any]) -> float:
    questions = data.get("questions") or []
    total = sum(float(q.get("score") or 0) for q in questions if isinstance(q, dict))
    info = data.setdefault("exam_info", {})
    if not isinstance(info, dict):
        info = {}
        data["exam_info"] = info
    info["total_score"] = round(total, 4)
    return total


def sanitize_for_student(questions: list[dict[str, Any]]) -> list[dict[str, Any]]:
    sanitized: list[dict[str, Any]] = []
    for q in questions:
        normalize_composite_question(q)
        public = {k: v for k, v in q.items() if k not in SENSITIVE_FIELDS and k != "subquestions"}
        if is_composite_question(q):
            public["subquestions"] = []
            for s in get_subquestions(q):
                if not isinstance(s, dict):
                    continue
                sub_public: dict[str, Any] = {
                    "id": s.get("id"),
                    "question": s.get("question", ""),
                    "score": s.get("score"),
                }
                if s.get("scoring_mode"):
                    sub_public["scoring_mode"] = s.get("scoring_mode")
                if s.get("allowed_languages"):
                    sub_public["allowed_languages"] = list(s["allowed_languages"])
                public["subquestions"].append(sub_public)
        sanitized.append(public)
    return sanitized


# ---------------------------------------------------------------------------
# Index / cache
# ---------------------------------------------------------------------------

def clear_question_cache(paper_id: str | None = None) -> None:
    global _index_cache
    if paper_id is None:
        _paper_cache.clear()
        _index_cache = None
    else:
        _paper_cache.pop(str(paper_id), None)
        _index_cache = None


def _empty_index() -> dict[str, Any]:
    return {"papers": []}


def _read_index_raw() -> dict[str, Any]:
    if not INDEX_PATH.exists():
        return _empty_index()
    try:
        with INDEX_PATH.open("r", encoding="utf-8") as f:
            data = json.load(f)
    except json.JSONDecodeError as e:
        _error(f"试卷索引 JSON 解析失败: {e}")
    if not isinstance(data, dict):
        _error("试卷索引根节点必须是对象")
    papers = data.get("papers")
    if papers is None:
        data["papers"] = []
    elif not isinstance(papers, list):
        _error("index.papers 必须是数组")
    return data


def load_index(*, force: bool = False) -> dict[str, Any]:
    global _index_cache
    if not force and _index_cache is not None:
        return _index_cache
    with _reload_lock:
        if not force and _index_cache is not None:
            return _index_cache
        _index_cache = _read_index_raw()
        return _index_cache


def list_papers() -> list[dict[str, Any]]:
    idx = load_index()
    return list(idx.get("papers") or [])


def get_paper_meta(slug: str) -> dict[str, Any] | None:
    slug = validate_slug(slug)
    for p in list_papers():
        if str(p.get("slug")) == slug:
            return dict(p)
    return None


def _read_paper_file(slug: str) -> dict[str, Any]:
    path = paper_path(slug)
    if not path.exists():
        _error(f"试卷不存在: {slug}", "PAPER_NOT_FOUND", 404)
    try:
        with path.open("r", encoding="utf-8") as f:
            data = json.load(f)
    except json.JSONDecodeError as e:
        _error(f"试卷 {slug} JSON 解析失败: {e}")
    if not isinstance(data, dict):
        _error(f"试卷 {slug} 根节点必须是对象")
    return data


def load_questions(paper_id: str | None = None) -> dict[str, Any]:
    """加载指定专业试卷（含答案）。paper_id 必填（多卷模式下）。"""
    if not paper_id:
        _error("缺少试卷/专业标识 paper", "PAPER_REQUIRED", 400)
    slug = validate_slug(paper_id)
    cached = _paper_cache.get(slug)
    if cached is not None:
        return cached
    with _reload_lock:
        cached = _paper_cache.get(slug)
        if cached is not None:
            return cached
        data = _read_paper_file(slug)
        # 允许空卷在编辑阶段？校验要求非空——编辑中途可能暂存
        # 考生侧与判分要求非空；管理端读可不强制非空
        questions = data.get("questions")
        if isinstance(questions, list) and questions:
            validate_questions(data)
        elif not isinstance(data.get("exam_info"), dict) or not data["exam_info"].get("title"):
            # 最小结构
            data.setdefault("exam_info", {"title": data.get("name") or slug, "total_score": 0})
            data.setdefault("questions", [])
        data.setdefault("paper_id", slug)
        _paper_cache[slug] = data
        return data


def reload_questions(paper_id: str | None = None) -> dict[str, Any]:
    """重载缓存。传 paper_id 只清该卷；不传则清全部并返回 index。"""
    with _reload_lock:
        if paper_id:
            clear_question_cache(paper_id)
            return load_questions(paper_id)
        clear_question_cache()
        return load_index(force=True)


def get_exam_info(paper_id: str) -> dict[str, Any]:
    return dict(load_questions(paper_id).get("exam_info", {}))


def get_question_list(paper_id: str) -> list[dict[str, Any]]:
    return list(load_questions(paper_id).get("questions", []))


def get_question_map(paper_id: str) -> dict[str, dict[str, Any]]:
    return {str(q["id"]): q for q in get_question_list(paper_id)}


def public_exam_payload(paper_id: str) -> dict[str, Any]:
    data = load_questions(paper_id)
    meta = get_paper_meta(paper_id) or {}
    return {
        "paper_id": paper_id,
        "paper_name": data.get("name") or meta.get("name") or paper_id,
        "status": meta.get("status", PAPER_STATUS_CLOSED),
        "exam_info": data.get("exam_info", {}),
        "questions": sanitize_for_student(data.get("questions", [])),
    }


def assert_paper_open(paper_id: str) -> dict[str, Any]:
    meta = get_paper_meta(paper_id)
    if not meta:
        _error(f"试卷不存在: {paper_id}", "PAPER_NOT_FOUND", 404)
    if meta.get("status") != PAPER_STATUS_OPEN:
        _error("该专业考试尚未开放，请使用管理员发布的有效链接", "EXAM_CLOSED", 403)
    return meta


def ensure_papers_layout() -> None:
    """确保 papers 目录存在，并在需要时从 legacy questions.json 迁移。"""
    PAPERS_DIR.mkdir(parents=True, exist_ok=True)
    BACKUPS_DIR.mkdir(parents=True, exist_ok=True)

    if INDEX_PATH.exists():
        return

    # 无 index：尝试迁移旧卷
    if LEGACY_QUESTIONS_PATH.exists():
        try:
            with LEGACY_QUESTIONS_PATH.open("r", encoding="utf-8") as f:
                legacy = json.load(f)
        except Exception:
            legacy = None
        if isinstance(legacy, dict) and isinstance(legacy.get("questions"), list) and legacy["questions"]:
            title = (legacy.get("exam_info") or {}).get("title") or "默认试卷"
            slug = "default"
            paper = {
                "paper_id": slug,
                "name": title,
                "exam_info": legacy.get("exam_info") or {"title": title, "total_score": 0},
                "questions": legacy["questions"],
            }
            recompute_total_score(paper)
            try:
                validate_questions(paper)
            except HTTPException:
                pass
            dest = PAPERS_DIR / f"{slug}.json"
            with dest.open("w", encoding="utf-8") as f:
                json.dump(paper, f, ensure_ascii=False, indent=2)
            index = {
                "papers": [
                    {
                        "slug": slug,
                        "name": title,
                        "status": PAPER_STATUS_CLOSED,
                        "question_count": len(paper["questions"]),
                        "total_score": float((paper.get("exam_info") or {}).get("total_score") or 0),
                        "updated_at": None,
                    }
                ]
            }
            with INDEX_PATH.open("w", encoding="utf-8") as f:
                json.dump(index, f, ensure_ascii=False, indent=2)
            return

    # 空 index
    with INDEX_PATH.open("w", encoding="utf-8") as f:
        json.dump(_empty_index(), f, ensure_ascii=False, indent=2)


# 模块导入时确保目录（main 预检也会调用）
try:
    ensure_papers_layout()
except Exception:
    pass
