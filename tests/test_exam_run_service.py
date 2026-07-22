"""考试轮次服务：发布、草稿、收卷。"""
from __future__ import annotations

import json
import sys
import types
from enum import Enum
from pathlib import Path

import pytest
from fastapi.testclient import TestClient


def _install_subjective_scoring_stub(monkeypatch):
    if "subjective_scoring" in sys.modules:
        return

    class ScoringMode(Enum):
        TEXT = "text"
        SQL = "sql"
        CODE = "code"
        CALCULATION = "calculation"

    class ReviewLevel(Enum):
        MANUAL_REQUIRED = "manual_required"
        SUGGESTED_REVIEW = "suggested_review"
        PASS = "pass"

    class ScoringRequest:
        @classmethod
        def model_validate(cls, payload):
            obj = cls()
            obj.__dict__.update(payload)
            return obj

    class ScoringResult:
        pass

    class CohereRerankerPairScorer:
        def __init__(self, *args, **kwargs):
            pass

        def close(self):
            pass

    class SubjectiveScoringService:
        def __init__(self, *args, **kwargs):
            pass

    module = types.ModuleType("subjective_scoring")
    module.CohereRerankerPairScorer = CohereRerankerPairScorer
    module.ReviewLevel = ReviewLevel
    module.ScoringMode = ScoringMode
    module.ScoringRequest = ScoringRequest
    module.ScoringResult = ScoringResult
    module.SubjectiveScoringService = SubjectiveScoringService
    monkeypatch.setitem(sys.modules, "subjective_scoring", module)


def _sample_questions():
    return [
        {
            "id": "q1",
            "type": "single_choice",
            "question": "1+1?",
            "options": [{"key": "A", "text": "1"}, {"key": "B", "text": "2"}],
            "answer": "B",
            "score": 5,
        },
        {
            "id": "q2",
            "type": "short_answer",
            "question": "REST?",
            "answer": "资源导向 HTTP",
            "score": 10,
            "scoring_mode": "text",
        },
    ]


@pytest.fixture()
def runs_env(tmp_path, monkeypatch):
    _install_subjective_scoring_stub(monkeypatch)
    from backend import question_loader as ql
    from backend import paper_store
    from backend import database
    from backend import exam_run_service

    papers = tmp_path / "papers"
    papers.mkdir()
    backups = tmp_path / "backups" / "papers"
    backups.mkdir(parents=True)
    (papers / "index.json").write_text(json.dumps({"papers": []}), encoding="utf-8")
    runs = tmp_path / "exam_runs"
    runs.mkdir()

    monkeypatch.setattr(ql, "PAPERS_DIR", papers)
    monkeypatch.setattr(ql, "INDEX_PATH", papers / "index.json")
    monkeypatch.setattr(ql, "BACKUPS_DIR", backups)
    monkeypatch.setattr(ql, "DATA_DIR", tmp_path)
    monkeypatch.setattr(ql, "LEGACY_QUESTIONS_PATH", tmp_path / "questions.json")
    ql.clear_question_cache()

    monkeypatch.setattr(database, "DB_PATH", tmp_path / "exam.db")
    database._initialized = False
    database.init_db()

    monkeypatch.setattr(exam_run_service, "EXAM_RUNS_DIR", runs)
    monkeypatch.setattr(exam_run_service, "PROJECT_ROOT", tmp_path)
    exam_run_service.set_grading_scheduler(lambda *a, **k: None)

    paper_store.create_paper(slug="mech", name="机电")
    paper_store.save_paper(
        "mech",
        {
            "name": "机电",
            "exam_info": {"title": "机电考试", "description": "d", "passing_score": 6},
            "questions": _sample_questions(),
        },
    )
    return tmp_path


def test_open_run_creates_snapshot_and_round(runs_env):
    from backend import exam_run_service, database

    run = exam_run_service.open_run("mech")
    assert run["status"] == "open"
    assert run["round_no"] == 1
    assert run["public_token"]
    snap = Path(runs_env / "exam_runs" / f"{run['id']}.json")
    assert snap.exists()
    assert database.get_active_run_for_paper("mech")["id"] == run["id"]

    with pytest.raises(Exception) as ei:
        exam_run_service.open_run("mech")
    assert "ACTIVE_RUN_EXISTS" in str(ei.value) or getattr(ei.value, "code", "") == "ACTIVE_RUN_EXISTS"


def test_start_resume_preserves_timer_and_draft(runs_env):
    from backend import exam_run_service

    run = exam_run_service.open_run("mech")
    token = run["public_token"]
    first = exam_run_service.start_or_resume_session(
        paper_id="mech",
        run_token=token,
        name="张三",
        employee_id="E1",
        department=None,
        client_ip="1.1.1.1",
        user_agent="t",
    )
    assert first["created"] is True
    assert first["session_token"]
    sid = first["session_id"]
    st = first["session_token"]
    started = first["started_at"]
    deadline = first["deadline_at"]

    exam_run_service.save_draft(
        sid, session_token=st, revision=1, answers={"q1": "B"}
    )
    second = exam_run_service.start_or_resume_session(
        paper_id="mech",
        run_token=token,
        name="张三",
        employee_id="E1",
        department=None,
        client_ip="1.1.1.1",
        user_agent="t",
    )
    assert second["created"] is False
    assert second["started_at"] == started
    assert second["deadline_at"] == deadline
    assert second["answers"].get("q1") == "B"


def test_stale_draft_revision_rejected(runs_env):
    from backend import exam_run_service

    run = exam_run_service.open_run("mech")
    first = exam_run_service.start_or_resume_session(
        paper_id="mech",
        run_token=run["public_token"],
        name="李四",
        employee_id="E2",
        department=None,
        client_ip=None,
        user_agent=None,
    )
    exam_run_service.save_draft(
        first["session_id"],
        session_token=first["session_token"],
        revision=2,
        answers={"q1": "A"},
    )
    with pytest.raises(Exception) as ei:
        exam_run_service.save_draft(
            first["session_id"],
            session_token=first["session_token"],
            revision=1,
            answers={"q1": "B"},
        )
    assert getattr(ei.value, "code", "") == "STALE_DRAFT_REVISION" or "STALE" in str(ei.value)


def test_manual_submit_and_second_round(runs_env):
    from backend import exam_run_service, database
    from backend.main import app

    monkey_client = TestClient(app)
    run = exam_run_service.open_run("mech")
    token = run["public_token"]

    r = monkey_client.get("/api/exam", params={"paper": "mech", "run": token})
    assert r.status_code == 200
    assert r.json()["questions"]

    start = exam_run_service.start_or_resume_session(
        paper_id="mech",
        run_token=token,
        name="王五",
        employee_id="E3",
        department="研发",
        client_ip=None,
        user_agent=None,
    )
    sub = exam_run_service.submit_manual(
        session_id=start["session_id"],
        session_token=start["session_token"],
        answers={"q1": "B", "q2": "资源导向 HTTP"},
    )
    assert sub["success"] is True
    assert database.submission_count(run_id=run["id"]) == 1

    # close finalize then reopen
    close = exam_run_service.begin_close("mech")
    assert close["status"] == "closing"
    # force finalize by setting finalize_at past via direct update
    with database.db_cursor() as conn:
        conn.execute(
            "UPDATE exam_runs SET finalize_at = '2000-01-01T00:00:00+00:00' WHERE id = ?",
            (run["id"],),
        )
    exam_run_service.finalize_run(run["id"])
    assert database.get_run_by_id(run["id"])["status"] == "closed"

    run2 = exam_run_service.open_run("mech")
    assert run2["round_no"] == 2
    # same employee can start again
    start2 = exam_run_service.start_or_resume_session(
        paper_id="mech",
        run_token=run2["public_token"],
        name="王五",
        employee_id="E3",
        department="研发",
        client_ip=None,
        user_agent=None,
    )
    assert start2["created"] is True

    # old token closed
    body = exam_run_service.get_public_exam("mech", token)
    assert body.get("closed") is True or body.get("run_status") == "closed"


def test_admin_close_auto_submits_drafts(runs_env):
    from backend import exam_run_service, database

    run = exam_run_service.open_run("mech")
    token = run["public_token"]

    a = exam_run_service.start_or_resume_session(
        paper_id="mech", run_token=token, name="A", employee_id="A1",
        department=None, client_ip=None, user_agent=None,
    )
    exam_run_service.submit_manual(
        session_id=a["session_id"], session_token=a["session_token"],
        answers={"q1": "B"},
    )

    b = exam_run_service.start_or_resume_session(
        paper_id="mech", run_token=token, name="B", employee_id="B1",
        department=None, client_ip=None, user_agent=None,
    )
    exam_run_service.save_draft(
        b["session_id"], session_token=b["session_token"], revision=1,
        answers={"q1": "A"},
    )

    c = exam_run_service.start_or_resume_session(
        paper_id="mech", run_token=token, name="C", employee_id="C1",
        department=None, client_ip=None, user_agent=None,
    )

    exam_run_service.begin_close("mech")
    with database.db_cursor() as conn:
        conn.execute(
            "UPDATE exam_runs SET finalize_at = '2000-01-01T00:00:00+00:00' WHERE id = ?",
            (run["id"],),
        )
    ids = exam_run_service.finalize_run(run["id"])
    # A already submitted; B and C auto
    assert len(ids) == 2
    assert database.submission_count(run_id=run["id"]) == 3

    subs = database.list_submissions(run_id=run["id"], limit=20)
    by_emp = {s["employee_id"]: s for s in subs}
    assert by_emp["B1"]["auto_submit_reason"] == "admin_closed"
    assert by_emp["C1"]["auto_submit_reason"] == "admin_closed"
    assert json.loads(by_emp["B1"]["answers_json"] if "answers_json" in by_emp["B1"] else "{}") or by_emp["B1"].get("answers")

    # idempotent finalize
    ids2 = exam_run_service.finalize_run(run["id"])
    assert ids2 == []
    assert database.submission_count(run_id=run["id"]) == 3


def test_api_open_and_exam_flow(runs_env):
    from backend.main import app
    from backend import exam_run_service

    client = TestClient(app)
    r = client.post("/api/admin/papers/mech/open")
    assert r.status_code == 200, r.text
    data = r.json()
    assert data["url"]
    token = data["public_token"]

    r = client.get("/api/exam", params={"paper": "mech", "run": token})
    assert r.status_code == 200
    assert "answer" not in r.json()["questions"][0]

    r = client.post(
        "/api/exam/start",
        json={
            "paper_id": "mech",
            "run_token": token,
            "name": "赵六",
            "employee_id": "E9",
        },
    )
    assert r.status_code == 200
    sess = r.json()
    assert sess["session_token"]

    r = client.put(
        f"/api/exam/sessions/{sess['session_id']}/draft",
        json={
            "session_token": sess["session_token"],
            "revision": 1,
            "answers": {"q1": "B"},
        },
    )
    assert r.status_code == 200

    r = client.post(
        "/api/submit",
        json={
            "session_id": sess["session_id"],
            "session_token": sess["session_token"],
            "answers": {"q1": "B", "q2": "x"},
        },
    )
    assert r.status_code == 200
    assert r.json()["submission_id"]

    r = client.post("/api/admin/papers/mech/close")
    assert r.status_code == 200
    assert r.json()["status"] == "closing"


def test_api_batch_open_not_captured_as_slug(runs_env):
    """/papers/batch/open 不得被 /papers/{slug}/open 抢路由（slug=batch）。"""
    from backend.main import app

    client = TestClient(app)
    r = client.post(
        "/api/admin/papers/batch/open",
        json={"slugs": ["mech"]},
    )
    assert r.status_code == 200, r.text
    data = r.json()
    assert data["updated"] == 1
    assert data["papers"][0]["slug"] == "mech"
    assert data["papers"][0].get("public_token")
    assert data["papers"][0].get("url")
    # 确认不是「试卷不存在: batch」
    assert not any(
        (e.get("code") == "PAPER_NOT_FOUND" and "batch" in str(e.get("message", "")))
        for e in (data.get("errors") or [])
    )


def test_api_batch_open_partial_errors(runs_env):
    from backend.main import app

    client = TestClient(app)
    r = client.post(
        "/api/admin/papers/batch/open",
        json={"slugs": ["mech", "no-such-paper-xyz"]},
    )
    assert r.status_code == 200, r.text
    data = r.json()
    assert data["updated"] == 1
    assert data["requested"] == 2
    assert len(data["errors"]) == 1
    assert data["errors"][0]["slug"] == "no-such-paper-xyz"


def test_reset_rounds_clears_history_and_blocks_active(runs_env):
    from backend.main import app
    from backend import exam_run_service, database

    # 发布并关闭一轮
    run = exam_run_service.open_run("mech")
    exam_run_service.begin_close("mech")
    with database.db_cursor() as conn:
        conn.execute(
            "UPDATE exam_runs SET finalize_at = '2000-01-01T00:00:00+00:00' WHERE id = ?",
            (run["id"],),
        )
    exam_run_service.finalize_run(run["id"])
    assert database.max_round_no("mech") == 1

    # 活动轮次不可重置
    exam_run_service.open_run("mech")
    blocked = exam_run_service.reset_rounds(["mech"])
    assert blocked["updated"] == 0
    assert blocked["errors"][0]["code"] == "ACTIVE_RUN_EXISTS"

    exam_run_service.begin_close("mech")
    with database.db_cursor() as conn:
        conn.execute(
            "UPDATE exam_runs SET finalize_at = '2000-01-01T00:00:00+00:00' WHERE paper_id = 'mech'"
        )
    exam_run_service.scan_and_finalize_due_runs()

    client = TestClient(app)
    r = client.post("/api/admin/exams/reset-rounds", json={"slugs": []})
    assert r.status_code == 200, r.text
    data = r.json()
    assert data["updated"] >= 1
    mech = next(p for p in data["papers"] if p["slug"] == "mech")
    assert mech["runs_deleted"] >= 1
    assert database.max_round_no("mech") == 0
    assert database.get_latest_run_for_paper("mech") is None

    # 再次发布应从第 1 轮开始
    run2 = exam_run_service.open_run("mech")
    assert run2["round_no"] == 1
