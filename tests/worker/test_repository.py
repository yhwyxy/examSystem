"""Repository SQL 集成测试: claim 并发与 fenced complete 行为 (需 PostgreSQL).

隔离: 用独立 schema (CREATE SCHEMA + search_path) 与 Go testutil.NewSchema 等价.
URL: TEST_DATABASE_URL 或 DATABASE_URL 环境变量.
"""
import os
import secrets

import psycopg
import pytest


def _pg_url() -> str:
    url = os.getenv("TEST_DATABASE_URL") or os.getenv("DATABASE_URL")
    if not url:
        pytest.skip("TEST_DATABASE_URL / DATABASE_URL 未设置")
    return url


def _apply_migrations(conn, schema: str) -> None:
    """跑 migrations/0001_initial.sql 在指定 schema 下."""
    sql_path = os.path.join(os.path.dirname(__file__), "..", "..",
                            "migrations", "0001_initial.sql")
    sql = open(sql_path).read()
    # migration_audit/PUBLIC 引用锁 + CREATE TABLE 等默认 public schema; 我们临时
    # 把 search_path 设为 schema, 让 CREATE TABLE 落在新 schema.
    cur = conn.cursor()
    cur.execute(f"SET search_path = {schema}, public;")
    cur.execute(sql)
    conn.commit()


@pytest.fixture
def schema_conn():
    """独立 schema fixture: 创建 schema + migrate + 返回 conn (已设 search_path).

    cleanup: drop schema + close. 并发各 test 用独立 schema, 互不污染.
    """
    url = _pg_url()
    schema = "test_" + secrets.token_hex(4)
    admin = psycopg.connect(url, autocommit=True)
    admin.execute(f"CREATE SCHEMA {schema};")
    admin.close()
    conn = psycopg.connect(url, autocommit=False)
    conn.execute(f"SET search_path = {schema}, public;")
    _apply_migrations(conn, schema)
    conn.commit()
    yield conn, schema
    try:
        conn.rollback()
    except Exception:
        pass
    conn.close()
    # DROP SCHEMA otherwise waits forever if a test leaked a transaction lock.
    # Make cleanup bounded so the original test failure remains visible.
    admin = None
    try:
        admin = psycopg.connect(url, autocommit=True)
        admin.execute("SET lock_timeout = '2s';")
        admin.execute(f"DROP SCHEMA IF EXISTS {schema} CASCADE;")
    except (psycopg.errors.LockNotAvailable, psycopg.errors.ObjectInUse):
        pass
    finally:
        if admin is not None:
            try:
                admin.close()
            except Exception:
                pass


@pytest.fixture
def grading_setup(schema_conn):
    """插入 run + submission + 1 grading_jobs row (gen=1) -> return (conn, schema, job_id)."""
    conn, schema = schema_conn
    cur = conn.cursor()
    cur.execute("""
        INSERT INTO exam_runs (id, paper_id, public_token_hash, status,
                                snapshot_path, snapshot_hash, round_no,
                                duration_minutes, opened_at, created_at)
        VALUES ('run-test', 'paper-test', 'phtok-test', 'open',
                '/tmp/x.json', 'sha-test', 1, 60, now(), now())
        RETURNING id;
    """)
    cur.fetchall()
    cur.execute("""
        INSERT INTO submissions (name, employee_id, paper_id, run_id,
                                 answers_json, objective_score,
                                 grading_detail_json, grading_status,
                                 review_status, grading_generation,
                                 submitted_at)
        VALUES ('tester', 'emp1', 'paper-test', 'run-test', '{}',
                0, '[]', 'pending', 'grading', 1, now())
        RETURNING id;
    """)
    sub_id = cur.fetchone()[0]
    cur.execute("""
        INSERT INTO grading_jobs (submission_id, paper_id, run_id,
                                   generation, status, attempts, max_attempts,
                                   available_at, created_at, updated_at)
        VALUES (%s, 'paper-test', 'run-test', 1, 'queued', 0, 3,
                now(), now(), now())
        RETURNING id;
    """, (sub_id,))
    job_id = cur.fetchone()[0]
    conn.commit()
    yield conn, schema, job_id




def _new_conn(schema: str):
    """建独立 conn, search_path 设为 schema + public."""
    conn = psycopg.connect(_pg_url(), autocommit=False)
    conn.execute(f"SET search_path = {schema}, public;")
    return conn


def test_claim_locks_single_job(grading_setup):
    """两 conn 并发 claim 同一 job -> w1 成功 + commit; w2 期间为 None (SkipLocked)."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup

    c1 = _new_conn(schema)
    j = repo.claim_job(c1, "w1", 30)
    assert j is not None and j.id == job_id
    c1.commit()
    c2 = _new_conn(schema)
    j2 = repo.claim_job(c2, "w2", 30)
    assert j2 is None, "w2 不应在 w1 租约期间抢同一 job"
    c2.rollback(); c2.close()
    repo.release_job(c1, j); c1.commit(); c1.close()


def test_complete_writes_submission_and_job_status(grading_setup):
    """complete_job: 写 submission grading_status=done + job status=done."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup
    c1 = _new_conn(schema)
    j = repo.claim_job(c1, "w1", 30)
    assert j is not None
    c1.commit(); c1.close()
    c2 = _new_conn(schema)
    out = repo.complete_job(c2, j, 0, 0.0, 0.0, b'[]', review_status="graded")
    assert out == "done"
    c2.commit()
    cur = c2.cursor()
    cur.execute("SELECT grading_status FROM submissions WHERE id=%s", (j.submission_id,))
    s_status = cur.fetchone()[0]
    assert s_status == "done", f"submission grading_status={s_status} want done"
    cur.execute("SELECT status FROM grading_jobs WHERE id=%s", (j.id,))
    j_status = cur.fetchone()[0]
    assert j_status == "done", f"job status={j_status} want done"
    c2.close()


def test_load_job_payload_returns_started_at(grading_setup):
    """任务载荷必须读取 submissions.started_at，而不是不存在的 starts_at。"""
    from scoring_worker import repository as repo
    conn, schema, job_id = grading_setup

    payload = repo.load_job_payload(conn, job_id)

    assert payload["submission_id"] > 0
    assert "started_at" in payload



def test_complete_lost_when_no_active_lease(grading_setup):
    """lease 过期后 complete -> 'lost' (fenced verify SQL 不匹配)."""
    from scoring_worker import repository as repo
    import time as _t
    base_conn, schema, job_id = grading_setup
    c1 = _new_conn(schema)
    j = repo.claim_job(c1, "w1", 1)
    assert j is not None
    c1.commit(); c1.close()
    _t.sleep(2.5)  # 等 lease 过期
    c2 = _new_conn(schema)
    out = repo.complete_job(c2, j, 0, 0.0, 0.0, b'[]', review_status="graded")
    assert out == "lost", f"lease 过期后 complete 应返'lost', got {out}"
    c2.rollback(); c2.close()


def test_fail_requeue_then_dead(grading_setup):
    """attempts < max -> queued + backoff; max_attempts 达到 -> dead."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup
    c = _new_conn(schema)
    j = repo.claim_job(c, "w1", 30); assert j is not None
    c.commit()
    out = repo.fail_job(c, j, error_msg="boom", base_backoff=0)
    assert out == "queued"
    c.commit()
    j2 = repo.claim_job(c, "w1", 30); c.commit()
    assert j2 is not None, "backoff 0 -> 应立即可 claim"
    out2 = repo.fail_job(c, j2, error_msg="boom", base_backoff=0)
    assert out2 == "queued"
    c.commit()
    j3 = repo.claim_job(c, "w1", 30); assert j3.attempts == 3
    out3 = repo.fail_job(c, j3, error_msg="boom", base_backoff=0)
    assert out3 == "dead", f"third fail expect dead, got {out3}"
    c.commit(); c.close()


def test_renew_lease_returns_true_within_window(grading_setup):
    """renew_lease 在 lease_until 前的真 token -> True, lease_until 被延长."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup
    c1 = _new_conn(schema)
    j = repo.claim_job(c1, "w1", 30); assert j is not None
    c1.commit(); c1.close()
    c2 = _new_conn(schema)
    ok = repo.renew_lease(c2, j.id, "w1", j.lease_token, 60)
    assert ok is True
    c2.commit(); c2.close()


def test_renew_lease_rejects_wrong_token(grading_setup):
    """错误 token -> False (fenced heartbeat 不允许跨 worker)."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup
    c1 = _new_conn(schema)
    j = repo.claim_job(c1, "w1", 30); assert j is not None
    c1.commit(); c1.close()
    c2 = _new_conn(schema)
    ok = repo.renew_lease(c2, j.id, "w1", "wrong_token", 60)
    assert ok is False
    c2.rollback(); c2.close()


def test_regrade_generation_higher_supersedes_old(grading_setup):
    """regrade gen=2: admin 提 submission.grading_generation -> 2 + 插 gen=2 job,
    完成 gen=1 时 submission 未被覆盖 -> 'superseded'."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup
    c1 = _new_conn(schema)
    j1 = repo.claim_job(c1, "w1", 30); assert j1 is not None
    c1.commit(); c1.close()
    c2 = _new_conn(schema)
    cur2 = c2.cursor()
    cur2.execute("UPDATE submissions SET grading_generation=2 WHERE id=%s", (j1.submission_id,))
    cur2.execute("""INSERT INTO grading_jobs (submission_id, paper_id, run_id,
                     generation, status, attempts, max_attempts,
                     available_at, created_at, updated_at)
                  VALUES (%s, 'paper-test', 'run-test', 2, 'queued', 0, 3,
                          now(), now(), now())""", (j1.submission_id,))
    c2.commit(); c2.close()
    c3 = _new_conn(schema)
    out = repo.complete_job(c3, j1, 0, 0.0, 0.0, b'[]', review_status="graded")
    assert out == "superseded", f"gen mismatch 应返 'superseded', got {out}"
    c3.rollback(); c3.close()


def test_double_complete_is_idempotent_on_done(grading_setup):
    """complete 完成后二次调 complete -> 'lost' (lease 已被清)."""
    from scoring_worker import repository as repo
    base_conn, schema, job_id = grading_setup
    c1 = _new_conn(schema)
    j = repo.claim_job(c1, "w1", 30); assert j is not None
    c1.commit(); c1.close()
    c2 = _new_conn(schema)
    out1 = repo.complete_job(c2, j, 0, 0.0, 0.0, b'[]', review_status="graded")
    assert out1 == "done"
    c2.commit(); c2.close()
    c3 = _new_conn(schema)
    out2 = repo.complete_job(c3, j, 0, 0.0, 0.0, b'[]', review_status="graded")
    assert out2 == "lost", f"double complete 应返 'lost', got {out2}"
    c3.rollback(); c3.close()


def test_ast_no_import_backend():
    """plan 边界: scoring_worker 不允许 import root.backend (独立 uv 工程)."""
    import ast, pathlib
    pkg_dir = pathlib.Path("scoring_worker")
    for f in pkg_dir.glob("*.py"):
        tree = ast.parse(f.read_text())
        for n in ast.walk(tree):
            if isinstance(n, ast.Import) and any(a.name == "backend" for a in n.names):
                pytest.fail(f"{f}: import backend")
            if isinstance(n, ast.ImportFrom) and n.module == "backend":
                pytest.fail(f"{f}: from backend import ...")
