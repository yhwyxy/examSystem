"""Task 3: PostgreSQL -> SQLite 回滚导出脚本

设计要点:
  * 把 PG 指定 schema 内的 4 张表数据 -> 新 SQLite 路径
  * SQLite schema 与 backend/database.py init_db 一致 (本脚本内自己 CREATE TABLE,
    不依赖 backend import 以确保回滚出来的库与生产 schema 完全等价)
  * grading_status 映射回 SQLite 的 review_status:
    - 'done' -> 'done'
    - 'pending' / 'grading' / 'failed' -> 'pending'
  * timestamp: PG timestamptz ISO8601 -> SQLite TEXT ISO8601, 保留 Z 后缀
  * submissions.id 显式塞 (AUTOINCREMENT 表允许显式 id), 结束后 update sqlite_sequence
"""
from __future__ import annotations

import argparse
import json
import logging
import os
import sqlite3
import sys
from pathlib import Path
from typing import Any

import psycopg

LOG = logging.getLogger("postgres_to_sqlite")

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))


# SQLite schema 定义 (与 backend.database.init_db 等价)
SQLITE_SCHEMA_SQL = """
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
"""


def _pg_to_sqlite_review_status(grading_status: str | None, review_status: str | None) -> str:
    """PG (grading_status, review_status) -> SQLite review_status.

    优先用 GRADING_STATUS; review_status 兼容保留原值。
      grading_status='done'   -> 'done'
      grading_status='pending'/'grading'/'failed' -> 'pending'
      无 grading_status 但 review_status 在 SQLite 合法集合 -> 保留
    """
    gs = (grading_status or "").lower()
    if gs == "done":
        return "done"
    if gs in ("pending", "grading", "failed"):
        return "pending"
    # 退回 review_status
    return (review_status or "pending").lower() or "pending"


def _ts_to_text(value) -> str | None:
    """PG timestamptz/timestamp -> SQLite TEXT ISO 字符串.

    None -> None. 已经是 str -> 原样. datetime -> isoformat
    """
    if value is None:
        return None
    if isinstance(value, str):
        return value
    # psycopg 返回 datetime.datetime
    if hasattr(value, "isoformat"):
        s = value.isoformat()
        # 替换 +00:00 为 Z 让 SQLite 端比对稳定
        if s.endswith("+00:00"):
            s = s[:-6] + "Z"
        return s
    return str(value)


def _jsonb_to_text(value) -> str:
    """PG jsonb -> SQLite TEXT 已 JSON-encoded 字符串.
    None -> '{}'. list/dict -> json.dumps 保证 JSON 文本格式.
    """
    if value is None:
        return "{}"
    if isinstance(value, str):
        # 校验已是合法 JSON 文本
        try:
            return json.dumps(json.loads(value), ensure_ascii=False)
        except (TypeError, ValueError):
            return "{}"
    return json.dumps(value, ensure_ascii=False)


def main(pg_url: str, pg_schema: str | None, target_sqlite_path: str) -> dict[str, int]:
    schema = pg_schema or "public"
    target = Path(target_sqlite_path)
    if target.exists():
        target.unlink()
    target.parent.mkdir(parents=True, exist_ok=True)

    # 1. 初始化 SQLite (建表)
    sconn = sqlite3.connect(str(target))
    sconn.executescript(SQLITE_SCHEMA_SQL)
    sconn.commit()

    # 2. 连 PG 读取 (options 走 kwargs, 不 URL 拼接避免 = 号被 libpq 拒)
    conn = psycopg.connect(pg_url, options=f"--search_path={schema},public")
    try:
        with conn.cursor() as cur:
            # 2.1 exam_runs
            cur.execute(f"""
                SELECT id, paper_id, round_no, public_token_hash, status,
                       duration_minutes, snapshot_path, snapshot_hash, is_legacy,
                       opened_at, closing_started_at, finalize_at, closed_at, created_at
                FROM {schema}.exam_runs ORDER BY created_at, id
            """)
            run_rows = cur.fetchall()
            for r in run_rows:
                sconn.execute("""
                    INSERT INTO exam_runs (
                        id, paper_id, round_no, public_token_hash, status,
                        duration_minutes, snapshot_path, snapshot_hash, is_legacy,
                        opened_at, closing_started_at, finalize_at, closed_at, created_at
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                """, (r[0], r[1], int(r[2]), r[3], r[4],
                      int(r[5]), r[6], r[7], 1 if r[8] else 0,
                      _ts_to_text(r[9]), _ts_to_text(r[10]), _ts_to_text(r[11]),
                      _ts_to_text(r[12]), _ts_to_text(r[13])))

            # 2.2 exam_sessions
            cur.execute(f"""
                SELECT id, run_id, employee_id, name, department,
                       session_token_hash, started_at, deadline_at,
                       draft_json, draft_revision, draft_saved_at,
                       status, client_ip, user_agent,
                       created_at, updated_at
                FROM {schema}.exam_sessions ORDER BY created_at, id
            """)
            sess_rows = cur.fetchall()
            for r in sess_rows:
                sconn.execute("""
                    INSERT INTO exam_sessions (
                        id, run_id, employee_id, name, department,
                        session_token_hash, started_at, deadline_at,
                        draft_json, draft_revision, draft_saved_at,
                        status, client_ip, user_agent,
                        created_at, updated_at
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                """, (r[0], r[1], r[2], r[3], r[4],
                      r[5], _ts_to_text(r[6]), _ts_to_text(r[7]),
                      _jsonb_to_text(r[8]), int(r[9] or 0), _ts_to_text(r[10]),
                      r[11], r[12], r[13],
                      _ts_to_text(r[14]), _ts_to_text(r[15])))

            # 2.3 submissions
            cur.execute(f"""
                SELECT id, name, employee_id, paper_id, paper_name, run_id, department,
                       answers_json, grading_detail_json,
                       objective_score, subjective_score_machine,
                       subjective_score_final, total_score,
                       review_status, grading_status,
                       started_at, submitted_at, reviewed_at, reviewer_note,
                       client_ip, user_agent, auto_submit_reason
                FROM {schema}.submissions ORDER BY id
            """)
            sub_rows = cur.fetchall()
            for r in sub_rows:
                rs = _pg_to_sqlite_review_status(r[14], r[13])
                sconn.execute("""
                    INSERT INTO submissions (
                        id, name, employee_id, paper_id, paper_name, run_id, department,
                        answers_json, grading_detail_json,
                        objective_score, subjective_score_machine,
                        subjective_score_final, total_score,
                        review_status, started_at, submitted_at,
                        reviewed_at, reviewer_note,
                        client_ip, user_agent, auto_submit_reason
                    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                """, (int(r[0]), r[1], r[2], r[3], r[4], r[5], r[6],
                      _jsonb_to_text(r[7]), _jsonb_to_text(r[8]),
                      float(r[9] or 0), float(r[10] or 0),
                      float(r[11] or 0), float(r[12] or 0),
                      rs,
                      _ts_to_text(r[15]), _ts_to_text(r[16]),
                      _ts_to_text(r[17]), r[18],
                      r[19], r[20], r[21]))

            # 2.4 review_logs
            cur.execute(f"""
                SELECT id, submission_id, question_id, old_score, new_score, note, created_at
                FROM {schema}.review_logs ORDER BY id
            """)
            log_rows = cur.fetchall()
            for r in log_rows:
                sconn.execute("""
                    INSERT INTO review_logs (
                        id, submission_id, question_id, old_score, new_score, note, created_at
                    ) VALUES (?,?,?,?,?,?,?)
                """, (int(r[0]), int(r[1]), r[2],
                      float(r[3]) if r[3] is not None else None,
                      float(r[4]) if r[4] is not None else None,
                      r[5], _ts_to_text(r[6])))

            # 3. 同步 sqlite_sequence 给 AUTOINCREMENT 表
            if log_rows:
                max_log_id = max(int(r[0]) for r in log_rows)
                sconn.execute("UPDATE sqlite_sequence SET seq=? WHERE name='review_logs'",
                              (max_log_id,))
            if sub_rows:
                max_sub_id = max(int(r[0]) for r in sub_rows)
                # 可能 sqlite_sequence 还没行, INSERT OR REPLACE
                sconn.execute("""
                    INSERT OR REPLACE INTO sqlite_sequence(name, seq) VALUES
                    ('submissions', ?), ('review_logs', ?)
                """, (max_sub_id,
                      max(int(r[0]) for r in log_rows) if log_rows else max_sub_id))

            sconn.commit()
    finally:
        conn.close()
    sconn.close()

    counts = {
        "exam_runs": len(run_rows),
        "exam_sessions": len(sess_rows),
        "submissions": len(sub_rows),
        "review_logs": len(log_rows),
        # 兼容测试断言(测试里看的就是 submissions / runs 短 key)
        "runs": len(run_rows),
        "sessions": len(sess_rows),
        "submissions_rows": len(sub_rows),
    }
    LOG.info("PG -> SQLite 完成 (schema=%s, target=%s): %s", schema, target, counts)
    return counts


def _cli():
    p = argparse.ArgumentParser(description="PostgreSQL -> SQLite 回滚导出")
    p.add_argument("--pg", required=True, help="PostgreSQL connection URL")
    p.add_argument("--schema", default=None, help="源 PG schema (默认 public)")
    p.add_argument("--target", required=True, help="目标 SQLite 路径")
    p.add_argument("--verbose", action="store_true")
    args = p.parse_args()
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s [%(levelname)s] %(message)s")
    res = main(args.pg, args.schema, args.target)
    print(json.dumps(res, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    _cli()
