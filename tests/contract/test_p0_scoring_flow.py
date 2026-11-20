"""A real Go API + PostgreSQL + worker journey using offline scoring engines."""
import copy
import json
import os
import secrets
import socket
import threading
import time
from pathlib import Path

import psycopg
import pytest

from subjective_scoring import SubjectiveScoringService
from subjective_scoring.engines.sql_execution import SQLiteDatasetRunner
from scoring_worker.config import Config
from scoring_worker.runtime import run_one_job
from scoring_worker.snapshot import SnapshotCache
from scoring_worker.grader_bridge import _sandbox_runner_options
from .conftest import DEFAULT_DSN, _schema_dsn


PAPER = json.loads((Path(__file__).resolve().parents[1] / "fixtures/contract/p0_scoring_paper.json").read_text())
ANSWERS = {"choice":"A","text":"地球绕太阳公转。","calc":"2+3=5，结果为5。",
           "sql":"select id from users order by id", "code":{"language":"javascript","answer":"function add(a,b) { return a+b; }"},
           "composite":{"explain":{"answer":"地球绕太阳公转。"},"query":{"answer":"select id from users"}}}


@pytest.fixture
def sandbox_options(monkeypatch):
    """Default: declared code/SQLite harnesses; configured: actual sandbox.

    An explicit SANDBOX configuration must validate, never silently use the
    offline harness. Deployment acceptance sets all three variables.
    """
    from subjective_scoring.sandbox_server import SandboxServer
    if any(os.environ.get(key) for key in ("SANDBOX_API_URL", "SANDBOX_AUTH_TOKEN", "SANDBOX_RUNNER_BUILD")):
        yield _sandbox_runner_options
        return
    token = secrets.token_urlsafe(48)
    class FixtureCodeRunner:
        # Protocol fixture only: these outcomes are not execution evidence.
        def run(self, source, language, cases):
            return {"status": "verified", "language": language, "passed": len(cases), "total": len(cases),
                    "cases": [{"passed": True, "reason": "output_match"} for _ in cases]}
    server = SandboxServer(("127.0.0.1", 0), token=token, runner_build="contract-sql-harness",
                           code_runner=FixtureCodeRunner(), sql_runner=SQLiteDatasetRunner())
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    monkeypatch.setenv("SANDBOX_API_URL", f"http://127.0.0.1:{server.server_address[1]}")
    monkeypatch.setenv("SANDBOX_AUTH_TOKEN", token)
    monkeypatch.setenv("SANDBOX_RUNNER_BUILD", server.runner_build)
    try:
        yield _sandbox_runner_options
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


@pytest.fixture
def scoring_exam(client,request):
    doc = copy.deepcopy(PAPER)
    if getattr(request,"param",None) == "missing_standard":
        doc["questions"][1].update(answer="",scoring_points=[])
    slug = "p0-journey-" + secrets.token_hex(4)
    doc["paper_id"] = slug
    response = client.put(f"/api/admin/papers/{slug}", json=doc)
    assert response.ok, response.text
    opened = client.post(f"/api/admin/papers/{slug}/open", json={})
    assert opened.ok, opened.text
    start = client.post("/api/exam/start", json={"paper_id":slug,"run_token":opened.json()["public_token"],
                                                "employee_id":"p0-employee","name":"P0 测试"})
    assert start.ok, start.text
    yield slug, start.json()
    client.post(f"/api/admin/papers/{slug}/close", json={})
    client.delete(f"/api/admin/papers/{slug}")


def grade_pending(service,submission_id):
    if os.environ.get("EXAM_CONTRACT_BASE_URL"):
        pytest.fail("P0 worker journey requires the isolated server fixture")
    dsn = _schema_dsn(os.environ.get("EXAM_CONTRACT_DATABASE_URL", DEFAULT_DSN))
    cfg = Config(dsn,"p0-worker",0.1,60,5,55,1,"INFO",True)
    with psycopg.connect(dsn) as conn:
        cache = SnapshotCache()
        for _ in range(100):
            assert run_one_job(conn,cfg,cache,service), "target submission has no runnable job"
            status = conn.execute("SELECT grading_status FROM submissions WHERE id=%s",(submission_id,)).fetchone()[0]
            conn.commit()
            if status == "done":
                return
            assert status != "failed"
        pytest.fail("target grading job was not completed")


def submit(client,session,answers):
    result = client.post("/api/submit",json={"session_id":session["session_id"],"session_token":session["session_token"],"answers":answers})
    assert result.ok, result.text
    return result.json()["submission_id"]


def completed_detection_report(client, run_id, kind):
    deadline = time.monotonic() + 20
    response = None
    while time.monotonic() < deadline:
        response = client.post(f"/api/admin/{kind}/runs/{run_id}/batches", json={})
        if response.ok:
            break
        assert response.status_code == 409, response.text
        time.sleep(0.1)
    assert response is not None and response.ok, response.text if response else "missing enqueue response"
    batch_id = response.json()["batch"]["id"]
    while time.monotonic() < deadline:
        page = client.get(f"/api/admin/{kind}/batches/{batch_id}").json()
        status = page["batch"]["status"]
        assert status != "failed", page
        if status == "completed":
            return page
        time.sleep(0.1)
    pytest.fail(f"{kind} report did not complete for {run_id}")


@pytest.mark.parametrize(("answer", "expected_score", "requires_review", "execution_fault"), [
    ("DELETE FROM items WHERE value IS NULL", 10, False, None),
    ("DELETE FROM items", 0, True, None),
    ("DELETE FROM items WHERE value IS NULL", 0, True, "build_mismatch"),
    ("DELETE FROM items WHERE value IS NULL", 0, True, "unavailable"),
])
def test_configured_sql_mutation_crosses_api_snapshot_worker_and_detail(client, answer, expected_score, requires_review,
                                                                       execution_fault, sandbox_options, monkeypatch):
    slug = "sql-mutation-" + secrets.token_hex(4)
    fixtures = [{"name": name, "visibility": visibility, "tables": [{
        "name": "items", "columns": [{"name": "value", "type": "INTEGER"}], "rows": rows,
    }]} for name, visibility, rows in (("sample", "public", [[1], [None], [1]]),
                                      ("secret", "hidden", [[2], [None], [3]]))]
    question = {"id": "mutation", "type": "short_answer", "question": "删除 items 的空值行", "score": 10,
                "scoring_mode": "sql", "sql_dialect": "sqlite", "answer": "DELETE FROM items WHERE value IS NULL",
                "sql_execution": {"mode": "mutation", "mutation_tables": ["items"]}, "sql_test_datasets": fixtures}
    doc = {"paper_id": slug, "name": "SQL mutation contract",
           "exam_info": {"title": "SQL mutation", "description": "状态验证契约", "total_score": 10, "passing_score": 6},
           "questions": [question]}
    saved = client.put(f"/api/admin/papers/{slug}", json=doc)
    assert saved.ok, saved.text
    try:
        loaded = client.get(f"/api/admin/papers/{slug}").json()["questions"][0]
        assert loaded["sql_execution"] == question["sql_execution"]
        preview = client.get(f"/api/admin/papers/{slug}/preview").json()["questions"][0]
        assert preview["sql_execution"] == question["sql_execution"]
        opened = client.post(f"/api/admin/papers/{slug}/open", json={})
        assert opened.ok, opened.text
        public_question = client.get("/api/exam", params={"paper": slug, "run": opened.json()["public_token"]}).json()["questions"][0]
        assert "sql_execution" not in public_question
        assert len(public_question["sql_test_datasets"]) == 1
        start = client.post("/api/exam/start", json={"paper_id": slug, "run_token": opened.json()["public_token"],
                                                    "employee_id": "mutation-test", "name": "去标识测试"})
        assert start.ok, start.text
        submission_id = submit(client, start.json(), {"mutation": answer})
        if execution_fault == "build_mismatch":
            monkeypatch.setenv("SANDBOX_RUNNER_BUILD", os.environ["SANDBOX_RUNNER_BUILD"] + "-wrong-build")
        elif execution_fault == "unavailable":
            # A deployment rehearsal may stop its own server first and retain
            # that exact URL; the ordinary suite uses an unused loopback port.
            if os.environ.get("EXAM_SANDBOX_STOPPED") != "1":
                with socket.socket() as probe:
                    probe.bind(("127.0.0.1", 0))
                    unused_port = probe.getsockname()[1]
                monkeypatch.setenv("SANDBOX_API_URL", f"http://127.0.0.1:{unused_port}")
        service = SubjectiveScoringService(allow_model_load=False, **sandbox_options())
        grade_pending(service, submission_id)
        body = client.get(f"/api/admin/submissions/{submission_id}").json()
        entry = body["submission"]["grading_detail"][0]
        assert entry["machine_score"] == expected_score
        detail = entry["detail"]
        report = detail["scoring_metadata"]["execution_validation"]
        assert report["status"] == ("unable_to_verify" if execution_fault else "verified")
        assert report["passed"] == (2 if expected_score else 0)
        assert detail["need_manual_review"] is requires_review
        assert "secret" not in json.dumps(report)
        points = detail["matched_points"] if expected_score else detail["missed_points"]
        assert len(points) == (0 if execution_fault else 2)
        assert all(point["evidence_kind"] == "execution_record" for point in points)
        assert len(body["grading_history"]) == 1
        if not requires_review or execution_fault:
            assert client.post(f"/api/admin/papers/{slug}/close", json={"grace_seconds": 1}).ok
            run_id = opened.json()["run_id"]
            completed_detection_report(client, run_id, "answer-similarity")
            anomaly = completed_detection_report(client, run_id, "score-anomalies")
            assert anomaly["batch"]["rule_version"] == "score-consistency-v2"
            assert anomaly["batch"]["blocking_count"] == 0, anomaly
            assert not any(finding["rule_code"] in {"credited_without_evidence", "invalid_execution_evidence"}
                           for finding in anomaly["findings"])
            if execution_fault:
                publication = client.post(f"/api/admin/publications/runs/{run_id}/batches", json={})
                assert publication.status_code == 409, publication.text
                assert publication.json()["detail"]["code"] == "REVIEW_REQUIRED"
    finally:
        client.post(f"/api/admin/papers/{slug}/close", json={})
        client.delete(f"/api/admin/papers/{slug}")


def test_p0_full_scoring_review_and_regrade_history(client,scoring_exam):
    slug,session = scoring_exam
    sid = submit(client,session,copy.deepcopy(ANSWERS))
    service = SubjectiveScoringService(allow_model_load=False,text_pair_scorer=lambda q,d:0.99,code_pair_scorer=lambda q,d:0.99)
    grade_pending(service,sid)
    response = client.get(f"/api/admin/submissions/{sid}")
    assert response.ok, response.text
    body = response.json()
    submission = body["submission"]
    assert submission["grading_status"] == "done"
    entries = {e["question_id"]:e for e in submission["grading_detail"]}
    for qid,mode in (("text","text"),("calc","calculation"),("sql","sql"),("code","code")):
        assert entries[qid]["detail"]["scoring_mode"] == mode
        assert entries[qid]["detail"]["grading_generation"] == 1
        assert len(entries[qid]["detail"]["scoring_config_version"]) == 64
    assert entries["code"]["detail"]["selected_language"] == "javascript"
    point = entries["text"]["detail"]["matched_points"][0]
    assert point["point_id"] == "orbit" and point["description"] == "地球绕太阳公转"
    assert point["evidence_located"] and point["evidence"] in ANSWERS["text"]
    assert len(body["grading_history"]) == 1
    original_history = body["grading_history"][0]
    review = client.post(f"/api/admin/submissions/{sid}/review",json={"question_id":"text","new_score":7,"note":"人工确认"})
    assert review.ok, review.text
    after_review = client.get(f"/api/admin/submissions/{sid}").json()
    assert after_review["grading_history"][0] == original_history
    assert after_review["submission"]["review_status"] != "reviewed"
    machine_before = entries["text"]["machine_score"]
    assert client.post(f"/api/admin/regrade/{sid}", json={"reason": "复核后重新验证机器评分历史"}).ok
    grade_pending(service,sid)
    after = client.get(f"/api/admin/submissions/{sid}").json()
    assert [h["generation"] for h in after["grading_history"]] == [2,1]
    assert after["grading_history"][1] == original_history
    text = next(e for e in after["submission"]["grading_detail"] if e["question_id"] == "text")
    assert text["final_score"] == 7 and text["machine_score"] == machine_before
    assert text["manually_reviewed"] is True
    assert text["detail"]["grading_generation"] == 2


def test_missing_standard_and_engine_failure_require_review(client,scoring_exam):
    _,session = scoring_exam
    answers = copy.deepcopy(ANSWERS)
    answers["text"] = "地球不绕太阳公转。"
    sid = submit(client,session,answers)
    def fail_pairs(q,d):
        raise RuntimeError("test scoring API failure")
    service = SubjectiveScoringService(allow_model_load=False,text_pair_scorer=fail_pairs,code_pair_scorer=lambda q,d:1)
    grade_pending(service,sid)
    body = client.get(f"/api/admin/submissions/{sid}").json()
    assert body["submission"]["grading_status"] == "done"
    assert body["submission"]["review_status"] == "need_review"
    text = next(e for e in body["submission"]["grading_detail"] if e["question_id"] == "text")
    assert text["machine_score"] == 0 and text["need_manual_review"] is True


@pytest.mark.parametrize("scoring_exam",["missing_standard"],indirect=True)
def test_missing_standard_never_becomes_a_publishable_result(client,scoring_exam):
    _,session = scoring_exam
    sid = submit(client,session,copy.deepcopy(ANSWERS))
    grade_pending(SubjectiveScoringService(allow_model_load=False,text_pair_scorer=lambda q,d:0.99,code_pair_scorer=lambda q,d:0.99),sid)
    body = client.get(f"/api/admin/submissions/{sid}").json()
    text = next(e for e in body["submission"]["grading_detail"] if e["question_id"] == "text")
    assert text["need_manual_review"] is True
    assert text["review_status"] == "open_ended"
    assert body["submission"]["review_status"] == "need_review"
    assert text["detail"]["matched_points"] == []


@pytest.mark.parametrize(("language", "source"), [
    ("python", "a,b=map(int,input().split()); print(a+b)"),
    ("javascript", "const a=require('fs').readFileSync(0,'utf8').trim().split(/\\s+/).map(Number); console.log(a[0]+a[1]);"),
    ("java", "import java.util.Scanner; public class Main { public static void main(String[] args) { Scanner s=new Scanner(System.in); System.out.println(s.nextInt()+s.nextInt()); } }"),
])
def test_code_execution_crosses_worker_history_reports_and_publication(client, sandbox_options, language, source):
    slug = "code-sandbox-" + secrets.token_hex(4)
    cases = [{"name": "sample", "visibility": "public", "input": "2 3\n", "expected_output": "5"},
             {"name": "private-case-name", "visibility": "hidden", "input": "-3 7\n", "expected_output": "4"}]
    question = {"id": "code", "type": "short_answer", "scoring_mode": "code", "question": "从 stdin 读两整数，输出它们的和。",
                "score": 10, "answer": source, "code_language": language, "allowed_languages": [language],
                "answers_by_language": {language: source}, "code_test_cases": cases}
    paper = {"paper_id": slug, "name": "Synthetic code sandbox", "questions": [question],
             "exam_info": {"title": "Code sandbox", "description": "合成执行验收", "total_score": 10, "passing_score": 6}}
    saved = client.put(f"/api/admin/papers/{slug}", json=paper)
    assert saved.ok, saved.text
    try:
        opened = client.post(f"/api/admin/papers/{slug}/open", json={})
        assert opened.ok, opened.text
        public = client.get("/api/exam", params={"paper": slug, "run": opened.json()["public_token"]}).json()["questions"][0]
        assert len(public["code_test_cases"]) == 1
        assert "answers_by_language" not in public
        start = client.post("/api/exam/start", json={"paper_id": slug, "run_token": opened.json()["public_token"],
                                                    "employee_id": "synthetic-code", "name": "Code test"})
        assert start.ok, start.text
        sid = submit(client, start.json(), {"code": {"language": language, "answer": source}})
        service = SubjectiveScoringService(allow_model_load=False, **sandbox_options())
        grade_pending(service, sid)
        detail = client.get(f"/api/admin/submissions/{sid}").json()
        entry = detail["submission"]["grading_detail"][0]
        evidence = entry["detail"]["scoring_metadata"]["execution_validation"]
        assert evidence["status"] == "verified" and evidence["passed"] == evidence["total"] == 2
        assert evidence["runner_build"] == os.environ["SANDBOX_RUNNER_BUILD"]
        assert evidence["language"] == language
        assert "private-case-name" not in json.dumps(evidence)
        assert entry["machine_score"] == entry["final_score"] == 10
        # These fixtures have no approved rubric/model judgment. Execution
        # success preserves that review requirement rather than clearing it.
        assert entry["need_manual_review"] is True
        assert all(point["evidence_kind"] == "execution_record" for point in entry["detail"]["matched_points"])
        assert len(detail["grading_history"]) == 1
        assert detail["grading_history"][0]["generation"] == 1
        reviewed = client.post(f"/api/admin/submissions/{sid}/review", json={
            "question_id": "code", "new_score": 10, "note": "合成流程验收：核对测试记录后人工确认整题分数。"})
        assert reviewed.ok, reviewed.text
        after_review = client.get(f"/api/admin/submissions/{sid}").json()
        assert after_review["grading_history"] == detail["grading_history"]
        assert after_review["submission"]["review_status"] == "reviewed"
        assert client.post(f"/api/admin/papers/{slug}/close", json={"grace_seconds": 1}).ok
        run_id = opened.json()["run_id"]
        completed_detection_report(client, run_id, "answer-similarity")
        anomalies = completed_detection_report(client, run_id, "score-anomalies")
        assert anomalies["batch"]["blocking_count"] == 0, anomalies
        published = client.post(f"/api/admin/publications/runs/{run_id}/batches", json={})
        assert published.ok, published.text
        assert published.json()["publication"]["grades"][0]["total_score"] == 10
    finally:
        client.post(f"/api/admin/papers/{slug}/close", json={})
        client.delete(f"/api/admin/papers/{slug}")


@pytest.mark.parametrize("operation", ["complete", "final_failure"])
@pytest.mark.parametrize("legacy_lock_control", [False, True])
def test_worker_and_real_go_regrade_lock_order(client, scoring_exam, operation, legacy_lock_control):
    """Legacy-order control proves the cycle; actual repository must avoid it."""
    from concurrent.futures import ThreadPoolExecutor
    from scoring_worker import repository as repo
    _, session = scoring_exam
    sid = int(submit(client, session, copy.deepcopy(ANSWERS)))
    dsn = _schema_dsn(os.environ.get("EXAM_CONTRACT_DATABASE_URL", DEFAULT_DSN))
    with psycopg.connect(dsn) as conn:
        conn.execute("UPDATE grading_jobs SET available_at='2000-01-01'::timestamptz WHERE submission_id=%s AND generation=1", (sid,))
        conn.commit()
        if operation == "final_failure":
            conn.execute("UPDATE grading_jobs SET max_attempts=1 WHERE submission_id=%s", (sid,))
            conn.commit()
        job = repo.claim_job(conn, "lock-race-worker", 30)
        assert job is not None and job.submission_id == sid
        conn.commit()
    locked = threading.Event()
    release = threading.Event()
    worker_pid = []

    class PausedConnection:
        def __init__(self, connection):
            self.connection = connection
        def execute(self, query, *args, **kwargs):
            result = self.connection.execute(query, *args, **kwargs)
            target = repo._COMPLETE_VERIFY_SQL if operation == "complete" else repo._FAIL_VERIFY_SQL
            if not legacy_lock_control and query == target:
                locked.set()
                assert release.wait(10), "worker gate was not released"
            return result

    def worker():
        try:
            with psycopg.connect(dsn) as conn:
                worker_pid.append(conn.execute("SELECT pg_backend_pid()").fetchone()[0])
                conn.commit()
                if legacy_lock_control:
                    conn.execute("SELECT id FROM grading_jobs WHERE id=%s FOR UPDATE", (job.id,))
                    locked.set()
                    assert release.wait(10)
                wrapped = PausedConnection(conn)
                if operation == "complete":
                    return repo.complete_job(wrapped, job, 10, 10, b'[]')
                return repo.fail_job(wrapped, job, "synthetic final failure")
        except psycopg.errors.DeadlockDetected:
            return "deadlock-control"

    with ThreadPoolExecutor(max_workers=2) as executor:
        processing = executor.submit(worker)
        try:
            assert locked.wait(5)
            regrading = executor.submit(client.post, f"/api/admin/regrade/{sid}",
                                        json={"reason": "验证重评与worker写回的锁顺序"})
            with psycopg.connect(dsn, autocommit=True) as monitor:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    waiting = monitor.execute("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE %s=ANY(pg_blocking_pids(pid)))",
                                              (worker_pid[0],)).fetchone()[0]
                    if waiting:
                        break
                    time.sleep(.05)
                assert waiting, "Go regrade did not reach the expected database lock"
        finally:
            release.set()
        outcome = processing.result(timeout=10)
        response = regrading.result(timeout=10)
    if legacy_lock_control:
        assert outcome == "deadlock-control" or (response.status_code == 500 and "deadlock" in response.text.lower())
        return
    assert outcome == ("done" if operation == "complete" else "dead")
    assert response.ok, response.text
    with psycopg.connect(dsn) as conn:
        assert conn.execute("SELECT grading_generation,grading_status FROM submissions WHERE id=%s", (sid,)).fetchone() == (2, "pending")
        assert conn.execute("SELECT count(*) FROM regrade_audit WHERE submission_id=%s AND from_generation=1 AND to_generation=2", (sid,)).fetchone()[0] == 1
        assert conn.execute("SELECT count(*) FROM grading_jobs WHERE submission_id=%s AND generation=2 AND status='queued'", (sid,)).fetchone()[0] == 1
        history_count = conn.execute("SELECT count(*) FROM grading_history WHERE submission_id=%s", (sid,)).fetchone()[0]
        assert history_count == (1 if operation == "complete" else 0)
        before_late_write = conn.execute("SELECT grading_generation,grading_status,total_score FROM submissions WHERE id=%s", (sid,)).fetchone()
        assert repo.complete_job(conn, job, 99, 99, b'[]') == "lost"
        conn.commit()
        assert conn.execute("SELECT grading_generation,grading_status,total_score FROM submissions WHERE id=%s", (sid,)).fetchone() == before_late_write
        assert conn.execute("SELECT count(*) FROM grading_history WHERE submission_id=%s", (sid,)).fetchone()[0] == history_count
