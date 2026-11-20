-- Historical review rows retain unknown attribution instead of inventing actors.
ALTER TABLE review_logs
    ADD COLUMN actor text,
    ADD COLUMN sub_question_id text,
    ADD COLUMN grading_generation bigint CHECK (grading_generation > 0);

CREATE TABLE regrade_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    submission_id bigint NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    actor text NOT NULL,
    reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 2000),
    from_generation bigint NOT NULL CHECK (from_generation > 0),
    to_generation bigint NOT NULL CHECK (to_generation = from_generation + 1),
    before_state jsonb NOT NULL,
    after_state jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (submission_id,to_generation)
);
CREATE INDEX idx_regrade_audit_submission ON regrade_audit(submission_id,id);

-- Submission deletion keeps its existing cascade semantics; audit content
-- cannot be rewritten while its submission exists.
CREATE FUNCTION reject_review_audit_update() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'review audit rows cannot be rewritten';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER review_log_no_update BEFORE UPDATE ON review_logs
    FOR EACH ROW EXECUTE FUNCTION reject_review_audit_update();
CREATE TRIGGER regrade_audit_no_update BEFORE UPDATE ON regrade_audit
    FOR EACH ROW EXECUTE FUNCTION reject_review_audit_update();
