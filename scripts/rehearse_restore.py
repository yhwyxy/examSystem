"""Rehearse old-schema upgrade and real backup/restore on a local test instance.

Creates and drops only uniquely named scratch databases. Uses synthetic grades,
never imports existing examination data, and does not publish a release.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import socket
import subprocess
import tarfile
import time
from urllib.parse import parse_qsl, urlsplit, urlunsplit
import uuid

import httpx
import psycopg
from psycopg import sql
if __package__:
    from .pg_native import connection_env
else:
    from pg_native import connection_env

ROOT = Path(__file__).resolve().parents[1]
ARTIFACTS = {"exam_system.sql.gz", "data.tar.gz", "config.production.yaml"}


def target_dsn(admin_url: str, database: str) -> str:
    parts = urlsplit(admin_url)
    if (parts.scheme not in {"postgres", "postgresql"}
            or parts.hostname not in {"127.0.0.1", "localhost", "::1"}
            or not parts.port or parts.path.lstrip("/") not in {"exam_goal_test", "exam_rehearsal_admin"}):
        raise ValueError("an explicitly named local rehearsal admin database is required")
    if any(key not in {"sslmode", "connect_timeout", "application_name"} for key, _ in parse_qsl(parts.query)):
        raise ValueError("unsupported rehearsal connection query parameter")
    return urlunsplit(parts._replace(path="/" + database))


def child_env(dsn: str) -> dict[str, str]:
    env = {key: os.environ[key] for key in ("PATH", "HOME", "GOCACHE", "GOMODCACHE", "TMPDIR") if key in os.environ}
    env.update(EXAM_DATABASE_URL=dsn, EXAM_MIGRATOR_DATABASE_URL=dsn,
               EXAM_ADMIN_PASSWORD="synthetic-rehearsal-password", PGDATABASE=dsn, PGCONNECT_TIMEOUT="5")
    return env


def run_command(command, *, cwd: Path, env: dict[str, str], log: Path, input_data=None):
    result = subprocess.run(command, cwd=cwd, env=env, input=input_data, capture_output=True, timeout=120)
    output = result.stdout + result.stderr
    # Native commands receive the DSN through env, never through argv or config.
    for key in ("EXAM_DATABASE_URL", "EXAM_MIGRATOR_DATABASE_URL", "PGDATABASE"):
        if env.get(key):
            output = output.replace(env[key].encode(), b"[rehearsal database]")
    log.write_bytes(output)
    if result.returncode:
        raise RuntimeError("rehearsal command failed; inspect its protected log")


def verify_backup(backup: Path) -> dict[str, str]:
    if (backup / "BACKUP_COMPLETE").read_text().strip() != "examSystem-backup-v1":
        raise ValueError("backup is not marked complete")
    hashes = {}
    for line in (backup / "SHA256SUMS").read_text().splitlines():
        digest, name = line.split(maxsplit=1)
        if name not in ARTIFACTS or name in hashes or len(digest) != 64:
            raise ValueError("invalid backup manifest")
        if hashlib.sha256((backup / name).read_bytes()).hexdigest() != digest:
            raise ValueError("backup checksum mismatch")
        hashes[name] = digest
    if set(hashes) != ARTIFACTS:
        raise ValueError("backup is incomplete")
    with tarfile.open(backup / "data.tar.gz") as archive:
        for entry in archive.getmembers():
            parts = PurePosixPath(entry.name).parts
            if (not parts or parts[0] != "data" or ".." in parts
                    or not (entry.isfile() or entry.isdir())):
                raise ValueError("unsafe data archive")
    return hashes


def file_hashes(workspace: Path) -> dict[str, str]:
    return {str(path.relative_to(workspace)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted((workspace / "data").rglob("*")) if path.is_file()}


def seed_old_schema(dsn: str, workspace: Path, nonce: str, baseline_root: Path) -> None:
    with psycopg.connect(dsn, autocommit=True) as conn:
        conn.execute("CREATE TABLE schema_migrations(version text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())")
        for name in ("0001_initial.sql", "0002_settings.sql"):
            source = (baseline_root / "migrations" / name).read_bytes()
            conn.execute(source.decode())
            conn.execute("INSERT INTO schema_migrations(version,checksum) VALUES(%s,%s)",
                         (Path(name).stem, hashlib.sha256(source).hexdigest()))
        snapshot = workspace / "data/exam_runs/historical.json"
        snapshot_hash = hashlib.sha256(snapshot.read_bytes()).hexdigest()
        for index, legacy in ((1, True), (2, False)):
            run_id = "historical-" + str(index)
            conn.execute("""INSERT INTO exam_runs(id,paper_id,round_no,public_token_hash,status,duration_minutes,
                            snapshot_path,snapshot_hash,is_legacy,opened_at,closed_at,created_at)
                            VALUES(%s,'historical',%s,%s,'closed',60,%s,%s,%s,now(),now(),now())""",
                         (run_id, index, None if legacy else "0" * 64,
                          None if legacy else "data/exam_runs/historical.json", None if legacy else snapshot_hash, legacy))
            submission_id = conn.execute("""INSERT INTO submissions(name,employee_id,paper_id,run_id,answers_json,
                grading_detail_json,objective_score,subjective_score_machine,subjective_score_final,total_score,
                review_status,grading_status,grading_generation,submitted_at,reviewed_at)
                VALUES('Synthetic historical grade',%s,'historical',%s,'{"q":"synthetic answer"}',
                '[{"question_id":"q","machine_score":20,"final_score":25,"max_score":30}]',
                30,20,25,55,'reviewed','done',1,now(),now()) RETURNING id""",
                (nonce + "-" + str(index), run_id)).fetchone()[0]
            conn.execute("INSERT INTO review_logs(submission_id,question_id,old_score,new_score,note,created_at) VALUES(%s,'q',20,25,'synthetic adjudication',now())",
                         (submission_id,))


def database_state(dsn: str) -> dict:
    with psycopg.connect(dsn) as conn:
        rows = conn.execute("SELECT id,employee_id,run_id,objective_score,subjective_score_machine,subjective_score_final,total_score,review_status,grading_generation FROM submissions ORDER BY id").fetchall()
        has_audit_context = conn.execute("SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='review_logs'::regclass AND attname='actor' AND NOT attisdropped)").fetchone()[0]
        audit_context = "actor,sub_question_id,grading_generation" if has_audit_context else "NULL::text,NULL::text,NULL::bigint"
        reviews = conn.execute("SELECT submission_id,question_id,old_score,new_score,note," + audit_context + " FROM review_logs ORDER BY id").fetchall()
        has_regrades = conn.execute("SELECT to_regclass('regrade_audit') IS NOT NULL").fetchone()[0]
        regrades = conn.execute("SELECT submission_id,actor,reason,from_generation,to_generation,before_state,after_state FROM regrade_audit ORDER BY id").fetchall() if has_regrades else []
        versions = conn.execute("SELECT version,checksum FROM schema_migrations ORDER BY version").fetchall()
        runs = conn.execute("SELECT id,status,is_legacy,snapshot_path,snapshot_hash FROM exam_runs ORDER BY id").fetchall()
        return {"grades": rows, "reviews": reviews, "regrades": regrades, "versions": versions, "runs": runs}


def verify_snapshot_files(dsn: str, workspace: Path) -> None:
    with psycopg.connect(dsn) as conn:
        for path, expected in conn.execute("SELECT snapshot_path,snapshot_hash FROM exam_runs WHERE snapshot_path IS NOT NULL"):
            snapshot = (workspace / path).resolve()
            if not snapshot.is_relative_to((workspace / "data").resolve()) or hashlib.sha256(snapshot.read_bytes()).hexdigest() != expected:
                raise RuntimeError("database snapshot reference does not match its file")


@contextmanager
def api_server(binary: Path, workspace: Path, dsn: str, *, previous=False, static_root: Path | None = None):
    with socket.socket() as socket_probe:
        socket_probe.bind(("127.0.0.1", 0))
        port = socket_probe.getsockname()[1]
    with (workspace / ("api-baseline.log" if previous else "api-candidate.log")).open("wb") as log:
        process = subprocess.Popen([str(binary), "serve", "--config", "config.production.yaml",
                                    "--bind", f"127.0.0.1:{port}", "--static", str(static_root or ROOT / "frontend")],
                                   cwd=workspace, env=child_env(dsn), stdout=log, stderr=subprocess.STDOUT)
        try:
            with httpx.Client(base_url=f"http://127.0.0.1:{port}", trust_env=False, timeout=3) as client:
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        raise RuntimeError("rehearsal API exited")
                    try:
                        if client.get("/api/health").status_code == 200:
                            break
                    except httpx.HTTPError:
                        pass
                    time.sleep(0.1)
                else:
                    raise RuntimeError("rehearsal API did not become healthy")
                yield client
        finally:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


def check_api(binary: Path, workspace: Path, dsn: str, nonce: str, *, previous=False, static_root: Path | None = None) -> None:
    with api_server(binary, workspace, dsn, previous=previous, static_root=static_root) as client:
        responses = []
        for index in (1, 2):
            response = client.get(f"/api/admin/submissions/{index}")
            response.raise_for_status()
            grade = response.json()["submission"]
            if grade["employee_id"] != nonce + "-" + str(index) or grade["total_score"] != 55:
                raise RuntimeError("historical grade changed or wrong server answered")
            if not previous and (grade.get("historical_grade") is not True or grade.get("run_is_legacy") is not (index == 1)):
                raise RuntimeError("historical grade classification was lost")
            responses.append(response.json())
            if not previous:
                if response.json()["regrade_events"] or any(log["actor"] or log["sub_question_id"] or log["grading_generation"] is not None for log in response.json()["review_logs"]):
                    raise RuntimeError("migration fabricated historical audit context")
                publication = client.post(f"/api/admin/publications/runs/historical-{index}/batches", json={})
                if publication.status_code != 409:
                    raise RuntimeError("historical grades were not explicitly refused for publication")
        if not previous:
            for feature in ("answer-similarity", "score-anomalies"):
                response = client.post(f"/api/admin/{feature}/runs/historical-1/batches", json={})
                if response.status_code != 409:
                    raise RuntimeError("legacy detection was not explicitly refused")
        name = "historical-api-baseline.json" if previous else "historical-api.json"
        (workspace / name).write_text(json.dumps(responses, ensure_ascii=False, indent=2))
    if not previous:
        with psycopg.connect(dsn) as conn:
            if conn.execute("SELECT count(*) FROM score_publication_batches").fetchone()[0] != 0:
                raise RuntimeError("migration automatically approved historical grades")


def baseline_version(output: Path, env: dict[str, str], baseline_ref: str) -> tuple[Path, Path, str]:
    revision = subprocess.run(["git", "rev-parse", "--verify", "--end-of-options", baseline_ref + "^{commit}"], cwd=ROOT, env=env,
                              check=True, capture_output=True, text=True).stdout.strip()
    archive = subprocess.run(["git", "archive", revision], cwd=ROOT, env=env,
                             check=True, capture_output=True)
    archive_path = output / "baseline-source.tar"
    archive_path.write_bytes(archive.stdout)
    source = output / "baseline-source"
    source.mkdir()
    with tarfile.open(archive_path) as archived:
        archived.extractall(path=source, filter="data")
    if {path.name for path in (source / "migrations").glob("*.sql")} != {"0001_initial.sql", "0002_settings.sql"}:
        raise ValueError("this historical fixture requires a baseline with exactly the first two migrations")
    binary = output / "exam-server-baseline"
    run_command(["go", "build", "-o", str(binary), "./cmd/exam-server"], cwd=source,
                env=env, log=output / "baseline-build.log")
    return source, binary, revision


def rehearse(admin_url: str, output: Path, baseline_ref: str) -> dict:
    nonce = uuid.uuid4().hex
    names = ["exam_rehearsal_" + nonce + suffix for suffix in ("_source", "_restore")]
    source_dsn, restore_dsn = (target_dsn(admin_url, name) for name in names)
    source, restored = output / "source", output / "restored"
    source.mkdir()
    restored.mkdir()
    (source / "data/papers").mkdir(parents=True)
    (source / "data/exam_runs").mkdir()
    paper = {"paper_id": "historical", "name": "Synthetic historical paper",
             "exam_info": {"title": "Synthetic", "description": "rehearsal only", "total_score": 60, "passing_score": 36},
             "questions": [{"id": "objective", "type": "true_false", "question": "synthetic objective", "score": 30, "answer": True},
                           {"id": "q", "type": "essay", "question": "synthetic task", "score": 30, "answer": "synthetic reference"}]}
    for path in (source / "data/papers/historical.json", source / "data/exam_runs/historical.json"):
        path.write_text(json.dumps(paper, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    config = "server:\n  host: 127.0.0.1\n  data_root: data\nadmin:\n  enable_auth: false\n"
    (source / "config.production.yaml").write_text(config)
    binary = output / "exam-server"
    build_env = child_env(source_dsn)
    build_env.setdefault("GOCACHE", str(output / "go-cache"))
    print("rehearsal: building candidate API", flush=True)
    run_command(["go", "build", "-o", str(binary), "./cmd/exam-server"], cwd=ROOT, env=build_env, log=output / "build.log")
    print("rehearsal: building committed baseline API", flush=True)
    baseline_root, baseline_binary, baseline_commit = baseline_version(output, build_env, baseline_ref)
    created = []
    with psycopg.connect(admin_url, autocommit=True) as admin:
        if not admin.execute("SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='exam_app')").fetchone()[0]:
            raise RuntimeError("local rehearsal instance requires the migration prerequisite role exam_app")
        try:
            for name in names:
                admin.execute(sql.SQL("CREATE DATABASE {}").format(sql.Identifier(name)))
                created.append(name)
            seed_old_schema(source_dsn, source, nonce, baseline_root)
            baseline = database_state(source_dsn)
            verify_snapshot_files(source_dsn, source)
            baseline_files = file_hashes(source)
            print("rehearsal: backing up old schema and synthetic files", flush=True)
            backup_env = child_env(source_dsn)
            backup_env.update(BACKUP_DATABASE_URL=source_dsn, PG_DUMP_BIN=shutil.which("pg_dump") or "pg_dump")
            run_command(["/bin/bash", str(ROOT / "scripts/backup-prod.sh"), str(output / "backups")],
                        cwd=source, env=backup_env, log=output / "backup.log")
            backup = next((output / "backups").glob("20*"))
            hashes = verify_backup(backup)
            print("rehearsal: migrating source and reading historical grades", flush=True)
            migrate = [str(binary), "migrate", "--config", "config.production.yaml"]
            run_command(migrate, cwd=source, env=child_env(source_dsn), log=output / "source-migrate.log")
            upgraded = database_state(source_dsn)
            if any(upgraded[key] != baseline[key] for key in ("grades", "reviews", "regrades", "runs")):
                raise RuntimeError("incremental migration changed historical data")
            check_api(binary, source, source_dsn, nonce)
            print("rehearsal: restoring into a second empty database", flush=True)
            run_command([shutil.which("psql") or "psql", "-X", "--no-password", "-v", "ON_ERROR_STOP=1"],
                        cwd=restored, env=connection_env(restore_dsn, child_env(restore_dsn)), log=output / "restore.log",
                        input_data=gzip.decompress((backup / "exam_system.sql.gz").read_bytes()))
            with tarfile.open(backup / "data.tar.gz") as archive:
                archive.extractall(path=restored, filter="data")
            shutil.copyfile(backup / "config.production.yaml", restored / "config.production.yaml")
            if database_state(restore_dsn) != baseline or file_hashes(restored) != baseline_files:
                raise RuntimeError("database or filesystem restore differs from the backup point")
            verify_snapshot_files(restore_dsn, restored)
            print("rehearsal: checking baseline API against the restored old schema", flush=True)
            check_api(baseline_binary, restored, restore_dsn, nonce, previous=True, static_root=baseline_root / "frontend")
            if database_state(restore_dsn) != baseline:
                raise RuntimeError("baseline recovery API changed restored historical data")
            run_command(migrate, cwd=restored, env=child_env(restore_dsn), log=output / "restored-migrate.log")
            migrated_restore = database_state(restore_dsn)
            if migrated_restore != upgraded:
                raise RuntimeError("restored upgrade differs from the source upgrade")
            check_api(binary, restored, restore_dsn, nonce)
            run_command(migrate, cwd=restored, env=child_env(restore_dsn), log=output / "idempotent-migrate.log")
            if database_state(restore_dsn) != migrated_restore:
                raise RuntimeError("idempotent migration changed restored data")
            return {"passed": True, "scope": "synthetic native PostgreSQL rehearsal; no production or Docker validation",
                    "baseline_migrations": len(baseline["versions"]), "candidate_migrations": len(upgraded["versions"]),
                    "historical_submissions": len(baseline["grades"]), "review_audits": len(baseline["reviews"]),
                    "files_verified": baseline_files, "backup_hashes": hashes,
                    "candidate_api_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                    "baseline_api_sha256": hashlib.sha256(baseline_binary.read_bytes()).hexdigest(),
                    "baseline_git_commit": baseline_commit,
                    "rollback_scope": "repository baseline with matching backup; production rollback version is not approved by this rehearsal",
                    "checks": ["old_schema_upgrade_preserves_scores_and_audits", "historical_api_read",
                               "historical_publication_denied", "legacy_detection_refused", "real_pg_dump_psql_restore",
                               "historical_grades_labelled", "historical_audit_attribution_remains_unknown",
                               "baseline_version_reads_restored_old_schema",
                               "filesystem_restore", "snapshot_hashes_match_database", "restored_upgrade_matches_source",
                               "migration_idempotence", "scratch_databases_cleaned"]}
        finally:
            for name in reversed(created):
                admin.execute(sql.SQL("DROP DATABASE {} WITH (FORCE)").format(sql.Identifier(name)))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-root", type=Path, required=True)
    parser.add_argument("--baseline-ref", required=True, help="local commit/ref for the matching two-migration historical baseline")
    args = parser.parse_args()
    admin_url = os.environ.get("REHEARSAL_ADMIN_DATABASE_URL", "")
    try:
        target_dsn(admin_url, "validation_only")
    except ValueError:
        parser.error("REHEARSAL_ADMIN_DATABASE_URL must name a dedicated local test database and explicit port")
    os.umask(0o077)
    output = args.output_root.resolve() / ("restore-rehearsal-" + uuid.uuid4().hex)
    output.mkdir(parents=True)
    try:
        report = rehearse(admin_url, output, args.baseline_ref)
    except Exception as exc:
        report = {"passed": False, "error_type": type(exc).__name__, "logs_directory": str(output)}
    (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
    print(json.dumps({"passed": report["passed"], "report": str(output / "report.json")}, ensure_ascii=False))
    return 0 if report["passed"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
