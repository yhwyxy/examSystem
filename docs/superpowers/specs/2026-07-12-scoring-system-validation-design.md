# Scoring System Validation Design

## Goal

Validate examSystem's end-to-end answer submission, asynchronous grading,
cloud reranker integration, and practical scoring accuracy without modifying
the user's production papers or submission database.

## Isolation

The validation harness creates a temporary paper directory, paper index,
backup directory, and SQLite database. It redirects `question_loader`,
`paper_store`, and `database` to those temporary paths for the lifetime of the
process. Temporary papers and database files are removed when the process
exits. The existing `data/papers` and `data/exam.db` remain untouched.

The final Markdown and JSON reports are retained under `reports/`. Reports may
contain generated questions, synthetic answers, scores, timing, and sanitized
errors. They must not contain `RERANK_API_KEY` or its value.

## Test Papers

The harness generates two papers:

1. A fundamentals paper containing objective questions and Chinese short
   answers about HTTP and REST design.
2. A backend engineering paper containing short-answer, Python code, and SQL
   questions.

Each paper is opened through the same paper lifecycle used by the application.
Every synthetic candidate uses a unique employee ID, starts the exam through
`POST /api/exam/start`, submits through `POST /api/submit`, and waits for the
asynchronous grading result.

## Answer Cases

Subjective questions receive multiple answer qualities:

- complete and technically correct;
- correct paraphrase;
- partially correct;
- related wording with a materially wrong conclusion;
- unrelated content.

Objective answers include both correct and incorrect cases. SQL cases include
structurally correct, partially correct, and incorrect queries. Code cases
include correct behavior, incomplete behavior, and unrelated code.

## Accuracy Evaluation

Each synthetic answer has an expert-style expected score or acceptable score
band defined before execution. The report includes:

- end-to-end submission success rate;
- grading completion and failure counts;
- per-question expected and actual scores;
- absolute error and mean absolute error for point estimates;
- acceptable-band hit rate;
- ordering checks ensuring better answers score above worse answers;
- latency per submission and total runtime;
- review status and confidence information when available.

The report distinguishes transport or application failures from scoring
quality failures. A successful API call alone is not considered evidence of
accuracy.

## Cloud and Security

The harness loads the existing root `.env`, requires
`RERANK_USE_REMOTE=true`, and uses the configured complete `/v1/rerank`
endpoint and model. HTTP traffic uses the local proxy
`http://127.0.0.1:6152`. Only generated test content is sent to the provider.
The API key is neither printed nor written to disk.

## Completion Criteria

The run is complete when both papers can be loaded, all synthetic candidates
can start and submit, asynchronous grading reaches a terminal state, accuracy
metrics are calculated, reports are written, and temporary test data is
confirmed removed. Any unmet criterion is recorded explicitly in the report.
