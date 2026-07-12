# Subjective Scoring GitHub Migration Design

## Goal

Make `subjective-scoring` the only source of subjective-scoring implementation and APIs used by `examSystem`. Remove the `backend/scoring/` forwarding package and make normal installs reproducible from a fixed GitHub release.

## Dependency Strategy

The committed `pyproject.toml` source for `subjective-scoring` will use the public GitHub repository and pin tag `v0.1.0`. `uv.lock` will be regenerated so clean environments do not require a sibling checkout.

Local library development remains separate from the committed dependency configuration. A developer may temporarily replace the installed distribution with an editable sibling checkout:

```bash
uv pip install -e "../subjective-scoring[text,sql,code]"
```

Running `uv sync` restores the committed GitHub-pinned dependency. After library changes are released, `examSystem` advances the tag and regenerates `uv.lock`.

## Import Migration

Production code and tests will import public APIs directly from `subjective_scoring` and its existing public submodules:

- `subjective_scoring`
- `subjective_scoring.components`
- `subjective_scoring.engines`
- `subjective_scoring.engines.code_hybrid`

The `try`/`except ImportError` fallback in `backend/grader.py` will be removed. Because the compatibility package itself imports `subjective_scoring`, it is not a functional fallback when the dependency is absent.

After all imports are migrated, the tracked files under `backend/scoring/` will be deleted. No replacement package or alias will remain.

## Documentation

The README will describe `v0.1.0` as the committed GitHub dependency and document the editable-install command for local library work. References presenting `backend.scoring` as a supported compatibility import will be removed.

## Failure Behavior

`subjective-scoring` remains a required project dependency. If it cannot be installed or imported, application startup and test collection should fail clearly instead of silently selecting a compatibility path.

## Verification

The migration is complete when:

1. No Python source or test imports `backend.scoring`.
2. `backend/scoring/` has no tracked files.
3. `uv.lock` resolves `subjective-scoring` from GitHub tag `v0.1.0`, not `../subjective-scoring`.
4. The scoring-focused test suite passes.
5. The full test suite passes.

Existing unrelated working-tree changes must remain untouched.
