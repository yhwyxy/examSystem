from __future__ import annotations

from pathlib import Path

from dotenv import dotenv_values

from backend.config import load_environment


ROOT = Path(__file__).resolve().parents[1]


def test_load_environment_reads_file_without_overriding_shell(monkeypatch, tmp_path):
    env_file = tmp_path / ".env"
    env_file.write_text(
        "ENV_LOADING_NEW=from-file\nENV_LOADING_EXISTING=from-file\n",
        encoding="utf-8",
    )
    monkeypatch.delenv("ENV_LOADING_NEW", raising=False)
    monkeypatch.setenv("ENV_LOADING_EXISTING", "from-shell")

    load_environment(env_file)

    assert __import__("os").environ["ENV_LOADING_NEW"] == "from-file"
    assert __import__("os").environ["ENV_LOADING_EXISTING"] == "from-shell"


def test_project_env_file_is_ignored_and_contains_no_secret():
    ignore_lines = {
        line.strip()
        for line in (ROOT / ".gitignore").read_text(encoding="utf-8").splitlines()
    }
    env_values = dotenv_values(ROOT / ".env")

    assert ".env" in ignore_lines
    assert {
        "RERANK_USE_REMOTE",
        "RERANK_API_URL",
        "RERANK_API_KEY",
        "RERANK_MODEL",
    }.issubset(env_values)
    assert str(env_values["RERANK_USE_REMOTE"]).strip().lower() in {"true", "false"}
    assert env_values["RERANK_API_KEY"] != "your-api-key"
