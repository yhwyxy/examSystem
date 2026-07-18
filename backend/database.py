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
from .utils import now_iso, seconds_between

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
    paper_id TEXT NOT NULL DEFAULT 'default',
    paper_name TEXT,
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
    auto_submit_reason TEXT,
    UNIQUE(employee_id, paper_id)
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

-- 考试会话：服务器端记录的「开始答题」时间，防止客户端篡改。
-- 主键为 (employee_id, paper_id)，支持同一员工跨专业并行开考。
CREATE TABLE IF NOT EXISTS exam_sessions (
    employee_id TEXT NOT NULL,
    paper_id    TEXT NOT NULL DEFAULT 'default',
    started_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (employee_id, paper_id)
);
CREATE INDEX IF NOT EXISTS idx_exam_sessions_created ON exam_sessions(created_at);
"""


_initialized = False


def get_connection() -> sqlite3.Connection:
    """创建并返回一个已设置 pragma 的新连接。调用方负责 close。"""
    global _initialized
    DB_PATH.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(DB_PATH), check_same_thread=False)
    conn.row_factory = sqlite3.Row
    if not _initialized:
        # 先建表（IF NOT EXISTS 不改旧表结构），再迁移补 paper_id 等
        conn.executescript(_SCHEMA)
        _migrate_schema(conn)
        conn.commit()
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
    """启动时调用一次，确保表存在并完成轻量迁移。"""
    with db_cursor() as conn:
        # get_connection 已执行 schema + migrate；此处再兜底一次（幂等）
        _migrate_schema(conn)


def _table_columns(conn: sqlite3.Connection, table: str) -> set[str]:
    rows = conn.execute(f"PRAGMA table_info({table})").fetchall()
    return {str(r["name"] if isinstance(r, sqlite3.Row) else r[1]) for r in rows}


def _migrate_schema(conn: sqlite3.Connection) -> None:
    """为旧库补齐 paper_id / 会话复合主键。"""
    # submissions.paper_id / paper_name
    cols = _table_columns(conn, "submissions")
    if "paper_id" not in cols:
        conn.execute("ALTER TABLE submissions ADD COLUMN paper_id TEXT NOT NULL DEFAULT 'default'")
    if "paper_name" not in cols:
        conn.execute("ALTER TABLE submissions ADD COLUMN paper_name TEXT")
    if "auto_submit_reason" not in cols:
        conn.execute("ALTER TABLE submissions ADD COLUMN auto_submit_reason TEXT")

    # 唯一约束：旧 UNIQUE(employee_id) → UNIQUE(employee_id, paper_id)
    # SQLite 无法直接改约束，用索引兜底；旧约束若仍在则可能阻止同工号跨专业。
    # 检测是否仍有仅 employee_id 的唯一索引，必要时重建表。
    idx_rows = conn.execute("PRAGMA index_list(submissions)").fetchall()
    need_rebuild = False
    for idx in idx_rows:
        # idx: seq, name, unique, origin, partial
        name = idx["name"] if isinstance(idx, sqlite3.Row) else idx[1]
        unique = idx["unique"] if isinstance(idx, sqlite3.Row) else idx[2]
        if not unique:
            continue
        info = conn.execute(f"PRAGMA index_info({name})").fetchall()
        col_names = []
        for r in info:
            cid = r["cid"] if isinstance(r, sqlite3.Row) else r[1]
            # cid 可能为列序号
            tinfo = conn.execute("PRAGMA table_info(submissions)").fetchall()
            for t in tinfo:
                tid = t["cid"] if isinstance(t, sqlite3.Row) else t[0]
                tname = t["name"] if isinstance(t, sqlite3.Row) else t[1]
                if tid == cid:
                    col_names.append(tname)
        if col_names == ["employee_id"]:
            need_rebuild = True
            break

    if need_rebuild:
        conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS submissions_new (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL,
                employee_id TEXT NOT NULL,
                paper_id TEXT NOT NULL DEFAULT 'default',
                paper_name TEXT,
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
                auto_submit_reason TEXT,
                UNIQUE(employee_id, paper_id)
            );
            INSERT INTO submissions_new (
                id, name, employee_id, paper_id, paper_name, department,
                answers_json, grading_detail_json,
                objective_score, subjective_score_machine, subjective_score_final,
                total_score, review_status, started_at, submitted_at, reviewed_at,
                reviewer_note, client_ip, user_agent
            )
            SELECT
                id, name, employee_id,
                COALESCE(NULLIF(paper_id, ''), 'default'),
                paper_name, department,
                answers_json, grading_detail_json,
                objective_score, subjective_score_machine, subjective_score_final,
                total_score, review_status, started_at, submitted_at, reviewed_at,
                reviewer_note, client_ip, user_agent
            FROM submissions;
            DROP TABLE submissions;
            ALTER TABLE submissions_new RENAME TO submissions;
            CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(review_status);
            CREATE INDEX IF NOT EXISTS idx_submissions_submitted ON submissions(submitted_at);
            CREATE INDEX IF NOT EXISTS idx_submissions_paper ON submissions(paper_id);
            """
        )

    # exam_sessions 迁移到复合主键
    sess_cols = _table_columns(conn, "exam_sessions")
    if sess_cols and "paper_id" not in sess_cols:
        conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS exam_sessions_new (
                employee_id TEXT NOT NULL,
                paper_id    TEXT NOT NULL DEFAULT 'default',
                started_at  TEXT NOT NULL,
                created_at  TEXT NOT NULL,
                PRIMARY KEY (employee_id, paper_id)
            );
            INSERT INTO exam_sessions_new (employee_id, paper_id, started_at, created_at)
            SELECT employee_id, 'default', started_at, created_at FROM exam_sessions;
            DROP TABLE exam_sessions;
            ALTER TABLE exam_sessions_new RENAME TO exam_sessions;
            CREATE INDEX IF NOT EXISTS idx_exam_sessions_created ON exam_sessions(created_at);
            """
        )

    # 迁移后再建依赖 paper_id 的索引（旧库在补列前不能建）
    cols = _table_columns(conn, "submissions")
    if "paper_id" in cols:
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_submissions_paper ON submissions(paper_id)"
        )


# ---------------- 提交记录 CRUD ----------------

def insert_submission(
    *,
    name: str,
    employee_id: str,
    paper_id: str,
    paper_name: str | None = None,
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
        (name, employee_id, paper_id, paper_name, department, answers_json, grading_detail_json,
         objective_score, subjective_score_machine, subjective_score_final,
         total_score, review_status, started_at, submitted_at, client_ip, user_agent)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    """
    with db_cursor() as conn:
        cur = conn.execute(
            sql,
            (
                name, employee_id, paper_id, paper_name, department,
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
    paper_id: str,
    paper_name: str | None = None,
    department: str | None,
    answers: dict[str, Any],
    started_at: str | None,
    client_ip: str | None,
    user_agent: str | None,
    auto_submit_reason: str | None = None,
) -> int:
    """立即保存提交记录，评分状态设为 grading，分数暂为 0。"""
    submitted_at = now_iso()
    sql = """
        INSERT INTO submissions
        (name, employee_id, paper_id, paper_name, department, answers_json, grading_detail_json,
         objective_score, subjective_score_machine, subjective_score_final,
         total_score, review_status, started_at, submitted_at, client_ip, user_agent, auto_submit_reason)
        VALUES (?, ?, ?, ?, ?, ?, '[]', 0, 0, 0, 0, 'grading', ?, ?, ?, ?, ?)
    """
    with db_cursor() as conn:
        cur = conn.execute(
            sql,
            (name, employee_id, paper_id, paper_name, department,
             json.dumps(answers, ensure_ascii=False),
             started_at, submitted_at, client_ip, user_agent, auto_submit_reason),
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
    paper_id: str | None = None,
    sort_by: str = "submitted_at",
    order: str = "desc",
    limit: int = 100,
    offset: int = 0,
) -> list[dict[str, Any]]:
    allowed_sort = {"submitted_at", "total_score", "name", "employee_id", "review_status", "paper_id"}
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
    if paper_id:
        sql += " AND paper_id = ?"
        params.append(paper_id)
    sql += f" ORDER BY {sort_by} {order} LIMIT ? OFFSET ?"
    params += [limit, offset]

    with db_cursor() as conn:
        rows = conn.execute(sql, params).fetchall()
        return [dict(r) for r in rows]


def get_stats(paper_id: str | None = None) -> dict[str, Any]:
    where = ""
    params: list[Any] = []
    if paper_id:
        where = " WHERE paper_id = ?"
        params = [paper_id]
    with db_cursor() as conn:
        total = conn.execute(f"SELECT COUNT(*) c FROM submissions{where}", params).fetchone()["c"]
        if total == 0:
            return {
                "submitted_count": 0, "avg_score": 0, "max_score": 0, "min_score": 0,
                "pending_review": 0, "low_confidence_count": 0, "paper_id": paper_id,
            }
        agg = conn.execute(
            f"SELECT AVG(total_score) a, MAX(total_score) mx, MIN(total_score) mn FROM submissions{where}",
            params,
        ).fetchone()
        pending_sql = (
            f"SELECT COUNT(*) c FROM submissions{where} "
            + ("AND" if where else "WHERE")
            + " review_status IN ('pending','need_review','low_confidence')"
        )
        pending = conn.execute(pending_sql, params).fetchone()["c"]
        low_sql = (
            f"SELECT COUNT(*) c FROM submissions{where} "
            + ("AND" if where else "WHERE")
            + " review_status = 'low_confidence'"
        )
        low = conn.execute(low_sql, params).fetchone()["c"]
        return {
            "submitted_count": total,
            "avg_score": round(agg["a"], 2),
            "max_score": round(agg["mx"], 2),
            "min_score": round(agg["mn"], 2),
            "pending_review": pending,
            "low_confidence_count": low,
            "paper_id": paper_id,
        }


def submission_count(paper_id: str | None = None) -> int:
    with db_cursor() as conn:
        if paper_id:
            row = conn.execute(
                "SELECT COUNT(*) c FROM submissions WHERE paper_id = ?", (paper_id,)
            ).fetchone()
        else:
            row = conn.execute("SELECT COUNT(*) c FROM submissions").fetchone()
        return int(row["c"])


def duplicate_exists(employee_id: str, paper_id: str | None = None) -> bool:
    with db_cursor() as conn:
        if paper_id:
            row = conn.execute(
                "SELECT 1 FROM submissions WHERE employee_id = ? AND paper_id = ? LIMIT 1",
                (employee_id, paper_id),
            ).fetchone()
        else:
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
    sub_question_id: str | None = None,
) -> dict[str, Any]:
    """人工复核：更新指定题目（或复合题子题）的分数并重新计算总分。"""
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
            if str(item.get("question_id")) == str(question_id):
                target = item
                break
        if target is None:
            return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "未找到对应题目"}

        log_qid = str(question_id)

        if sub_question_id:
            if not target.get("is_composite") or not isinstance(target.get("sub_results"), list):
                return {"success": False, "code": "NOT_COMPOSITE", "message": "题目不是复合题"}
            sub = next(
                (
                    s
                    for s in target["sub_results"]
                    if str(s.get("sub_question_id")) == str(sub_question_id)
                ),
                None,
            )
            if sub is None:
                return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "未找到子题"}
            max_score = float(sub.get("max_score", 0))
            if new_score < 0 or new_score > max_score:
                return {"success": False, "code": "REVIEW_SCORE_INVALID", "message": "复核分数非法"}
            old_score = float(sub.get("final_score", sub.get("score", 0)))
            sub["score"] = new_score
            sub["final_score"] = new_score
            sub["reviewed_by"] = operator
            sub["review_note"] = note or ""
            sub["review_status"] = "reviewed"
            target["score"] = sum(float(s.get("score") or 0) for s in target["sub_results"])
            target["final_score"] = sum(
                float(s.get("final_score", s.get("score") or 0)) for s in target["sub_results"]
            )
            target["reviewed_by"] = operator
            if all(s.get("review_status") == "reviewed" for s in target["sub_results"]):
                target["review_status"] = "reviewed"
            else:
                target["review_status"] = "need_review"
            log_qid = f"{question_id}#{sub_question_id}"
        else:
            max_score = float(target.get("max_score", 0))
            if new_score < 0 or new_score > max_score:
                return {"success": False, "code": "REVIEW_SCORE_INVALID", "message": "复核分数非法"}

            old_score = float(target.get("final_score", target.get("score", 0)))
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

        still_need = any(
            d.get("review_status") in {"need_review", "low_confidence", "pending"}
            for d in details
            if d.get("type") in {"short_answer", "essay"}
        )
        review_status = "need_review" if still_need else "reviewed"

        conn.execute(
            "UPDATE submissions SET grading_detail_json = ?, subjective_score_final = ?, total_score = ?, review_status = ? WHERE id = ?",
            (json.dumps(details, ensure_ascii=False), new_subjective, new_total, review_status, submission_id),
        )

        insert_review_log(
            submission_id=submission_id,
            question_id=log_qid,
            old_score=old_score,
            new_score=new_score,
            note=note,
            conn=conn,
        )

        return {
            "success": True,
            "total_score": new_total,
            "subjective_score_final": new_subjective,
            "review_status": review_status,
            "old_score": old_score,
            "new_score": new_score,
        }


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

# ---------------- 考试会话 CRUD ----------------

def upsert_exam_session(*, employee_id: str, paper_id: str, started_at: str) -> None:
    """记录或覆盖某员工在指定专业的服务器端开始时间。"""
    with db_cursor() as conn:
        conn.execute(
            """INSERT INTO exam_sessions (employee_id, paper_id, started_at, created_at)
               VALUES (?, ?, ?, ?)
               ON CONFLICT(employee_id, paper_id) DO UPDATE SET
                 started_at = excluded.started_at,
                 created_at = excluded.created_at""",
            (employee_id, paper_id, started_at, now_iso()),
        )

def pop_exam_session(employee_id: str, paper_id: str) -> str | None:
    """读取并删除某员工在指定专业的开始时间，返回 ISO 字符串或 None。"""
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT started_at FROM exam_sessions WHERE employee_id = ? AND paper_id = ?",
            (employee_id, paper_id),
        ).fetchone()
        if not row:
            return None
        conn.execute(
            "DELETE FROM exam_sessions WHERE employee_id = ? AND paper_id = ?",
            (employee_id, paper_id),
        )
        return str(row["started_at"])

def cleanup_exam_sessions(older_than_seconds: int = 86400) -> int:
    """清理超过 older_than_seconds 的残留会话，避免长期未提交的记录堆积。返回删除条数。"""
    from datetime import datetime, timezone
    cutoff_dt = datetime.now(timezone.utc).timestamp() - older_than_seconds
    cutoff_iso = datetime.fromtimestamp(cutoff_dt, timezone.utc).isoformat()
    with db_cursor() as conn:
        cur = conn.execute(
            "DELETE FROM exam_sessions WHERE created_at < ?", (cutoff_iso,)
        )
        return cur.rowcount
