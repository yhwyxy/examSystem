CREATE TABLE score_publication_batches (
    id                  bigserial PRIMARY KEY,
    run_id              text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    paper_id            text NOT NULL,
    paper_name          text NOT NULL,
    version             integer NOT NULL CHECK (version > 0),
    supersedes_batch_id bigint REFERENCES score_publication_batches(id) ON DELETE RESTRICT,
    correction_reason   text,
    actor               text NOT NULL,
    snapshot_hash       text NOT NULL CHECK (length(snapshot_hash) = 64),
    answer_set_hash     text NOT NULL CHECK (length(answer_set_hash) = 64),
    grading_set_hash    text NOT NULL CHECK (length(grading_set_hash) = 64),
    similarity_batch_id bigint NOT NULL REFERENCES answer_similarity_batches(id) ON DELETE RESTRICT,
    anomaly_batch_id    bigint NOT NULL REFERENCES score_anomaly_batches(id) ON DELETE RESTRICT,
    submission_count    integer NOT NULL CHECK (submission_count > 0),
    published_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, version),
    UNIQUE (run_id, anomaly_batch_id),
    CHECK ((version = 1 AND supersedes_batch_id IS NULL AND correction_reason IS NULL)
        OR (version > 1 AND supersedes_batch_id IS NOT NULL AND length(trim(correction_reason)) >= 5))
);

CREATE INDEX idx_score_publication_batches_latest
    ON score_publication_batches (run_id, version DESC);

CREATE TABLE score_publication_grades (
    batch_id             bigint NOT NULL REFERENCES score_publication_batches(id) ON DELETE RESTRICT,
    submission_id        bigint NOT NULL REFERENCES submissions(id) ON DELETE RESTRICT,
    participant_name     text NOT NULL,
    employee_id          text NOT NULL,
    department            text,
    grading_generation   bigint NOT NULL CHECK (grading_generation > 0),
    review_status         text NOT NULL,
    objective_score      double precision NOT NULL,
    subjective_machine   double precision NOT NULL,
    subjective_final     double precision NOT NULL,
    total_score          double precision NOT NULL,
    max_score            double precision NOT NULL CHECK (max_score > 0),
    PRIMARY KEY (batch_id, submission_id)
);

CREATE INDEX idx_score_publication_grades_submission
    ON score_publication_grades (submission_id, batch_id);

CREATE TABLE score_publication_audit (
    id                  bigserial PRIMARY KEY,
    batch_id            bigint NOT NULL REFERENCES score_publication_batches(id) ON DELETE RESTRICT,
    actor               text NOT NULL,
    event_type          text NOT NULL CHECK (event_type IN ('published','corrected')),
    supersedes_batch_id bigint REFERENCES score_publication_batches(id) ON DELETE RESTRICT,
    reason              text,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION reject_score_publication_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'published score snapshots are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER score_publication_batches_immutable
    BEFORE UPDATE OR DELETE ON score_publication_batches
    FOR EACH ROW EXECUTE FUNCTION reject_score_publication_mutation();

CREATE TRIGGER score_publication_grades_immutable
    BEFORE UPDATE OR DELETE ON score_publication_grades
    FOR EACH ROW EXECUTE FUNCTION reject_score_publication_mutation();

CREATE TRIGGER score_publication_audit_immutable
    BEFORE UPDATE OR DELETE ON score_publication_audit
    FOR EACH ROW EXECUTE FUNCTION reject_score_publication_mutation();
