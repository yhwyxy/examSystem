-- A generation is recorded only after the fenced worker write succeeds.
-- Review edits update the active result and review_logs, never this snapshot.
CREATE TABLE grading_history (
    submission_id            bigint NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    generation               bigint NOT NULL CHECK (generation > 0),
    grading_detail_json      jsonb NOT NULL,
    subjective_score_machine double precision NOT NULL,
    subjective_score_final   double precision NOT NULL,
    total_score              double precision NOT NULL,
    review_status            text NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (submission_id, generation)
);
GRANT SELECT, INSERT ON grading_history TO exam_app;
