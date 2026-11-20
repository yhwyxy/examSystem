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
    # migration_audit/PUBLIC 引用锁 + CREATE TABLE 等默认 public schema; 我们临时
    # 把 search_path 设为 schema, 让 CREATE TABLE 落在新 schema.
    cur = conn.cursor()
    cur.execute(f"SET search_path = {schema}, public;")
    from pathlib import Path
    migration_dir = Path(__file__).resolve().parents[2] / "migrations"
    for path in sorted(migration_dir.glob("*.sql")):
        cur.execute(path.read_text())
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
    out = repo.complete_job(c2, j, 0.0, 0.0, b'[]', review_status="graded")
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
    out = repo.complete_job(c2, j, 0.0, 0.0, b'[]', review_status="graded")
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
    c.commit()
    # job 判死必须同步 submissions -> grading_status='failed' + grading_error +
    # review_status='need_review', 否则管理端永远显示"评分中".
    cur = c.cursor()
    cur.execute("SELECT grading_status, grading_error, review_status "
                "FROM submissions WHERE id=%s", (j3.submission_id,))
    gstatus, gerr, rstatus = cur.fetchone()
    assert gstatus == "failed", f"submission grading_status={gstatus} want failed"
    assert gerr == "boom", f"grading_error={gerr!r} want 'boom'"
    assert rstatus == "need_review", f"review_status={rstatus} want need_review"
    c.close()


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
    out = repo.complete_job(c3, j1, 0.0, 0.0, b'[]', review_status="graded")
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
    out1 = repo.complete_job(c2, j, 0.0, 0.0, b'[]', review_status="graded")
    assert out1 == "done"
    c2.commit(); c2.close()
    c3 = _new_conn(schema)
    out2 = repo.complete_job(c3, j, 0.0, 0.0, b'[]', review_status="graded")
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


def test_runtime_process_kill_recovers_once_and_fences_late_writes(grading_setup, tmp_path):
    """Kill a real runtime during grading, reclaim its lease, and persist one result."""
    import signal
    import subprocess
    import sys
    import time
    from pathlib import Path
    from psycopg.conninfo import make_conninfo
    from psycopg.rows import dict_row
    from scoring_worker import repository as repo

    conn, schema, job_id = grading_setup
    ready = tmp_path / "grading-started"
    env = {**os.environ, "CRASH_TEST_DSN": make_conninfo(_pg_url(), options=f"-c search_path={schema},public"),
           "CRASH_TEST_READY": str(ready)}
    child = r'''
import os, time
from pathlib import Path
from types import SimpleNamespace
import psycopg
from scoring_worker import grading, runtime

def blocked_grade(*args, **kwargs):
    Path(os.environ['CRASH_TEST_READY']).write_text('grading')
    while True:
        time.sleep(1)

grading.grade_submission = blocked_grade
cfg = SimpleNamespace(database_url=os.environ['CRASH_TEST_DSN'], worker_id='crash-test',
                      lease_seconds=2, heartbeat_seconds=20, multiple_choice_partial=True)
cache = SimpleNamespace(get=lambda *args: {})
with psycopg.connect(cfg.database_url) as conn:
    runtime.run_one_job(conn, cfg, cache, None)
'''
    process = subprocess.Popen([sys.executable, "-c", child], cwd=Path(__file__).resolve().parents[2],
                               env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        deadline = time.monotonic() + 10
        while not ready.exists() and time.monotonic() < deadline and process.poll() is None:
            time.sleep(0.05)
        assert ready.exists(), "runtime did not enter synthetic grading"
        old = repo._row_to_job(conn.cursor(row_factory=dict_row).execute(
            "SELECT * FROM grading_jobs WHERE id=%s", (job_id,)).fetchone())
        conn.commit()
        assert old.status == "leased" and old.attempts == 1
        process.kill()
        process.wait(timeout=5)
        assert process.returncode == -signal.SIGKILL
        # Observe real lease expiration instead of manually rewriting this lease.
        deadline = time.monotonic() + 6
        replacement = None
        while time.monotonic() < deadline:
            replacement = repo.claim_job(conn, "recovery-test", 30)
            conn.commit()
            if replacement is not None:
                break
            time.sleep(0.1)
        assert replacement is not None and replacement.id == job_id
        assert replacement.attempts == 2 and replacement.lease_token != old.lease_token
        assert repo.complete_job(conn, replacement, 8, 8, b'[]') == "done"
        conn.commit()
        assert repo.complete_job(conn, old, 99, 99, b'[]') == "lost"
        assert repo.fail_job(conn, old, "late crash failure") == "lost"
        conn.commit()
        assert conn.execute("SELECT total_score,grading_status FROM submissions WHERE id=%s",
                            (old.submission_id,)).fetchone() == (8, "done")
        assert conn.execute("SELECT count(*) FROM grading_history WHERE submission_id=%s",
                            (old.submission_id,)).fetchone()[0] == 1
        assert conn.execute("SELECT status,last_error FROM grading_jobs WHERE id=%s", (job_id,)).fetchone() == ("done", None)
        assert repo.claim_job(conn, "third-worker", 30) is None
        conn.commit()
    finally:
        if process.poll() is None:
            process.kill()
        process.communicate(timeout=5)


@pytest.mark.parametrize("new_generation", [False, True])
@pytest.mark.parametrize("final_failure", ["expired", "reported"])
def test_attempt_limit_and_old_generation_failure_guard(grading_setup, new_generation, final_failure):
    """Final live leases may finish; final expired leases stop, never failing a newer grade."""
    from types import SimpleNamespace
    from scoring_worker import repository as repo, runtime
    conn, schema, job_id = grading_setup
    job = None
    for attempt in range(1, 4):
        job = repo.claim_job(conn, "crashing-worker", 30)
        conn.commit()
        assert job is not None and job.attempts == attempt
        if attempt < 3:
            conn.execute("UPDATE grading_jobs SET lease_until=now()-interval '1 second' WHERE id=%s", (job_id,))
            conn.commit()
    assert repo.claim_job(conn, "other-worker", 30) is None
    conn.commit()
    assert conn.execute("SELECT status FROM grading_jobs WHERE id=%s", (job_id,)).fetchone()[0] == "leased"
    if new_generation:
        conn.execute("""UPDATE submissions SET grading_generation=2,grading_status='done',
                        review_status='reviewed',total_score=9 WHERE id=%s""", (job.submission_id,))
        conn.commit()
    if final_failure == "reported":
        assert repo.fail_job(conn, job, "final reported failure") == "dead"
        conn.commit()
    else:
        conn.execute("UPDATE grading_jobs SET lease_until=now()-interval '1 second' WHERE id=%s", (job_id,))
        conn.commit()
        # Idle cleanup must commit even when the caller later rolls back/closes.
        cfg = SimpleNamespace(worker_id="cleanup-worker", lease_seconds=30)
        assert runtime.run_one_job(conn, cfg, None, None) is False
        conn.rollback()
    assert conn.execute("SELECT status,attempts,lease_token FROM grading_jobs WHERE id=%s", (job_id,)).fetchone() == ("dead", 3, None)
    row = conn.execute("SELECT grading_generation,grading_status,review_status,total_score,grading_error FROM submissions WHERE id=%s",
                       (job.submission_id,)).fetchone()
    if new_generation:
        assert row == (2, "done", "reviewed", 9, None)
    else:
        assert row[:3] == (1, "failed", "need_review")
        assert row[4] == ("LEASE_ATTEMPTS_EXHAUSTED" if final_failure == "expired" else "final reported failure")
    assert conn.execute("SELECT count(*) FROM grading_history").fetchone()[0] == 0
    conn.commit()


def test_eight_workers_complete_sixty_four_jobs_without_duplicate_history(grading_setup):
    """Local queue contention check, not a production capacity benchmark."""
    from concurrent.futures import ThreadPoolExecutor
    import threading
    from scoring_worker import repository as repo
    conn, schema, _ = grading_setup
    for index in range(1, 64):
        sid = conn.execute("""INSERT INTO submissions(name,employee_id,paper_id,run_id,answers_json,
            objective_score,grading_detail_json,grading_status,review_status,grading_generation,submitted_at)
            VALUES('synthetic load',%s,'paper-test','run-test','{}',4.5,'[]','pending','grading',1,now()) RETURNING id""",
            (f"load-{index}",)).fetchone()[0]
        conn.execute("""INSERT INTO grading_jobs(submission_id,paper_id,run_id,generation,status,
            attempts,max_attempts,available_at,created_at,updated_at)
            VALUES(%s,'paper-test','run-test',1,'queued',0,3,now(),now(),now())""", (sid,))
    conn.commit()
    start = threading.Barrier(8)

    def consume(index):
        with _new_conn(schema) as worker:
            worker.commit()
            start.wait(timeout=10)
            completed = []
            while True:
                job = repo.claim_job(worker, f"load-worker-{index}", 30)
                worker.commit()
                if job is None:
                    return completed
                score = job.submission_id % 7
                assert repo.complete_job(worker, job, score, score, b'[]') == "done"
                worker.commit()
                completed.append(job.id)

    with ThreadPoolExecutor(max_workers=8) as executor:
        receipts = [job for completed in executor.map(consume, range(8)) for job in completed]
    assert len(receipts) == len(set(receipts)) == 64
    assert conn.execute("SELECT count(*) FROM grading_jobs WHERE status='done' AND attempts=1").fetchone()[0] == 64
    assert conn.execute("SELECT count(*) FROM grading_history").fetchone()[0] == 64
    assert conn.execute("""SELECT count(*) FROM submissions WHERE grading_status <> 'done'
        OR total_score <> coalesce(objective_score,0) + (id % 7)
        OR subjective_score_machine <> (id % 7) OR subjective_score_final <> (id % 7)""").fetchone()[0] == 0
    assert conn.execute("SELECT count(*) FROM grading_jobs WHERE lease_owner IS NOT NULL OR lease_token IS NOT NULL").fetchone()[0] == 0
    conn.commit()


def test_exhausted_cleanup_skips_submission_locked_by_regrade(grading_setup):
    from concurrent.futures import ThreadPoolExecutor
    from scoring_worker import repository as repo
    conn, schema, job_id = grading_setup
    job = repo.claim_job(conn, "last-attempt", 30)
    conn.execute("UPDATE grading_jobs SET attempts=max_attempts,lease_until=now()-interval '1 second' WHERE id=%s", (job_id,))
    conn.commit()
    conn.execute("SELECT id FROM submissions WHERE id=%s FOR UPDATE", (job.submission_id,))
    def cleanup():
        with _new_conn(schema) as worker:
            return repo.claim_job(worker, "cleanup", 30)
    with ThreadPoolExecutor(max_workers=1) as executor:
        result = executor.submit(cleanup)
        try:
            assert result.result(timeout=3) is None
        finally:
            conn.rollback()
    assert repo.claim_job(conn, "cleanup-after-regrade", 30) is None
    conn.commit()
    assert conn.execute("SELECT status FROM grading_jobs WHERE id=%s", (job_id,)).fetchone()[0] == "dead"


@pytest.mark.parametrize("operation", ["complete", "failure", "renew"])
def test_completion_checks_wall_clock_even_in_older_transaction(grading_setup, operation):
    import time
    from scoring_worker import repository as repo
    conn, schema, job_id = grading_setup
    job = repo.claim_job(conn, "short-lease", 1)
    conn.commit()
    with _new_conn(schema) as completing:
        # SET search_path already began this transaction before expiry.
        completing.execute("SELECT now()")
        time.sleep(1.2)
        if operation == "complete":
            assert repo.complete_job(completing, job, 8, 8, b'[]') == "lost"
        elif operation == "failure":
            assert repo.fail_job(completing, job, "late failure") == "lost"
        else:
            assert repo.renew_lease(completing, job.id, "short-lease", job.lease_token, 30) is False
    assert conn.execute("SELECT count(*) FROM grading_history").fetchone()[0] == 0
