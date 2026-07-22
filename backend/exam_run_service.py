"""考试轮次、会话草稿与管理员收卷服务。

路由层只做参数校验与错误映射，业务组合集中在此模块。
"""
from __future__ import annotations

import hashlib
import json
import logging
import os
import threading
import time
from copy import deepcopy
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable

from fastapi import HTTPException

from . import database, paper_store, question_loader
from .config import get_config
from .utils import now_iso, parse_iso

logger = logging.getLogger(__name__)

PROJECT_ROOT = Path(__file__).resolve().parent.parent
EXAM_RUNS_DIR = PROJECT_ROOT / "data" / "exam_runs"

CLOSING_BUFFER_SECONDS = 5

# 后台收卷
_finalize_stop = threading.Event()
_finalize_thread: threading.Thread | None = None
_schedule_grading: Callable[[int, dict[str, Any], str, str | None], None] | None = None


class ServiceError(Exception):
    def __init__(self, status: int, code: str, message: str):
        self.status = status
        self.code = code
        self.message = message
        super().__init__(message)

    def as_http(self) -> HTTPException:
        return HTTPException(
            status_code=self.status,
            detail={"code": self.code, "message": self.message},
        )


def set_grading_scheduler(
    fn: Callable[[int, dict[str, Any], str, str | None], None] | None,
) -> None:
    global _schedule_grading
    _schedule_grading = fn


def _error(status: int, code: str, message: str) -> None:
    raise ServiceError(status, code, message)


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _utc_iso(dt: datetime | None = None) -> str:
    """统一用本地时区秒精度 ISO，与 database.now_iso / 字符串比较一致。"""
    d = dt or datetime.now().astimezone()
    if d.tzinfo is None:
        d = d.astimezone()
    return d.replace(microsecond=0).isoformat()


def _is_due(finalize_at: str | None, now: datetime | None = None) -> bool:
    if not finalize_at:
        return True
    try:
        deadline = parse_iso(str(finalize_at))
        current = now or datetime.now().astimezone()
        if deadline.tzinfo is None:
            deadline = deadline.astimezone()
        if current.tzinfo is None:
            current = current.astimezone()
        return deadline <= current
    except Exception:
        return True


def _atomic_write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def write_run_snapshot(run_id: str, paper: dict[str, Any]) -> tuple[str, str]:
    EXAM_RUNS_DIR.mkdir(parents=True, exist_ok=True)
    path = EXAM_RUNS_DIR / f"{run_id}.json"
    payload = deepcopy(paper)
    raw = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    digest = hashlib.sha256(raw.encode("utf-8")).hexdigest()
    _atomic_write_json(path, payload)
    # 存相对路径便于迁移
    rel = str(path.relative_to(PROJECT_ROOT)) if path.is_relative_to(PROJECT_ROOT) else str(path)
    return rel, digest


def _token_path(run_id: str) -> Path:
    return EXAM_RUNS_DIR / f"{run_id}.token"


def save_public_token(run_id: str, public_token: str) -> None:
    """明文 token 仅存文件系统侧车文件，不入库（库内只存哈希）。供管理端重建链接。"""
    EXAM_RUNS_DIR.mkdir(parents=True, exist_ok=True)
    path = _token_path(run_id)
    path.write_text(public_token, encoding="utf-8")
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass


def load_public_token(run_id: str) -> str | None:
    path = _token_path(run_id)
    if not path.exists():
        return None
    try:
        token = path.read_text(encoding="utf-8").strip()
        return token or None
    except OSError:
        return None


def load_run_snapshot(run: dict[str, Any]) -> dict[str, Any]:
    snap = run.get("snapshot_path")
    if not snap:
        _error(404, "RUN_SNAPSHOT_MISSING", "该轮次无试卷快照")
    path = Path(snap)
    if not path.is_absolute():
        path = PROJECT_ROOT / path
    if not path.exists():
        _error(404, "RUN_SNAPSHOT_MISSING", "该轮次试卷快照文件不存在")
    with path.open("r", encoding="utf-8") as f:
        data = json.load(f)
    if not isinstance(data, dict):
        _error(500, "RUN_SNAPSHOT_INVALID", "试卷快照格式无效")
    return data


def delete_orphan_snapshot(snapshot_path: str | None) -> None:
    if not snapshot_path:
        return
    path = Path(snapshot_path)
    if not path.is_absolute():
        path = PROJECT_ROOT / path
    try:
        if path.exists():
            path.unlink()
    except OSError:
        logger.exception("清理孤立快照失败 path=%s", path)


def has_active_run(paper_id: str) -> bool:
    return database.get_active_run_for_paper(paper_id) is not None


def has_any_run(paper_id: str) -> bool:
    return database.has_any_run(paper_id)


def open_run(slug: str) -> dict[str, Any]:
    """发布新轮次：写快照 + 创建 open 轮次。返回含明文 public_token。"""
    slug = question_loader.validate_slug(slug)
    meta = question_loader.get_paper_meta(slug)
    if not meta:
        _error(404, "PAPER_NOT_FOUND", f"试卷不存在: {slug}")

    paper = question_loader.load_questions(slug)
    questions = paper.get("questions") or []
    if not questions:
        _error(400, "EMPTY_QUESTION_BANK", "试卷无题目，无法开考")
    question_loader.validate_questions(paper)

    if database.get_active_run_for_paper(slug):
        _error(409, "ACTIVE_RUN_EXISTS", "该试卷已有进行中的考试轮次")

    duration = int(get_config().exam.duration_minutes)
    run_id = database.new_token()
    public_token = database.new_token()
    token_hash = database.hash_token(public_token)
    round_no = database.max_round_no(slug) + 1
    opened_at = _utc_iso()

    snapshot_path, snapshot_hash = write_run_snapshot(run_id, paper)
    try:
        run = database.create_exam_run(
            run_id=run_id,
            paper_id=slug,
            round_no=round_no,
            public_token_hash=token_hash,
            status="open",
            duration_minutes=duration,
            snapshot_path=snapshot_path,
            snapshot_hash=snapshot_hash,
            is_legacy=0,
            opened_at=opened_at,
            created_at=opened_at,
        )
        save_public_token(run_id, public_token)
    except Exception:
        delete_orphan_snapshot(snapshot_path)
        try:
            _token_path(run_id).unlink(missing_ok=True)
        except Exception:
            pass
        raise

    # 兼容 index.json
    try:
        paper_store.sync_index_status_only(slug, question_loader.PAPER_STATUS_OPEN)
    except Exception:
        logger.exception("同步 index open 失败 slug=%s", slug)

    out = dict(run)
    out["public_token"] = public_token
    out["paper_name"] = paper.get("name") or meta.get("name") or slug
    return out


def begin_close(slug: str) -> dict[str, Any]:
    slug = question_loader.validate_slug(slug)
    active = database.get_active_run_for_paper(slug)
    if not active:
        _error(404, "RUN_NOT_FOUND", "当前没有进行中的考试轮次")

    if active["status"] == "closing":
        return {
            "success": True,
            "status": "closing",
            "run_id": active["id"],
            "active_sessions": database.count_active_sessions_for_run(active["id"]),
            "finalize_at": active.get("finalize_at"),
            "round_no": active.get("round_no"),
        }

    now = datetime.now().astimezone()
    finalize_at = now + timedelta(seconds=CLOSING_BUFFER_SECONDS)
    updated = database.transition_run_to_closing(
        active["id"],
        closing_started_at=_utc_iso(now),
        finalize_at=_utc_iso(finalize_at),
    )
    if not updated:
        _error(409, "RUN_STATE_CONFLICT", "轮次状态已变化，请刷新后重试")

    return {
        "success": True,
        "status": "closing",
        "run_id": updated["id"],
        "active_sessions": database.count_active_sessions_for_run(updated["id"]),
        "finalize_at": updated.get("finalize_at"),
        "round_no": updated.get("round_no"),
    }


def finalize_run(run_id: str) -> list[int]:
    """幂等收卷：active 会话按草稿建提交，轮次 closed。返回新建 submission_ids。"""
    run = database.get_run_by_id(run_id)
    if not run:
        return []
    if run["status"] != "closing":
        return []
    finalize_at = run.get("finalize_at")
    if finalize_at and not _is_due(str(finalize_at)):
        return []

    paper_id = str(run["paper_id"])
    paper_name = None
    try:
        meta = question_loader.get_paper_meta(paper_id) or {}
        paper_name = meta.get("name")
    except Exception:
        pass

    new_ids: list[int] = []
    grade_jobs: list[tuple[int, dict[str, Any], str, str]] = []

    with database.db_cursor() as conn:
        # 再确认
        row = conn.execute("SELECT * FROM exam_runs WHERE id = ?", (run_id,)).fetchone()
        if not row or row["status"] != "closing":
            return []
        sessions = database.list_active_sessions_for_run(run_id, conn=conn)
        for sess in sessions:
            answers = sess.get("draft") or {}
            if not isinstance(answers, dict):
                answers = {}
            try:
                sid = database.insert_submission_pending(
                    name=str(sess.get("name") or ""),
                    employee_id=str(sess.get("employee_id") or ""),
                    paper_id=paper_id,
                    run_id=run_id,
                    paper_name=paper_name,
                    department=sess.get("department"),
                    answers=answers,
                    started_at=sess.get("started_at"),
                    client_ip=sess.get("client_ip"),
                    user_agent=sess.get("user_agent"),
                    auto_submit_reason="admin_closed",
                    conn=conn,
                )
                database.mark_session_submitted(str(sess["id"]), conn=conn)
                new_ids.append(sid)
                grade_jobs.append((sid, answers, paper_id, run_id))
            except Exception as e:
                if "UNIQUE" in str(e).upper():
                    database.mark_session_submitted(str(sess["id"]), conn=conn)
                    continue
                raise
        database.mark_run_closed(run_id, conn=conn)

    try:
        paper_store.sync_index_status_only(paper_id, question_loader.PAPER_STATUS_CLOSED)
    except Exception:
        logger.exception("同步 index closed 失败 paper=%s", paper_id)

    if _schedule_grading:
        for sid, answers, pid, rid in grade_jobs:
            try:
                _schedule_grading(sid, answers, pid, rid)
            except Exception:
                logger.exception("收卷后调度评分失败 submission_id=%s", sid)

    return new_ids


def scan_and_finalize_due_runs() -> int:
    now = _utc_iso()
    due = database.list_closing_runs_due(now)
    count = 0
    for run in due:
        try:
            ids = finalize_run(str(run["id"]))
            count += 1
            if ids:
                logger.info("收卷完成 run_id=%s submissions=%d", run["id"], len(ids))
        except Exception:
            logger.exception("收卷失败 run_id=%s", run.get("id"))
    return count


def _finalize_loop() -> None:
    scan_and_finalize_due_runs()
    while not _finalize_stop.wait(1.0):
        try:
            scan_and_finalize_due_runs()
        except Exception:
            logger.exception("收卷循环异常")


def start_finalize_loop() -> None:
    global _finalize_thread
    if _finalize_thread is not None and _finalize_thread.is_alive():
        return
    _finalize_stop.clear()
    _finalize_thread = threading.Thread(
        target=_finalize_loop, name="exam-finalize", daemon=True
    )
    _finalize_thread.start()
    logger.info("考试收卷循环已启动")


def stop_finalize_loop() -> None:
    global _finalize_thread
    _finalize_stop.set()
    t = _finalize_thread
    _finalize_thread = None
    if t is not None and t.is_alive():
        t.join(timeout=3.0)


def resume_on_startup() -> None:
    """启动时收卷到期轮次，并重入队未完成评分。"""
    try:
        n = scan_and_finalize_due_runs()
        if n:
            logger.info("启动恢复：处理 %d 个到期收卷轮次", n)
    except Exception:
        logger.exception("启动收卷恢复失败")

    if _schedule_grading:
        try:
            pending = database.list_pending_grading_submissions()
            for row in pending:
                _schedule_grading(
                    int(row["id"]),
                    row.get("answers") or {},
                    str(row.get("paper_id") or ""),
                    row.get("run_id"),
                )
            if pending:
                logger.info("启动恢复：重新入队 %d 个评分任务", len(pending))
        except Exception:
            logger.exception("启动评分恢复失败")


def batch_open(slugs: list[str]) -> dict[str, Any]:
    seen: list[str] = []
    for s in slugs:
        s = (s or "").strip()
        if s and s not in seen:
            seen.append(s)
    papers: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    for slug in seen:
        try:
            run = open_run(slug)
            papers.append({
                "slug": slug,
                "run_id": run["id"],
                "round_no": run["round_no"],
                "status": run["status"],
                "public_token": run["public_token"],
            })
        except ServiceError as e:
            errors.append({"slug": slug, "code": e.code, "message": e.message})
        except HTTPException as e:
            detail = e.detail if isinstance(e.detail, dict) else {}
            errors.append({
                "slug": slug,
                "code": detail.get("code", "ERROR"),
                "message": detail.get("message", str(e.detail)),
            })
        except Exception as e:
            logger.exception("batch open failed slug=%s", slug)
            errors.append({"slug": slug, "code": "OPEN_FAILED", "message": str(e)})
    return {
        "success": len(errors) == 0,
        "requested": len(seen),
        "updated": len(papers),
        "skipped": 0,
        "papers": papers,
        "errors": errors,
    }


def batch_close(slugs: list[str]) -> dict[str, Any]:
    seen: list[str] = []
    for s in slugs:
        s = (s or "").strip()
        if s and s not in seen:
            seen.append(s)
    papers: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    for slug in seen:
        try:
            result = begin_close(slug)
            papers.append({"slug": slug, **result})
        except ServiceError as e:
            errors.append({"slug": slug, "code": e.code, "message": e.message})
        except HTTPException as e:
            detail = e.detail if isinstance(e.detail, dict) else {}
            errors.append({
                "slug": slug,
                "code": detail.get("code", "ERROR"),
                "message": detail.get("message", str(e.detail)),
            })
        except Exception as e:
            logger.exception("batch close failed slug=%s", slug)
            errors.append({"slug": slug, "code": "CLOSE_FAILED", "message": str(e)})
    return {
        "success": len(errors) == 0,
        "requested": len(seen),
        "updated": len(papers),
        "skipped": 0,
        "papers": papers,
        "errors": errors,
    }


def _cleanup_run_files(run_id: str, snapshot_path: str | None) -> None:
    delete_orphan_snapshot(snapshot_path)
    try:
        _token_path(run_id).unlink(missing_ok=True)
    except Exception:
        logger.exception("清理轮次 token 失败 run_id=%s", run_id)
    # 标准路径再兜底删一次
    try:
        (EXAM_RUNS_DIR / f"{run_id}.json").unlink(missing_ok=True)
    except Exception:
        pass


def reset_rounds(slugs: list[str] | None = None) -> dict[str, Any]:
    """清除专业考试轮次（链接/快照/会话），下次发布从第 1 轮开始。

    - 进行中 / 收卷中的专业跳过并记入 errors
    - 成绩提交记录保留
    - slugs 为空时重置全部专业
    """
    if slugs:
        seen: list[str] = []
        for s in slugs:
            s = (s or "").strip()
            if s and s not in seen:
                seen.append(s)
        targets = seen
    else:
        targets = [str(p.get("slug") or "") for p in question_loader.list_papers() if p.get("slug")]

    papers: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    for slug in targets:
        try:
            slug = question_loader.validate_slug(slug)
        except Exception:
            errors.append({"slug": slug, "code": "INVALID_SLUG", "message": f"无效专业编码: {slug}"})
            continue
        meta = question_loader.get_paper_meta(slug)
        if not meta:
            errors.append({"slug": slug, "code": "PAPER_NOT_FOUND", "message": f"试卷不存在: {slug}"})
            continue
        try:
            result = database.purge_runs_for_paper(slug)
        except ValueError as e:
            msg = str(e)
            if msg.startswith("ACTIVE_RUN:"):
                st = msg.split(":", 1)[1]
                errors.append({
                    "slug": slug,
                    "code": "ACTIVE_RUN_EXISTS",
                    "message": f"考试{'中' if st == 'open' else '收卷中'}，请先结束后再重置",
                })
            else:
                errors.append({"slug": slug, "code": "RESET_FAILED", "message": msg})
            continue
        except Exception as e:
            logger.exception("重置轮次失败 slug=%s", slug)
            errors.append({"slug": slug, "code": "RESET_FAILED", "message": str(e)})
            continue

        for item in result.get("runs") or []:
            _cleanup_run_files(str(item.get("run_id") or ""), item.get("snapshot_path"))

        try:
            paper_store.sync_index_status_only(slug, question_loader.PAPER_STATUS_CLOSED)
        except Exception:
            logger.exception("重置后同步 index closed 失败 slug=%s", slug)

        papers.append({
            "slug": slug,
            "runs_deleted": result.get("runs_deleted", 0),
            "sessions_deleted": result.get("sessions_deleted", 0),
            "status": "unpublished",
        })

    return {
        "success": len(errors) == 0,
        "requested": len(targets),
        "updated": len(papers),
        "papers": papers,
        "errors": errors,
    }


def derived_status_for_paper(paper_id: str) -> str:
    active = database.get_active_run_for_paper(paper_id)
    if active:
        return str(active["status"])  # open | closing
    latest = database.get_latest_run_for_paper(paper_id)
    if not latest:
        return "unpublished"
    return "closed"


def list_exam_summaries() -> list[dict[str, Any]]:
    papers = question_loader.list_papers()
    latest_map = database.list_all_runs_latest_by_paper()
    items: list[dict[str, Any]] = []
    for p in papers:
        slug = str(p.get("slug") or "")
        run = latest_map.get(slug)
        active = database.get_active_run_for_paper(slug)
        display = active or run
        if not display:
            status = "unpublished"
            items.append({
                "paper_id": slug,
                "paper_name": p.get("name") or slug,
                "status": status,
                "round_no": None,
                "run_id": None,
                "duration_minutes": get_config().exam.duration_minutes,
                "opened_at": None,
                "closing_started_at": None,
                "finalize_at": None,
                "closed_at": None,
                "started_count": 0,
                "submitted_count": 0,
                "active_count": 0,
                "has_public_link": False,
                "is_legacy": False,
            })
            continue
        run_id = str(display["id"])
        status = str(display["status"]) if active else "closed"
        started = database.count_sessions_for_run(run_id)
        active_n = database.count_active_sessions_for_run(run_id)
        submitted = database.submission_count(run_id=run_id)
        items.append({
            "paper_id": slug,
            "paper_name": p.get("name") or slug,
            "status": status if active else ("closed" if display else "unpublished"),
            "round_no": display.get("round_no"),
            "run_id": run_id,
            "duration_minutes": display.get("duration_minutes"),
            "opened_at": display.get("opened_at"),
            "closing_started_at": display.get("closing_started_at"),
            "finalize_at": display.get("finalize_at"),
            "closed_at": display.get("closed_at"),
            "started_count": started,
            "submitted_count": submitted,
            "active_count": active_n,
            "has_public_link": bool(display.get("public_token_hash")) and not display.get("is_legacy"),
            "is_legacy": bool(display.get("is_legacy")),
            "public_token_present": bool(display.get("public_token_hash")),
        })

    order = {"closing": 0, "open": 1, "unpublished": 2, "closed": 3}
    items.sort(
        key=lambda x: (
            order.get(str(x.get("status")), 9),
            -(parse_iso(x["opened_at"]).timestamp() if x.get("opened_at") else 0),
        )
    )
    return items


def _resolve_run_by_token(run_token: str) -> dict[str, Any]:
    token = (run_token or "").strip()
    if not token:
        _error(404, "RUN_NOT_FOUND", "考试链接无效")
    run = database.get_run_by_public_token_hash(database.hash_token(token))
    if not run:
        _error(404, "RUN_NOT_FOUND", "考试链接无效")
    return run


def get_public_exam(paper_id: str, run_token: str) -> dict[str, Any]:
    paper_id = question_loader.validate_slug(paper_id)
    run = _resolve_run_by_token(run_token)
    if str(run["paper_id"]) != paper_id:
        _error(404, "RUN_NOT_FOUND", "考试链接无效")

    status = str(run["status"])
    base = {
        "paper_id": paper_id,
        "paper_name": (question_loader.get_paper_meta(paper_id) or {}).get("name") or paper_id,
        "run_id": run["id"],
        "round_no": run.get("round_no"),
        "run_status": status,
        "duration_minutes": run.get("duration_minutes"),
        "finalize_at": run.get("finalize_at"),
        "server_time": _utc_iso(),
        "auto_submit": get_config().exam.auto_submit,
        "config": {
            "duration_minutes": run.get("duration_minutes"),
            "auto_submit": get_config().exam.auto_submit,
        },
    }

    if status == "closed":
        return {
            **base,
            "closed": True,
            "message": "本轮考试已结束",
            "questions": [],
            "exam_info": {},
        }

    if run.get("is_legacy") or not run.get("snapshot_path"):
        _error(403, "RUN_CLOSED", "本轮考试已结束")

    paper = load_run_snapshot(run)
    questions = question_loader.sanitize_for_student(paper.get("questions") or [])
    return {
        **base,
        "closed": False,
        "paper_name": paper.get("name") or base["paper_name"],
        "exam_info": paper.get("exam_info") or {},
        "questions": questions,
    }


def start_or_resume_session(
    *,
    paper_id: str,
    run_token: str,
    name: str,
    employee_id: str,
    department: str | None,
    client_ip: str | None,
    user_agent: str | None,
) -> dict[str, Any]:
    paper_id = question_loader.validate_slug(paper_id)
    run = _resolve_run_by_token(run_token)
    if str(run["paper_id"]) != paper_id:
        _error(404, "RUN_NOT_FOUND", "考试链接无效")

    status = str(run["status"])
    employee_id = employee_id.strip()
    name = name.strip()
    if not employee_id:
        _error(400, "INVALID_EMPLOYEE_ID", "请提供工号")
    if not name:
        _error(400, "INVALID_NAME", "请提供姓名")

    existing = database.get_session_by_run_employee(str(run["id"]), employee_id)
    if existing:
        # 恢复：不返回新 token
        draft = existing.get("draft") or {}
        return {
            "session_id": existing["id"],
            "session_token": None,
            "started_at": existing["started_at"],
            "deadline_at": existing["deadline_at"],
            "draft_revision": existing.get("draft_revision") or 0,
            "answers": draft,
            "run_status": status,
            "session_status": existing.get("status"),
            "created": False,
        }

    if status != "open":
        if status == "closing":
            _error(403, "RUN_CLOSING", "考试正在收卷，无法开始新会话")
        _error(403, "RUN_CLOSED", "本轮考试已结束")

    # 已提交则禁止再开始
    if database.duplicate_exists_for_run(employee_id, str(run["id"])):
        _error(409, "DUPLICATE_SUBMISSION", "您已在本轮考试提交，不能重复参加")

    started = _utcnow()
    duration = int(run.get("duration_minutes") or get_config().exam.duration_minutes)
    deadline = started + timedelta(minutes=duration)
    session_id = database.new_token()
    session_token = database.new_token()
    token_hash = database.hash_token(session_token)

    sess, created = database.create_exam_session_if_absent(
        session_id=session_id,
        run_id=str(run["id"]),
        employee_id=employee_id,
        name=name,
        department=(department or "").strip() or None,
        session_token_hash=token_hash,
        started_at=_utc_iso(started),
        deadline_at=_utc_iso(deadline),
        client_ip=client_ip,
        user_agent=user_agent,
    )
    draft = sess.get("draft") if "draft" in sess else json.loads(sess.get("draft_json") or "{}")
    return {
        "session_id": sess["id"],
        "session_token": session_token if created else None,
        "started_at": sess["started_at"],
        "deadline_at": sess["deadline_at"],
        "draft_revision": sess.get("draft_revision") or 0,
        "answers": draft or {},
        "run_status": status,
        "session_status": sess.get("status"),
        "created": created,
    }


def _auth_session(session_id: str, session_token: str) -> dict[str, Any]:
    sess = database.get_session_by_id(session_id)
    if not sess:
        _error(404, "SESSION_NOT_FOUND", "无有效考试会话")
    if database.hash_token(session_token) != sess.get("session_token_hash"):
        _error(401, "INVALID_SESSION_TOKEN", "会话凭证无效")
    return sess


def _question_map_for_run(run: dict[str, Any]) -> dict[str, dict[str, Any]]:
    if run.get("is_legacy") or not run.get("snapshot_path"):
        paper = question_loader.load_questions(str(run["paper_id"]))
    else:
        paper = load_run_snapshot(run)
    return {str(q["id"]): q for q in (paper.get("questions") or []) if isinstance(q, dict)}


def _validate_answers(
    qmap: dict[str, dict[str, Any]],
    answers: dict[str, Any],
    *,
    strict: bool,
) -> None:
    if not isinstance(answers, dict):
        _error(422, "INVALID_ANSWER_SHAPE", "答案必须是对象")
    unknown = sorted(str(qid) for qid in answers.keys() if str(qid) not in qmap)
    if unknown:
        _error(422, "UNKNOWN_QUESTION_ID", f"提交包含未知题目 ID: {', '.join(unknown)}")
    for qid, ans in answers.items():
        if ans is None or ans == "" or ans == [] or ans == {}:
            if not strict:
                continue
        q = qmap[str(qid)]
        try:
            question_loader.validate_answer_shape(q, ans)
        except ValueError as e:
            message = str(e)
            code = "INVALID_CODE_LANGUAGE" if message.startswith("INVALID_CODE_LANGUAGE") else "INVALID_ANSWER_SHAPE"
            _error(422, code, message)


def save_draft(
    session_id: str,
    *,
    session_token: str,
    revision: int,
    answers: dict[str, Any],
) -> dict[str, Any]:
    sess = _auth_session(session_id, session_token)
    if sess.get("status") != "active":
        _error(409, "SESSION_SUBMITTED", "会话已提交，无法保存草稿")

    run = database.get_run_by_id(str(sess["run_id"]))
    if not run:
        _error(404, "RUN_NOT_FOUND", "考试轮次不存在")
    status = str(run["status"])
    if status == "closed":
        _error(403, "RUN_CLOSED", "本轮考试已结束")
    if status == "closing":
        finalize_at = run.get("finalize_at")
        if finalize_at:
            try:
                if parse_iso(str(finalize_at)) <= _utcnow().astimezone():
                    _error(403, "RUN_CLOSING", "收卷窗口已结束")
            except Exception:
                pass

    current_rev = int(sess.get("draft_revision") or 0)
    if int(revision) <= current_rev:
        _error(409, "STALE_DRAFT_REVISION", "草稿版本过旧，已保留服务器版本")

    qmap = _question_map_for_run(run)
    _validate_answers(qmap, answers or {}, strict=False)

    updated = database.update_session_draft(
        session_id,
        expected_revision=current_rev,
        answers=answers or {},
        new_revision=int(revision),
    )
    if not updated:
        # 并发：重读
        sess2 = database.get_session_by_id(session_id)
        cur = int((sess2 or {}).get("draft_revision") or 0)
        if int(revision) <= cur:
            _error(409, "STALE_DRAFT_REVISION", "草稿版本过旧，已保留服务器版本")
        _error(409, "DRAFT_SAVE_FAILED", "草稿保存失败，请重试")

    return {
        "success": True,
        "draft_revision": updated.get("draft_revision"),
        "draft_saved_at": updated.get("draft_saved_at"),
        "run_status": status,
        "session_status": updated.get("status"),
        "finalize_at": run.get("finalize_at"),
    }


def get_session_status(session_id: str, session_token: str) -> dict[str, Any]:
    sess = _auth_session(session_id, session_token)
    run = database.get_run_by_id(str(sess["run_id"]))
    if not run:
        _error(404, "RUN_NOT_FOUND", "考试轮次不存在")

    submission_id = None
    if sess.get("status") == "submitted":
        sub = database.get_submission_for_run(str(sess["employee_id"]), str(sess["run_id"]))
        if sub:
            submission_id = sub.get("id")

    return {
        "session_id": sess["id"],
        "session_status": sess.get("status"),
        "run_status": run.get("status"),
        "started_at": sess.get("started_at"),
        "deadline_at": sess.get("deadline_at"),
        "draft_revision": sess.get("draft_revision") or 0,
        "draft_saved_at": sess.get("draft_saved_at"),
        "finalize_at": run.get("finalize_at"),
        "submission_id": submission_id,
        "server_time": _utc_iso(),
    }


def submit_manual(
    *,
    session_id: str,
    session_token: str,
    answers: dict[str, Any],
    auto_submit_reason: str | None = None,
) -> dict[str, Any]:
    sess = _auth_session(session_id, session_token)
    run = database.get_run_by_id(str(sess["run_id"]))
    if not run:
        _error(404, "RUN_NOT_FOUND", "考试轮次不存在")

    status = str(run["status"])
    if status == "closing":
        _error(403, "RUN_CLOSING", "考试正在收卷，请等待自动提交")
    if status == "closed":
        _error(403, "RUN_CLOSED", "本轮考试已结束")
    if sess.get("status") != "active":
        sub = database.get_submission_for_run(str(sess["employee_id"]), str(sess["run_id"]))
        if sub:
            _error(409, "DUPLICATE_SUBMISSION", "该员工已在本轮提交，不能重复提交")
        _error(409, "SESSION_SUBMITTED", "会话已提交")

    # 截止校验
    grace = int(getattr(get_config().exam, "grace_period_seconds", 30))
    try:
        deadline = parse_iso(str(sess["deadline_at"]))
        if _utcnow().astimezone() > deadline + timedelta(seconds=grace):
            _error(403, "EXAM_TIMEOUT", "考试已超时，提交被拒绝")
    except Exception:
        pass

    qmap = _question_map_for_run(run)
    answers = answers or {}
    _validate_answers(qmap, answers, strict=True)

    paper_id = str(run["paper_id"])
    paper_name = (question_loader.get_paper_meta(paper_id) or {}).get("name")

    try:
        with database.db_cursor() as conn:
            # 双重检查 run 仍 open
            row = conn.execute("SELECT status FROM exam_runs WHERE id = ?", (run["id"],)).fetchone()
            if not row or row["status"] != "open":
                _error(403, "RUN_CLOSING", "考试正在收卷，请等待自动提交")
            submission_id = database.insert_submission_pending(
                name=str(sess["name"]),
                employee_id=str(sess["employee_id"]),
                paper_id=paper_id,
                run_id=str(run["id"]),
                paper_name=paper_name,
                department=sess.get("department"),
                answers=answers,
                started_at=sess.get("started_at"),
                client_ip=sess.get("client_ip"),
                user_agent=sess.get("user_agent"),
                auto_submit_reason=auto_submit_reason,
                conn=conn,
            )
            database.mark_session_submitted(session_id, conn=conn)
    except ServiceError:
        raise
    except Exception as e:
        if "UNIQUE" in str(e).upper():
            sub = database.get_submission_for_run(str(sess["employee_id"]), str(run["id"]))
            _error(
                409,
                "DUPLICATE_SUBMISSION",
                "该员工已在本轮提交，不能重复提交",
            )
        logger.exception("提交保存失败")
        _error(500, "SUBMIT_FAILED", "提交失败，请联系管理员")

    if _schedule_grading:
        try:
            _schedule_grading(submission_id, answers, paper_id, str(run["id"]))
        except Exception:
            logger.exception("调度评分失败 submission_id=%s", submission_id)

    return {
        "success": True,
        "submission_id": submission_id,
        "status": "grading",
        "paper_id": paper_id,
        "run_id": run["id"],
        "message": "提交成功，系统正在评分中",
    }


def build_exam_url(base: str, paper_id: str, public_token: str) -> str:
    base = base.rstrip("/")
    return f"{base}/exam?paper={paper_id}&run={public_token}"


def get_run_public_token(run_id: str) -> str | None:
    return load_public_token(run_id)
