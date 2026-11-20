-- No backfill: historical bank versions have unknown attribution.
CREATE TABLE question_bank_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    question_id text NOT NULL REFERENCES question_bank_questions(id) ON DELETE RESTRICT,
    version integer NOT NULL CHECK (version > 0),
    action text NOT NULL CHECK (action IN ('create','update','import','review','enable','disable','archive')),
    actor text,
    reason text CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 2000),
    before_state jsonb,
    after_state jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_question_bank_audit_history ON question_bank_audit(question_id,id);
CREATE FUNCTION reject_question_bank_audit_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'question bank audit rows cannot be rewritten or deleted';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER question_bank_audit_immutable BEFORE UPDATE OR DELETE ON question_bank_audit
    FOR EACH ROW EXECUTE FUNCTION reject_question_bank_audit_change();
GRANT SELECT, INSERT ON question_bank_audit TO exam_app;
GRANT USAGE, SELECT ON SEQUENCE question_bank_audit_id_seq TO exam_app;
