ALTER TABLE score_anomaly_findings
    ADD COLUMN review_status text NOT NULL DEFAULT 'open'
        CHECK (review_status IN ('open','resolved')),
    ADD COLUMN review_disposition text
        CHECK (review_disposition IN ('false_positive','manually_resolved')),
    ADD COLUMN review_note text,
    ADD COLUMN reviewed_by text,
    ADD COLUMN reviewed_at timestamptz,
    ADD COLUMN review_revision bigint NOT NULL DEFAULT 1 CHECK (review_revision > 0),
    ADD CONSTRAINT score_anomaly_review_state_consistent CHECK (
        (review_status = 'open' AND review_disposition IS NULL AND review_note IS NULL AND reviewed_by IS NULL AND reviewed_at IS NULL)
        OR
        (review_status = 'resolved' AND review_disposition IS NOT NULL AND length(trim(review_note)) >= 5
            AND reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL)
    );

CREATE TABLE score_anomaly_review_audit (
    id                  bigserial PRIMARY KEY,
    finding_id          bigint NOT NULL REFERENCES score_anomaly_findings(id) ON DELETE CASCADE,
    actor               text NOT NULL,
    from_status         text NOT NULL CHECK (from_status IN ('open','resolved')),
    to_status           text NOT NULL CHECK (to_status IN ('open','resolved')),
    from_disposition    text CHECK (from_disposition IN ('false_positive','manually_resolved')),
    to_disposition      text CHECK (to_disposition IN ('false_positive','manually_resolved')),
    revision            bigint NOT NULL CHECK (revision > 1),
    note                text NOT NULL CHECK (length(trim(note)) >= 5),
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_score_anomaly_review_audit_finding
    ON score_anomaly_review_audit (finding_id,id);
