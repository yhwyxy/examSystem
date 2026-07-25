"""Task 0 契约: 脱敏与鉴权安全面。

锁定要点:
- GET /api/exam 必须不含 answer/answers_by_language/scoring_points/answer_rubric/calculation
- 学生会话身份由 session_token 证明; 缺 token 或错误 token 必被拒
- admin 路由在 enable_auth=False 时放行 (Task 0 期间默认 false, 后续 Go 增强再切)
- 对错误 paper slug 必返 4xx, 不可 5xx
"""
from __future__ import annotations

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


SENSITIVE = {
    "answer", "answers_by_language", "answer_aliases",
    "scoring_points", "scoring_points_by_language",
    "answer_rubric", "calculation",
}


def _assert_no_sensitive(node, path="q"):
    if isinstance(node, dict):
        for k, v in node.items():
            assert k not in SENSITIVE, f"敏感字段泄漏: {path}.{k}"
            _assert_no_sensitive(v, f"{path}.{k}")
    elif isinstance(node, list):
        for i, x in enumerate(node):
            _assert_no_sensitive(x, f"{path}[{i}]")


# ---------------------------------------------------------------------------
# /api/exam 脱敏
# ---------------------------------------------------------------------------
def test_get_exam_no_sensitive_fields(client, paper_loaded):
    slug, run = paper_loaded
    body = client.get("/api/exam", params={"paper": slug, "run": run["public_token"]}).json()
    questions = body.get("questions", [])
    assert questions, "题量为空, fixture 未正确写入"
    _assert_no_sensitive(questions)


def test_admin_preview_does_keep_answers(client, paper_loaded):
    """admin preview 路由明确"返回完整数据包括答案";

    反向断言: 学生脱敏发生在 /api/exam, admin 必须能看答案。"""
    slug, _ = paper_loaded
    body = client.get(f"/api/admin/papers/{slug}/preview").json()
    # admin 侧必含 is_preview=True 标识
    assert body.get("is_preview") is True
    questions = body.get("questions", [])
    assert questions, "admin preview 题量为空, fixture 未正确写入"
    # 至少 1 题必含 answer (admin 视角)
    assert any("answer" in q for q in questions)


# ---------------------------------------------------------------------------
# 鉴权: session_token 缺失/错误必被拒
# ---------------------------------------------------------------------------
def test_draft_requires_valid_session_token(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "鉴权测试", "employee_id": "AUTH001",
    }).json()
    sid = s["session_id"]
    # 无 token
    r = client.put(f"/api/exam/sessions/{sid}/draft",
                   json={"session_token": "", "revision": 1, "answers": {}})
    assert r.status_code in (400, 401, 403, 404)
    # 错 token
    r = client.put(f"/api/exam/sessions/{sid}/draft",
                   json={"session_token": "not-the-token",
                         "revision": 1, "answers": {}})
    assert r.status_code in (400, 401, 403, 404)


def test_submit_requires_valid_session_token(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "鉴权-提交", "employee_id": "AUTH002",
    }).json()
    sid = s["session_id"]
    r = client.post("/api/submit", json={
        "session_id": sid, "session_token": "WRONG", "answers": {},
    })
    assert r.status_code in (400, 401, 403, 404)


# ---------------------------------------------------------------------------
# 错误 paper / run 必返 4xx (不可 5xx)
# ---------------------------------------------------------------------------
def test_unknown_paper_returns_4xx(client):
    r = client.get("/api/exam", params={"paper": "does-not-exist",
                                         "run": "any"})
    assert 400 <= r.status_code < 500


def test_unknown_run_returns_4xx(client, paper_loaded):
    slug, _ = paper_loaded
    r = client.get("/api/exam", params={"paper": slug,
                                         "run": "no-such-run-token"})
    assert 400 <= r.status_code < 500


# ---------------------------------------------------------------------------
# run token 不在 student 端泄露
# ---------------------------------------------------------------------------
def test_exam_response_does_not_leak_run_token(client, paper_loaded):
    slug, run = paper_loaded
    body = client.get("/api/exam", params={"paper": slug,
                                           "run": run["public_token"]}).json()
    # 学生公开卷不应回显 public_token 或任何敏感令牌
    assert "public_token" not in body
    assert "session_token" not in body
    # 同时题面无敏感字段
    _assert_no_sensitive(body.get("questions", []))


# ---------------------------------------------------------------------------
# admin 路由: enable_auth=False 放行 (Task 0 期间)
# ---------------------------------------------------------------------------
def test_admin_routes_open_when_auth_disabled(client):
    # 若生产 endpoint 仍要求 token, 则此断言在 Go 增强阶段会被替换;
    # 但 Task 0 Python 基线默认 enable_auth=False, admin 全放
    r = client.get("/api/admin/papers")
    assert r.status_code == 200, r.text if hasattr(r, "text") else r.json()
    # /api/admin/papers 直接返回 list, 不包一层
    body = r.json()
    assert isinstance(body, list)
