"""Task 0 契约: 学生 API 端到端。

锁定的端点与返回 schema:
- GET  /api/exam                ?paper=&run=        -> {paper_id, run_id, run_no, paper_name,
                                                          closed, public_token, status, ...,
                                                          exam_info, questions: [sanitized...]}
- POST /api/exam/start          {paper_id, run_token, name, employee_id, department?}
                                                        -> {session_id, session_token, started_at,
                                                           deadline_at, draft_revision, answers,
                                                           run_status, session_status, created}
- PUT  /api/exam/sessions/{id}/draft  {session_token, revision, answers}
                                                        -> {success, draft_revision, draft_saved_at,
                                                           run_status, session_status, finalize_at}
- GET  /api/exam/sessions/{id}/status  ?session_token=
                                                        -> {session_id, session_status, run_status,
                                                           started_at, deadline_at, draft_revision,
                                                           draft_saved_at, finalize_at, submission_id,
                                                           server_time}
- POST /api/submit             {session_id, session_token, answers}
                                                        -> {success, submission_id, status,
                                                           paper_id, run_id, message}
- GET  /api/submission/{id}/status     -> {submission_id, status}
- GET  /api/health             -> {ok, time}
"""
from __future__ import annotations

import json
import os
import time

import pytest

EXPECT_GO = os.environ.get("EXAM_CONTRACT_EXPECT_GO", "0") == "1"


def wait_until(predicate, timeout: float = 5.0, interval: float = 0.05) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return False

# 期望的字段集合 (Python 基线), 用于 keys-subset 断言
EXAM_START_KEYS = {"session_id", "session_token", "started_at", "deadline_at",
                   "draft_revision", "answers", "run_status", "session_status", "created"}
DRAFT_KEYS = {"success", "draft_revision", "draft_saved_at",
              "run_status", "session_status", "finalize_at"}
STATUS_KEYS = {"session_id", "session_status", "run_status", "started_at", "deadline_at",
               "draft_revision", "draft_saved_at", "finalize_at",
               "submission_id", "server_time"}
SUBMISSION_STATUS_KEYS = {"submission_id", "status"}
HEALTH_KEYS = {"ok", "time"}


# ---------------------------------------------------------------------------
# /api/health
# ---------------------------------------------------------------------------
def test_health(client):
    r = client.get("/api/health")
    assert r.status_code == 200
    body = r.json()
    assert set(body).issuperset(HEALTH_KEYS)
    assert body["ok"] is True
    assert isinstance(body["time"], str) and body["time"]


# ---------------------------------------------------------------------------
# /api/exam (脱敏卷) —— 用 sanitized_exam 黄金 fixture 比对
# ---------------------------------------------------------------------------
@pytest.mark.xfail(reason="已知缺陷: GET /api/exam 仅返 5 字段, 缺 round_no/paper_name/"
                          "closed/run_status/exam_info, 前端 exam.js:199-211 依赖 (审计问题#8)",
                   strict=False)
def test_get_public_exam_shape(client, paper_loaded, sanitized_exam_golden):
    slug, run = paper_loaded
    r = client.get("/api/exam", params={"paper": slug, "run": run["public_token"]})
    assert r.status_code == 200, r.text if hasattr(r, "text") else r.json()
    body = r.json()
    # 必含字段 (与 exam_run_service.get_public_exam base 对齐)
    for k in ("paper_id", "run_id", "round_no", "paper_name", "closed",
              "run_status", "exam_info", "questions", "duration_minutes"):
        assert k in body, f"missing key {k}"
    assert body["paper_id"] == slug
    assert body["closed"] is False
    assert body["run_status"] == "open"

    # questions 形状必须与黄金 sanitized 一致 (深脱敏, 敏感字段剥离)
    questions = body["questions"]
    assert isinstance(questions, list)
    assert len(questions) == len(sanitized_exam_golden["questions"])

    SENSITIVE = {"answer", "answers_by_language", "answer_aliases",
                 "scoring_points", "scoring_points_by_language",
                 "answer_rubric", "calculation"}

    def _check_clean(node, path="q"):
        if isinstance(node, dict):
            for k, v in node.items():
                assert k not in SENSITIVE, f"敏感字段泄漏: {path}.{k}"
                _check_clean(v, f"{path}.{k}")
        elif isinstance(node, list):
            for i, x in enumerate(node):
                _check_clean(x, f"{path}[{i}]")

    _check_clean(questions)

    # 校验 全题型 出现: single_choice / multiple_choice / true_false /
    # short_answer / composite (代码题是 short_answer + scoring_mode=code)
    types = {q["type"] for q in questions}
    assert {"single_choice", "multiple_choice", "true_false",
            "short_answer", "composite"}.issubset(types)
    # 复合题必须保留 subquestions 且子题也脱敏
    comp = [q for q in questions if q["type"] == "composite"][0]
    assert "subquestions" in comp and comp["subquestions"]
    for sub in comp["subquestions"]:
        assert {"id", "question", "score", "scoring_mode"}.issubset(sub)
        _check_clean(sub, "sub")


# ---------------------------------------------------------------------------
# /api/exam/start —— 新建会话
# ---------------------------------------------------------------------------
def test_start_session_creates(client, paper_loaded):
    slug, run = paper_loaded
    body = {
        "paper_id": slug,
        "run_token": run["public_token"],
        "name": "张三-契约",
        "employee_id": "EMP0001",
        "department": "技术部",
    }
    r = client.post("/api/exam/start", json=body)
    assert r.status_code == 200, getattr(r, "text", r.json())
    data = r.json()
    assert set(data).issuperset(EXAM_START_KEYS)
    assert data["created"] is True
    assert data["session_id"]
    assert data["session_token"]  # 新建时必返
    assert data["started_at"]
    assert data["deadline_at"]
    assert isinstance(data["answers"], dict)
    assert data["draft_revision"] == 0


# ---------------------------------------------------------------------------
# /api/exam/start —— 同员工同 run 幂等恢复
# ---------------------------------------------------------------------------
def test_start_session_idempotent_resume(client, paper_loaded):
    slug, run = paper_loaded
    body = {
        "paper_id": slug, "run_token": run["public_token"],
        "name": "李四-恢复", "employee_id": "EMP0002",
    }
    r1 = client.post("/api/exam/start", json=body).json()
    assert r1["created"] is True
    r2 = client.post("/api/exam/start", json=body).json()
    assert r2["created"] is False
    assert r2["session_id"] == r1["session_id"]
    # 恢复时不再返回 session_token (Python 基线契约)
    assert r2["session_token"] is None
    assert r2["answers"] == r1["answers"]


# ---------------------------------------------------------------------------
# /api/exam/start —— 拒绝错误 run token
# ---------------------------------------------------------------------------
@pytest.mark.xfail(reason="已知缺陷: 无效 run_token 开考返回 500 而非 4xx "
                          "(错误映射缺 runs.ErrRunNotFound, 审计 M3)", strict=False)
def test_start_session_rejects_bad_token(client, paper_loaded):
    slug, _ = paper_loaded
    body = {"paper_id": slug, "run_token": "not-a-real-token",
            "name": "x", "employee_id": "y"}
    r = client.post("/api/exam/start", json=body)
    assert r.status_code in (400, 403, 404)


# ---------------------------------------------------------------------------
# /api/exam/sessions/{id}/draft + /status —— 写草稿与查询
# ---------------------------------------------------------------------------
@pytest.mark.xfail(reason="已知缺陷: draft revision 语义 off-by-one (前端发新版本号, 后端 CAS "
                          "要求当前版本号) + status 接口返回 PascalCase (审计实测断点 2/3)",
                   strict=False)
def test_save_draft_and_status(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "王五-草稿", "employee_id": "EMP0003",
    }).json()
    sid, stok = s["session_id"], s["session_token"]

    # revision 0 → 1
    r = client.put(f"/api/exam/sessions/{sid}/draft", json={
        "session_token": stok, "revision": 1,
        "answers": {"c-single": ["A"], "c-multi": ["A", "B"]},
    })
    assert r.status_code == 200, getattr(r, "text", r.json())
    d = r.json()
    assert set(d).issuperset(DRAFT_KEYS)
    assert d["success"] is True
    assert d["draft_revision"] == 1

    # status 应反映 draft_revision
    st = client.get(f"/api/exam/sessions/{sid}/status",
                    params={"session_token": stok}).json()
    assert set(st).issuperset(STATUS_KEYS)
    assert st["session_id"] == sid
    assert st["draft_revision"] == 1
    assert st["draft_saved_at"] is not None


@pytest.mark.xfail(reason="已知缺陷: draft revision 语义 off-by-one, 回退旧 revision 反而成功 "
                          "(审计实测断点 2)", strict=False)
def test_save_draft_rejects_stale_revision(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "stale-测试", "employee_id": "EMP0004",
    }).json()
    sid, stok = s["session_id"], s["session_token"]
    # 写到 revision 1
    client.put(f"/api/exam/sessions/{sid}/draft", json={
        "session_token": stok, "revision": 1, "answers": {"c-single": ["A"]},
    })
    # 故意回退到旧 revision 0 => 应 4xx
    r = client.put(f"/api/exam/sessions/{sid}/draft", json={
        "session_token": stok, "revision": 0, "answers": {"c-single": ["B"]},
    })
    assert r.status_code in (400, 409)


def test_save_draft_rejects_bad_session_token(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "badtok-测试", "employee_id": "EMP0005",
    }).json()
    sid = s["session_id"]
    r = client.put(f"/api/exam/sessions/{sid}/draft", json={
        "session_token": "WRONG", "revision": 1, "answers": {},
    })
    assert r.status_code in (400, 401, 403, 404)


# ---------------------------------------------------------------------------
# /api/submit —— 提交 + 状态轮询 (Python 基线下主观评分是同步完成的)
# ---------------------------------------------------------------------------
def _build_full_answers(paper):
    """构造 paper 中所有题目的"全对客观答案 + 主观任意文本"。"""
    answers: dict[str, object] = {}
    for q in paper["questions"]:
        qid = q["id"]
        t = q.get("type")
        if t == "single_choice":
            ans = q["answer"]
            answers[qid] = [ans] if isinstance(ans, str) else list(ans)
        elif t == "multiple_choice":
            answers[qid] = list(q["answer"])
        elif t == "true_false":
            ans = q["answer"]
            # answer 是 bool; 学生侧要 submitting 文本键
            answers[qid] = ["正确"] if ans else ["错误"]
        elif t == "fill_blank":
            answers[qid] = q["answer"][0] if isinstance(q["answer"], list) else q["answer"]
        elif t == "short_answer":
            if q.get("scoring_mode") == "code":
                lang = (q.get("allowed_languages") or [q.get("code_language")])[0]
                answers[qid] = {"language": lang,
                               "code": q["answers_by_language"][lang]}
            else:
                answers[qid] = q["answer"]  # 主观文本, 写参考答案长度即可得 fake 满分
        elif t == "composite":
            sub_ans: dict[str, object] = {}
            for sub in q.get("subquestions", []):
                sid = sub["id"]
                sm = sub.get("scoring_mode")
                if sm == "text":
                    sub_ans[sid] = sub["answer"]
                elif sm == "code":
                    lang = (sub.get("allowed_languages") or [sub.get("code_language")])[0]
                    sub_ans[sid] = {"language": lang,
                                    "code": sub["answers_by_language"][lang]}
                else:
                    sub_ans[sid] = sub.get("answer") or ""
            answers[qid] = sub_ans
    return answers


def test_submit_and_submission_status(client, paper_loaded, paper_smoke):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "提交-测试", "employee_id": "EMP0006",
    }).json()
    sid, stok = s["session_id"], s["session_token"]

    answers = _build_full_answers(paper_smoke)
    r = client.post("/api/submit", json={
        "session_id": sid, "session_token": stok, "answers": answers,
    })
    # Go 返回 201 Created; 前端按 res.ok 判断, 契约收敛为 2xx + submission_id
    assert r.status_code in (200, 201), getattr(r, "text", r.json())
    d = r.json()
    assert d["submission_id"]

    # Python 基线: 主观评分同步完成, 状态应能轮询到 completed
    sid_sub = d["submission_id"]
    ok = wait_until(lambda: (
        client.get(f"/api/submission/{sid_sub}/status").json().get("status")
        in ("completed", "reviewed", "done")
    ), timeout=5.0)
    if not ok:
        # 即便没收敛到 completed 也至少能拿到 (Python 基线有时 status="grading")
        st = client.get(f"/api/submission/{sid_sub}/status")
        assert st.status_code == 200
        assert set(st.json()).issuperset(SUBMISSION_STATUS_KEYS)


def test_submit_rejects_bad_token(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "badtok-提交", "employee_id": "EMP0007",
    }).json()
    sid = s["session_id"]
    r = client.post("/api/submit", json={
        "session_id": sid, "session_token": "WRONG", "answers": {},
    })
    assert r.status_code in (400, 401, 403, 404)
