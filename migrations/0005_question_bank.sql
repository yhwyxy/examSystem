CREATE TABLE question_bank_questions (
    id               text PRIMARY KEY,
    current_version  integer NOT NULL CHECK (current_version > 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE question_bank_versions (
    question_id       text NOT NULL REFERENCES question_bank_questions(id) ON DELETE RESTRICT,
    version           integer NOT NULL CHECK (version > 0),
    discipline        text NOT NULL CHECK (length(trim(discipline)) > 0),
    difficulty        text NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    knowledge_points  text[] NOT NULL CHECK (cardinality(knowledge_points) > 0),
    question_json     jsonb NOT NULL CHECK (jsonb_typeof(question_json) = 'object'),
    status            text NOT NULL CHECK (status IN ('draft', 'reviewed', 'enabled', 'disabled', 'archived', 'superseded')),
    reviewed_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (question_id, version)
);

CREATE INDEX idx_question_bank_head
    ON question_bank_versions (discipline, difficulty, question_id, version DESC);

CREATE INDEX idx_question_bank_knowledge_points
    ON question_bank_versions USING gin (knowledge_points);

GRANT SELECT, INSERT, UPDATE, DELETE ON question_bank_questions, question_bank_versions TO exam_app;
