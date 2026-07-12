# Subjective Scoring GitHub Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the `backend.scoring` compatibility package and make `examSystem` consume `subjective-scoring` directly from GitHub tag `v0.1.0`, while preserving an editable local-development workflow.

**Architecture:** Application code and tests import the library's public `subjective_scoring` modules directly. A dependency-boundary test prevents the compatibility namespace and local path source from returning. `uv.lock` records the immutable GitHub tag, while developers may temporarily overlay a sibling checkout with `uv pip install -e`.

**Tech Stack:** Python 3.12+, pytest, uv, TOML, Git dependencies

## Global Constraints

- Pin `subjective-scoring` to GitHub tag `v0.1.0`.
- Do not retain a `backend.scoring` alias or fallback.
- Do not modify or revert unrelated working-tree changes.
- Keep local editable development as a documented, uncommitted environment override.

---

### Task 1: Add Dependency Boundary Coverage

**Files:**
- Create: `tests/test_dependency_boundaries.py`

**Interfaces:**
- Consumes: repository Python files and `pyproject.toml`
- Produces: regression checks for forbidden `backend.scoring` imports and the committed GitHub source

- [ ] **Step 1: Write the failing boundary tests**

```python
from __future__ import annotations

import ast
from pathlib import Path
import tomllib


ROOT = Path(__file__).resolve().parents[1]


def test_python_sources_do_not_import_backend_scoring():
    offenders: list[str] = []
    for base in (ROOT / "backend", ROOT / "tests"):
        for path in base.rglob("*.py"):
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
            for node in ast.walk(tree):
                if isinstance(node, ast.ImportFrom) and node.module:
                    if node.module == "backend.scoring" or node.module.startswith("backend.scoring."):
                        offenders.append(f"{path.relative_to(ROOT)}:{node.lineno}")
                elif isinstance(node, ast.Import):
                    for alias in node.names:
                        if alias.name == "backend.scoring" or alias.name.startswith("backend.scoring."):
                            offenders.append(f"{path.relative_to(ROOT)}:{node.lineno}")
    assert offenders == []


def test_subjective_scoring_uses_pinned_github_source():
    config = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    source = config["tool"]["uv"]["sources"]["subjective-scoring"]
    assert source == {
        "git": "https://github.com/yhwyxy/subjective-scoring",
        "tag": "v0.1.0",
    }
```

- [ ] **Step 2: Run the tests and verify RED**

Run: `env PYTHONPATH=. uv run pytest tests/test_dependency_boundaries.py -q`

Expected: both tests fail because existing sources import `backend.scoring` and `pyproject.toml` uses the editable sibling path.

---

### Task 2: Migrate Imports and Remove the Compatibility Package

**Files:**
- Modify: `backend/grader.py:20-35`
- Modify: `tests/test_core.py`
- Modify: `tests/test_scoring_aggregator.py`
- Modify: `tests/test_scoring_engines_code.py`
- Modify: `tests/test_scoring_engines_sql.py`
- Modify: `tests/test_scoring_engines_text.py`
- Modify: `tests/test_scoring_models.py`
- Modify: `tests/test_scoring_normalizer.py`
- Modify: `tests/test_scoring_router.py`
- Modify: `tests/test_scoring_service.py`
- Delete: `backend/scoring/__init__.py`
- Delete: `backend/scoring/components/__init__.py`
- Delete: `backend/scoring/engines/__init__.py`
- Delete: `backend/scoring/engines/code_hybrid.py`
- Delete: `backend/scoring/models/__init__.py`
- Delete: `backend/scoring/service.py`

**Interfaces:**
- Consumes: public exports from `subjective_scoring`
- Produces: direct application and test imports with no compatibility namespace

- [ ] **Step 1: Replace imports**

Use these one-to-one namespace replacements:

```text
backend.scoring                         -> subjective_scoring
backend.scoring.components              -> subjective_scoring.components
backend.scoring.engines                 -> subjective_scoring.engines
backend.scoring.engines.code_hybrid     -> subjective_scoring.engines.code_hybrid
```

In `backend/grader.py`, replace the entire `try`/`except ImportError` block with one direct `from subjective_scoring import (...)` statement.

- [ ] **Step 2: Delete the forwarding files**

Delete all six tracked files under `backend/scoring/`. Generated `__pycache__` content is not part of the migration diff.

- [ ] **Step 3: Run focused tests**

Run: `env PYTHONPATH=. uv run pytest tests/test_dependency_boundaries.py tests/test_scoring_*.py tests/test_core.py -q`

Expected: the import-boundary test passes; the Git source test still fails until Task 3; scoring tests continue to pass.

---

### Task 3: Pin GitHub Dependency and Document Local Development

**Files:**
- Modify: `pyproject.toml:33-34`
- Modify: `README.md:29-50`
- Modify: `uv.lock`

**Interfaces:**
- Consumes: GitHub repository `https://github.com/yhwyxy/subjective-scoring`, tag `v0.1.0`
- Produces: reproducible dependency resolution and documented editable overlay workflow

- [ ] **Step 1: Change the committed uv source**

```toml
[tool.uv.sources]
subjective-scoring = { git = "https://github.com/yhwyxy/subjective-scoring", tag = "v0.1.0" }
```

- [ ] **Step 2: Update README guidance**

State that normal installs use GitHub tag `v0.1.0`. Remove the supported `backend.scoring` import example. Add the local development command:

```bash
uv pip install -e "../subjective-scoring[text,sql,code]"
```

Explain that `uv sync` restores the pinned GitHub distribution and that a new library release requires updating the tag and lock file.

- [ ] **Step 3: Regenerate the lock file**

Run: `uv lock`

Expected: `uv.lock` records the Git repository and `v0.1.0` revision instead of `editable = "../subjective-scoring"`.

- [ ] **Step 4: Verify the boundary tests GREEN**

Run: `env PYTHONPATH=. uv run pytest tests/test_dependency_boundaries.py -q`

Expected: 2 passed.

---

### Task 4: Verify the Complete Migration

**Files:**
- Inspect: all modified and deleted files

**Interfaces:**
- Consumes: completed migration state
- Produces: evidence that imports, dependency resolution, and application behavior remain valid

- [ ] **Step 1: Check for stale references**

Run: `rg -n "backend\\.scoring|editable = \"\.\./subjective-scoring\"|path = \"\.\./subjective-scoring\"" backend tests pyproject.toml uv.lock README.md`

Expected: no matches.

- [ ] **Step 2: Run scoring-focused tests**

Run: `env PYTHONPATH=. uv run pytest tests/test_scoring_*.py tests/test_core.py -q`

Expected: all focused tests pass.

- [ ] **Step 3: Run the full test suite**

Run: `env PYTHONPATH=. uv run pytest -q`

Expected: all tests pass.

- [ ] **Step 4: Review the final diff**

Run: `git diff --check` and `git status --short`.

Expected: no whitespace errors; unrelated pre-existing changes remain present and untouched.
