# Remote Reranker Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `RERANK_USE_REMOTE=true/false` so cloud reranking is explicitly enabled and local reranking remains the default.

**Architecture:** Keep environment parsing and scorer selection in `backend/grader.py`. A strict boolean parser decides whether remote configuration is relevant; only the enabled branch validates credentials and creates the shared remote scorer.

**Tech Stack:** Python 3.10+, pytest, python-dotenv, Docker Compose

## Global Constraints

- `RERANK_USE_REMOTE=true` requires `RERANK_API_URL`, `RERANK_API_KEY`, and `RERANK_MODEL`.
- `RERANK_USE_REMOTE=false`, unset, or empty selects local reranking.
- Boolean parsing is case-insensitive and rejects non-boolean values.
- Error messages must never expose `RERANK_API_KEY` values.
- Existing user changes in the dirty worktree must remain untouched.

---

### Task 1: Specify Switch Behavior With Tests

**Files:**
- Modify: `tests/test_remote_scoring_config.py`

**Interfaces:**
- Consumes: `backend.grader.get_subjective_service()` and `validate_remote_reranker_config()`.
- Produces: regression coverage for the new environment contract.

- [ ] **Step 1: Clear the switch in the autouse fixture**

Add `RERANK_USE_REMOTE` to the environment variables removed by
`reset_subjective_service`:

```python
for name in (
    "RERANK_USE_REMOTE",
    "RERANK_API_URL",
    "RERANK_API_KEY",
    "RERANK_MODEL",
):
    monkeypatch.delenv(name, raising=False)
```

- [ ] **Step 2: Write failing switch tests**

Update the cloud tests to set `RERANK_USE_REMOTE=true`, and add focused tests:

```python
def test_remote_credentials_do_not_enable_cloud_without_switch(monkeypatch):
    calls = []
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "test-model")
    monkeypatch.setattr(
        grader,
        "SubjectiveScoringService",
        lambda **kwargs: calls.append(kwargs) or object(),
    )

    grader.get_subjective_service()

    assert calls == [{"allow_model_load": True}]


def test_explicit_false_uses_local_model_with_remote_credentials(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "false")
    monkeypatch.setenv("RERANK_API_URL", "https://router.example.test/v1/rerank")
    monkeypatch.setenv("RERANK_API_KEY", "secret-key")
    monkeypatch.setenv("RERANK_MODEL", "test-model")
    assert grader.validate_remote_reranker_config() is None


def test_invalid_remote_switch_value_fails_startup(monkeypatch):
    monkeypatch.setenv("RERANK_USE_REMOTE", "yes")
    with pytest.raises(RuntimeError, match="RERANK_USE_REMOTE"):
        main._preflight_check()
```

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
uv run pytest tests/test_remote_scoring_config.py -q
```

Expected: failures show that credentials still auto-enable cloud reranking and
that invalid `RERANK_USE_REMOTE` values are not rejected.

---

### Task 2: Implement Strict Boolean Selection

**Files:**
- Modify: `backend/grader.py:52-93`
- Test: `tests/test_remote_scoring_config.py`

**Interfaces:**
- Consumes: process environment variables loaded by `backend.config`.
- Produces: `validate_remote_reranker_config() -> dict[str, str] | None` with explicit switch semantics.

- [ ] **Step 1: Parse the switch before reading cloud credentials**

Implement the beginning of `validate_remote_reranker_config` as:

```python
raw_enabled = os.environ.get("RERANK_USE_REMOTE", "").strip().lower()
if raw_enabled in {"", "false"}:
    return None
if raw_enabled != "true":
    raise RuntimeError(
        "RERANK_USE_REMOTE 只能设置为 true 或 false"
    )
```

Keep the existing three-variable completeness check after this block. When the
switch is true, return the complete mapping; otherwise raise the existing
sanitized missing-variable error.

- [ ] **Step 2: Run the focused tests and verify GREEN**

Run:

```bash
uv run pytest tests/test_remote_scoring_config.py -q
```

Expected: all tests in the file pass.

- [ ] **Step 3: Review the implementation for secret-safe errors**

Confirm no exception string interpolates the contents of `RERANK_API_KEY` and
that the local branch never instantiates `CohereRerankerPairScorer`.

---

### Task 3: Expose the Switch in Local and Docker Configuration

**Files:**
- Modify: `.env`
- Modify: `.env.example`
- Modify: `docker-compose.yml`
- Modify: `tests/test_env_loading.py`

**Interfaces:**
- Consumes: `RERANK_USE_REMOTE` from a root `.env` file or container environment.
- Produces: documented defaults for local and Docker execution.

- [ ] **Step 1: Write the failing environment-file assertion**

Add:

```python
assert "RERANK_USE_REMOTE=false" in env_text
```

to `test_project_env_file_is_ignored_and_contains_no_secret`.

- [ ] **Step 2: Run the environment test and verify RED**

Run:

```bash
uv run pytest tests/test_env_loading.py -q
```

Expected: failure because `.env` does not yet contain the switch.

- [ ] **Step 3: Add the switch to environment files**

Place this before the three remote connection settings in `.env` and
`.env.example`:

```dotenv
# Set to true to use the cloud reranker; false uses the local model.
RERANK_USE_REMOTE=false
```

Update `.env.example` guidance so it says all three connection settings are
required when the switch is true.

- [ ] **Step 4: Pass the switch through Docker Compose**

Add under `services.exam.environment`:

```yaml
- RERANK_USE_REMOTE=${RERANK_USE_REMOTE:-false}
```

- [ ] **Step 5: Run the environment tests and verify GREEN**

Run:

```bash
uv run pytest tests/test_env_loading.py -q
```

Expected: all tests pass.

---

### Task 4: Regression Verification

**Files:**
- Verify only; no planned production changes.

**Interfaces:**
- Consumes: completed Tasks 1-3.
- Produces: evidence that the explicit switch did not regress scoring or startup.

- [ ] **Step 1: Run remote configuration and environment tests together**

Run:

```bash
uv run pytest tests/test_remote_scoring_config.py tests/test_env_loading.py -q
```

Expected: all selected tests pass.

- [ ] **Step 2: Run the complete suite**

Run:

```bash
uv run pytest -q
```

Expected: the full suite passes with only previously known warnings.

- [ ] **Step 3: Inspect the scoped diff**

Run:

```bash
git diff -- backend/grader.py tests/test_remote_scoring_config.py \
  tests/test_env_loading.py .env.example docker-compose.yml
```

Also inspect `.env` directly because it is intentionally ignored. Confirm no
real API key appears in tracked output.
