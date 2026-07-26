-- examSystem PostgreSQL 初始化 schema (Task 2, migration 0001)
--
-- 设计要点:
--   * exam_runs 是 paper 在某一轮开考的实体, 通过部分唯一索引保证同一 paper
--     同时只能有一个 open/closing 状态的 run. round_no 单调递增.
--   * exam_sessions 是学生在单次 run 里的会话, 用 (run_id, employee_id) 唯一
--     保证同员工同 run 只有一份会话 (与 Python 端 _get_or_resume 一致).
--   * submissions 是已提交待/正在/已评分的快照; review_logs 记录调分审计.
--   * grading_jobs 是评分 worker 队列; 用 FOR UPDATE SKIP LOCKED (Task 7) 消费.
--   * migration_audit 记录 SQLite -> PG 一次性导入的校验快照 (Task 3).
--   * schema_migrations 表由 Go migrator 运行时自己创建, 不包含在此文件内.

-- =====================================================================
-- 1. exam_runs
-- =====================================================================
CREATE TABLE exam_runs (
    id                 text PRIMARY KEY,
    paper_id           text NOT NULL,
    round_no           integer NOT NULL CHECK (round_no > 0),
    public_token_hash  text UNIQUE,
    status             text NOT NULL CHECK (status IN ('open','closing','closed')),
    duration_minutes   integer NOT NULL CHECK (duration_minutes > 0),
    snapshot_path      text,
    snapshot_hash      text,
    is_legacy          boolean NOT NULL DEFAULT false,
    opened_at          timestamptz NOT NULL,
    closing_started_at timestamptz,
    finalize_at        timestamptz,
    closed_at          timestamptz,
    created_at         timestamptz NOT NULL,
    UNIQUE (paper_id, round_no),
    CHECK (
        is_legacy OR (
            public_token_hash IS NOT NULL
            AND snapshot_path IS NOT NULL
            AND snapshot_hash IS NOT NULL
        )
    )
);

CREATE UNIQUE INDEX uq_exam_runs_active
    ON exam_runs (paper_id)
    WHERE status IN ('open','closing');

-- =====================================================================
-- 2. exam_sessions
-- =====================================================================
CREATE TABLE exam_sessions (
    id                  text PRIMARY KEY,
    run_id              text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    employee_id         text NOT NULL,
    name                text NOT NULL,
    department          text,
    session_token_hash  text NOT NULL,
    started_at          timestamptz NOT NULL,
    deadline_at         timestamptz NOT NULL,
    draft_json          jsonb NOT NULL DEFAULT '{}'::jsonb,
    draft_revision     integer NOT NULL DEFAULT 0 CHECK (draft_revision >= 0),
    draft_saved_at      timestamptz,
    status              text NOT NULL CHECK (status IN ('active','submitted')),
    client_ip           text,
    user_agent          text,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    UNIQUE (run_id, employee_id)
);

CREATE INDEX idx_exam_sessions_token       ON exam_sessions (session_token_hash);
CREATE INDEX idx_exam_sessions_run_status  ON exam_sessions (run_id, status);

-- =====================================================================
-- 3. submissions
-- =====================================================================
CREATE TABLE submissions (
    id                            bigserial PRIMARY KEY,
    name                          text NOT NULL,
    employee_id                   text NOT NULL,
    paper_id                      text NOT NULL,
    paper_name                    text,
    run_id                        text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    department                    text,
    answers_json                  jsonb NOT NULL,
    grading_detail_json           jsonb NOT NULL DEFAULT '[]'::jsonb,
    objective_score               double precision NOT NULL DEFAULT 0,
    subjective_score_machine     double precision NOT NULL DEFAULT 0,
    subjective_score_final       double precision NOT NULL DEFAULT 0,
    total_score                   double precision NOT NULL DEFAULT 0,
    review_status                 text NOT NULL DEFAULT 'grading',
    grading_status                text NOT NULL CHECK (grading_status IN ('pending','grading','done','failed')),
    grading_error                 text,
    grading_generation            bigint NOT NULL DEFAULT 0 CHECK (grading_generation >= 0),
    graded_at                     timestamptz,
    started_at                    timestamptz,
    submitted_at                   timestamptz NOT NULL,
    reviewed_at                   timestamptz,
    reviewer_note                 text,
    client_ip                     text,
    user_agent                    text,
    auto_submit_reason            text,
    UNIQUE (employee_id, run_id)
);

CREATE INDEX idx_submissions_paper_submitted  ON submissions (paper_id, submitted_at DESC);
CREATE INDEX idx_submissions_run              ON submissions (run_id);
CREATE INDEX idx_submissions_review_status    ON submissions (review_status);
CREATE INDEX idx_submissions_grading_status   ON submissions (grading_status);

-- =====================================================================
-- 4. review_logs
-- =====================================================================
CREATE TABLE review_logs (
    id            bigserial PRIMARY KEY,
    submission_id bigint NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    question_id   text NOT NULL,
    old_score     double precision,
    new_score     double precision,
    note          text,
    created_at    timestamptz NOT NULL
);

CREATE INDEX idx_review_logs_submission_created ON review_logs (submission_id, created_at);

-- =====================================================================
-- 5. grading_jobs
-- =====================================================================
CREATE TABLE grading_jobs (
    id            bigserial PRIMARY KEY,
    submission_id bigint NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    paper_id      text NOT NULL,
    run_id        text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    generation    bigint NOT NULL CHECK (generation > 0),
    status        text NOT NULL CHECK (status IN ('queued','leased','done','dead','superseded')),
    attempts      integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts  integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    lease_owner   text,
    lease_token   text,
    lease_until   timestamptz,
    available_at  timestamptz NOT NULL DEFAULT now(),
    last_error    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (submission_id, generation)
);

-- 队列扫描: 状态 queued/leased 且到点可领
CREATE INDEX idx_grading_jobs_due
    ON grading_jobs (available_at, id)
    WHERE status IN ('queued','leased');

-- 租约超时回收扫描
CREATE INDEX idx_grading_jobs_lease
    ON grading_jobs (lease_until, id)
    WHERE status = 'leased';

-- =====================================================================
-- 6. migration_audit (Task 3 用)
-- =====================================================================
CREATE TABLE migration_audit (
    source_sha256           text PRIMARY KEY,
    source_path             text NOT NULL,
    table_counts            jsonb NOT NULL,
    skipped_legacy_sessions integer NOT NULL DEFAULT 0,
    started_at              timestamptz NOT NULL,
    completed_at            timestamptz NOT NULL
);
