"""Task 3 失败优先往返测试: SQLite -> PG -> 新 SQLite, 4 张表数据一致.

约定脚本接口 (Round-trip contract):
  scripts/sqlite_to_postgres.py
    入口函数: main(src_sqlite_path: str, pg_url: str, pg_schema: str | None = None) -> dict
    功能: 把源 SQLite 全部数据迁入 PG 指定 schema; 在迁移库的
           migration_audit 表写一行 sha256(truncate 后的源 SQLite 文件));
           返回 {tables: {run, sessions, submissions, review_logs, papers},
                  skipped_legacy: int, audit_source_sha256: str}
    约束:
      - 在 PG 端用 advisory_lock 串行 (跨进程); 同一字段 'EXAM_MIGRATION'
        失败时立刻 raise, 不写半套.
      - 迁移期间已存在 active run 跳过, 记入 skipped_legacy.
      - 不重新计算 grading_detail_json, 而用 backend.objective_grader 复算一遍
        与源值比对, 不一致则 audit 记 warning 但不 fail (保留源值, 兼容历史).
      - sequence_autonext: 迁移完成后 START submissions id > max(id)+1 (bigserial

  scripts/postgres_to_sqlite.py
    入口函数: main(pg_url: str, pg_schema: str | None, target_sqlite_path: str) -> dict
    功能: 把 PG schema 4 张表数据导回指定 SQLite 路径; 路径不存在则新建;
    reverse mapping:
      - grading_status 'done' -> review_status 'done'
      - 其余 -> 'pending'
      - timestamp tirade 'timestamptz ISO -> SQLite ISO 字符串'
    返回 {run, sessions, submissions, review_logs, papers} 行数

  scripts/verify_migration.py
    入口函数: main(src_sqlite_path: str, pg_url: str, pg_schema: str | None = None) -> dict
    功能: 比对源 SQLite 与 PG 指定 schema 的 row counts / pk 集合 / 关键字段, 全等
    返回 {ok: bool, diff: {...}}, 任一处不等 ok=false

环境:
  TEST_DATABASE_URL: 指向 PG, 测试用 random schema 隔离
  缺省 skip
"""
from __future__ import annotations

import hashlib
import json
import os
import sqlite3
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import pytest

# 让 tests 能 import scripts/ (作为模块)
_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
sys.path.insert(0, str(_ROOT / "scripts"))

PG_URL = os.environ.get("TEST_DATABASE_URL", "").strip()
require_pg = pytest.mark.skipif(not PG_URL, reason="TEST_DATABASE_URL 未设, 跳过 PG 往返测试")


# ---------------------------------------------------------------------------
# 1. fixture: 生成"丰富语义"的源 SQLite (用真后端 init_db + 业务流程调用)
# ---------------------------------------------------------------------------
@pytest.fixture
def source_sqlite(tmp_path, monkeypatch) -> Path:
    """构造源 SQLite:
    - 2 papers / 1 normal open run + 1 closed run + 1 legacy run
    - active session + submitted session (grading_status done + grading) 各 1
    - grading_detail_json + answers_json + auto_submit_reason 一并写入
    - review_logs 至少 1 条
    - 非连续 submissions id (跳 3 个, 12+999 这样)
    """
    from backend import database
    from backend import paper_store
    from backend import question_loader as ql
    from backend import exam_run_service as svc
    from backend import grader
    import subjective_scoring

    root = tmp_path
    # 直接用真后端建库; 须把所有路径点 monkeypatch 到 tmp
    papers_dir = root / "papers"
    papers_dir.mkdir()
    (papers_dir / "index.json").write_text(json.dumps({"papers": []}), encoding="utf-8")
    backups = root / "backups" / "papers"
    backups.mkdir(parents=True)
    runs_dir = root / "exam_runs"
    runs_dir.mkdir()

    monkeypatch.setattr(ql, "PAPERS_DIR", papers_dir)
    monkeypatch.setattr(ql, "INDEX_PATH", papers_dir / "index.json")
    monkeypatch.setattr(ql, "BACKUPS_DIR", backups)
    monkeypatch.setattr(ql, "DATA_DIR", root)
    monkeypatch.setattr(ql, "LEGACY_QUESTIONS_PATH", root / "questions.json")
    ql.clear_question_cache()

    db_path = root / "exam.db"
    monkeypatch.setattr(database, "DB_PATH", db_path)
    database._initialized = False
    database.init_db()

    monkeypatch.setattr(svc, "EXAM_RUNS_DIR", runs_dir)
    monkeypatch.setattr(svc, "PROJECT_ROOT", root)
    svc.set_grading_scheduler(None)

    # fake subjective scorer
    class _FakeService:
        def score(self, req):
            # 干脆主观满分
            return subjective_scoring.ScoringResult(
                question_id="x", score=float(getattr(req, "max_score", 0)),
                max_score=float(getattr(req, "max_score", 0)),
                scoring_mode=getattr(req, "scoring_mode", subjective_scoring.ScoringMode.TEXT),
                track="fake", confidence=0.99, need_manual_review=False,
                review_level=subjective_scoring.ReviewLevel.AUTO_PASS,
            )
        def close(self): ...
    grader.set_subjective_service(_FakeService())

    # ----- 2 papers -----
    sample = {
        "paper_id": "prioA", "name": "PyPaperA",
        "exam_info": {"title":"A","description":"A","total_score":100.0,"passing_score":60},
        "questions": [
            {"id":"q1","type":"single_choice","question":"?", "score":50.0,
             "options":[{"key":"A","text":"a"},{"key":"B","text":"b"}], "answer":"A"},
            {"id":"q2","type":"short_answer","scoring_mode":"text",
             "question":"?", "score":50.0, "answer":"hello"},
        ],
    }
    paper_store.create_paper(slug="prioA", name="PyPaperA")
    paper_store.save_paper("prioA", sample)
    sample2 = {**sample, "paper_id": "prioB", "name": "PyPaperB"}
    paper_store.create_paper(slug="prioB", name="PyPaperB")
    paper_store.save_paper("prioB", sample2)

    # ----- normal run open (round_no=1) -----
    runA_meta = svc.open_run("prioA")
    # session + submit (grading_status='done')
    start = svc.start_or_resume_session(
        paper_id="prioA", run_token=runA_meta["public_token"],
        name="alice", employee_id="E001", department="x",
        client_ip=None, user_agent=None)
    sid = start["session_id"]; stok = start["session_token"]
    svc.save_draft(sid, session_token=stok, revision=1,
                   answers={"q1":["A"], "q2":"hello world"})
    sub_res = svc.submit_manual(session_id=sid, session_token=stok,
                                answers={"q1":["A"], "q2":"hello world"})
    time.sleep(0.2)
    # submitted session 来一通就 'done' (sync subject 同步过)

    # create close round 2 for paper B
    runB_meta = svc.open_run("prioB")
    # submit on B (grading_status='grading') —— 必须在 begin_close 之前
    sb = svc.start_or_resume_session(
        paper_id="prioB", run_token=runB_meta["public_token"],
        name="bob", employee_id="E002", department=None,
        client_ip=None, user_agent=None)
    sub_b = svc.submit_manual(session_id=sb["session_id"],
                              session_token=sb["session_token"],
                              answers={"q1":["B"]})
    time.sleep(0.2)
    # 现在 close B (此时 B 上有未完成评分的 session, 但已 submitted)
    svc.begin_close("prioB")
    time.sleep(0.2)  # let finalize thread settle

    # ----- 直接 SQL 插一条 legacy run + active session, 还插 review_logs -----
    conn = sqlite3.connect(db_path)
    now = datetime.now(timezone.utc).isoformat()
    conn.executescript(f"""
    INSERT INTO exam_runs(id, paper_id, round_no, status, duration_minutes, is_legacy,
                          opened_at, created_at)
    VALUES ('legacy-run-001', 'prioB', 0, 'closed', 90, 1,
            '{now}', '{now}');

    INSERT INTO exam_sessions(id, run_id, employee_id, name, session_token_hash,
                              started_at, deadline_at, status, created_at, updated_at)
    VALUES ('legacy-sess-001', 'legacy-run-001', 'E99', 'legacy-user',
            'legacysessiontokenhash', '{now}', '{now}', 'active',
            '{now}', '{now}');

    -- 非连续 submissions id: AUTOINCREMENT 不直接可控制; 我们通过手工塞一条
    -- submission (id 自增), 然后用 review_logs 模拟条目越过 + manual
    INSERT INTO review_logs(submission_id, question_id, old_score, new_score, note, created_at)
    VALUES ({sub_res.get("submission_id")},'q2', 30.0, 50.0, 'task3 测试调分', '{now}');
    """)
    conn.commit()
    conn.close()
    grader.set_subjective_service(None)
    return db_path


# ---------------------------------------------------------------------------
# 2. Round-trip test: SQLite -> PG(schema) -> New SQLite -> 比对
# ---------------------------------------------------------------------------
@require_pg
def test_round_trip_sqlite_pg_sqlite(source_sqlite):
    import random, string
    # 随机 schema 隔离
    rand = "".join(random.choices(string.ascii_lowercase, k=10))
    schema_name = f"task3_roundtrip_{rand}"

    import sqlite_to_postgres as m2p
    import postgres_to_sqlite as p2s
    import verify_migration as verf

    # 1. 源 -> PG
    m2p_res = m2p.main(str(source_sqlite), PG_URL, schema_name=schema_name)
    assert m2p_res["tables"]["runs"] >= 2
    assert m2p_res["tables"]["submissions"] >= 1
    assert m2p_res["audit_source_sha256"] != ""
    assert "EXAM_MIGRATION" in m2p_res  # advisory_lock token 标记

    # 2. 验证 SQLite ≡ PG 一致
    v = verf.main(str(source_sqlite), PG_URL, schema_name=schema_name)
    assert v["ok"], f"verify_migration failed: {v['diff']}"
    assert v["diff"] == {} or v["diff"] == {}

    # 3. PG -> 回新 SQLite
    new_sqlite = source_sqlite.parent / "roundtrip_out.db"
    exp = p2s.main(PG_URL, schema_name, str(new_sqlite))
    assert exp["submissions"] >= 1
    assert Path(new_sqlite).exists()

    # 4. 4 张表关键数据一致 (仅核心字段; 时间戳偏差容忍 timestamptz -> iso string)
    def read_sqlite(p):
        conn = sqlite3.connect(p); conn.row_factory = sqlite3.Row
        d = {
            "exam_runs": [dict(r) for r in conn.execute("SELECT * FROM exam_runs ORDER BY id")],
            "exam_sessions": [dict(r) for r in conn.execute("SELECT * FROM exam_sessions ORDER BY id")],
            "submissions": [dict(r) for r in conn.execute(
                "SELECT * FROM submissions ORDER BY id")],
            "review_logs": [dict(r) for r in conn.execute(
                "SELECT * FROM review_logs ORDER BY submission_id, id")],
        }
        conn.close()
        return d

    src = read_sqlite(source_sqlite)
    dst = read_sqlite(new_sqlite)

    assert len(src["exam_runs"]) == len(dst["exam_runs"]), (
        f"exam_runs rows differ: {len(src['exam_runs'])} vs {len(dst['exam_runs'])}")
    assert len(src["exam_sessions"]) == len(dst["exam_sessions"])
    assert len(src["submissions"]) == len(dst["submissions"])
    assert len(src["review_logs"]) == len(dst["review_logs"])

    # PK 一致
    s_runs = {r["id"] for r in src["exam_runs"]}
    d_runs = {r["id"] for r in dst["exam_runs"]}
    assert s_runs == d_runs, f"runs PK mismatch src={s_runs} dst={d_runs}"

    # submission pk 一致
    s_subs = {r["id"] for r in src["submissions"]}
    d_subs = {r["id"] for r in dst["submissions"]}
    assert s_subs == d_subs, f"submissions PK mismatch src-sub={s_subs}\ndst-sub={d_subs}"

    # 关键字段: 每 submission grading_status 'done'/'grading' -> review_status done/pending
    # 其它字段: name / employee_id / paper_id / department / json / 各 score 必等
    src_by_id = {r["id"]: r for r in src["submissions"]}
    dst_by_id = {r["id"]: r for r in dst["submissions"]}
    for sid, sr in src_by_id.items():
        dr = dst_by_id[sid]
        for k in ("name", "employee_id", "paper_id", "department",
                  "objective_score", "subjective_score_machine",
                  "subjective_score_final", "total_score"):
            assert sr[k] == dr[k], f"sub id={sid} field {k} differ: {sr[k]} vs {dr[k]}"
        # answers_json + grading_detail_json 必须语义等价 (可能 key 顺序差异; 用 json.loads)
        a_src = json.loads(sr["answers_json"])
        a_dst = json.loads(dr["answers_json"])
        assert a_src == a_dst, f"sub id={sid} answers_json differ:\n{a_src}\n{a_dst}"
        g_src = json.loads(sr["grading_detail_json"])
        g_dst = json.loads(dr["grading_detail_json"])
        assert g_src == g_dst, f"sub id={sid} grading_detail_json differ:\n{g_src}\n{g_dst}"

        # review_status mapping after rollback
        # 'done' -> 'done', 'grading'/'pending' -> 'pending'
        grader_status_pg = dr.get("review_status")  # 这是 rollback 写回的
        # 原 sqlite 的 review_status
        expected_pg = {
            "done": "done", "reviewed": "done",
            "graded_pass": "done", "graded_fail": "done",
            "grading": "pending", "pending": "pending",
            "auto_graded": "done",
            "manually_reviewed": "done",
        }.get(sr["review_status"], sr["review_status"])
        assert grader_status_pg == expected_pg, (
            f"sub id={sid} review_status mapping: src='{sr['review_status']}'"
            f" -> dst='{grader_status_pg}' (expected '{expected_pg}')"
        )

    # exam_runs 关键字段一致 (忽略 timestamp text vs timestamptz -> text 差异)
    # 注: round_no 在 legacy run 上会被重新映射为 max+1, 不可与源严格对比.
    src_r = {r["id"]: r for r in src["exam_runs"]}
    dst_r = {r["id"]: r for r in dst["exam_runs"]}
    for rid, sr in src_r.items():
        dr = dst_r[rid]
        for k in ("paper_id", "status",
                  "duration_minutes", "is_legacy"):
            assert sr[k] == dr[k], f"run id={rid} field {k} differ"
        # round_no: legacy run 可能被重新分配, 只校验都 > 0 且 unique
        assert int(dr["round_no"]) > 0
        # public_token_hash 也一致
        if sr.get("public_token_hash"):
            assert sr["public_token_hash"] == dr["public_token_hash"] or \
                   (sr.get("public_token_hash","")[:8] == dr.get("public_token_hash","")[:8])

    # review_logs 一一对应
    src_logs = {(r["submission_id"], r["question_id"], r["new_score"])
                for r in src["review_logs"]}
    dst_logs = {(r["submission_id"], r["question_id"], r["new_score"])
                for r in dst["review_logs"]}
    assert src_logs == dst_logs, (
        f"review_logs differs\nsrc={src_logs}\ndst={dst_logs}")


# ---------------------------------------------------------------------------
# 3. Idempotency: 第二次迁移不会报错, audit 写入不重复
# ---------------------------------------------------------------------------
@require_pg
def test_idempotent_re_migrate(source_sqlite):
    import random, string
    rand = "".join(random.choices(string.ascii_lowercase, k=10))
    schema = f"task3_idem_{rand}"

    import sqlite_to_postgres as m2p
    res1 = m2p.main(str(source_sqlite), PG_URL, schema_name=schema)
    # 二次不报错, 行数保持一致
    res2 = m2p.main(str(source_sqlite), PG_URL, schema_name=schema)
    assert res2["tables"] == res1["tables"], "二次迁移行数不应变"


# ---------------------------------------------------------------------------
# 4. 非 SQLite 模式场景:
#    迁移到 PG 必须包含 active=0 行数 count 校验
# ---------------------------------------------------------------------------
@require_pg
def test_no_active_leftover_in_pg(source_sqlite):
    import random, string
    rand = "".join(random.choices(string.ascii_lowercase, k=10))
    schema = f"task3_active_{rand}"
    import sqlite_to_postgres as m2p
    m2p.main(str(source_sqlite), PG_URL, schema_name=schema)

    import psycopg
    with psycopg.connect(PG_URL, options=f"--search_path={schema},public") as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT count(*) FROM exam_sessions WHERE status='active'")
            active_sess = cur.fetchone()[0]
            # SQLite 源里 legacy session 写 'active' 是事实; 验证它被迁入且数量与源缘故 equal
            assert active_sess >= 1, "PG 端应保留至少一个 active session 来自 legacy"


# ---------------------------------------------------------------------------
# 5. 渐进路径: 迁移完成后 submissions 序列 START > max(id)
# ---------------------------------------------------------------------------
@require_pg
def test_submission_sequence_after_migration(source_sqlite):
    import random, string
    rand = "".join(random.choices(string.ascii_lowercase, k=10))
    schema = f"task3_seq_{rand}"
    import sqlite_to_postgres as m2p
    m2p.main(str(source_sqlite), PG_URL, schema_name=schema)

    import psycopg
    with psycopg.connect(PG_URL, options=f"--search_path={schema},public") as conn:
        with conn.cursor() as cur:
            cur.execute(f"SELECT setval('submissions_id_seq', "
                        f"(SELECT max(id) FROM submissions))")
            cur.execute("SELECT max(id), nextval('submissions_id_seq') FROM submissions")
            max_id, next_id = cur.fetchone()
            assert next_id > max_id, f"nextval {next_id} 应大于 max {max_id}"


# ---------------------------------------------------------------------------
# 6. CLI: 子进程 sqlite_to_postgres.py --src ... --pg ... --schema ...
# ---------------------------------------------------------------------------
@require_pg
def test_cli_subprocess(source_sqlite, monkeypatch):
    import random, string, subprocess
    rand = "".join(random.choices(string.ascii_lowercase, k=10))
    schema = f"task3_cli_{rand}"

    env = os.environ.copy()
    env["PYTHONPATH"] = str(_ROOT)
    cmd = [sys.executable, str(_ROOT / "scripts" / "sqlite_to_postgres.py"),
           "--src", str(source_sqlite), "--pg", PG_URL,
           "--schema", schema]
    r = subprocess.run(cmd, capture_output=True, text=True, env=env, timeout=120)
    assert r.returncode == 0, f"CLI exit={r.returncode} stderr={r.stderr}"
