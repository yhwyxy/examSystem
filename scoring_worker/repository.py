"""PostgreSQL 队列 repository: 原子 claim / heartbeat renew / fenced complete / failure.

所有 SQL 使用 psycopg 3 Connection (sync, 支持 autocommit). 通过 CTE + FOR UPDATE
SKIP LOCKED 保证多 worker 并发安全; fenced complete 校验 lease_owner / lease_token
/ lease_until > now 防止超 - 期 write-back 误覆盖.
"""
from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Optional

from psycopg.rows import dict_row
from psycopg.types.json import Jsonb


@dataclass
class Job:
    """队列行 (grading_jobs)."""
    id: int
    submission_id: int
    paper_id: str
    run_id: str
    generation: int
    attempts: int
    max_attempts: int
    lease_owner: Optional[str]
    lease_token: Optional[str]
    lease_until: Optional["object"]  # datetime; 用 Optional 防止 import datetime 强约束
    status: str
    available_at: "object"  # datetime
    created_at: "object"
    updated_at: "object"


# 完成结果语义别名: 'done' 状态正常完成, 'superseded' 被 higher generation 抢占.
COMPLETE_DONE = "done"
COMPLETE_SUPERSEDED = "superseded"

# 失败结果语义别名: 'queued' 重新入队 (backoff), 'dead' 超过 max_attempts, 'lost' 租约失效.
FAIL_QUEUED = "queued"
FAIL_DEAD = "dead"
FAIL_LOST = "lost"


_CLAIM_SQL = """
WITH candidate AS (
    SELECT id
    FROM grading_jobs
    WHERE (status = 'queued' AND available_at <= now())
       OR (status = 'leased' AND lease_until < now())
    ORDER BY available_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE grading_jobs j
SET status = 'leased',
    attempts = j.attempts + 1,
    lease_owner = %(worker_id)s,
    lease_token = %(lease_token)s,
    lease_until = now() + make_interval(secs => %(lease_seconds)s),
    updated_at = now()
FROM candidate
WHERE j.id = candidate.id
RETURNING j.*;
"""


def claim_job(conn, worker_id: str, lease_seconds: int) -> Optional[Job]:
    """原子抢占一条 job. 返回 Job 或 None (空队列).

    SQL: 单条 CTE + UPDATE + FOR UPDATE SKIP LOCKED.
    两 worker 并发不会抢同一行 (SkipLocked); lease 过期可被回收.
    """
    import secrets
    cur = conn.cursor(row_factory=dict_row)
    cur.execute(
        _CLAIM_SQL,
        {
            "worker_id": worker_id,
            "lease_token": secrets.token_hex(32),
            "lease_seconds": max(1, int(lease_seconds)),
        },
    )
    row = cur.fetchone()
    if row is None:
        return None
    return _row_to_job(row)


_RENEW_SQL = """
UPDATE grading_jobs
SET lease_until = now() + make_interval(secs => %(lease_seconds)s),
    updated_at = now()
WHERE id = %(job_id)s
  AND status = 'leased'
  AND lease_owner = %(worker_id)s
  AND lease_token = %(lease_token)s
  AND lease_until > now()
RETURNING id;
"""


def renew_lease(conn, job_id: int, worker_id: str, lease_token: str,
                lease_seconds: int) -> bool:
    """心跳续租. 必须在 lease_until 之前; 否则租失效 -> 返回 False."""
    cur = conn.execute(
        _RENEW_SQL,
        {
            "job_id": job_id,
            "worker_id": worker_id,
            "lease_token": lease_token,
            "lease_seconds": max(1, int(lease_seconds)),
        },
    )
    return cur.fetchone() is not None


_COMPLETE_VERIFY_SQL = """
SELECT id FROM grading_jobs
WHERE id = %(job_id)s
  AND status = 'leased'
  AND lease_owner = %(worker_id)s
  AND lease_token = %(lease_token)s
  AND lease_until > now()
FOR UPDATE;
"""

_COMPLETE_SUBMISSION_SQL = """
UPDATE submissions
SET grading_detail_json = %(grading_detail_json)s,
    subjective_score_machine = %(subjective_score_machine)s,
    subjective_score_final = %(subjective_score_final)s,
    total_score = %(objective_score)s + %(subjective_score_final)s,
    review_status = %(review_status)s,
    grading_status = 'done',
    graded_at = now()
WHERE id = %(submission_id)s AND grading_generation = %(generation)s
RETURNING id;
"""

_COMPLETE_JOB_DONE_SQL = """
UPDATE grading_jobs
SET status = 'done',
    lease_owner = NULL,
    lease_token = NULL,
    lease_until = NULL,
    updated_at = now()
WHERE id = %(job_id)s
RETURNING id;
"""

_COMPLETE_JOB_SUPERSEDED_SQL = """
UPDATE grading_jobs
SET status = 'superseded',
    lease_owner = NULL,
    lease_token = NULL,
    lease_until = NULL,
    updated_at = now()
WHERE id = %(job_id)s
RETURNING id;
"""



def complete_job(conn, job: Job, objective_score: int,
                 subjective_score_machine: float, subjective_score_final: float,
                 grading_detail_json, review_status: str = "graded") -> str:
    """Fenced 完成: verify lease -> update submission -> job done.

    返回 'done' (正常) 或 'lost' (租约已失效 / 被回收). Submission 写入位置仅当
    grading_generation 匹配 (防 higher generation 抢占 -> return 'lost').
    ``grading_detail_json`` accepts JSON-compatible objects plus serialized JSON
    bytes/strings, because the runtime serializes the detail before writing it.
    """
    if isinstance(grading_detail_json, bytes):
        grading_detail_json = grading_detail_json.decode("utf-8")
    if isinstance(grading_detail_json, str):
        grading_detail_json = json.loads(grading_detail_json)

    # 1) 校验租约仍在 (lease_owner / lease_token / lease_until > now)
    if conn.execute(_COMPLETE_VERIFY_SQL, {
        "job_id": job.id, "worker_id": job.lease_owner,
        "lease_token": job.lease_token,
    }).fetchone() is None:
        return "lost"
    # 2) 写 submission; grading_generation 校验防跨代写入.
    if conn.execute(_COMPLETE_SUBMISSION_SQL, {
        "submission_id": job.submission_id,
        "grading_detail_json": Jsonb(grading_detail_json),
        "subjective_score_machine": subjective_score_machine,
        "subjective_score_final": subjective_score_final,
        "objective_score": objective_score,
        "review_status": review_status,
        "generation": job.generation,
    }).fetchone() is None:
        conn.execute(_COMPLETE_JOB_SUPERSEDED_SQL, {"job_id": job.id})
        return "superseded"
    # 3) 写 job status='done' + 清空 lease
    conn.execute(_COMPLETE_JOB_DONE_SQL, {"job_id": job.id})
    return "done"


_FAIL_VERIFY_SQL = """
SELECT 1 FROM grading_jobs
WHERE id = %(job_id)s
  AND lease_owner = %(worker_id)s
  AND lease_token = %(lease_token)s
  AND lease_until > now()
FOR UPDATE;
"""

_FAIL_REQUEUE_SQL = """
UPDATE grading_jobs
SET status = 'queued',
    lease_owner = NULL,
    lease_token = NULL,
    lease_until = NULL,
    available_at = now() + make_interval(secs => %(backoff)s),
    last_error = %(error_msg)s,
    updated_at = now()
WHERE id = %(job_id)s
RETURNING id;
"""

_FAIL_DEAD_SQL = """
UPDATE grading_jobs
SET status = 'dead',
    lease_owner = NULL,
    lease_token = NULL,
    lease_until = NULL,
    last_error = %(error_msg)s,
    updated_at = now()
WHERE id = %(job_id)s
RETURNING id;
"""

_FAIL_LOST_SET_SQL = """
UPDATE grading_jobs
SET last_error = %(error_msg)s,
    updated_at = now()
WHERE id = %(job_id)s AND status != 'leased'
RETURNING status;
"""


def fail_job(conn, job: Job, error_msg: str, base_backoff: int = 2) -> str:
    """失败处理: 验证租约 -> 重新入队 with backoff / dead / lost.

    返回 'queued' (重新入队 backoff) / 'dead' (max_attempts 达到) /
    'lost' (lease 已失效, 不再修改).
    """
    if conn.execute(_FAIL_VERIFY_SQL, {
        "job_id": job.id, "worker_id": job.lease_owner,
        "lease_token": job.lease_token,
    }).fetchone() is None:
        conn.execute(_FAIL_LOST_SET_SQL, {
            "job_id": job.id, "error_msg": error_msg[:2000]
        })
        return FAIL_LOST
    attempts = job.attempts  # claim 已 attempts+1, 故 attempts 是已增加的值
    if attempts >= job.max_attempts:
        conn.execute(_FAIL_DEAD_SQL, {
            "job_id": job.id, "error_msg": error_msg[:2000]
        })
        return FAIL_DEAD
    backoff = min(3600, base_backoff * (2 ** (attempts - 1)))
    conn.execute(_FAIL_REQUEUE_SQL, {
        "job_id": job.id,
        "backoff": backoff,
        "error_msg": error_msg[:2000],
    })
    return FAIL_QUEUED


_RELEASE_SQL = """
UPDATE grading_jobs
SET status = 'queued', lease_owner = NULL, lease_token = NULL, lease_until = NULL,
    available_at = now(), updated_at = now()
WHERE id = %(job_id)s
  AND lease_owner = %(worker_id)s AND lease_token = %(lease_token)s
RETURNING id;
"""


def release_job(conn, job: Job) -> bool:
    """Worker 关闭时主动释放当前租约: 返回是否释放成功 (租约还在才允许释.)."""
    cur = conn.execute(_RELEASE_SQL, {
        "job_id": job.id, "worker_id": job.lease_owner,
        "lease_token": job.lease_token,
    })
    return cur.fetchone() is not None


def _row_to_job(row) -> Job:
    r = row  # row_factory=dict_row 返回 dict, 直接取用.
    return Job(
        id=r["id"], submission_id=r["submission_id"], paper_id=r["paper_id"],
        run_id=r["run_id"], generation=r["generation"], attempts=r["attempts"],
        max_attempts=r["max_attempts"], lease_owner=r.get("lease_owner"),
        lease_token=r.get("lease_token"), lease_until=r.get("lease_until"),
        status=r["status"], available_at=r["available_at"],
        created_at=r["created_at"], updated_at=r["updated_at"],
    )


_LOAD_JOB_PAYLOAD_SQL = """
SELECT s.id AS submission_id, s.answers_json, s.starts_at, s.submitted_at,
       s.objective_score, s.grading_detail_json,
       r.snapshot_path, r.snapshot_hash, r.paper_id,
       j.generation, j.attempts, j.max_attempts
FROM grading_jobs j
JOIN submissions s ON s.id = j.submission_id
JOIN exam_runs r ON r.id = j.run_id
WHERE j.id = %(job_id)s
"""


def load_job_payload(conn, job_id: int) -> dict:
    """读 job 关联的 submission + run snapshot 字段, 给 grading 路径用."""
    cur = conn.cursor(row_factory=dict_row)
    cur.execute(_LOAD_JOB_PAYLOAD_SQL, {"job_id": job_id})
    row = cur.fetchone()
    if row is None:
        raise KeyError(f"job [{job_id}] payload not found")
    return dict(row)
