-- Keep the scoring thresholds used by each immutable exam run available to
-- reporting queries. Historical and legacy runs remain NULL when unknown.
ALTER TABLE exam_runs
    ADD COLUMN max_score double precision CHECK (max_score IS NULL OR max_score >= 0),
    ADD COLUMN passing_score double precision CHECK (passing_score IS NULL OR passing_score >= 0);
