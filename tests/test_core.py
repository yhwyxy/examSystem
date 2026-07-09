from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from backend.objective_grader import grade_multiple_choice, grade_single_choice, grade_true_false
from backend.llm_grader import parse_llm_output


def _cfg(**overrides):
    base = {
        "server": SimpleNamespace(allow_origins=["http://testserver"], port=8000),
        "exam": SimpleNamespace(
            duration_minutes=60,
            auto_submit=True,
            enable_global_time_window=False,
            start_time=None,
            end_time=None,
            grace_period_seconds=30,
        ),
        "scoring": SimpleNamespace(score_precision=1),
        "review": SimpleNamespace(
            high_confidence_threshold=0.75,
            need_review_threshold=0.5,
            low_confidence_threshold=0.35,
        ),
        "grading": SimpleNamespace(
            use_llm=True,
            use_embedding_fallback=True,
            llm=SimpleNamespace(
                provider="ollama",
                endpoint="http://localhost:11434",
                model="test-model",
                timeout_seconds=1,
                retry_times=0,
            ),
            embedding=SimpleNamespace(model="test-embedding", device="cpu"),
        ),
        "admin": SimpleNamespace(enable_auth=False, password=None),
        "export": SimpleNamespace(format="xlsx"),
    }
    base.update(overrides)
    return SimpleNamespace(**base)


def test_parse_iso_keeps_timezone_when_present():
    from backend.utils import parse_iso

    dt = parse_iso("2026-07-09T10:00:00+08:00")
    assert dt.tzinfo is not None
    assert dt.utcoffset().total_seconds() == 8 * 3600


def test_parse_iso_assumes_local_timezone_when_naive():
    from backend.utils import parse_iso

    dt = parse_iso("2026-07-09T10:00:00")
    # naive 输入应被补上本地时区，避免后续时差��算出错
    assert dt.tzinfo is not None


def test_parse_iso_raises_on_illegal_format():
    from backend.utils import parse_iso

    # 非法格式应直接抛 ValueError，而不是被无效的 try/except 静默吞掉
    with pytest.raises(ValueError):
        parse_iso("not-a-date")


def test_single_choice_scores_full_when_answer_matches():
    assert grade_single_choice('B', 'B', 5)['score'] == 5


def test_multiple_choice_partial_scores_by_correct_ratio_without_wrong_choice():
    result = grade_multiple_choice(['A', 'C'], ['A', 'C', 'D'], 6, partial=True)
    assert result['score'] == 4
    assert result['is_correct'] is False


def test_multiple_choice_scores_zero_when_contains_wrong_choice():
    result = grade_multiple_choice(['A', 'B'], ['A', 'C', 'D'], 6, partial=True)
    assert result['score'] == 0
    assert result['wrong_choices'] == ['B']


def test_true_false_accepts_string_bool():
    assert grade_true_false('false', False, 2)['score'] == 2


def test_parse_llm_output_rejects_out_of_range_score():
    assert parse_llm_output('{"score": 11, "confidence": 0.8, "reason": "x"}', 10) is None


def test_parse_llm_output_accepts_json_fenced_content():
    parsed = parse_llm_output('```json\n{"score": 8, "confidence": 0.7, "reason": "基本正确"}\n```', 10)
    assert parsed is not None
    assert parsed['machine_score'] == 8
    assert parsed['confidence'] == 0.7


def test_admin_export_uses_app_export_config(monkeypatch):
    from backend import main

    monkeypatch.setattr(main, "get_config", lambda: _cfg())
    monkeypatch.setattr(main.exporter, "export_submissions_xlsx", lambda: b"xlsx-bytes")

    client = TestClient(main.app)
    response = client.get("/api/admin/export")

    assert response.status_code == 200
    assert response.content == b"xlsx-bytes"
    assert response.headers["content-type"] == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"


def test_exam_page_uses_mounted_static_asset_paths():
    from backend import main

    client = TestClient(main.app)
    response = client.get("/exam")

    assert response.status_code == 200
    assert '/css/style.css' in response.text
    assert '/js/exam.js' in response.text
    assert '/static/css/style.css' not in response.text
    assert '/static/js/exam.js' not in response.text


def test_admin_submissions_with_default_sort_and_order(monkeypatch):
    from backend import main

    monkeypatch.setattr(main, "get_config", lambda: _cfg())
    client = TestClient(main.app)
    response = client.get("/api/admin/submissions?keyword=008")
    assert response.status_code == 200


def test_delete_submission_removes_record_and_logs():
    from backend import database
    import time

    uid = str(int(time.time() * 1000))[-6:]
    sid = database.insert_submission(
        name="删除测试", employee_id=f"X{uid}", department="测试部",
        answers={"q1": "a"},
        grading_detail=[{"question_id": "q1", "type": "short_answer", "score": 5, "max_score": 10}],
        scores={"objective_score": 0, "subjective_score_machine": 5,
                "subjective_score_final": 5, "total_score": 5},
        review_status="pending", started_at=None, client_ip=None, user_agent=None,
    )
    result = database.apply_review(submission_id=sid, question_id="q1", new_score=6, note="test")
    assert result["success"]

    assert database.get_submission(sid) is not None
    assert len(database.list_review_logs(sid)) == 1

    assert database.delete_submission(sid) is True
    assert database.get_submission(sid) is None
    assert len(database.list_review_logs(sid)) == 0
    assert database.delete_submission(sid) is False  # 再删一次返回 False


def test_delete_submissions_batch():
    from backend import database
    import time

    ids = []
    for i in range(3):
        uid = str(int(time.time() * 1000))[-6:] + str(i)
        sid = database.insert_submission(
            name=f"批量删除{i}", employee_id=f"B{uid}", department="测试部",
            answers={"q1": "a"}, grading_detail=[{"question_id": "q1", "score": 5}],
            scores={"objective_score": 0, "subjective_score_machine": 5,
                    "subjective_score_final": 5, "total_score": 5},
            review_status="pending", started_at=None, client_ip=None, user_agent=None,
        )
        ids.append(sid)

    deleted = database.delete_submissions(ids)
    assert deleted == 3
    for sid in ids:
        assert database.get_submission(sid) is None

    assert database.delete_submissions([]) == 0


def test_admin_submission_detail_includes_parsed_grading_detail(monkeypatch):
    from backend import main, database
    import time

    monkeypatch.setattr(main, "get_config", lambda: _cfg())
    uid = str(int(time.time() * 1000))[-6:]
    submission_id = database.insert_submission(
        name="测试用户",
        employee_id=f"D{uid}",
        department="测试部",
        answers={"q1": "ans1"},
        grading_detail=[
            {"question_id": "q1", "question": "问题1", "type": "short_answer",
             "max_score": 10, "final_score": 8, "student_answer": "ans1",
             "reference_answer": "ref1", "machine_score": 8, "confidence": 0.9,
             "grading_method": "llm", "review_status": "pending"}
        ],
        scores={"objective_score": 0, "subjective_score_machine": 8, "subjective_score_final": 8, "total_score": 8},
        review_status="pending",
        started_at=None,
        client_ip=None,
        user_agent=None,
    )

    client = TestClient(main.app)
    response = client.get(f"/api/admin/submissions/{submission_id}")
    assert response.status_code == 200
    data = response.json()
    assert isinstance(data["grading_detail"], list)
    assert len(data["grading_detail"]) == 1
    assert data["grading_detail"][0]["question_id"] == "q1"


def _insert_submission(database, suffix=""):
    import time
    uid = str(int(time.time() * 1000))[-6:] + suffix
    return database.insert_submission(
        name="API删除", employee_id=f"A{uid}", department="测试部",
        answers={"q1": "a"},
        grading_detail=[{"question_id": "q1", "type": "short_answer", "score": 5, "max_score": 10}],
        scores={"objective_score": 0, "subjective_score_machine": 5,
                "subjective_score_final": 5, "total_score": 5},
        review_status="pending", started_at=None, client_ip=None, user_agent=None,
    )


def test_admin_delete_endpoint_rejects_empty_ids(monkeypatch):
    from backend import main

    monkeypatch.setattr(main, "get_config", lambda: _cfg())
    client = TestClient(main.app)
    resp = client.request("DELETE", "/api/admin/submissions", json={"ids": []})
    assert resp.status_code == 400


def test_admin_delete_endpoint_single_and_batch(monkeypatch):
    from backend import main, database

    monkeypatch.setattr(main, "get_config", lambda: _cfg())
    client = TestClient(main.app)

    # 单条删除
    sid = _insert_submission(database, "s1")
    assert database.get_submission(sid) is not None
    resp = client.request("DELETE", "/api/admin/submissions", json={"ids": [sid]})
    assert resp.status_code == 200
    assert resp.json()["deleted"] == 1
    assert database.get_submission(sid) is None

    # 批量删除
    ids = [_insert_submission(database, f"b{i}") for i in range(3)]
    resp = client.request("DELETE", "/api/admin/submissions", json={"ids": ids})
    assert resp.status_code == 200
    assert resp.json()["deleted"] == 3
    for i in ids:
        assert database.get_submission(i) is None


def test_similarity_prefers_ollama_embedding_when_available(monkeypatch):
    import backend.embedding_grader as eg

    responses = [
        {"embedding": [1.0, 0.0]},
        {"embedding": [1.0, 0.0]},
    ]
    requests = []

    class FakeResponse:
        def __init__(self, payload):
            self.payload = payload

        def raise_for_status(self):
            return None

        def json(self):
            return self.payload

    class FakeClient:
        def __init__(self, timeout, **kwargs):
            self.timeout = timeout
            self.kwargs = kwargs

        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def post(self, url, json):
            requests.append((url, json))
            return FakeResponse(responses.pop(0))

    monkeypatch.setattr(eg, "httpx", SimpleNamespace(Client=FakeClient), raising=False)
    eg._load_model.cache_clear()

    sim, method = eg.similarity("REST 使用 HTTP 方法", "REST 使用 HTTP 方法")

    assert method == "ollama_embedding"
    assert sim == 1.0
    assert len(requests) == 2
    assert requests[0][0].endswith("/api/embeddings")
    assert requests[0][1]["model"] == "quentinz/bge-large-zh-v1.5:latest"


def test_grade_submission_aggregates_objective_final_scores(monkeypatch):
    import asyncio
    from backend import grader

    monkeypatch.setattr(grader, "get_config", lambda: _cfg())
    monkeypatch.setattr(grader, "get_question_map", lambda: {
        "q1": {"id": "q1", "type": "single_choice", "score": 5, "answer": "B", "question": "Q1"},
        "q2": {"id": "q2", "type": "multiple_choice", "score": 6, "answer": ["A", "C", "D"], "question": "Q2"},
    })

    result = asyncio.run(grader.grade_submission({"q1": "B", "q2": ["A", "C"]}))

    assert result.objective_score == 9
    assert result.total_score == 9
    assert result.grading_detail[0]["score"] == 5
    assert result.grading_detail[0]["final_score"] == 5


def test_grade_submission_uses_llm_machine_score_for_subjective(monkeypatch):
    import asyncio
    from backend import grader

    monkeypatch.setattr(grader, "get_config", lambda: _cfg())
    monkeypatch.setattr(grader, "get_question_map", lambda: {
        "q1": {"id": "q1", "type": "short_answer", "score": 10, "answer": "ref", "question": "Q1"},
    })
    monkeypatch.setattr(grader.llm_grader, "grade_subjective", lambda question, answer, cfg: {
        "machine_score": 8,
        "confidence": 0.8,
        "reason": "基本正确",
    })

    result = asyncio.run(grader.grade_submission({"q1": "answer"}))

    assert result.subjective_score_machine == 8
    assert result.subjective_score_final == 8
    assert result.total_score == 8
    assert result.review_status == "auto_scored"


def test_grade_submission_marks_embedding_low_confidence_for_review(monkeypatch):
    import asyncio
    from backend import grader

    monkeypatch.setattr(grader, "get_config", lambda: _cfg(grading=SimpleNamespace(
        use_llm=False,
        use_embedding_fallback=True,
        llm=SimpleNamespace(endpoint="http://localhost:11434", timeout_seconds=1),
        embedding=SimpleNamespace(model="test-embedding", device="cpu"),
    )))
    monkeypatch.setattr(grader, "get_question_map", lambda: {
        "q1": {"id": "q1", "type": "short_answer", "score": 10, "answer": "ref", "question": "Q1"},
    })
    monkeypatch.setattr(grader, "grade_with_embedding", lambda question, answer, reason: {
        "machine_score": 2,
        "final_score": 2,
        "confidence": 0.2,
        "reason": "低相似度",
        "review_status": "low_confidence",
    })

    result = asyncio.run(grader.grade_submission({"q1": "answer"}))

    assert result.subjective_score_final == 2
    assert result.review_status == "low_confidence"
    assert result.grading_detail[0]["review_status"] == "low_confidence"


def test_exam_api_returns_config_contract():
    from backend import main

    client = TestClient(main.app)
    response = client.get("/api/exam")

    assert response.status_code == 200
    data = response.json()
    assert data["config"]["duration_minutes"] == data["duration_minutes"]
    assert data["config"]["auto_submit"] is True


def test_submit_without_server_start_is_rejected(monkeypatch):
    from backend import main

    main._exam_start_times.clear()
    client = TestClient(main.app)
    response = client.post("/api/submit", json={
        "name": "未开始",
        "employee_id": "NO_START_001",
        "answers": {},
    })

    assert response.status_code == 403
    assert response.json()["detail"]["code"] == "EXAM_NOT_STARTED"


def test_submit_with_server_start_succeeds(monkeypatch):
    from backend import main

    monkeypatch.setattr(main.database, "insert_submission_pending", lambda **kwargs: 123)
    monkeypatch.setattr(main, "schedule_grading", lambda submission_id, answers: None, raising=False)
    main._exam_start_times.clear()
    client = TestClient(main.app)

    started = client.post("/api/exam/start", json={"employee_id": "START_OK_001"})
    assert started.status_code == 200

    response = client.post("/api/submit", json={
        "name": "已开始",
        "employee_id": "START_OK_001",
        "answers": {},
    })

    assert response.status_code == 200
    assert response.json()["submission_id"] == 123


def test_admin_endpoint_rejects_unauthenticated_when_auth_enabled(monkeypatch):
    from backend import main

    monkeypatch.setattr(main, "get_config", lambda: _cfg(admin=SimpleNamespace(enable_auth=True, password="secret")))
    client = TestClient(main.app)
    response = client.get("/api/admin/stats")

    assert response.status_code == 401
    assert response.json()["detail"]["code"] == "UNAUTHORIZED"


def test_global_time_window_ignored_when_disabled(monkeypatch):
    from datetime import datetime, timedelta, timezone
    from backend import main

    future = datetime.now(timezone.utc) + timedelta(days=1)
    monkeypatch.setattr(main, "get_config", lambda: _cfg(exam=SimpleNamespace(
        duration_minutes=60,
        auto_submit=True,
        enable_global_time_window=False,
        start_time=future,
        end_time=None,
        grace_period_seconds=30,
    )))

    client = TestClient(main.app)
    response = client.get("/api/exam")

    assert response.status_code == 200


def test_global_time_window_rejects_when_enabled_before_start(monkeypatch):
    from datetime import datetime, timedelta, timezone
    from backend import main

    future = datetime.now(timezone.utc) + timedelta(days=1)
    monkeypatch.setattr(main, "get_config", lambda: _cfg(exam=SimpleNamespace(
        duration_minutes=60,
        auto_submit=True,
        enable_global_time_window=True,
        start_time=future,
        end_time=None,
        grace_period_seconds=30,
    )))

    client = TestClient(main.app)
    response = client.get("/api/exam")

    assert response.status_code == 403
    assert response.json()["detail"]["code"] == "EXAM_NOT_STARTED"


def test_submission_status_omits_total_score(monkeypatch):
    from backend import main

    monkeypatch.setattr(main.database, "get_submission_status", lambda submission_id: {
        "id": submission_id,
        "review_status": "grading",
        "total_score": 88,
    })

    client = TestClient(main.app)
    response = client.get("/api/submission/123/status")

    assert response.status_code == 200
    assert response.json() == {"submission_id": 123, "status": "grading"}


def test_llm_grader_ignores_environment_proxy_for_local_ollama(monkeypatch):
    from backend import llm_grader

    client_kwargs = {}

    class FakeResponse:
        def raise_for_status(self):
            return None

        def json(self):
            return {"response": '{"score": 8, "confidence": 0.8, "reason": "基本正确"}'}

    class FakeClient:
        def __init__(self, **kwargs):
            client_kwargs.update(kwargs)

        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def post(self, url, json):
            return FakeResponse()

    monkeypatch.setattr(llm_grader, "get_config", lambda: _cfg())
    monkeypatch.setattr(llm_grader.httpx, "Client", FakeClient)

    result = llm_grader.grade_with_llm(
        {"id": "q1", "type": "short_answer", "score": 10, "question": "Q", "answer": "A"},
        "A",
    )

    assert result["machine_score"] == 8
    assert client_kwargs["trust_env"] is False


def test_embedding_grader_ignores_environment_proxy_for_local_ollama(monkeypatch):
    from backend import embedding_grader

    client_kwargs = {}

    class FakeResponse:
        def raise_for_status(self):
            return None

        def json(self):
            return {"embedding": [1.0, 0.0]}

    class FakeClient:
        def __init__(self, **kwargs):
            client_kwargs.update(kwargs)

        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def post(self, url, json):
            return FakeResponse()

    monkeypatch.setattr(embedding_grader, "get_config", lambda: _cfg())
    monkeypatch.setattr(embedding_grader.httpx, "Client", FakeClient)

    assert embedding_grader._ollama_embedding("hello") == [1.0, 0.0]
    assert client_kwargs["trust_env"] is False
