from __future__ import annotations

import ast
from pathlib import Path
import tomllib


ROOT = Path(__file__).resolve().parents[1]


def test_python_sources_do_not_import_legacy_backend():
    """旧 Python backend 包已删除; tests/ 与 scoring_worker/ 不得再 import backend.

    例外: tests/contract/ 双跑装置仍引用 backend, 待改造为纯 Go 黑盒后移除
    (backend 缺失时整个 contract 套件会被 conftest skip).
    """
    offenders: list[str] = []
    for base in (ROOT / "tests", ROOT / "scoring_worker"):
        for path in base.rglob("*.py"):
            if (ROOT / "tests" / "contract") in path.parents:
                continue
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
            for node in ast.walk(tree):
                if isinstance(node, ast.ImportFrom) and node.module:
                    if node.module == "backend" or node.module.startswith("backend."):
                        offenders.append(f"{path.relative_to(ROOT)}:{node.lineno}")
                elif isinstance(node, ast.Import):
                    for alias in node.names:
                        if alias.name == "backend" or alias.name.startswith("backend."):
                            offenders.append(f"{path.relative_to(ROOT)}:{node.lineno}")
    assert offenders == []


def test_subjective_scoring_uses_pinned_github_source():
    config = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    source = config["tool"]["uv"]["sources"]["subjective-scoring"]
    assert source == {
        "git": "https://github.com/yhwyxy/subjective-scoring",
        "tag": "v0.1.7",
    }
