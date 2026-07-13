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
                    if node.module == "backend.scoring" or node.module.startswith(
                        "backend.scoring."
                    ):
                        offenders.append(f"{path.relative_to(ROOT)}:{node.lineno}")
                elif isinstance(node, ast.Import):
                    for alias in node.names:
                        if alias.name == "backend.scoring" or alias.name.startswith(
                            "backend.scoring."
                        ):
                            offenders.append(f"{path.relative_to(ROOT)}:{node.lineno}")
    assert offenders == []


def test_subjective_scoring_uses_pinned_github_source():
    config = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    source = config["tool"]["uv"]["sources"]["subjective-scoring"]
    assert source == {
        "git": "https://github.com/yhwyxy/subjective-scoring",
        "tag": "v0.1.4",
    }

    requirements = (ROOT / "requirements.txt").read_text(encoding="utf-8")
    assert (
        "subjective-scoring[text,sql,code,remote] @ "
        "git+https://github.com/yhwyxy/subjective-scoring.git@v0.1.4"
        in requirements
    )
