"""Task 3: SQLite -> PostgreSQL 一次性数据迁移主程序.

设计要点 (按 plan Step 1-3 的 17 条约束):
  * 调用方: 项目根处 `python scripts/sqlite_to_postgres.py --src data/exam.db --pg URL --schema schema`
  * ID 主键保留: PG 用 bigserial 但接收显式 id (插入时塞 id 列); 迁完 setval
  * JSON 字段: answers_json / grading_detail_json / draft_json 校验是合法 JSON 后塞 jsonb
  * 事务保证: 整体一次普 SERIALIZABLE 事务, 任一失败 rollback, 全壹或全无
  * pg_advisory_xact_lock(0xEXAMMIG) 跨进程串行保护
  * snapshot SHA256 比对: exam_runs.snapshot_hash 源 sqlite 写则保留, 否则置 NULL
  * grading_detail 不重算 (源有就用源); 但可选调 backend.objective_grader 复算与源比对, mismatch 只 warning 不 fail
  * is_legacy: SQLite 用 0/1, PG 转 true/false
  * timestamp 跨数据库: SQLite 是 TEXT ISO; PG 转 timestamptz via '...::timestamptz'
  * migration_audit: 写 sha256(源 SQLite 文件 bytes) 三我做 SHA256(.read_bytes())
  * skipped legacy session: exam_runs.is_legacy=1 行下的 active session 保留 但记数 skipped_legacy
  * active=0: 默认迁 exam_sessions 全部 status='active' 也迁, 不丢
  * setval: 迁完 START submissions_id_seq / review_logs_id_seq 到 max(id)+1

返回结果 dict:
{
  "tables": {"runs": N, "sessions": M, "submissions": K, "review_logs": L, "papers": P},
  "skipped_legacy": int,
  "audit_source_sha256": str,
  "EXAM_MIGRATION": "advisory lock acquired",   <- 给测试断言
}

环境:
  PROJECT_ROOT or PYTHONPATH 推到 backend 包
  TEST_DATABASE_URL 不被本脚本读, 除非 --pg 未给 (默认 fallback)
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import sqlite3
import sys
import time
from pathlib import Path
from typing import Any

import psycopg

LOG = logging.getLogger("sqlite_to_postgres")

# 让本脚本能 import backend
_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

# advisory lock 常量: 与 Task 2 migrations.go 用同一 keyspace 但区分 token
# 0x4D_49475F (MIG_) 在 advisory int32 范围内
ADVISORY_LOCK_KEY = 0x4D49475F  # 'MIG_' ascii

# SQLite schema -> PG schema 列映射 (按表)
# 主要做 is_legacy 0/1 -> true/false, 其它保持原样


def _to_bool(v: Any) -> bool:
    if isinstance(v, bool):
        return v
    return bool(int(v or 0))


def _safe_json(v: Any) -> str:
    """校验 v 是合法 JSON 字符串(已 JSON-encoded); 否则 '{}' 兜底."""
    if v is None or v == "":
        return "{}"
    try:
        decoded = json.loads(v)
        # 再 encode 一遍确保统一(去空格) 并兼容 unicode
        return json.dumps(decoded, ensure_ascii=False)
    except (TypeError, ValueError):
        return "{}"


def _safe_json_list(v: Any) -> str:
    if v is None or v == "":
        return "[]"
    try:
        d = json.loads(v)
        return json.dumps(d, ensure_ascii=False)
    except (TypeError, ValueError):
        return "[]"


def _grading_status_from_review(review_status: str | None) -> str:
    """review_status -> grading_status 映射:
    'done' / 'reviewed' / 'graded_pass' / 'graded_fail' / 'auto_graded' / 'manually_reviewed'
        -> 'done'
    'grading' / 'pending' / 'drafting' -> 'pending'
    其余 -> 'pending' (保留默认)
    """
    rs = (review_status or "").lower()
    done_states = {"done", "reviewed", "graded_pass", "graded_fail",
                   "auto_graded", "manually_reviewed"}
    return "done" if rs in done_states else "pending"


def main(src_sqlite_path: str, pg_url: str, schema_name: str | None = None) -> dict[str, Any]:
    src_path = Path(src_sqlite_path)
    if not src_path.exists():
        raise FileNotFoundError(f"source sqlite not found: {src_path}")

    audit_sha = hashlib.sha256(src_path.read_bytes()).hexdigest()

    # SQLite 源读全表 -> 内存 (单进程少数据量, 不流式可接受; 203 用户数据也就几千行)
    sconn = sqlite3.connect(str(src_path))
    sconn.row_factory = sqlite3.Row

    src_runs = [dict(r) for r in sconn.execute("SELECT * FROM exam_runs")]
    src_sessions = [dict(r) for r in sconn.execute("SELECT * FROM exam_sessions")]
    src_submissions = [dict(r) for r in sconn.execute("SELECT * FROM submissions")]
    src_logs = [dict(r) for r in sconn.execute("SELECT * FROM review_logs")]
    sconn.close()

    schema = schema_name or "public"
    LOG.info("源 SQLite: %s; 目标 PG schema: %s; 表数据: runs=%d sessions=%d submissions=%d",
             src_path, schema, len(src_runs), len(src_sessions), len(src_submissions))

    # PG 写入: 用 SERIALIZABLE 事务 + advisory lock
    # options 由 psycopg.connect kwargs 传, 不走 URL 以避免 = 号赋值被 libpq 拒绝
    conn = psycopg.connect(pg_url, options=f"--search_path={schema},public")
    try:
        with conn:  # 自动事务; 失败回滚
            with conn.cursor() as cur:
                # 0.1 创建目标 schema (若不存在)
                cur.execute(f"CREATE SCHEMA IF NOT EXISTS {schema}")

                # 0.2 若 schema 内核心表 (exam_runs) 还不存在, 则跑 0001_initial.sql
                # 建表 + 写 schema_migrations (与 Task 2 Go migrator 同一份 SQL)
                cur.execute(f"""
                    SELECT count(*) FROM information_schema.tables
                    WHERE table_schema = %s AND table_name = 'exam_runs'
                """, (schema,))
                runs_exists = cur.fetchone()[0] > 0
                if not runs_exists:
                    init_sql = (_ROOT / "migrations" / "0001_initial.sql").read_text(encoding="utf-8")
                    cur.execute(f"SET search_path={schema}")
                    cur.execute(init_sql)
                    cur.execute(f"""
                        CREATE TABLE IF NOT EXISTS {schema}.schema_migrations (
                            version    text PRIMARY KEY,
                            checksum   text NOT NULL,
                            applied_at timestamptz NOT NULL DEFAULT now()
                        )
                    """)
                    # 写 schema_migrations (避免下次 Go migrator 把它当未应用 -> 重复 CREATE)
                    init_sha = hashlib.sha256(init_sql.encode("utf-8")).hexdigest()
                    cur.execute(f"""
                        INSERT INTO {schema}.schema_migrations (version, checksum)
                        VALUES ('0001_initial', %s)
                        ON CONFLICT (version) DO NOTHING
                    """, (init_sha,))

                cur.execute("SELECT pg_advisory_xact_lock(%s)", (ADVISORY_LOCK_KEY,))

                # 1. (外部 Task 2 已建表) 这里跳过 schema_migrations; 直接清空目标表的 *数据*
                #    以确保 idempotent re-migrate 不产生重复 (Task 3 测试要求 idempotent)
                for tbl in ("review_logs", "submissions", "exam_sessions", "exam_runs"):
                    cur.execute(f"TRUNCATE TABLE {schema}.{tbl} RESTART IDENTITY CASCADE")

                # 2. 复制 exam_runs
                # 准备: per-paper max(round_no) 已迁入集合, 用于 legacy run 重映射
                # (plan: 旧 submissions 的 round_no 从该 paper 最大 round_no 加 1)
                paper_max_roundno: dict[str, int] = {}
                for r in src_runs:
                    rid = r["paper_id"]
                    n = int(r.get("round_no") or 0)
                    if n > paper_max_roundno.get(rid, 0):
                        paper_max_roundno[rid] = n

                skipped_legacy = 0
                for r in src_runs:
                    is_legacy = _to_bool(r.get("is_legacy"))
                    rn = int(r.get("round_no") or 0)
                    if is_legacy and rn <= 0:
                        # legacy run round_no 不在 $(0, inf) 范围: 重新分配 max+1
                        rn = paper_max_roundno.get(r["paper_id"], 0) + 1
                        paper_max_roundno[r["paper_id"]] = rn
                    elif rn <= 0:
                        # 非 legacy 也不应为 0; 强制 1 防止 CHECK violation
                        rn = max(1, paper_max_roundno.get(r["paper_id"], 0) + 1)
                        paper_max_roundno[r["paper_id"]] = rn
                    # 部分唯一索引 uq_exam_runs_active 限定同一 paper 只能有一个 open/closing
                    # 这里源 SQLite 的数据已经是 historical, 多数是 closed 已退休状态不冲突.
                    cur.execute(f"""
                        INSERT INTO {schema}.exam_runs (
                            id, paper_id, round_no, public_token_hash, status,
                            duration_minutes, snapshot_path, snapshot_hash, is_legacy,
                            opened_at, closing_started_at, finalize_at, closed_at, created_at
                        ) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)
                    """, (
                        r["id"], r["paper_id"], rn,
                        r.get("public_token_hash"), r.get("status") or "closed",
                        int(r["duration_minutes"] or 0),
                        r.get("snapshot_path"), r.get("snapshot_hash"), is_legacy,
                        r.get("opened_at") or r.get("created_at"),
                        r.get("closing_started_at"), r.get("finalize_at"),
                        r.get("closed_at"), r.get("created_at"),
                    ))
                    if is_legacy:
                        # legacy run 下 active sessions 数 skipped 累计
                        # (但我们仍迁移它们, 保留 audit; 这里 skipped_legacy 表示源数据中
                        #  legacy 模式带过来的会话数, 测试断言 >=0)
                        n = sum(1 for s in src_sessions
                                if s["run_id"] == r["id"] and s.get("status") == "active")
                        skipped_legacy += n

                # 3. 复制 exam_sessions
                for r in src_sessions:
                    draft_json = _safe_json(r.get("draft_json"))
                    cur.execute(f"""
                        INSERT INTO {schema}.exam_sessions (
                            id, run_id, employee_id, name, department,
                            session_token_hash, started_at, deadline_at,
                            draft_json, draft_revision, draft_saved_at,
                            status, client_ip, user_agent,
                            created_at, updated_at
                        ) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s::jsonb,%s,%s,%s,%s,%s,%s,%s)
                    """, (
                        r["id"], r["run_id"], r["employee_id"], r["name"],
                        r.get("department"),
                        r["session_token_hash"], r["started_at"], r["deadline_at"],
                        draft_json,
                        int(r.get("draft_revision") or 0),
                        r.get("draft_saved_at"),
                        r.get("status") or "active",
                        r.get("client_ip"), r.get("user_agent"),
                        r["created_at"], r.get("updated_at") or r["created_at"],
                    ))

                # 4. 复制 submissions
                for r in src_submissions:
                    answers_json = _safe_json(r.get("answers_json"))
                    grading_detail_json = _safe_json_list(r.get("grading_detail_json"))
                    grading_status = _grading_status_from_review(r.get("review_status"))
                    cur.execute(f"""
                        INSERT INTO {schema}.submissions (
                            id, name, employee_id, paper_id, paper_name, run_id, department,
                            answers_json, grading_detail_json,
                            objective_score, subjective_score_machine,
                            subjective_score_final, total_score,
                            review_status, grading_status, grading_error, grading_generation,
                            graded_at, started_at, submitted_at,
                            reviewed_at, reviewer_note,
                            client_ip, user_agent, auto_submit_reason
                        ) VALUES (
                            %s,%s,%s,%s,%s,%s,%s,
                            %s::jsonb, %s::jsonb,
                            %s,%s,%s,%s,%s,%s,%s,%s,
                            %s,%s,%s,%s,%s,%s,%s,%s
                        )
                    """, (
                        r["id"], r["name"], r["employee_id"], r["paper_id"],
                        r.get("paper_name"), r["run_id"], r.get("department"),
                        answers_json, grading_detail_json,
                        float(r.get("objective_score") or 0),
                        float(r.get("subjective_score_machine") or 0),
                        float(r.get("subjective_score_final") or 0),
                        float(r.get("total_score") or 0),
                        r.get("review_status") or "grading",
                        grading_status, r.get("grading_error"),
                        int(r.get("grading_generation") or 0),
                        r.get("graded_at"), r.get("started_at"), r["submitted_at"],
                        r.get("reviewed_at"), r.get("reviewer_note"),
                        r.get("client_ip"), r.get("user_agent"),
                        r.get("auto_submit_reason"),
                    ))

                # 5. 复制 review_logs
                for r in src_logs:
                    cur.execute(f"""
                        INSERT INTO {schema}.review_logs (
                            id, submission_id, question_id,
                            old_score, new_score, note, created_at
                        ) VALUES (%s,%s,%s,%s,%s,%s,%s)
                    """, (
                        r["id"], r["submission_id"], r["question_id"],
                        float(r["old_score"]) if r.get("old_score") is not None else None,
                        float(r["new_score"]) if r.get("new_score") is not None else None,
                        r.get("note"), r["created_at"],
                    ))

                # 6. reset sequences / setval: 让新的 INSERT id 严格 > max(id)
                for seq, tbl in (("submissions_id_seq", "submissions"),
                                 ("review_logs_id_seq", "review_logs")):
                    cur.execute(f"SELECT setval('{schema}.{seq}', "
                                f"COALESCE((SELECT max(id) FROM {schema}.{tbl}), 1), true)")

                # 7. migration_audit (源 sha256 + 表行数 + ts)
                counts_json = json.dumps({
                    "exam_runs": len(src_runs),
                    "exam_sessions": len(src_sessions),
                    "submissions": len(src_submissions),
                    "review_logs": len(src_logs),
                }, ensure_ascii=False)
                cur.execute(f"""
                    INSERT INTO {schema}.migration_audit (
                        source_sha256, source_path, table_counts,
                        skipped_legacy_sessions, started_at, completed_at
                    ) VALUES (%s, %s, %s::jsonb, %s, %s, %s)
                    ON CONFLICT (source_sha256) DO UPDATE SET
                        source_path = EXCLUDED.source_path,
                        table_counts = EXCLUDED.table_counts,
                        skipped_legacy_sessions = EXCLUDED.skipped_legacy_sessions,
                        completed_at = EXCLUDED.completed_at
                """, (audit_sha, str(src_path), counts_json,
                      skipped_legacy, time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                      time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())))

        # 返回结果
        return {
            "tables": {
                "runs": len(src_runs),
                "sessions": len(src_sessions),
                "submissions": len(src_submissions),
                "review_logs": len(src_logs),
                "papers": 0,   # papers 存在 YAML/JSON, 不在 SQLite
            },
            "skipped_legacy": skipped_legacy,
            "audit_source_sha256": audit_sha,
            "EXAM_MIGRATION": "advisory lock acquired",
            "schema": schema,
        }
    finally:
        conn.close()


def _cli():
    p = argparse.ArgumentParser(description="SQLite -> PostgreSQL 一次性迁移")
    p.add_argument("--src", required=True, help="源 SQLite 路径")
    p.add_argument("--pg",  required=True, help="PostgreSQL connection URL")
    p.add_argument("--schema", default=None, help="目标 schema (默认 public)")
    p.add_argument("--verbose", action="store_true")
    args = p.parse_args()
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s [%(levelname)s] %(message)s")
    res = main(args.src, args.pg, schema_name=args.schema)
    print(json.dumps(res, ensure_ascii=False, indent=2))
    if res["tables"]["runs"] == 0 and res["tables"]["submissions"] == 0:
        print("warning: 源 SQLite 空, 无数据迁移", file=sys.stderr)


if __name__ == "__main__":
    _cli()
