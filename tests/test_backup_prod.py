from __future__ import annotations

import gzip
import hashlib
import os
from pathlib import Path
import subprocess
import tarfile

import pytest

SCRIPT = Path(__file__).parents[1] / "scripts/backup-prod.sh"


def prepare(tmp_path):
    (tmp_path / "data").mkdir()
    (tmp_path / "data/paper.json").write_text('{"synthetic":true}')
    (tmp_path / "config.production.yaml").write_text("server:\n  data_root: data\n")
    tools = tmp_path / "tools"
    tools.mkdir()
    dump = tools / "pg_dump"
    dump.write_text("#!/bin/sh\nprintf 'SELECT 1;\\n'\nif [ -f \"$(dirname \"$0\")/dump_fail\" ]; then exit 7; fi\n")
    dump.chmod(0o755)
    date = tools / "date"
    date.write_text("#!/bin/sh\nprintf '20261013-120000\\n'\n")
    date.chmod(0o755)
    return {**os.environ, "PATH": str(tools) + ":" + os.environ.get("PATH", ""),
            "BACKUP_DATABASE_URL": "postgres://private-test-secret@unused/test",
            "PG_DUMP_BIN": str(dump), "BACKUP_KEEP": "1"}


def run_backup(tmp_path, env):
    return subprocess.run(["/bin/bash", str(SCRIPT), "backups with spaces"], cwd=tmp_path,
                          env=env, capture_output=True, text=True, timeout=10)


def test_completed_backup_is_unique_private_and_verified_even_in_same_second(tmp_path):
    env = prepare(tmp_path)
    first = run_backup(tmp_path, env)
    assert first.returncode == 0, first.stderr
    first_dir = next((tmp_path / "backups with spaces").glob("20*"))
    second = run_backup(tmp_path, env)
    assert second.returncode == 0, second.stderr
    directories = list((tmp_path / "backups with spaces").glob("20*"))
    assert len(directories) == 1
    backup = directories[0]
    assert backup != first_dir and not first_dir.exists()
    assert str(backup.name) in second.stdout
    assert "private-test-secret" not in first.stdout + first.stderr + second.stdout + second.stderr
    assert gzip.decompress((backup / "exam_system.sql.gz").read_bytes()) == b"SELECT 1;\n"
    assert (backup / "BACKUP_COMPLETE").read_text().strip() == "examSystem-backup-v1"
    assert backup.stat().st_mode & 0o077 == 0
    with tarfile.open(backup / "data.tar.gz") as archive:
        assert all(entry.name == "data" or entry.name.startswith("data/") for entry in archive.getmembers())
    for line in (backup / "SHA256SUMS").read_text().splitlines():
        expected, filename = line.split(maxsplit=1)
        assert hashlib.sha256((backup / filename).read_bytes()).hexdigest() == expected


@pytest.mark.parametrize("failure", ["dump", "hash", "config"])
def test_incomplete_backup_never_looks_complete_or_prunes_existing_data(tmp_path, failure):
    env = prepare(tmp_path)
    assert run_backup(tmp_path, env).returncode == 0
    root = tmp_path / "backups with spaces"
    existing = next(root.glob("20*"))
    unmanaged = root / "unrelated directory"
    unmanaged.mkdir()
    (unmanaged / "keep").write_text("unrelated")
    if failure == "dump":
        (tmp_path / "tools/dump_fail").write_text("fail")
    elif failure == "hash":
        tool = tmp_path / "tools/sha256sum"
        tool.write_text("#!/bin/sh\nexit 9\n")
        tool.chmod(0o755)
    else:
        (tmp_path / "config.production.yaml").unlink()
    failed = run_backup(tmp_path, env)
    assert failed.returncode != 0
    assert existing.exists()
    assert unmanaged.exists()
    assert len(list(root.glob("20*"))) == 1
    assert not list(root.glob(".incomplete-*"))
    assert not (root / ".backup.lock").exists()


def test_backup_refuses_existing_lock_without_removing_it(tmp_path):
    env = prepare(tmp_path)
    lock = tmp_path / "backups with spaces/.backup.lock"
    lock.mkdir(parents=True)
    (lock / "pid").write_text("existing-process")
    result = run_backup(tmp_path, env)
    assert result.returncode != 0
    assert "locked" in result.stderr
    assert (lock / "pid").read_text() == "existing-process"
