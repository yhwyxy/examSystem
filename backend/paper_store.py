"""多专业试卷写入：创建/保存/题目 CRUD/开闭考/备份。

所有写操作经本模块，禁止在路由中直接写 questions 文件。
open 状态默认禁止改题。
"""
from __future__ import annotations

import json
import logging
import os
import threading
from copy import deepcopy
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from fastapi import HTTPException

from . import question_loader as ql

logger = logging.getLogger(__name__)

_write_lock = threading.Lock()
_BACKUP_KEEP = 20


def _error(message: str, code: str, status: int = 400) -> None:
    raise HTTPException(status_code=status, detail={"code": code, "message": message})


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _atomic_write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.replace(tmp, path)


def _backup_paper(slug: str, data: dict[str, Any]) -> None:
    backup_dir = ql.BACKUPS_DIR / slug
    backup_dir.mkdir(parents=True, exist_ok=True)
    ts = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    path = backup_dir / f"questions-{ts}.json"
    try:
        with path.open("w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)
    except OSError:
        logger.exception("备份试卷失败 slug=%s", slug)
        return
    files = sorted(backup_dir.glob("questions-*.json"), key=lambda p: p.name, reverse=True)
    for old in files[_BACKUP_KEEP:]:
        try:
            old.unlink()
        except OSError:
            pass


def _load_index_unlocked() -> dict[str, Any]:
    return ql._read_index_raw()


def _save_index_unlocked(index: dict[str, Any]) -> None:
    _atomic_write_json(ql.INDEX_PATH, index)
    ql.clear_question_cache()


def _find_meta(index: dict[str, Any], slug: str) -> dict[str, Any] | None:
    for p in index.get("papers") or []:
        if str(p.get("slug")) == slug:
            return p
    return None


def _assert_editable(meta: dict[str, Any] | None, slug: str) -> None:
    if not meta:
        _error(f"试卷不存在: {slug}", "PAPER_NOT_FOUND", 404)
    if meta.get("status") == ql.PAPER_STATUS_OPEN:
        _error("考试进行中，禁止修改试卷；请先结束考试", "PAPER_OPEN_LOCKED", 409)


def _sync_meta_from_paper(meta: dict[str, Any], paper: dict[str, Any]) -> None:
    questions = paper.get("questions") or []
    meta["question_count"] = len(questions) if isinstance(questions, list) else 0
    info = paper.get("exam_info") or {}
    meta["total_score"] = float(info.get("total_score") or 0)
    if paper.get("name"):
        meta["name"] = paper["name"]
    meta["updated_at"] = _now_iso()


def _empty_paper(slug: str, name: str) -> dict[str, Any]:
    return {
        "paper_id": slug,
        "name": name,
        "exam_info": {
            "title": f"{name}专业考试",
            "description": "请在规定时间内完成答题。",
            "total_score": 0,
            "passing_score": 0,
        },
        "questions": [],
    }


def _write_paper_unlocked(slug: str, paper: dict[str, Any], meta: dict[str, Any], index: dict[str, Any]) -> None:
    path = ql.paper_path(slug)
    if path.exists():
        try:
            prev = ql._read_paper_file(slug)
            _backup_paper(slug, prev)
        except Exception:
            logger.exception("读取旧卷备份失败 slug=%s", slug)
    _atomic_write_json(path, paper)
    _sync_meta_from_paper(meta, paper)
    _save_index_unlocked(index)
    ql.clear_question_cache(slug)


def list_papers_with_status() -> list[dict[str, Any]]:
    ql.ensure_papers_layout()
    return ql.list_papers()


def create_paper(*, slug: str, name: str) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    name = (name or "").strip()
    if not name:
        _error("专业名称不能为空", "INVALID_REQUEST")
    with _write_lock:
        ql.ensure_papers_layout()
        index = _load_index_unlocked()
        if _find_meta(index, slug):
            _error(f"专业编码已存在: {slug}", "DUPLICATE_PAPER_SLUG")
        paper = _empty_paper(slug, name)
        _atomic_write_json(ql.paper_path(slug), paper)
        meta = {
            "slug": slug,
            "name": name,
            "status": ql.PAPER_STATUS_CLOSED,
            "question_count": 0,
            "total_score": 0,
            "updated_at": _now_iso(),
        }
        papers = list(index.get("papers") or [])
        papers.append(meta)
        index["papers"] = papers
        _save_index_unlocked(index)
        return meta


def get_paper_full(slug: str) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    ql.ensure_papers_layout()
    meta = ql.get_paper_meta(slug)
    if not meta:
        _error(f"试卷不存在: {slug}", "PAPER_NOT_FOUND", 404)
    data = ql.load_questions(slug)
    return {
        "meta": meta,
        "paper_id": data.get("paper_id") or slug,
        "name": data.get("name") or meta.get("name"),
        "exam_info": data.get("exam_info") or {},
        "questions": data.get("questions") or [],
        "status": meta.get("status"),
        "editable": meta.get("status") != ql.PAPER_STATUS_OPEN,
    }


def save_paper(slug: str, payload: dict[str, Any]) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        _assert_editable(meta, slug)

        paper = {
            "paper_id": slug,
            "name": (payload.get("name") or meta.get("name") or slug).strip(),
            "exam_info": payload.get("exam_info") or {},
            "questions": payload.get("questions") or [],
        }
        if not isinstance(paper["exam_info"], dict):
            _error("exam_info 必须是对象", "INVALID_REQUEST")
        if not paper["exam_info"].get("title"):
            paper["exam_info"]["title"] = paper["name"]
        ql.recompute_total_score(paper)
        if not paper["questions"]:
            _error("试卷至少需要一道题", "EMPTY_QUESTION_BANK")
        ql.validate_questions(paper)
        _write_paper_unlocked(slug, paper, meta, index)
        return get_paper_full(slug)


def update_meta(slug: str, *, name: str | None = None) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        _assert_editable(meta, slug)
        if name is not None:
            name = name.strip()
            if not name:
                _error("专业名称不能为空", "INVALID_REQUEST")
            meta["name"] = name
            paper = ql._read_paper_file(slug)
            paper["name"] = name
            _atomic_write_json(ql.paper_path(slug), paper)
        meta["updated_at"] = _now_iso()
        _save_index_unlocked(index)
        ql.clear_question_cache(slug)
        return dict(meta)


def delete_paper(slug: str, *, has_submissions: bool) -> None:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        if not meta:
            _error(f"试卷不存在: {slug}", "PAPER_NOT_FOUND", 404)
        if meta.get("status") == ql.PAPER_STATUS_OPEN:
            _error("请先结束考试再删除专业", "PAPER_OPEN_LOCKED", 409)
        if has_submissions:
            _error("该专业已有提交记录，禁止删除", "PAPER_HAS_SUBMISSIONS", 409)

        path = ql.paper_path(slug)
        if path.exists():
            try:
                _backup_paper(slug, ql._read_paper_file(slug))
            except Exception:
                pass
            try:
                path.unlink()
            except OSError:
                logger.exception("删除试卷文件失败 slug=%s", slug)

        index["papers"] = [p for p in (index.get("papers") or []) if str(p.get("slug")) != slug]
        _save_index_unlocked(index)
        ql.clear_question_cache(slug)


def set_status(slug: str, status: str) -> dict[str, Any]:
    if status not in {ql.PAPER_STATUS_OPEN, ql.PAPER_STATUS_CLOSED}:
        _error("status 仅允许 open 或 closed", "INVALID_REQUEST")
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        if not meta:
            _error(f"试卷不存在: {slug}", "PAPER_NOT_FOUND", 404)
        if status == ql.PAPER_STATUS_OPEN:
            paper = ql._read_paper_file(slug)
            questions = paper.get("questions") or []
            if not questions:
                _error("试卷无题目，无法开考", "EMPTY_QUESTION_BANK")
            ql.validate_questions(paper)
        meta["status"] = status
        meta["updated_at"] = _now_iso()
        _save_index_unlocked(index)
        return dict(meta)


def next_question_id(questions: list[dict[str, Any]]) -> str:
    existing = {str(q.get("id")) for q in questions if isinstance(q, dict)}
    n = 1
    while f"q{n}" in existing:
        n += 1
    return f"q{n}"


def add_question(slug: str, question: dict[str, Any]) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        _assert_editable(meta, slug)
        paper = ql._read_paper_file(slug)
        questions = list(paper.get("questions") or [])
        q = deepcopy(question)
        if not q.get("id"):
            q["id"] = next_question_id(questions)
        else:
            qid = str(q["id"])
            if any(str(x.get("id")) == qid for x in questions):
                _error(f"题目 ID 已存在: {qid}", "DUPLICATE_QUESTION_ID")
        questions.append(q)
        paper["questions"] = questions
        ql.recompute_total_score(paper)
        ql.validate_questions(paper)
        _write_paper_unlocked(slug, paper, meta, index)
        return q


def update_question(slug: str, question_id: str, question: dict[str, Any]) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        _assert_editable(meta, slug)
        paper = ql._read_paper_file(slug)
        questions = list(paper.get("questions") or [])
        idx = next((i for i, q in enumerate(questions) if str(q.get("id")) == str(question_id)), None)
        if idx is None:
            _error(f"题目不存在: {question_id}", "QUESTION_NOT_FOUND", 404)
        q = deepcopy(question)
        q["id"] = str(question_id)
        questions[idx] = q
        paper["questions"] = questions
        ql.recompute_total_score(paper)
        ql.validate_questions(paper)
        _write_paper_unlocked(slug, paper, meta, index)
        return q


def delete_question(slug: str, question_id: str) -> None:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        _assert_editable(meta, slug)
        paper = ql._read_paper_file(slug)
        questions = list(paper.get("questions") or [])
        new_qs = [q for q in questions if str(q.get("id")) != str(question_id)]
        if len(new_qs) == len(questions):
            _error(f"题目不存在: {question_id}", "QUESTION_NOT_FOUND", 404)
        if not new_qs:
            _error("不能删除最后一道题", "EMPTY_QUESTION_BANK")
        paper["questions"] = new_qs
        ql.recompute_total_score(paper)
        ql.validate_questions(paper)
        _write_paper_unlocked(slug, paper, meta, index)


def reorder_questions(slug: str, ids: list[str]) -> dict[str, Any]:
    slug = ql.validate_slug(slug)
    with _write_lock:
        index = _load_index_unlocked()
        meta = _find_meta(index, slug)
        _assert_editable(meta, slug)
        paper = ql._read_paper_file(slug)
        questions = list(paper.get("questions") or [])
        qmap = {str(q.get("id")): q for q in questions}
        if set(ids) != set(qmap.keys()) or len(ids) != len(qmap):
            _error("排序 id 列表必须与现有题目完全一致", "INVALID_REORDER")
        paper["questions"] = [qmap[i] for i in ids]
        _write_paper_unlocked(slug, paper, meta, index)
        return get_paper_full(slug)
