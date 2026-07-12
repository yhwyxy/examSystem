# Scoring System Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run an isolated, real-cloud, end-to-end benchmark of examSystem and retain accuracy reports without touching production papers or submissions.

**Architecture:** A reusable script redirects paper and database globals to a temporary directory, creates two papers through `paper_store`, submits seven synthetic candidates through FastAPI's real HTTP routes, waits for background grading, calculates accuracy metrics, and writes sanitized Markdown and JSON reports.

**Tech Stack:** Python 3.13, FastAPI TestClient, SQLite, subjective-scoring v0.1.1, Tumuer `/v1/rerank`, Markdown/JSON

## Global Constraints

- Use the existing `.env` and require `RERANK_USE_REMOTE=true`.
- Route HTTP through `http://127.0.0.1:6152`.
- Never print or persist the API key.
- Never modify `data/papers` or `data/exam.db`.
- Send only generated synthetic content to the cloud provider.
- Retain reports under `reports/`; remove temporary papers and database files.

---

### Task 1: Build the Isolated Validation Harness

**Files:**
- Create: `scripts/validate_scoring_system.py`
- Create at runtime: `reports/scoring-validation-<timestamp>.json`
- Create at runtime: `reports/scoring-validation-<timestamp>.md`

**Interfaces:**
- Consumes: root `.env`, `backend.paper_store`, `backend.question_loader`, `backend.database`, `backend.main.app`, and `backend.review_service.get_submission_detail`.
- Produces: `main() -> int`, returning zero when the benchmark completes and nonzero for configuration, workflow, or cleanup failure.

- [ ] **Step 1: Define synthetic paper and candidate fixtures**

Create two papers:

```python
PAPERS = {
    "validation-fundamentals": {
        "name": "评分验证-基础知识",
        "questions": [
            # single choice, REST short answer, HTTP status short answer
        ],
    },
    "validation-backend": {
        "name": "评分验证-后端工程",
        "questions": [
            # transaction short answer, Python code, SQL
        ],
    },
}
```

Define seven candidates with unique employee IDs and predeclared expected score
bands for every question. Candidate quality levels cover exact, paraphrased,
partial, materially wrong, and unrelated answers.

- [ ] **Step 2: Redirect storage to a temporary directory**

Within `tempfile.TemporaryDirectory`, create `papers/index.json`, backup paths,
and an isolated `exam.db`. Assign the temporary paths to the same module globals
used by `tests/test_papers.py`, clear question caches, reset database
initialization, and call `database.init_db()`.

- [ ] **Step 3: Exercise real paper and HTTP workflows**

For each paper, call `paper_store.create_paper`, `paper_store.save_paper`, and
`paper_store.set_status(..., "open")`. For each candidate:

```python
client.post("/api/exam/start", json={
    "employee_id": candidate.employee_id,
    "paper_id": candidate.paper_id,
})
client.post("/api/submit", json={
    "name": candidate.name,
    "employee_id": candidate.employee_id,
    "paper_id": candidate.paper_id,
    "answers": candidate.answers,
})
```

Poll `/api/submission/{id}/status` and then load the final record through
`review_service.get_submission_detail`. Treat `grading_method` ending in
`:error` as a scoring failure even if the submission reaches a terminal state.

- [ ] **Step 4: Calculate and sanitize metrics**

Calculate workflow success rate, expected-band hit rate, mean absolute error,
ordering checks, scoring failures, and latency. Recursively redact values for
keys containing `key`, `token`, `authorization`, or `secret` before report
serialization.

- [ ] **Step 5: Verify syntax without making a cloud request**

Run:

```bash
env PYTHONPATH=. uv run python -m py_compile scripts/validate_scoring_system.py
```

Expected: exit code 0.

---

### Task 2: Execute and Review the Benchmark

**Files:**
- Execute: `scripts/validate_scoring_system.py`
- Review: newest `reports/scoring-validation-*.md`
- Review: newest `reports/scoring-validation-*.json`

**Interfaces:**
- Consumes: completed Task 1 and the configured cloud reranker.
- Produces: measured evidence about workflow usability and scoring accuracy.

- [ ] **Step 1: Run through the local proxy outside restricted networking**

Run:

```bash
env PYTHONPATH=. \
  HTTP_PROXY=http://127.0.0.1:6152 \
  HTTPS_PROXY=http://127.0.0.1:6152 \
  uv run python scripts/validate_scoring_system.py
```

Expected: seven submissions complete and report paths are printed without any
API key value.

- [ ] **Step 2: Validate report safety and isolation**

Confirm the report contains no value from `RERANK_API_KEY`, the production
paper index checksum is unchanged, the production database checksum is
unchanged or remains absent, and the temporary directory no longer exists.

- [ ] **Step 3: Interpret results**

Summarize workflow failures separately from accuracy misses. Report per-paper
ordering, band-hit rate, MAE, suspicious high scores for wrong answers, and
whether the system is suitable for automatic scoring or requires threshold and
rubric tuning.

- [ ] **Step 4: Run the existing regression suite**

Run:

```bash
env PYTHONPATH=. uv run pytest -q
```

Expected: the existing suite passes with only known warnings.
