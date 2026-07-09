# Exam System Hardening Design

## Purpose

This design fixes the defects found in the project review without changing the product shape of the exam system. The work focuses on score correctness, exam timing enforcement, configuration consistency, and admin data protection.

The current system can save submissions and run grading, but several contracts are inconsistent:

- Objective and subjective graders return different score field names, causing total scores to be wrong.
- The frontend does not call the server-side exam start endpoint, so duration enforcement can be bypassed.
- The exam API and frontend disagree about where duration and auto-submit settings live.
- Admin authentication is disabled in the checked-in config, leaving sensitive endpoints open.
- The unauthenticated submission status endpoint returns scores by numeric submission ID.

## Goals

- Produce correct `objective_score`, `subjective_score_machine`, `subjective_score_final`, and `total_score` for full-paper grading.
- Ensure low-confidence subjective grading reliably enters manual review status.
- Enforce exam duration from server-recorded start time in the normal frontend flow.
- Make exam timing configuration explicit and internally consistent.
- Protect admin endpoints by default.
- Prevent unauthenticated score disclosure from submission status polling.
- Add regression tests for the corrected behavior.

## Non-Goals

- Do not replace SQLite.
- Do not introduce Celery, Redis, RQ, or another external queue.
- Do not redesign the admin UI beyond authentication and API contract fixes.
- Do not expose final scores to candidates.
- Do not build multi-admin user management.
- Do not migrate existing database rows unless a future implementation needs a small compatibility cleanup.

## Recommended Approach

Use staged hardening with narrow compatibility-preserving changes.

1. Normalize grading contracts and aggregation first, because incorrect scores are the highest-impact defect.
2. Fix exam session timing and frontend API use next, because it affects fairness.
3. Harden admin and polling access after the core exam flow is correct.
4. Wrap background grading scheduling in a small local abstraction, but defer a durable worker queue.

This approach avoids a broad rewrite and keeps each behavior testable with the existing FastAPI, JavaScript, and pytest stack.

## Architecture

### Backend

The backend remains a FastAPI application backed by SQLite. Existing modules keep their responsibilities:

- `backend/objective_grader.py`: objective rule scoring.
- `backend/llm_grader.py`: LLM call and output parsing.
- `backend/embedding_grader.py`: semantic or keyword fallback scoring.
- `backend/grader.py`: full-submission orchestration and score aggregation.
- `backend/main.py`: API endpoints, authentication dependency, exam timing endpoints.
- `backend/database.py`: persistence.
- `backend/config.py`: typed config loading.

The main design change is that `backend/grader.py` becomes the single place that interprets grading detail scores for paper-level totals. Grader detail dictionaries may keep legacy fields for UI compatibility, but aggregation must read a single normalized final score.

### Frontend

The existing static frontend remains in `frontend/js/exam.js` and `frontend/js/admin.js`.

The exam page will call the backend start endpoint before rendering the exam form. It will use server-returned exam config for duration and auto-submit behavior. The admin page continues to use the existing login panel and `authFetch` pattern, but the backend configuration should require that path by default.

## Data Contracts

### Grading Detail

Every question detail should expose these fields:

- `question_id: string`
- `type: string`
- `max_score: number`
- `score: number`
- `machine_score: number`
- `final_score: number`
- `grading_method: string`
- `confidence: number | null`
- `review_status: "auto_scored" | "need_review" | "low_confidence" | "reviewed"`

For objective questions:

- `score`, `machine_score`, and `final_score` all represent the rule score.
- `confidence` is `1.0`.
- `review_status` is `auto_scored`.

For LLM subjective questions:

- The parsed LLM `score` becomes `machine_score`, `score`, and `final_score`.
- Low confidence becomes `review_status: need_review` or `low_confidence`, based on configured thresholds.

For embedding fallback subjective questions:

- The fallback `final_score` becomes `machine_score`, `score`, and `final_score`.
- `need_review` and `low_confidence` must both count as requiring manual attention.

### Full Submission Result

Full-paper aggregation must use this rule:

- Objective total: sum `final_score` for objective questions.
- Subjective machine total: sum `machine_score` for subjective questions.
- Subjective final total: sum `final_score` for subjective questions.
- Total score: `objective_score + subjective_score_final`.

If a legacy detail does not contain `final_score`, aggregation may fall back to `score`, then `machine_score`, then `0`.

### Exam API

`GET /api/exam` should return both current top-level fields and a `config` object:

```json
{
  "exam_info": {},
  "questions": [],
  "server_time": "2026-07-09T00:00:00+00:00",
  "duration_minutes": 60,
  "auto_submit": true,
  "config": {
    "duration_minutes": 60,
    "auto_submit": true
  }
}
```

The top-level fields preserve compatibility. The frontend should prefer `config.duration_minutes` and `config.auto_submit`, then fall back to the top-level fields.

### Exam Start API

`POST /api/exam/start` should accept:

```json
{
  "employee_id": "E001"
}
```

It should return:

```json
{
  "started_at": "2026-07-09T00:00:00+00:00",
  "server_time": "2026-07-09T00:00:00+00:00"
}
```

The backend records start time by normalized `employee_id`. Client IP and user-agent may be retained for auditing but must not be the primary key for exam timing.

### Submit API

`POST /api/submit` continues to accept the existing submission payload. The server must not trust client-provided `started_at` for duration enforcement.

Required timing behavior:

- If no server-side start record exists for the submitted `employee_id`, reject the submission with `403 EXAM_NOT_STARTED`.
- If elapsed time exceeds `duration_minutes * 60 + grace_period_seconds`, reject with `403 EXAM_TIMEOUT`.
- On accepted submit, consume the start record so a second submit cannot reuse it.
- Duplicate submission continues to rely on the database unique constraint for `employee_id`.

### Submission Status API

`GET /api/submission/{submission_id}/status` should not return `total_score` to unauthenticated users.

Allowed response:

```json
{
  "submission_id": 1,
  "status": "grading"
}
```

Future work may add an opaque submission receipt token if the product needs candidate-side status privacy stronger than numeric ID polling.

## Configuration

### Exam Time Window

`ExamConfig.start_time` and `ExamConfig.end_time` should be parsed as timezone-aware datetimes or left as `None`.

Global time window enforcement should only run when `enable_global_time_window` is `true`.

Invalid or naive datetime configuration should fail during config validation with a clear error. The preferred format is ISO 8601 with timezone, for example:

```yaml
exam:
  enable_global_time_window: true
  start_time: "2026-07-09T09:00:00+08:00"
  end_time: "2026-07-09T18:00:00+08:00"
```

### Admin Authentication

The checked-in default should require admin authentication:

```yaml
admin:
  enable_auth: true
  password: null
```

When auth is enabled and no password is configured:

- `POST /api/admin/login` returns `500 NO_PASSWORD`.
- Admin endpoints return `401` without a valid bearer token.

For local demos, an operator may explicitly set `admin.enable_auth: false`. That should be treated as an intentional insecure mode.

## Error Handling

Use existing structured error details:

```json
{
  "detail": {
    "code": "ERROR_CODE",
    "message": "中文错误信息"
  }
}
```

Required codes:

- `EXAM_NOT_STARTED`: submit called without a server-side start record, or global window has not opened.
- `EXAM_ENDED`: global time window has closed.
- `EXAM_TIMEOUT`: elapsed duration exceeds configured limit plus grace period.
- `DUPLICATE_SUBMISSION`: database unique constraint rejects repeated employee ID.
- `UNAUTHORIZED`: admin endpoint called without login.
- `NO_PASSWORD`: auth is enabled but no admin password is configured.

## Background Grading

Short-term implementation should keep the in-process background grading model but isolate it behind a helper such as `schedule_grading(submission_id, answers)`.

The helper should:

- Start grading asynchronously from the submit endpoint.
- Log failures.
- Mark failed grading as `need_review`.
- Avoid changing the API response contract.

This does not solve all operational risks of daemon threads. It simply creates a boundary so a later worker queue can replace the implementation without changing submit logic.

## Testing Strategy

Add or update tests using `env PYTHONPATH=. uv run pytest`.

Required backend tests:

- Objective full-paper aggregation includes objective scores in `objective_score` and `total_score`.
- LLM parsed `machine_score` is converted into subjective final score.
- Embedding fallback `need_review` and `low_confidence` set the submission review status accordingly.
- `GET /api/exam` includes `config.duration_minutes` and `config.auto_submit`.
- Submit without `/api/exam/start` returns `403 EXAM_NOT_STARTED`.
- Submit after an expired server start returns `403 EXAM_TIMEOUT`.
- Submit within duration succeeds.
- Global time window is ignored when `enable_global_time_window` is false.
- Global time window rejects too-early or too-late access when enabled.
- Admin endpoints reject unauthenticated access when auth is enabled.
- Submission status response omits `total_score`.

Required frontend-oriented checks:

- `frontend/js/exam.js` reads duration from `state.exam.config.duration_minutes`.
- Starting the exam calls `/api/exam/start` with the entered `employee_id`.
- Auto-submit uses backend-provided `auto_submit`.

If browser automation is not introduced, cover frontend changes with static checks or targeted unit-style JavaScript tests only if the project already has a JS test harness. Do not add a new JS build system only for this repair.

## Rollout Plan

1. Add regression tests that expose current scoring, timing, and auth failures.
2. Fix grading contracts and aggregation.
3. Fix exam API config shape and frontend duration usage.
4. Add employee-ID-based server start enforcement.
5. Harden admin default config and status API response.
6. Run full test suite with `env PYTHONPATH=. uv run pytest`.
7. Manually smoke test:
   - Load exam page.
   - Enter name and employee ID.
   - Start exam.
   - Submit within duration.
   - Confirm admin list shows correct total.
   - Confirm unauthenticated admin API rejects access when auth is enabled.

## Compatibility Notes

- Existing database schema can remain unchanged.
- Existing `grading_detail_json` rows may contain older field shapes. New aggregation should tolerate missing `final_score`.
- Existing frontend pages should continue to load static assets from `/css` and `/js`.
- Existing admin login token storage can remain in memory for this single-instance application.

## Open Decisions

No open product decisions are required before implementation. The design intentionally chooses strict submit rejection when no server start exists, because accepting such submissions would preserve the current bypass.

## Acceptance Criteria

- Correct objective answers contribute to `objective_score`.
- Valid LLM subjective scores contribute to `subjective_score_machine`, `subjective_score_final`, and `total_score`.
- Low-confidence subjective grading appears in manual-review counts.
- A configured 10-minute exam displays and enforces 10 minutes, not 60 minutes.
- A submission without a prior server start is rejected.
- Admin APIs are not accessible without authentication when `admin.enable_auth` is true.
- Candidate status polling does not reveal scores.
- `env PYTHONPATH=. uv run pytest` passes.
