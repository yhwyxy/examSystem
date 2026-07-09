"""SQLite 数据库连接与基础 CRUD。

设计：
- 使用 WAL 模式，支持近似同时提交（NFR-006）。
- 每次操作新建连接，避免跨线程共享连接（FastAPI 同步端点在 threadpool 中运行）。
- 各服务通过本模块的函数访问数据库，避免循环依赖。
"""
from __future__ import annotations

import json
import sqlite3
from contextlib import contextmanager
from typing import Any

from .config import get_config
from .utils import now_iso, parse_iso, seconds_between

PROJECT_ROOT = __import__("pathlib").Path(__file__).resolve().parent.parent
DB_PATH = PROJECT_ROOT / "data" / "exam.db"

_SCHEMA = """
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS submissions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    employee_id TEXT NOT NULL,
    department TEXT,
    answers_json TEXT NOT NULL,
    grading_detail_json TEXT NOT NULL,
    objective_score REAL NOT NULL DEFAULT 0,
    subjective_score_machine REAL NOT NULL DEFAULT 0,
    subjective_score_final REAL NOT NULL DEFAULT 0,
    total_score REAL NOT NULL DEFAULT 0,
    review_status TEXT NOT NULL DEFAULT 'pending',
    started_at TEXT,
    submitted_at TEXT NOT NULL,
    reviewed_at TEXT,
    reviewer_note TEXT,
    client_ip TEXT,
    user_agent TEXT,
    UNIQUE(employee_id)
);

CREATE TABLE IF NOT EXISTS review_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    submission_id INTEGER NOT NULL,
    question_id TEXT NOT NULL,
    old_score REAL,
    new_score REAL,
    note TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(submission_id) REFERENCES submissions(id)
);

CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(review_status);
CREATE INDEX IF NOT EXISTS idx_submissions_submitted ON submissions(submitted_at);
CREATE INDEX IF NOT EXISTS idx_review_logs_sub ON review_logs(submission_id);
"""


_initialized = False


def get_connection() -> sqlite3.Connection:
    """创建并返回一个已设置 pragma 的新连接。调用方负责 close。"""
    global _initialized
    DB_PATH.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(DB_PATH), check_same_thread=False)
    conn.row_factory = sqlite3.Row
    if not _initialized:
        conn.executescript(_SCHEMA)
        _initialized = True
    return conn


@contextmanager
def db_cursor():
    """打开连接并自动提交/回滚/关闭。短事务。"""
    conn = get_connection()
    try:
        yield conn
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()


def init_db() -> None:
    """启动时调用一次，确保表存在。"""
    with db_cursor() as conn:
        conn.executescript(_SCHEMA)


# ---------------- 提交记录 CRUD ----------------

def insert_submission(
    *,
    name: str,
    employee_id: str,
    department: str | None,
    answers: dict[str, Any],
    grading_detail: list[dict[str, Any]],
    scores: dict[str, float],
    review_status: str,
    started_at: str | None,
    client_ip: str | None,
    user_agent: str | None,
) -> int:
    submitted_at = now_iso()
    sql = """
        INSERT INTO submissions
        (name, employee_id, department, answers_json, grading_detail_json,
         objective_score, subjective_score_machine, subjective_score_final,
         total_score, review_status, started_at, submitted_at, client_ip, user_agent)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    """
    with db_cursor() as conn:
        cur = conn.execute(
            sql,
            (
                name, employee_id, department,
                json.dumps(answers, ensure_ascii=False),
                json.dumps(grading_detail, ensure_ascii=False),
                scores["objective_score"],
                scores["subjective_score_machine"],
                scores["subjective_score_final"],
                scores["total_score"],
                review_status,
                started_at, submitted_at, client_ip, user_agent,
            ),
        )
        lastrowid = cur.lastrowid
        if lastrowid is None:
            raise RuntimeError("insert_submission 失败：未获取到 lastrowid")
        return int(lastrowid)


def insert_submission_pending(
    *,
    name: str,
    employee_id: str,
    department: str | None,
    answers: dict[str, Any],
    started_at: str | None,
    client_ip: str | None,
    user_agent: str | None,
) -> int:
    """立即保存提交记录，评分状态设为 grading，分数暂为 0。"""
    submitted_at = now_iso()
    sql = """
        INSERT INTO submissions
        (name, employee_id, department, answers_json, grading_detail_json,
         objective_score, subjective_score_machine, subjective_score_final,
         total_score, review_status, started_at, submitted_at, client_ip, user_agent)
        VALUES (?, ?, ?, ?, '[]', 0, 0, 0, 0, 'grading', ?, ?, ?, ?)
    """
    with db_cursor() as conn:
        cur = conn.execute(
            sql,
            (name, employee_id, department, json.dumps(answers, ensure_ascii=False),
             started_at, submitted_at, client_ip, user_agent),
        )
        lastrowid = cur.lastrowid
        if lastrowid is None:
            raise RuntimeError("insert_submission_pending 失败：未获取到 lastrowid")
        return int(lastrowid)


def update_submission_grading_result(
    *,
    submission_id: int,
    grading_detail: list[dict[str, Any]],
    scores: dict[str, float],
    review_status: str,
) -> None:
    """后台评分完成后更新记录。"""
    with db_cursor() as conn:
        conn.execute(
            """UPDATE submissions
               SET grading_detail_json = ?,
                   objective_score = ?,
                   subjective_score_machine = ?,
                   subjective_score_final = ?,
                   total_score = ?,
                   review_status = ?
               WHERE id = ?""",
            (
                json.dumps(grading_detail, ensure_ascii=False),
                scores["objective_score"],
                scores["subjective_score_machine"],
                scores["subjective_score_final"],
                scores["total_score"],
                review_status,
                submission_id,
            ),
        )


def get_submission_status(submission_id: int) -> dict[str, Any] | None:
    """查询提交状态（轻量级，用于前端轮询）。"""
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT id, review_status, total_score, submitted_at FROM submissions WHERE id = ?",
            (submission_id,),
        ).fetchone()
        return dict(row) if row else None


def get_submission(submission_id: int) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT * FROM submissions WHERE id = ?", (submission_id,)
        ).fetchone()
        if not row:
            return None
        d = dict(row)
        d["grading_detail"] = json.loads(d.get("grading_detail_json") or "[]")
        d["answers"] = json.loads(d.get("answers_json") or "{}")
        return d


def list_submissions(
    *,
    keyword: str | None = None,
    review_status: str | None = None,
    sort_by: str = "submitted_at",
    order: str = "desc",
    limit: int = 100,
    offset: int = 0,
) -> list[dict[str, Any]]:
    allowed_sort = {"submitted_at", "total_score", "name", "employee_id", "review_status"}
    sort_by = sort_by if sort_by in allowed_sort else "submitted_at"
    order = "ASC" if order.lower() == "asc" else "DESC"

    sql = "SELECT * FROM submissions WHERE 1=1"
    params: list[Any] = []
    if keyword:
        sql += " AND (name LIKE ? OR employee_id LIKE ?)"
        params += [f"%{keyword}%", f"%{keyword}%"]
    if review_status:
        sql += " AND review_status = ?"
        params.append(review_status)
    sql += f" ORDER BY {sort_by} {order} LIMIT ? OFFSET ?"
    params += [limit, offset]

    with db_cursor() as conn:
        rows = conn.execute(sql, params).fetchall()
        return [dict(r) for r in rows]


def get_stats() -> dict[str, Any]:
    with db_cursor() as conn:
        total = conn.execute("SELECT COUNT(*) c FROM submissions").fetchone()["c"]
        if total == 0:
            return {
                "submitted_count": 0, "avg_score": 0, "max_score": 0, "min_score": 0,
                "pending_review": 0, "low_confidence_count": 0,
            }
        agg = conn.execute(
            "SELECT AVG(total_score) a, MAX(total_score) mx, MIN(total_score) mn FROM submissions"
        ).fetchone()
        pending = conn.execute(
            """SELECT COUNT(*) c FROM submissions
               WHERE review_status IN ('pending','need_review','low_confidence')"""
        ).fetchone()["c"]
        low = conn.execute(
            "SELECT COUNT(*) c FROM submissions WHERE review_status = 'low_confidence'"
        ).fetchone()["c"]
        return {
            "submitted_count": total,
            "avg_score": round(agg["a"], 2),
            "max_score": round(agg["mx"], 2),
            "min_score": round(agg["mn"], 2),
            "pending_review": pending,
            "low_confidence_count": low,
        }


def duplicate_exists(employee_id: str) -> bool:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT 1 FROM submissions WHERE employee_id = ? LIMIT 1", (employee_id,)
        ).fetchone()
        return row is not None


def validate_time_window(started_at: str | None, duration_minutes: int) -> None:
    """校验考试时长是否超时。超时抛 ValueError，由调用方映射为 EXAM_TIMEOUT。"""
    cfg = get_config().exam
    grace = getattr(cfg, "grace_period_seconds", 30)
    if started_at:
        try:
            elapsed = seconds_between(started_at, now_iso())
        except Exception:
            return
        if elapsed > duration_minutes * 60 + grace:
            raise ValueError("EXAM_TIMEOUT")


# ---------------- 复核日志 CRUD ----------------

def insert_review_log(
    *, submission_id: int, question_id: str,
    old_score: float, new_score: float, note: str | None,
    conn: sqlite3.Connection | None = None,
) -> None:
    def _do(c: sqlite3.Connection) -> None:
        c.execute(
            """INSERT INTO review_logs
               (submission_id, question_id, old_score, new_score, note, created_at)
               VALUES (?, ?, ?, ?, ?, ?)""",
            (submission_id, question_id, old_score, new_score, note, now_iso()),
        )
    if conn is not None:
        _do(conn)
    else:
        with db_cursor() as c:
            _do(c)


def list_review_logs(submission_id: int) -> list[dict[str, Any]]:
    with db_cursor() as conn:
        rows = conn.execute(
            """SELECT * FROM review_logs WHERE submission_id = ? ORDER BY created_at ASC""",
            (submission_id,),
        ).fetchall()
        return [dict(r) for r in rows]


def update_submission_after_review(
    *,
    submission_id: int,
    grading_detail: list[dict[str, Any]],
    subjective_score_final: float,
    total_score: float,
    review_status: str,
    reviewer_note: str | None,
) -> None:
    with db_cursor() as conn:
        conn.execute(
            """UPDATE submissions
               SET grading_detail_json = ?,
                   subjective_score_final = ?,
                   total_score = ?,
                   review_status = ?,
                   reviewed_at = ?,
                   reviewer_note = COALESCE(?, reviewer_note)
               WHERE id = ?""",
            (
                json.dumps(grading_detail, ensure_ascii=False),
                subjective_score_final, total_score, review_status,
                now_iso(), reviewer_note, submission_id,
            ),
        )


def apply_review(
    *,
    submission_id: int,
    question_id: str,
    new_score: float,
    note: str | None,
    operator: str = "human",
) -> dict[str, Any]:
    """人工复核：更新指定题目的分数并重新计算总分。"""
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT grading_detail_json, objective_score, subjective_score_machine FROM submissions WHERE id = ?",
            (submission_id,),
        ).fetchone()
        if not row:
            return {"success": False, "code": "NOT_FOUND", "message": "提交记录不存在"}

        details: list[dict[str, Any]] = json.loads(row["grading_detail_json"])
        target = None
        for item in details:
            if item.get("question_id") == question_id:
                target = item
                break
        if target is None:
            return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "未找到对应题目"}

        max_score = float(target.get("max_score", 0))
        if new_score < 0 or new_score > max_score:
            return {"success": False, "code": "REVIEW_SCORE_INVALID", "message": "复核分数非法"}

        old_score = float(target.get("score", 0))
        target["score"] = new_score
        target["final_score"] = new_score
        target["reviewed_by"] = operator
        target["review_note"] = note or ""
        target["review_status"] = "reviewed"

        new_subjective = sum(
            float(d.get("final_score", d.get("score", 0)))
            for d in details
            if d.get("type") in {"short_answer", "essay"}
        )
        new_total = float(row["objective_score"]) + new_subjective

        conn.execute(
            "UPDATE submissions SET grading_detail_json = ?, subjective_score_final = ?, total_score = ?, review_status = 'reviewed' WHERE id = ?",
            (json.dumps(details, ensure_ascii=False), new_subjective, new_total, submission_id),
        )

        insert_review_log(
            submission_id=submission_id,
            question_id=question_id,
            old_score=old_score,
            new_score=new_score,
            note=note,
            conn=conn,
        )

        return {"success": True, "total_score": new_total}


def save_grading_result(
    submission_id: int,
    result: dict[str, Any],
) -> None:
    """保存完整的评分结果（用于重新判分）。"""
    with db_cursor() as conn:
        conn.execute(
            """UPDATE submissions
               SET objective_score = ?,
                   subjective_score_machine = ?,
                   subjective_score_final = ?,
                   total_score = ?,
                   review_status = ?,
                   grading_detail_json = ?
               WHERE id = ?""",
            (
                result["objective_score"],
                result["subjective_score_machine"],
                result["subjective_score_final"],
                result["total_score"],
                result["review_status"],
                json.dumps(result["grading_detail"], ensure_ascii=False),
                submission_id,
            ),
        )


# ---------------- 删除记录 ----------------

def delete_submission(submission_id: int) -> bool:
    """删除单条提交记录及其关联的复核日志。返回是否删除成功。"""
    with db_cursor() as conn:
        conn.execute("DELETE FROM review_logs WHERE submission_id = ?", (submission_id,))
        cur = conn.execute("DELETE FROM submissions WHERE id = ?", (submission_id,))
        return cur.rowcount > 0


def delete_submissions(submission_ids: list[int]) -> int:
    """批量删除提交记录及其关联的复核日志。返回实际删除条数。"""
    if not submission_ids:
        return 0
    placeholders = ",".join("?" * len(submission_ids))
    with db_cursor() as conn:
        conn.execute(
            f"DELETE FROM review_logs WHERE submission_id IN ({placeholders})",
            submission_ids,
        )
        cur = conn.execute(
            f"DELETE FROM submissions WHERE id IN ({placeholders})",
            submission_ids,
        )
        return cur.rowcount
