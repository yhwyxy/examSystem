CREATE TABLE score_anomaly_batches (
    id                  bigserial PRIMARY KEY,
    run_id              text NOT NULL REFERENCES exam_runs(id) ON DELETE CASCADE,
    similarity_batch_id bigint NOT NULL REFERENCES answer_similarity_batches(id) ON DELETE RESTRICT,
    snapshot_hash       text NOT NULL CHECK (length(snapshot_hash) = 64),
    answer_set_hash     text NOT NULL CHECK (length(answer_set_hash) = 64),
    grading_set_hash    text NOT NULL CHECK (length(grading_set_hash) = 64),
    rule_version        text NOT NULL,
    status              text NOT NULL CHECK (status IN ('queued','running','completed','failed')),
    attempt_count       integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts        integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 5),
    available_at        timestamptz NOT NULL DEFAULT now(),
    lease_owner         text,
    lease_until         timestamptz,
    error_code          text,
    submission_count    integer NOT NULL DEFAULT 0 CHECK (submission_count >= 0),
    finding_count       integer NOT NULL DEFAULT 0 CHECK (finding_count >= 0),
    blocking_count      integer NOT NULL DEFAULT 0 CHECK (blocking_count >= 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    started_at          timestamptz,
    completed_at        timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, similarity_batch_id, grading_set_hash, rule_version)
);

CREATE INDEX idx_score_anomaly_batches_due
    ON score_anomaly_batches (available_at, id)
    WHERE status IN ('queued','running');

CREATE TABLE score_anomaly_findings (
    id                 bigserial PRIMARY KEY,
    batch_id           bigint NOT NULL REFERENCES score_anomaly_batches(id) ON DELETE CASCADE,
    rule_code          text NOT NULL,
    severity           text NOT NULL CHECK (severity IN ('blocking','attention')),
    question_id        text NOT NULL DEFAULT '',
    submission_ids     jsonb NOT NULL CHECK (jsonb_typeof(submission_ids) = 'array'),
    participant_names  jsonb NOT NULL CHECK (jsonb_typeof(participant_names) = 'array'),
    message            text NOT NULL,
    score_gap          double precision NOT NULL DEFAULT 0 CHECK (score_gap BETWEEN 0 AND 1),
    evidence           jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(evidence) = 'object'),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (batch_id, rule_code, question_id, submission_ids)
);

CREATE INDEX idx_score_anomaly_findings_batch
    ON score_anomaly_findings (batch_id, severity, rule_code, id);
