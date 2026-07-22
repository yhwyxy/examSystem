"""SQLite 数据库连接与基础 CRUD。

设计：
- 使用 WAL 模式，支持近似同时提交（NFR-006）。
- 每次操作新建连接，避免跨线程共享连接（FastAPI 同步端点在 threadpool 中运行）。
- 各服务通过本模块的函数访问数据库，避免循环依赖。
"""
from __future__ import annotations

import hashlib
import json
import secrets
import sqlite3
from contextlib import contextmanager
from datetime import datetime
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

CREATE TABLE IF NOT EXISTS exam_runs (
    id                   TEXT PRIMARY KEY,
    paper_id             TEXT NOT NULL,
    round_no             INTEGER NOT NULL,
    public_token_hash    TEXT UNIQUE,
    status               TEXT NOT NULL,
    duration_minutes     INTEGER NOT NULL,
    snapshot_path        TEXT,
    snapshot_hash        TEXT,
    is_legacy            INTEGER NOT NULL DEFAULT 0,
    opened_at            TEXT NOT NULL,
    closing_started_at   TEXT,
    finalize_at          TEXT,
    closed_at            TEXT,
    created_at           TEXT NOT NULL,
    UNIQUE(paper_id, round_no)
);

CREATE TABLE IF NOT EXISTS exam_sessions (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL,
    employee_id           TEXT NOT NULL,
    name                  TEXT NOT NULL,
    department            TEXT,
    session_token_hash    TEXT NOT NULL,
    started_at            TEXT NOT NULL,
    deadline_at           TEXT NOT NULL,
    draft_json            TEXT NOT NULL DEFAULT '{}',
    draft_revision        INTEGER NOT NULL DEFAULT 0,
    draft_saved_at        TEXT,
    status                TEXT NOT NULL,
    client_ip             TEXT,
    user_agent            TEXT,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL,
    UNIQUE(run_id, employee_id)
);

CREATE TABLE IF NOT EXISTS submissions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    employee_id TEXT NOT NULL,
    paper_id TEXT NOT NULL DEFAULT 'default',
    paper_name TEXT,
    run_id TEXT NOT NULL,
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
    UNIQUE(employee_id, run_id)
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
CREATE INDEX IF NOT EXISTS idx_exam_runs_paper_status ON exam_runs(paper_id, status);
CREATE INDEX IF NOT EXISTS idx_exam_sessions_created ON exam_sessions(created_at);
"""
# 注意：依赖 run_id / paper_id 的索引不能写在 _SCHEMA 里。
# 旧库 CREATE TABLE IF NOT EXISTS 不会改表结构，executescript 会在迁移前
# 因「no such column: run_id」失败。这些索引在 _migrate_schema 末尾按列存在情况创建。


_initialized = False


# ---------------- token helpers ----------------

def new_token() -> str:
    return secrets.token_urlsafe(32)


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


def get_connection() -> sqlite3.Connection:
    """创建并返回一个已设置 pragma 的新连接。调用方负责 close。"""
    global _initialized
    DB_PATH.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(DB_PATH), check_same_thread=False)
    conn.row_factory = sqlite3.Row
    if not _initialized:
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
        _migrate_schema(conn)


def _table_columns(conn: sqlite3.Connection, table: str) -> set[str]:
    try:
        rows = conn.execute(f"PRAGMA table_info({table})").fetchall()
    except sqlite3.Error:
        return set()
    return {str(r["name"] if isinstance(r, sqlite3.Row) else r[1]) for r in rows}


def _table_exists(conn: sqlite3.Connection, table: str) -> bool:
    row = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (table,)
    ).fetchone()
    return row is not None


def _unique_index_columns(conn: sqlite3.Connection, table: str) -> list[list[str]]:
    result: list[list[str]] = []
    if not _table_exists(conn, table):
        return result
    idx_rows = conn.execute(f"PRAGMA index_list({table})").fetchall()
    tinfo = conn.execute(f"PRAGMA table_info({table})").fetchall()
    cid_to_name = {
        (t["cid"] if isinstance(t, sqlite3.Row) else t[0]): (
            t["name"] if isinstance(t, sqlite3.Row) else t[1]
        )
        for t in tinfo
    }
    for idx in idx_rows:
        unique = idx["unique"] if isinstance(idx, sqlite3.Row) else idx[2]
        if not unique:
            continue
        name = idx["name"] if isinstance(idx, sqlite3.Row) else idx[1]
        info = conn.execute(f"PRAGMA index_info({name})").fetchall()
        cols = []
        for r in info:
            cid = r["cid"] if isinstance(r, sqlite3.Row) else r[1]
            cols.append(str(cid_to_name.get(cid, "")))
        result.append(cols)
    return result


def _migrate_schema(conn: sqlite3.Connection) -> None:
    """迁移：paper_id 时代 → exam runs / run_id / 新会话表。"""
    # 确保 exam_runs 存在（旧库可能只有 submissions）
    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS exam_runs (
            id                   TEXT PRIMARY KEY,
            paper_id             TEXT NOT NULL,
            round_no             INTEGER NOT NULL,
            public_token_hash    TEXT UNIQUE,
            status               TEXT NOT NULL,
            duration_minutes     INTEGER NOT NULL,
            snapshot_path        TEXT,
            snapshot_hash        TEXT,
            is_legacy            INTEGER NOT NULL DEFAULT 0,
            opened_at            TEXT NOT NULL,
            closing_started_at   TEXT,
            finalize_at          TEXT,
            closed_at            TEXT,
            created_at           TEXT NOT NULL,
            UNIQUE(paper_id, round_no)
        );
        CREATE INDEX IF NOT EXISTS idx_exam_runs_paper_status ON exam_runs(paper_id, status);
        """
    )

    # ---- submissions: 旧列补齐 ----
    if _table_exists(conn, "submissions"):
        cols = _table_columns(conn, "submissions")
        if "paper_id" not in cols:
            conn.execute("ALTER TABLE submissions ADD COLUMN paper_id TEXT NOT NULL DEFAULT 'default'")
        if "paper_name" not in cols:
            conn.execute("ALTER TABLE submissions ADD COLUMN paper_name TEXT")
        if "auto_submit_reason" not in cols:
            conn.execute("ALTER TABLE submissions ADD COLUMN auto_submit_reason TEXT")

        cols = _table_columns(conn, "submissions")
        unique_cols = _unique_index_columns(conn, "submissions")
        has_run_id = "run_id" in cols
        has_run_unique = any(c == ["employee_id", "run_id"] for c in unique_cols)

        if not has_run_id or not has_run_unique:
            _migrate_submissions_to_run_id(conn)

    # ---- exam_sessions: 旧会话表直接丢弃重建 ----
    need_session_rebuild = True
    if _table_exists(conn, "exam_sessions"):
        sess_cols = _table_columns(conn, "exam_sessions")
        if "session_token_hash" in sess_cols and "run_id" in sess_cols and "id" in sess_cols:
            need_session_rebuild = False
    if need_session_rebuild:
        conn.executescript(
            """
            DROP TABLE IF EXISTS exam_sessions;
            CREATE TABLE exam_sessions (
                id                    TEXT PRIMARY KEY,
                run_id                TEXT NOT NULL,
                employee_id           TEXT NOT NULL,
                name                  TEXT NOT NULL,
                department            TEXT,
                session_token_hash    TEXT NOT NULL,
                started_at            TEXT NOT NULL,
                deadline_at           TEXT NOT NULL,
                draft_json            TEXT NOT NULL DEFAULT '{}',
                draft_revision        INTEGER NOT NULL DEFAULT 0,
                draft_saved_at        TEXT,
                status                TEXT NOT NULL,
                client_ip             TEXT,
                user_agent            TEXT,
                created_at            TEXT NOT NULL,
                updated_at            TEXT NOT NULL,
                UNIQUE(run_id, employee_id)
            );
            CREATE INDEX IF NOT EXISTS idx_exam_sessions_run ON exam_sessions(run_id);
            CREATE INDEX IF NOT EXISTS idx_exam_sessions_created ON exam_sessions(created_at);
            """
        )

    # 索引兜底（含依赖迁移列的索引；见 _SCHEMA 注释）
    if _table_exists(conn, "submissions"):
        cols = _table_columns(conn, "submissions")
        if "paper_id" in cols:
            conn.execute("CREATE INDEX IF NOT EXISTS idx_submissions_paper ON submissions(paper_id)")
        if "run_id" in cols:
            conn.execute("CREATE INDEX IF NOT EXISTS idx_submissions_run ON submissions(run_id)")
    if _table_exists(conn, "exam_sessions"):
        sess_cols = _table_columns(conn, "exam_sessions")
        if "run_id" in sess_cols:
            conn.execute("CREATE INDEX IF NOT EXISTS idx_exam_sessions_run ON exam_sessions(run_id)")
        if "created_at" in sess_cols:
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_exam_sessions_created ON exam_sessions(created_at)"
            )


def _migrate_submissions_to_run_id(conn: sqlite3.Connection) -> None:
    """为已有提交创建 legacy 轮次并重建 UNIQUE(employee_id, run_id)。"""
    duration = 60
    try:
        duration = int(get_config().exam.duration_minutes)
    except Exception:
        pass
    now = now_iso()

    paper_ids = [
        str(r[0])
        for r in conn.execute(
            "SELECT DISTINCT COALESCE(NULLIF(paper_id, ''), 'default') FROM submissions"
        ).fetchall()
    ]
    paper_to_run: dict[str, str] = {}
    for paper_id in paper_ids:
        existing = conn.execute(
            "SELECT id FROM exam_runs WHERE paper_id = ? AND is_legacy = 1 ORDER BY round_no ASC LIMIT 1",
            (paper_id,),
        ).fetchone()
        if existing:
            paper_to_run[paper_id] = str(existing["id"] if isinstance(existing, sqlite3.Row) else existing[0])
            continue
        run_id = f"legacy-{paper_id}-{secrets.token_hex(8)}"
        opened = conn.execute(
            "SELECT MIN(submitted_at) FROM submissions WHERE COALESCE(NULLIF(paper_id, ''), 'default') = ?",
            (paper_id,),
        ).fetchone()
        opened_at = (opened[0] if opened and opened[0] else now)
        max_round = conn.execute(
            "SELECT COALESCE(MAX(round_no), 0) FROM exam_runs WHERE paper_id = ?",
            (paper_id,),
        ).fetchone()
        round_no = int(max_round[0] if max_round else 0) + 1
        conn.execute(
            """INSERT INTO exam_runs
               (id, paper_id, round_no, public_token_hash, status, duration_minutes,
                snapshot_path, snapshot_hash, is_legacy, opened_at, closing_started_at,
                finalize_at, closed_at, created_at)
               VALUES (?, ?, ?, NULL, 'closed', ?, NULL, NULL, 1, ?, NULL, NULL, ?, ?)""",
            (run_id, paper_id, round_no, duration, opened_at, now, now),
        )
        paper_to_run[paper_id] = run_id

    # 若 submissions 尚无 run_id 列，先加可空列再回填，再 rebuild
    cols = _table_columns(conn, "submissions")
    if "run_id" not in cols:
        conn.execute("ALTER TABLE submissions ADD COLUMN run_id TEXT")
        for paper_id, run_id in paper_to_run.items():
            conn.execute(
                "UPDATE submissions SET run_id = ? WHERE COALESCE(NULLIF(paper_id, ''), 'default') = ?",
                (run_id, paper_id),
            )
        # 孤儿
        conn.execute(
            "UPDATE submissions SET run_id = ? WHERE run_id IS NULL OR run_id = ''",
            (f"legacy-orphan-{secrets.token_hex(6)}",),
        )
        # 为孤儿建 run（若需要）
        orphans = conn.execute(
            "SELECT DISTINCT run_id, COALESCE(NULLIF(paper_id, ''), 'default') FROM submissions WHERE run_id LIKE 'legacy-orphan-%'"
        ).fetchall()
        for row in orphans:
            rid = str(row[0])
            pid = str(row[1])
            exists = conn.execute("SELECT 1 FROM exam_runs WHERE id = ?", (rid,)).fetchone()
            if not exists:
                conn.execute(
                    """INSERT INTO exam_runs
                       (id, paper_id, round_no, public_token_hash, status, duration_minutes,
                        snapshot_path, snapshot_hash, is_legacy, opened_at, closed_at, created_at)
                       VALUES (?, ?, 1, NULL, 'closed', ?, NULL, NULL, 1, ?, ?, ?)""",
                    (rid, pid, duration, now, now, now),
                )

    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS submissions_new (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            name TEXT NOT NULL,
            employee_id TEXT NOT NULL,
            paper_id TEXT NOT NULL DEFAULT 'default',
            paper_name TEXT,
            run_id TEXT NOT NULL,
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
            UNIQUE(employee_id, run_id)
        );
        """
    )
    conn.execute(
        """
        INSERT INTO submissions_new (
            id, name, employee_id, paper_id, paper_name, run_id, department,
            answers_json, grading_detail_json,
            objective_score, subjective_score_machine, subjective_score_final,
            total_score, review_status, started_at, submitted_at, reviewed_at,
            reviewer_note, client_ip, user_agent, auto_submit_reason
        )
        SELECT
            id, name, employee_id,
            COALESCE(NULLIF(paper_id, ''), 'default'),
            paper_name, run_id, department,
            answers_json, grading_detail_json,
            objective_score, subjective_score_machine, subjective_score_final,
            total_score, review_status, started_at, submitted_at, reviewed_at,
            reviewer_note, client_ip, user_agent, auto_submit_reason
        FROM submissions
        """
    )
    conn.executescript(
        """
        DROP TABLE submissions;
        ALTER TABLE submissions_new RENAME TO submissions;
        CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(review_status);
        CREATE INDEX IF NOT EXISTS idx_submissions_submitted ON submissions(submitted_at);
        CREATE INDEX IF NOT EXISTS idx_submissions_paper ON submissions(paper_id);
        CREATE INDEX IF NOT EXISTS idx_submissions_run ON submissions(run_id);
        """
    )


# ---------------- 提交记录 CRUD ----------------

def insert_submission(
    *,
    name: str,
    employee_id: str,
    paper_id: str,
    run_id: str,
    paper_name: str | None = None,
    department: str | None,
    answers: dict[str, Any],
    grading_detail: list[dict[str, Any]],
    scores: dict[str, float],
    review_status: str,
    started_at: str | None,
    client_ip: str | None,
    user_agent: str | None,
    auto_submit_reason: str | None = None,
) -> int:
    submitted_at = now_iso()
    sql = """
        INSERT INTO submissions
        (name, employee_id, paper_id, paper_name, run_id, department, answers_json, grading_detail_json,
         objective_score, subjective_score_machine, subjective_score_final,
         total_score, review_status, started_at, submitted_at, client_ip, user_agent, auto_submit_reason)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    """
    with db_cursor() as conn:
        cur = conn.execute(
            sql,
            (
                name, employee_id, paper_id, paper_name, run_id, department,
                json.dumps(answers, ensure_ascii=False),
                json.dumps(grading_detail, ensure_ascii=False),
                scores["objective_score"],
                scores["subjective_score_machine"],
                scores["subjective_score_final"],
                scores["total_score"],
                review_status,
                started_at, submitted_at, client_ip, user_agent, auto_submit_reason,
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
    run_id: str,
    paper_name: str | None = None,
    department: str | None,
    answers: dict[str, Any],
    started_at: str | None,
    client_ip: str | None,
    user_agent: str | None,
    auto_submit_reason: str | None = None,
    conn: sqlite3.Connection | None = None,
) -> int:
    """立即保存提交记录，评分状态设为 grading，分数暂为 0。"""
    submitted_at = now_iso()
    sql = """
        INSERT INTO submissions
        (name, employee_id, paper_id, paper_name, run_id, department, answers_json, grading_detail_json,
         objective_score, subjective_score_machine, subjective_score_final,
         total_score, review_status, started_at, submitted_at, client_ip, user_agent, auto_submit_reason)
        VALUES (?, ?, ?, ?, ?, ?, ?, '[]', 0, 0, 0, 0, 'grading', ?, ?, ?, ?, ?)
    """
    params = (
        name, employee_id, paper_id, paper_name, run_id, department,
        json.dumps(answers, ensure_ascii=False),
        started_at, submitted_at, client_ip, user_agent, auto_submit_reason,
    )

    def _do(c: sqlite3.Connection) -> int:
        cur = c.execute(sql, params)
        lastrowid = cur.lastrowid
        if lastrowid is None:
            raise RuntimeError("insert_submission_pending 失败：未获取到 lastrowid")
        return int(lastrowid)

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


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
            "SELECT id, review_status, total_score, submitted_at, run_id FROM submissions WHERE id = ?",
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
    run_id: str | None = None,
    sort_by: str = "submitted_at",
    order: str = "desc",
    limit: int = 100,
    offset: int = 0,
) -> list[dict[str, Any]]:
    allowed_sort = {
        "submitted_at", "total_score", "name", "employee_id",
        "review_status", "paper_id", "run_id",
    }
    sort_by = sort_by if sort_by in allowed_sort else "submitted_at"
    order = "ASC" if order.lower() == "asc" else "DESC"

    sql = """
        SELECT s.*, er.round_no AS round_no, er.is_legacy AS run_is_legacy
        FROM submissions s
        LEFT JOIN exam_runs er ON er.id = s.run_id
        WHERE 1=1
    """
    params: list[Any] = []
    if keyword:
        sql += " AND (s.name LIKE ? OR s.employee_id LIKE ?)"
        params += [f"%{keyword}%", f"%{keyword}%"]
    if review_status:
        sql += " AND s.review_status = ?"
        params.append(review_status)
    if paper_id:
        sql += " AND s.paper_id = ?"
        params.append(paper_id)
    if run_id:
        sql += " AND s.run_id = ?"
        params.append(run_id)
    sql += f" ORDER BY s.{sort_by} {order} LIMIT ? OFFSET ?"
    params += [limit, offset]

    with db_cursor() as conn:
        rows = conn.execute(sql, params).fetchall()
        return [dict(r) for r in rows]


def get_stats(paper_id: str | None = None, run_id: str | None = None) -> dict[str, Any]:
    where_parts: list[str] = []
    params: list[Any] = []
    if paper_id:
        where_parts.append("paper_id = ?")
        params.append(paper_id)
    if run_id:
        where_parts.append("run_id = ?")
        params.append(run_id)
    where = (" WHERE " + " AND ".join(where_parts)) if where_parts else ""
    with db_cursor() as conn:
        total = conn.execute(f"SELECT COUNT(*) c FROM submissions{where}", params).fetchone()["c"]
        if total == 0:
            return {
                "submitted_count": 0, "avg_score": 0, "max_score": 0, "min_score": 0,
                "pending_review": 0, "low_confidence_count": 0,
                "paper_id": paper_id, "run_id": run_id,
            }
        agg = conn.execute(
            f"SELECT AVG(total_score) a, MAX(total_score) mx, MIN(total_score) mn FROM submissions{where}",
            params,
        ).fetchone()
        and_or_where = " AND " if where else " WHERE "
        pending = conn.execute(
            f"SELECT COUNT(*) c FROM submissions{where}{and_or_where}"
            "review_status IN ('pending','need_review','low_confidence')",
            params,
        ).fetchone()["c"]
        low = conn.execute(
            f"SELECT COUNT(*) c FROM submissions{where}{and_or_where} review_status = 'low_confidence'",
            params,
        ).fetchone()["c"]
        return {
            "submitted_count": total,
            "avg_score": round(agg["a"], 2),
            "max_score": round(agg["mx"], 2),
            "min_score": round(agg["mn"], 2),
            "pending_review": pending,
            "low_confidence_count": low,
            "paper_id": paper_id,
            "run_id": run_id,
        }


def submission_count(paper_id: str | None = None, run_id: str | None = None) -> int:
    with db_cursor() as conn:
        if run_id:
            row = conn.execute(
                "SELECT COUNT(*) c FROM submissions WHERE run_id = ?", (run_id,)
            ).fetchone()
        elif paper_id:
            row = conn.execute(
                "SELECT COUNT(*) c FROM submissions WHERE paper_id = ?", (paper_id,)
            ).fetchone()
        else:
            row = conn.execute("SELECT COUNT(*) c FROM submissions").fetchone()
        return int(row["c"])


def duplicate_exists(employee_id: str, paper_id: str | None = None) -> bool:
    """兼容旧接口：按 paper 判断是否已有提交。"""
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


def duplicate_exists_for_run(employee_id: str, run_id: str) -> bool:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT 1 FROM submissions WHERE employee_id = ? AND run_id = ? LIMIT 1",
            (employee_id, run_id),
        ).fetchone()
        return row is not None


def get_submission_for_run(employee_id: str, run_id: str) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT * FROM submissions WHERE employee_id = ? AND run_id = ? LIMIT 1",
            (employee_id, run_id),
        ).fetchone()
        return dict(row) if row else None


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


def list_pending_grading_submissions() -> list[dict[str, Any]]:
    """进程恢复：列出尚未完成评分的提交。"""
    with db_cursor() as conn:
        rows = conn.execute(
            """SELECT id, paper_id, run_id, answers_json FROM submissions
               WHERE review_status = 'grading'"""
        ).fetchall()
        out = []
        for r in rows:
            d = dict(r)
            d["answers"] = json.loads(d.get("answers_json") or "{}")
            out.append(d)
        return out


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
    from .question_loader import SUBJECTIVE_TYPES

    with db_cursor() as conn:
        row = conn.execute(
            "SELECT grading_detail_json, objective_score, subjective_score_machine FROM submissions WHERE id = ?",
            (submission_id,),
        ).fetchone()
        if not row:
            return {"success": False, "code": "NOT_FOUND", "message": "提交记录不存在"}

        details: list[dict[str, Any]] = json.loads(row["grading_detail_json"])
        target = None
        for d in details:
            if str(d.get("question_id")) == str(question_id):
                target = d
                break
        if target is None:
            return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "题目不存在"}

        if sub_question_id and isinstance(target.get("sub_results"), list):
            sub = next(
                (s for s in target["sub_results"] if str(s.get("sub_question_id")) == str(sub_question_id)),
                None,
            )
            if sub is None:
                return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "子题不存在"}
            old_score = float(sub.get("final_score", sub.get("score", 0)) or 0)
            sub["final_score"] = float(new_score)
            sub["score"] = float(new_score)
            sub["review_status"] = "reviewed"
            sub["manually_reviewed"] = True
            sub["reviewed_by"] = operator
            sub["low_confidence"] = False
            sub["need_manual_review"] = False
            if note is not None:
                sub["reviewer_note"] = note
                sub["review_note"] = note
            # 汇总父题
            target["final_score"] = sum(
                float(s.get("final_score", s.get("score", 0)) or 0) for s in target["sub_results"]
            )
            target["score"] = target["final_score"]
            target["manually_reviewed"] = True
            target["reviewed_by"] = operator
            target["review_status"] = "reviewed"
            target["low_confidence"] = False
            target["need_manual_review"] = False
            if note is not None:
                target["reviewer_note"] = note
                target["review_note"] = note
        else:
            old_score = float(target.get("final_score", target.get("score", 0)) or 0)
            target["final_score"] = float(new_score)
            target["score"] = float(new_score)
            target["manually_reviewed"] = True
            target["reviewed_by"] = operator
            target["review_status"] = "reviewed"
            target["low_confidence"] = False
            target["need_manual_review"] = False
            if note is not None:
                target["reviewer_note"] = note
                target["review_note"] = note

        subjective_final = sum(
            float(d.get("final_score", 0) or 0)
            for d in details
            if d.get("type") in SUBJECTIVE_TYPES
        )
        total = round(float(row["objective_score"] or 0) + subjective_final, 6)
        all_reviewed = all(
            d.get("review_status") == "reviewed" or d.get("type") not in SUBJECTIVE_TYPES
            for d in details
        )
        review_status = "reviewed" if all_reviewed else "pending"

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
                json.dumps(details, ensure_ascii=False),
                subjective_final, total, review_status,
                now_iso(), note, submission_id,
            ),
        )
        conn.execute(
            """INSERT INTO review_logs
               (submission_id, question_id, old_score, new_score, note, created_at)
               VALUES (?, ?, ?, ?, ?, ?)""",
            (
                submission_id,
                f"{question_id}:{sub_question_id}" if sub_question_id else question_id,
                old_score, float(new_score), note, now_iso(),
            ),
        )
        return {"success": True, "total_score": total, "review_status": review_status}


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


# ---------------- exam_runs CRUD ----------------

def create_exam_run(
    *,
    run_id: str,
    paper_id: str,
    round_no: int,
    public_token_hash: str | None,
    status: str,
    duration_minutes: int,
    snapshot_path: str | None,
    snapshot_hash: str | None,
    is_legacy: int = 0,
    opened_at: str | None = None,
    created_at: str | None = None,
    conn: sqlite3.Connection | None = None,
) -> dict[str, Any]:
    opened = opened_at or now_iso()
    created = created_at or opened
    sql = """
        INSERT INTO exam_runs
        (id, paper_id, round_no, public_token_hash, status, duration_minutes,
         snapshot_path, snapshot_hash, is_legacy, opened_at, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    """
    params = (
        run_id, paper_id, round_no, public_token_hash, status, duration_minutes,
        snapshot_path, snapshot_hash, is_legacy, opened, created,
    )

    def _do(c: sqlite3.Connection) -> dict[str, Any]:
        c.execute(sql, params)
        row = c.execute("SELECT * FROM exam_runs WHERE id = ?", (run_id,)).fetchone()
        return dict(row) if row else {"id": run_id}

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


def get_run_by_id(run_id: str) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute("SELECT * FROM exam_runs WHERE id = ?", (run_id,)).fetchone()
        return dict(row) if row else None


def get_run_by_public_token_hash(token_hash: str) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT * FROM exam_runs WHERE public_token_hash = ?", (token_hash,)
        ).fetchone()
        return dict(row) if row else None


def get_active_run_for_paper(paper_id: str, conn: sqlite3.Connection | None = None) -> dict[str, Any] | None:
    sql = """
        SELECT * FROM exam_runs
        WHERE paper_id = ? AND status IN ('open', 'closing')
        ORDER BY round_no DESC LIMIT 1
    """

    def _do(c: sqlite3.Connection) -> dict[str, Any] | None:
        row = c.execute(sql, (paper_id,)).fetchone()
        return dict(row) if row else None

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


def get_latest_run_for_paper(paper_id: str) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute(
            """SELECT * FROM exam_runs WHERE paper_id = ?
               ORDER BY round_no DESC LIMIT 1""",
            (paper_id,),
        ).fetchone()
        return dict(row) if row else None


def max_round_no(paper_id: str, conn: sqlite3.Connection | None = None) -> int:
    def _do(c: sqlite3.Connection) -> int:
        row = c.execute(
            "SELECT COALESCE(MAX(round_no), 0) AS m FROM exam_runs WHERE paper_id = ?",
            (paper_id,),
        ).fetchone()
        return int(row["m"] if isinstance(row, sqlite3.Row) else row[0])

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


def has_any_run(paper_id: str) -> bool:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT 1 FROM exam_runs WHERE paper_id = ? LIMIT 1", (paper_id,)
        ).fetchone()
        return row is not None


def list_runs_for_paper(paper_id: str) -> list[dict[str, Any]]:
    with db_cursor() as conn:
        rows = conn.execute(
            "SELECT * FROM exam_runs WHERE paper_id = ? ORDER BY round_no ASC",
            (paper_id,),
        ).fetchall()
        return [dict(r) for r in rows]


def purge_runs_for_paper(paper_id: str) -> dict[str, Any]:
    """删除某专业全部 exam_runs 与关联 exam_sessions。

    成绩 submissions 保留（run_id 成为历史引用，列表用 LEFT JOIN 仍可查）。
    若存在 open/closing 活动轮次则拒绝。
    """
    with db_cursor() as conn:
        active = conn.execute(
            """SELECT id, status FROM exam_runs
               WHERE paper_id = ? AND status IN ('open', 'closing')
               LIMIT 1""",
            (paper_id,),
        ).fetchone()
        if active:
            st = active["status"] if isinstance(active, sqlite3.Row) else active[1]
            raise ValueError(f"ACTIVE_RUN:{st}")
        runs = conn.execute(
            "SELECT id, snapshot_path FROM exam_runs WHERE paper_id = ?",
            (paper_id,),
        ).fetchall()
        run_meta: list[dict[str, Any]] = []
        run_ids: list[str] = []
        for r in runs:
            rid = str(r["id"] if isinstance(r, sqlite3.Row) else r[0])
            snap = r["snapshot_path"] if isinstance(r, sqlite3.Row) else r[1]
            run_ids.append(rid)
            run_meta.append({"run_id": rid, "snapshot_path": snap})
        sessions_deleted = 0
        if run_ids:
            placeholders = ",".join("?" * len(run_ids))
            cur = conn.execute(
                f"DELETE FROM exam_sessions WHERE run_id IN ({placeholders})",
                run_ids,
            )
            sessions_deleted = cur.rowcount
            conn.execute(
                f"DELETE FROM exam_runs WHERE id IN ({placeholders})",
                run_ids,
            )
        return {
            "paper_id": paper_id,
            "runs_deleted": len(run_ids),
            "sessions_deleted": sessions_deleted,
            "runs": run_meta,
        }


def transition_run_to_closing(
    run_id: str,
    *,
    closing_started_at: str,
    finalize_at: str,
) -> dict[str, Any] | None:
    with db_cursor() as conn:
        cur = conn.execute(
            """UPDATE exam_runs
               SET status = 'closing',
                   closing_started_at = ?,
                   finalize_at = ?
               WHERE id = ? AND status = 'open'""",
            (closing_started_at, finalize_at, run_id),
        )
        if cur.rowcount == 0:
            row = conn.execute("SELECT * FROM exam_runs WHERE id = ?", (run_id,)).fetchone()
            return dict(row) if row else None
        row = conn.execute("SELECT * FROM exam_runs WHERE id = ?", (run_id,)).fetchone()
        return dict(row) if row else None


def list_closing_runs_due(now_iso_str: str) -> list[dict[str, Any]]:
    """返回已到期的 closing 轮次。

    不用字符串比较 finalize_at（时区/微秒格式会导致永远不到期或立即到期），
    在应用层用 parse_iso 做真实时间比较。
    """
    try:
        now_dt = parse_iso(now_iso_str)
    except Exception:
        now_dt = datetime.now().astimezone()

    with db_cursor() as conn:
        rows = conn.execute(
            """SELECT * FROM exam_runs
               WHERE status = 'closing' AND finalize_at IS NOT NULL"""
        ).fetchall()
        due: list[dict[str, Any]] = []
        for r in rows:
            item = dict(r)
            try:
                deadline = parse_iso(str(item["finalize_at"]))
                if deadline.tzinfo is None:
                    deadline = deadline.astimezone()
                if now_dt.tzinfo is None:
                    now_dt = now_dt.astimezone()
                if deadline <= now_dt:
                    due.append(item)
            except Exception:
                # 无法解析则视为到期，避免卡死
                due.append(item)
        return due


def mark_run_closed(run_id: str, closed_at: str | None = None, conn: sqlite3.Connection | None = None) -> bool:
    ts = closed_at or now_iso()

    def _do(c: sqlite3.Connection) -> bool:
        cur = c.execute(
            """UPDATE exam_runs
               SET status = 'closed', closed_at = ?
               WHERE id = ? AND status = 'closing'""",
            (ts, run_id),
        )
        return cur.rowcount > 0

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


def list_all_runs_latest_by_paper() -> dict[str, dict[str, Any]]:
    """每个 paper_id 取最新轮次（优先活动轮次，否则 round_no 最大）。"""
    with db_cursor() as conn:
        rows = conn.execute(
            """SELECT * FROM exam_runs
               ORDER BY paper_id ASC,
                 CASE status WHEN 'closing' THEN 0 WHEN 'open' THEN 1 ELSE 2 END,
                 round_no DESC"""
        ).fetchall()
        out: dict[str, dict[str, Any]] = {}
        for r in rows:
            d = dict(r)
            pid = str(d["paper_id"])
            if pid not in out:
                out[pid] = d
        return out


# ---------------- exam_sessions CRUD ----------------

def create_exam_session_if_absent(
    *,
    session_id: str,
    run_id: str,
    employee_id: str,
    name: str,
    department: str | None,
    session_token_hash: str,
    started_at: str,
    deadline_at: str,
    client_ip: str | None,
    user_agent: str | None,
) -> tuple[dict[str, Any], bool]:
    """创建会话；若 (run_id, employee_id) 已存在则返回已有行。

    返回 (session_row, created)。
    created=True 时调用方持有明文 session_token（仅此次）。
    """
    now = now_iso()
    with db_cursor() as conn:
        conn.execute(
            """INSERT INTO exam_sessions
               (id, run_id, employee_id, name, department, session_token_hash,
                started_at, deadline_at, draft_json, draft_revision, draft_saved_at,
                status, client_ip, user_agent, created_at, updated_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}', 0, NULL, 'active', ?, ?, ?, ?)
               ON CONFLICT(run_id, employee_id) DO NOTHING""",
            (
                session_id, run_id, employee_id, name, department, session_token_hash,
                started_at, deadline_at, client_ip, user_agent, now, now,
            ),
        )
        row = conn.execute(
            "SELECT * FROM exam_sessions WHERE run_id = ? AND employee_id = ?",
            (run_id, employee_id),
        ).fetchone()
        if not row:
            raise RuntimeError("create_exam_session_if_absent 失败：未找到会话")
        d = dict(row)
        created = d["id"] == session_id
        return d, created


def get_session_by_id(session_id: str) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT * FROM exam_sessions WHERE id = ?", (session_id,)
        ).fetchone()
        if not row:
            return None
        d = dict(row)
        d["draft"] = json.loads(d.get("draft_json") or "{}")
        return d


def get_session_by_run_employee(run_id: str, employee_id: str) -> dict[str, Any] | None:
    with db_cursor() as conn:
        row = conn.execute(
            "SELECT * FROM exam_sessions WHERE run_id = ? AND employee_id = ?",
            (run_id, employee_id),
        ).fetchone()
        if not row:
            return None
        d = dict(row)
        d["draft"] = json.loads(d.get("draft_json") or "{}")
        return d


def update_session_draft(
    session_id: str,
    *,
    expected_revision: int,
    answers: dict[str, Any],
    new_revision: int,
) -> dict[str, Any] | None:
    """仅当 draft_revision == expected_revision 时更新。返回更新后的行或 None。"""
    now = now_iso()
    with db_cursor() as conn:
        cur = conn.execute(
            """UPDATE exam_sessions
               SET draft_json = ?, draft_revision = ?, draft_saved_at = ?, updated_at = ?
               WHERE id = ? AND status = 'active' AND draft_revision = ?""",
            (
                json.dumps(answers, ensure_ascii=False),
                new_revision, now, now, session_id, expected_revision,
            ),
        )
        if cur.rowcount == 0:
            return None
        row = conn.execute("SELECT * FROM exam_sessions WHERE id = ?", (session_id,)).fetchone()
        if not row:
            return None
        d = dict(row)
        d["draft"] = json.loads(d.get("draft_json") or "{}")
        return d


def mark_session_submitted(
    session_id: str,
    conn: sqlite3.Connection | None = None,
) -> bool:
    now = now_iso()

    def _do(c: sqlite3.Connection) -> bool:
        cur = c.execute(
            """UPDATE exam_sessions
               SET status = 'submitted', updated_at = ?
               WHERE id = ? AND status = 'active'""",
            (now, session_id),
        )
        return cur.rowcount > 0

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


def list_active_sessions_for_run(run_id: str, conn: sqlite3.Connection | None = None) -> list[dict[str, Any]]:
    def _do(c: sqlite3.Connection) -> list[dict[str, Any]]:
        rows = c.execute(
            "SELECT * FROM exam_sessions WHERE run_id = ? AND status = 'active'",
            (run_id,),
        ).fetchall()
        out = []
        for r in rows:
            d = dict(r)
            d["draft"] = json.loads(d.get("draft_json") or "{}")
            out.append(d)
        return out

    if conn is not None:
        return _do(conn)
    with db_cursor() as c:
        return _do(c)


def count_sessions_for_run(run_id: str, status: str | None = None) -> int:
    with db_cursor() as conn:
        if status:
            row = conn.execute(
                "SELECT COUNT(*) c FROM exam_sessions WHERE run_id = ? AND status = ?",
                (run_id, status),
            ).fetchone()
        else:
            row = conn.execute(
                "SELECT COUNT(*) c FROM exam_sessions WHERE run_id = ?",
                (run_id,),
            ).fetchone()
        return int(row["c"])


def count_active_sessions_for_run(run_id: str) -> int:
    return count_sessions_for_run(run_id, status="active")
