CREATE TABLE answer_similarity_batches (
    id                 bigserial PRIMARY KEY,
    run_id             text NOT NULL REFERENCES exam_runs(id) ON DELETE CASCADE,
    snapshot_hash      text NOT NULL CHECK (length(snapshot_hash) = 64),
    answer_set_hash    text NOT NULL CHECK (length(answer_set_hash) = 64),
    rule_version       text NOT NULL,
    status             text NOT NULL CHECK (status IN ('queued','running','completed','failed')),
    attempt_count      integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts       integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 5),
    available_at       timestamptz NOT NULL DEFAULT now(),
    lease_owner        text,
    lease_until        timestamptz,
    error_code         text,
    submission_count   integer NOT NULL DEFAULT 0 CHECK (submission_count >= 0),
    question_count     integer NOT NULL DEFAULT 0 CHECK (question_count >= 0),
    finding_count      integer NOT NULL DEFAULT 0 CHECK (finding_count >= 0),
    created_at         timestamptz NOT NULL DEFAULT now(),
    started_at         timestamptz,
    completed_at       timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, snapshot_hash, answer_set_hash, rule_version)
);

CREATE INDEX idx_answer_similarity_batches_due
    ON answer_similarity_batches (available_at, id)
    WHERE status IN ('queued','running');

CREATE TABLE answer_similarity_findings (
    id                    bigserial PRIMARY KEY,
    batch_id              bigint NOT NULL REFERENCES answer_similarity_batches(id) ON DELETE CASCADE,
    question_id           text NOT NULL,
    question_label        text NOT NULL DEFAULT '',
    left_submission_id    bigint NOT NULL,
    right_submission_id   bigint NOT NULL,
    left_participant_id   text NOT NULL,
    right_participant_id  text NOT NULL,
    left_name             text NOT NULL,
    right_name            text NOT NULL,
    left_answer           text NOT NULL,
    right_answer          text NOT NULL,
    risk_level            text NOT NULL CHECK (risk_level IN ('medium','high')),
    risk_score            double precision NOT NULL CHECK (risk_score BETWEEN 0 AND 1),
    text_overlap          double precision NOT NULL CHECK (text_overlap BETWEEN 0 AND 1),
    semantic_similarity   double precision NOT NULL DEFAULT 0 CHECK (semantic_similarity BETWEEN 0 AND 1),
    reference_similarity double precision NOT NULL CHECK (reference_similarity BETWEEN 0 AND 1),
    shared_excerpt        text NOT NULL,
    left_excerpt          text NOT NULL,
    right_excerpt         text NOT NULL,
    reasons               jsonb NOT NULL DEFAULT '[]'::jsonb,
    common_error_evidence jsonb NOT NULL DEFAULT '[]'::jsonb,
    related_question_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
    features              jsonb NOT NULL DEFAULT '{}'::jsonb,
    review_state          text NOT NULL DEFAULT 'needs_review'
                          CHECK (review_state IN ('normal_consistent','needs_review','confirmed_abnormal')),
    review_revision       integer NOT NULL DEFAULT 0 CHECK (review_revision >= 0),
    review_note           text,
    reviewed_by           text,
    reviewed_at           timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (batch_id, question_id, left_submission_id, right_submission_id),
    CHECK (left_submission_id < right_submission_id)
);

CREATE INDEX idx_answer_similarity_findings_batch
    ON answer_similarity_findings (batch_id, risk_score DESC, id);

CREATE TABLE answer_similarity_review_audit (
    id             bigserial PRIMARY KEY,
    finding_id     bigint NOT NULL REFERENCES answer_similarity_findings(id) ON DELETE CASCADE,
    actor          text NOT NULL,
    from_state     text NOT NULL CHECK (from_state IN ('normal_consistent','needs_review','confirmed_abnormal')),
    to_state       text NOT NULL CHECK (to_state IN ('normal_consistent','needs_review','confirmed_abnormal')),
    revision       integer NOT NULL CHECK (revision > 0),
    note           text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_answer_similarity_review_audit_finding
    ON answer_similarity_review_audit (finding_id, id);
