"""Repository SQL 单测: claim 并发与 fenced complete 行为 (需 PostgreSQL, 通过
TestSchema fixture).
"""
import os
import psycopg
import pytest
import threading


def _skip_if_no_db():
    url = os.getenv("DATABASE_URL") or os.getenv("TEST_DATABASE_URL")
    if not url:
        pytest.skip("DATABASE_URL 未设置")
    return url


@pytest.fixture
def schema():
    url = _skip_if_no_db()
    conn = psycopg.connect(url, autocommit=False)
    # pretrip ping
    cur = conn.cursor()
    cur.execute("SELECT 1;")
    conn.commit()
    return conn


@pytest.fixture
def grading_setup(schema):
    """插入 run + submission + 1 grading_jobs row (gen=1) -> return job_id."""
    cur = schema.cursor()
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
                0, '[]', 'pending', 'grading', 0, now())
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
    schema.commit()
    yield job_id
    cur.execute("DELETE FROM submissions WHERE run_id='run-test'; DELETE FROM exam_runs WHERE id='run-test';")
    schema.commit()


def test_claim_locks_single_job(grading_setup):
    """并发两个 worker (连接) claim 同一 job -> 只有一个成功 (lease 期间另一个为空)."""
    from scoring_worker import repository as repo
    url = _skip_if_no_db()
    # w1: claim 并 commit -> lease 持久
    c1 = psycopg.connect(url, autocommit=False)
    j = repo.claim_job(c1, "w1", 30)
    assert j is not None
    c1.commit()
    # w2: 用独立连接尝试 claim -> 应该空
    c2 = psycopg.connect(url, autocommit=False)
    j2 = repo.claim_job(c2, "w2", 30)
    assert j2 is None, "w2 不应在 w1 租约期间抢同一 job"
    c2.rollback(); c2.close()
    # 释放 lease
    repo.release_job(c1, j)
    c1.commit(); c1.close()


def test_complete_writes_submission_and_job_status(grading_setup):
    """complete_job: 写 submission (grading_status=done) + job status=done."""
    from scoring_worker import repository as repo
    url = _skip_if_no_db()
    c1 = psycopg.connect(url, autocommit=False)
    j = repo.claim_job(c1, "w1", 30)
    assert j is not None
    c1.commit()
    c2 = psycopg.connect(url, autocommit=False)
    detail_json = b'[]'
    out = repo.complete_job(c2, j, 0, 0.0, 0.0, detail_json, review_status="graded")
    assert out == "done", f"complete 应成功 return 'done', got {out}"
    c2.commit()
    cur = psycopg.connect(url, autocommit=True).cursor()
    res = cur.execute("SELECT grading_status, review_status FROM submissions WHERE id=%s", (j.submission_id,))
    s = cur.fetchone()
    assert s[0] == "done", f"submission grading_status={s[0]} want done"
    cur.execute("SELECT status FROM grading_jobs WHERE id=%s", (j.id,))
    j_status = cur.fetchone()[0]
    assert j_status == "done", f"job status={j_status} want done"


def test_complete_lost_when_no_active_lease(grading_setup):
    """如果 lease 已过期, 不再租 -> complete 返回 'lost'."""
    from scoring_worker import repository as repo
    url = _skip_if_no_db()
    c1 = psycopg.connect(url, autocommit=False)
    j = repo.claim_job(c1, "w1", 1)  # 1s lease
    assert j is not None
    c1.commit()
    import time as _t
    _t.sleep(2.5)  # 等 lease 过期
    c2 = psycopg.connect(url, autocommit=False)
    out = repo.complete_job(c2, j, 0, 0.0, 0.0, b'[]', review_status="graded")
    assert out == "lost", f"lease 过期后 complete 应返'lost', got {out}"
    c2.close()  # rollback


def test_fail_requeue_then_dead(grading_setup):
    """attempts<max -> queued + backoff available_at > now; max_attempts 达到 -> dead."""
    from scoring_worker import repository as repo
    url = _skip_if_no_db()
    # create a separate job with max_attempts=2 for cleaner assertions
    c = psycopg.connect(url, autocommit=False)
    j = repo.claim_job(c, "w1", 30)
    assert j is not None
    c.commit()
    # attempts 现在为 1; max_attempts=3 -> queued
    out = repo.fail_job(c, j, error_msg="boom")
    assert out == "queued"
    c.commit()
    # re-claim -> attempts=2 -> queued again
    j2 = repo.claim_job(c, "w1", 30)
    c.commit()
    out2 = repo.fail_job(c, j2, error_msg="boom")
    assert out2 == "queued"
    c.commit()
    # claim 第 3 次 attempts=3 -> dead
    j3 = repo.claim_job(c, "w1", 30)
    assert j3.attempts == 3
    out3 = repo.fail_job(c, j3, error_msg="boom")
    assert out3 == "dead", f"third fail expect dead, got {out3}"
    c.commit()
