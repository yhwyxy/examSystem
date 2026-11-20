from __future__ import annotations

import hashlib
import io
from pathlib import Path
import tarfile

import pytest

from scripts.rehearse_restore import ARTIFACTS, target_dsn, verify_backup
from scripts.pg_native import connection_env


def make_backup(tmp_path: Path, entry_name="data/paper.json"):
    (tmp_path / "BACKUP_COMPLETE").write_text("examSystem-backup-v1\n")
    (tmp_path / "exam_system.sql.gz").write_bytes(b"synthetic dump")
    (tmp_path / "config.production.yaml").write_text("synthetic config")
    with tarfile.open(tmp_path / "data.tar.gz", "w:gz") as archive:
        entry = tarfile.TarInfo(entry_name)
        entry.size = 2
        archive.addfile(entry, io.BytesIO(b"{}"))
    (tmp_path / "SHA256SUMS").write_text("".join(
        hashlib.sha256((tmp_path / name).read_bytes()).hexdigest() + "  " + name + "\n"
        for name in sorted(ARTIFACTS)))


def test_backup_integrity_gate_rejects_corruption_before_restore(tmp_path):
    make_backup(tmp_path)
    assert set(verify_backup(tmp_path)) == ARTIFACTS
    (tmp_path / "exam_system.sql.gz").write_bytes(b"corrupted")
    with pytest.raises(ValueError, match="checksum"):
        verify_backup(tmp_path)


@pytest.mark.parametrize("entry", ["../outside", "data/../../outside", "/data/paper.json", "config.production.yaml"])
def test_restore_rejects_outside_paths_even_with_matching_manifest(tmp_path, entry):
    make_backup(tmp_path, entry)
    with pytest.raises(ValueError, match="unsafe"):
        verify_backup(tmp_path)


@pytest.mark.parametrize("dsn", [
    "postgres://user@remote.example:5432/exam_goal_test",
    "postgres://user@127.0.0.1:5432/exam_system",
    "postgres://user@127.0.0.1/exam_goal_test",
    "postgres://user@127.0.0.1:55439/exam_goal_test?password=secret",
])
def test_rehearsal_requires_dedicated_local_admin_connection(dsn):
    with pytest.raises(ValueError):
        target_dsn(dsn, "exam_rehearsal_scratch")


def test_native_clients_use_explicit_uri_fields_instead_of_ambient_database():
    env = connection_env("postgres://test:private%40secret@127.0.0.1:55439/exam_rehearsal_scratch?sslmode=disable",
                         {"PATH": "/tools", "PGHOST": "ambient-production", "PGDATABASE": "other",
                          "PGPASSWORD": "ambient-secret", "EXAM_ADMIN_PASSWORD": "unrelated"})
    assert env["PGHOST"] == "127.0.0.1"
    assert env["PGPORT"] == "55439"
    assert env["PGDATABASE"] == "exam_rehearsal_scratch"
    assert env["PGUSER"] == "test"
    assert env["PGPASSWORD"] == "private@secret"
    assert env["PGSSLMODE"] == "disable"
    assert "EXAM_ADMIN_PASSWORD" not in env
    assert not any("postgres://" in value for value in env.values())
