"""Task 3: SQLite vs PostgreSQL 数据比对验证脚本.

比对维度 (按 plan 要求):
  * row counts: 4 张表行数必等
  * PK 集合: exam_runs.id / exam_sessions.id / submissions.id / review_logs.id 一致
  * 关键字段: paper_id / employee_id / name / 各 score / answers_json / grading_detail_json 语义等价
  * FK 完整性: exam_sessions.run_id 必在 exam_runs.id 中; review_logs.submission_id 必在 submissions.id 中
  * active=0 (按 plan: 迁移后不应遗留 SQLite source 里没有的 active run) -- 我们把源 SQLite
    里的 active runs 集合, 与 PG 端 active runs 集合交集比对
  * sequence next > max(id): PG 端 submissions / review_logs 的 nextval 必大于 max(id)

返回:
  { "ok": bool, "diff": {table: {count:..., pk:..., ...}} 或 {} }
  ok=True 表示所有维度全等, diff={} .
  ok=False, diff 内给出每张表的差异明细.
"""
from __future__ import annotations

import argparse
import json
import logging
import sqlite3
import sys
from pathlib import Path
from typing import Any

import psycopg

LOG = logging.getLogger("verify_migration")

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))


def _sqlite_counts(p: str) -> dict[str, int]:
    conn = sqlite3.connect(p)
    try:
        return {
            "exam_runs": conn.execute("SELECT count(*) FROM exam_runs").fetchone()[0],
            "exam_sessions": conn.execute("SELECT count(*) FROM exam_sessions").fetchone()[0],
            "submissions": conn.execute("SELECT count(*) FROM submissions").fetchone()[0],
            "review_logs": conn.execute("SELECT count(*) FROM review_logs").fetchone()[0],
        }
    finally:
        conn.close()


def _sqlite_pks(p: str) -> dict[str, set]:
    conn = sqlite3.connect(p)
    try:
        return {
            "exam_runs": {r[0] for r in conn.execute("SELECT id FROM exam_runs")},
            "exam_sessions": {r[0] for r in conn.execute("SELECT id FROM exam_sessions")},
            "submissions": {r[0] for r in conn.execute("SELECT id FROM submissions")},
            "review_logs": {r[0] for r in conn.execute("SELECT id FROM review_logs")},
        }
    finally:
        conn.close()


def _sqlite_active_runs(p: str) -> set:
    conn = sqlite3.connect(p)
    try:
        return {r[0] for r in conn.execute(
            "SELECT id FROM exam_runs WHERE status IN ('open','closing')")}
    finally:
        conn.close()


def _sqlite_submissions(p: str) -> dict[int, dict[str, Any]]:
    conn = sqlite3.connect(p)
    conn.row_factory = sqlite3.Row
    try:
        rows = list(conn.execute("SELECT * FROM submissions ORDER BY id"))
        return {int(r["id"]): dict(r) for r in rows}
    finally:
        conn.close()


def _sqlite_runs(p: str) -> dict[str, dict[str, Any]]:
    conn = sqlite3.connect(p)
    conn.row_factory = sqlite3.Row
    try:
        rows = list(conn.execute("SELECT * FROM exam_runs ORDER BY id"))
        return {r["id"]: dict(r) for r in rows}
    finally:
        conn.close()


def _pg_ensure_search_path(cur, schema: str):
    cur.execute(f"SET search_path={schema},public")


def _pg_counts(cur, schema: str) -> dict[str, int]:
    out = {}
    for tbl in ("exam_runs", "exam_sessions", "submissions", "review_logs"):
        cur.execute(f"SELECT count(*) FROM {schema}.{tbl}")
        out[tbl] = cur.fetchone()[0]
    return out


def _pg_pks(cur, schema: str) -> dict[str, set]:
    out: dict[str, set] = {}
    cur.execute(f"SELECT id FROM {schema}.exam_runs")
    out["exam_runs"] = {r[0] for r in cur.fetchall()}
    cur.execute(f"SELECT id FROM {schema}.exam_sessions")
    out["exam_sessions"] = {r[0] for r in cur.fetchall()}
    cur.execute(f"SELECT id FROM {schema}.submissions")
    out["submissions"] = {r[0] for r in cur.fetchall()}
    cur.execute(f"SELECT id FROM {schema}.review_logs")
    out["review_logs"] = {r[0] for r in cur.fetchall()}
    return out


def _pg_active_runs(cur, schema: str) -> set:
    cur.execute(f"SELECT id FROM {schema}.exam_runs WHERE status IN ('open','closing')")
    return {r[0] for r in cur.fetchall()}


def _pg_submissions(cur, schema: str) -> dict[int, dict[str, Any]]:
    cur.execute(f"""
        SELECT id, name, employee_id, paper_id, paper_name, run_id, department,
               answers_json, grading_detail_json,
               objective_score, subjective_score_machine,
               subjective_score_final, total_score,
               review_status, grading_status, auto_submit_reason
        FROM {schema}.submissions
    """)
    out: dict[int, dict[str, Any]] = {}
    for r in cur.fetchall():
        out[r[0]] = {
            "id": r[0], "name": r[1], "employee_id": r[2], "paper_id": r[3],
            "paper_name": r[4], "run_id": r[5], "department": r[6],
            "answers_json": r[7] if isinstance(r[7], str) else json.dumps(r[7], ensure_ascii=False),
            "grading_detail_json": r[8] if isinstance(r[8], str) else json.dumps(r[8], ensure_ascii=False),
            "objective_score": float(r[9] or 0), "subjective_score_machine": float(r[10] or 0),
            "subjective_score_final": float(r[11] or 0), "total_score": float(r[12] or 0),
            "review_status": r[13] or "pending", "grading_status": r[14] or "pending",
            "auto_submit_reason": r[15],
        }
    return out


def _pg_runs(cur, schema: str) -> dict[str, dict[str, Any]]:
    cur.execute(f"""
        SELECT id, paper_id, round_no, status, duration_minutes, is_legacy
        FROM {schema}.exam_runs
    """)
    out: dict[str, dict[str, Any]] = {}
    for r in cur.fetchall():
        out[r[0]] = {
            "id": r[0], "paper_id": r[1], "round_no": int(r[2]),
            "status": r[3], "duration_minutes": int(r[4]),
            "is_legacy": 1 if r[5] else 0,
        }
    return out


def _nextval_gt_max_check(cur, schema: str) -> tuple[bool, str]:
    """sequence next > max(id) 检查."""
    for seq, tbl in (("submissions_id_seq", "submissions"),
                     ("review_logs_id_seq", "review_logs")):
        cur.execute(f"SELECT nextval('{schema}.{seq}')")
        nxt = cur.fetchone()[0]
        cur.execute(f"SELECT max(id) FROM {schema}.{tbl}")
        mx = cur.fetchone()[0] or 0
        if not (nxt > mx):
            return False, f"{tbl}: nextval {nxt} <= max {mx}"
    return True, ""


def main(src_sqlite: str, pg_url: str, schema_name: str | None = None) -> dict[str, Any]:
    schema = schema_name or "public"
    src = Path(src_sqlite)
    if not src.exists():
        return {"ok": False, "diff": {"source": {"not_found": str(src)}}}

    s_counts = _sqlite_counts(str(src))
    s_pks = _sqlite_pks(str(src))
    s_act = _sqlite_active_runs(str(src))
    s_subs = _sqlite_submissions(str(src))
    s_runs = _sqlite_runs(str(src))

    conn = psycopg.connect(pg_url, options=f"--search_path={schema},public")
    diff: dict[str, dict] = {}
    try:
        with conn.cursor() as cur:
            _pg_ensure_search_path(cur, schema)
            p_counts = _pg_counts(cur, schema)
            p_pks = _pg_pks(cur, schema)
            p_act = _pg_active_runs(cur, schema)
            p_subs = _pg_submissions(cur, schema)
            p_runs = _pg_runs(cur, schema)

            # 1. count
            for tbl in ("exam_runs", "exam_sessions", "submissions", "review_logs"):
                if s_counts[tbl] != p_counts[tbl]:
                    diff[f"{tbl}.count"] = {"sqlite": s_counts[tbl], "pg": p_counts[tbl]}

            # 2. PK 集合
            for tbl, key in (("exam_runs", "exam_runs"), ("exam_sessions", "exam_sessions"),
                             ("submissions", "submissions"), ("review_logs", "review_logs")):
                if s_pks[key] != p_pks[key]:
                    diff[f"{tbl}.pks"] = {
                        "sqlite_only": sorted(s_pks[key] - p_pks[key]),
                        "pg_only": sorted(p_pks[key] - s_pks[key]),
                    }

            # 3. active PK 集合(单测: active=0 期望两边 active 集合交集 == sqlite active 集合)
            if s_act != (s_act & p_act):
                diff["active_runs_incongruent"] = {
                    "sqlite_only": sorted(s_act - p_act),
                    "pg_only": sorted(p_act - s_act),
                }

            # 4. submissions 关键字段比对
            for sid, sr in s_subs.items():
                pr = p_subs.get(sid)
                if pr is None:
                    diff[f"submissions.{sid}.missing_pg"] = True
                    continue
                for k in ("name", "employee_id", "paper_id", "run_id", "department",
                          "objective_score", "subjective_score_machine",
                          "subjective_score_final", "total_score"):
                    if sr.get(k) != pr.get(k):
                        diff[f"submissions.{sid}.{k}"] = {"sqlite": sr.get(k), "pg": pr.get(k)}

                # answers_json / grading_detail_json 语义比对
                try:
                    a_s = json.loads(sr.get("answers_json") or "{}")
                    a_p = json.loads(pr.get("answers_json") or "{}")
                except Exception:
                    a_s = sr.get("answers_json"); a_p = pr.get("answers_json")
                if a_s != a_p:
                    diff[f"submissions.{sid}.answers_json"] = {"sqlite": a_s, "pg": a_p}
                try:
                    g_s = json.loads(sr.get("grading_detail_json") or "[]")
                    g_p = json.loads(pr.get("grading_detail_json") or "[]")
                except Exception:
                    g_s = sr.get("grading_detail_json"); g_p = pr.get("grading_detail_json")
                if g_s != g_p:
                    diff[f"submissions.{sid}.grading_detail_json"] = {"sqlite": g_s, "pg": g_p}

            # 5. exam_runs 关键字段 (round_no 在 legacy 上会被重新分配, 不严格比较)
            for rid, sr in s_runs.items():
                pr = p_runs.get(rid)
                if pr is None:
                    diff[f"exam_runs.{rid}.missing_pg"] = True
                    continue
                for k in ("paper_id", "status", "duration_minutes", "is_legacy"):
                    if sr.get(k) != pr.get(k):
                        diff[f"exam_runs.{rid}.{k}"] = {"sqlite": sr.get(k), "pg": pr.get(k)}
                # round_no: 双方都 > 0, 不阻塞
                if pr.get("round_no", 0) <= 0:
                    diff[f"exam_runs.{rid}.round_no_invalid_pg"] = {"pg": pr.get("round_no")}

            # 6. FK 完整性 (exam_sessions.run_id, review_logs.submission_id)
            cur.execute(f"""
                SELECT count(*) FROM {schema}.exam_sessions s
                LEFT JOIN {schema}.exam_runs r ON s.run_id = r.id
                WHERE r.id IS NULL
            """)
            orphan_sess = cur.fetchone()[0]
            if orphan_sess:
                diff["exam_sessions.orphan_run_id"] = {"count": orphan_sess}
            cur.execute(f"""
                SELECT count(*) FROM {schema}.review_logs l
                LEFT JOIN {schema}.submissions s ON l.submission_id = s.id
                WHERE s.id IS NULL
            """)
            orphan_log = cur.fetchone()[0]
            if orphan_log:
                diff["review_logs.orphan_submission_id"] = {"count": orphan_log}

            # 7. sequence next > max
            ok_seq, msg = _nextval_gt_max_check(cur, schema)
            if not ok_seq:
                diff["sequences.bad_nextval"] = {"msg": msg}
    finally:
        conn.close()

    return {"ok": not diff, "diff": diff}


def _cli():
    p = argparse.ArgumentParser(description="SQLite vs PostgreSQL 比对验证")
    p.add_argument("--src", required=True, help="源 SQLite 路径")
    p.add_argument("--pg", required=True, help="PostgreSQL connection URL")
    p.add_argument("--schema", default=None, help="目标 PG schema (默认 public)")
    p.add_argument("--verbose", action="store_true")
    args = p.parse_args()
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s [%(levelname)s] %(message)s")
    res = main(args.src, args.pg, schema_name=args.schema)
    print(json.dumps(res, ensure_ascii=False, indent=2))
    if not res["ok"]:
        sys.exit(2)


if __name__ == "__main__":
    _cli()
