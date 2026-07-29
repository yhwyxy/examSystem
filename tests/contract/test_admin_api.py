"""Task 0 契约: 管理端 API。

锁定的端点与返回 schema (enable_auth=False, 全程免鉴权):
- GET  /api/admin/papers                 -> {papers: [{slug, name, has_questions, active_status, ...}]}
- POST /api/admin/papers                 {slug, name}            -> {slug, name, ...meta}
- GET  /api/admin/papers/{slug}          -> {slug, name, questions, exam_info, paper_id, ...}
- PUT  /api/admin/papers/{slug}           {doc}                  -> 同上
- PATCH /api/admin/papers/{slug}/meta    {name? description? passing_score? ...} -> {slug, name, ...}
- DELETE /api/admin/papers/{slug}         -> 200 / 204
- GET  /api/admin/papers/{slug}/preview   -> 与 /api/exam 等价的脱敏形态
- POST /api/admin/papers/{slug}/open      {}                      -> {success, status, run_id, round_no, finalize_at?}
- POST /api/admin/papers/{slug}/close     {}                      -> {success, status, run_id, round_no, active_sessions? finalize_at?}
- GET  /api/admin/exam-link?paper=&run=  -> {url, paper, run_token?}
- GET  /api/admin/papers/{slug}/exam-link -> {url, run_token}
- GET  /api/admin/exams                  -> {items: [{paper_id, id, round_no, status, ...}]}
- POST /api/admin/exams/reset-rounds     -> {reset: True}
- GET  /api/admin/stats                  -> {papers, runs, submissions, sessions, ...}
- GET  /api/admin/submissions            -> {items or submissions: [...]}
- GET  /api/admin/submissions/{id}       -> {submission, items: [...], ...} (review detail)
- POST /api/admin/regrade/{id}           -> {success, status}
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


# ---------------------------------------------------------------------------
# papers CRUD
# ---------------------------------------------------------------------------
def test_admin_papers_crud(client):
    # list 可用 (黑盒服务可能已有其他试卷, 只要求不含本用例 slug)
    r = client.get("/api/admin/papers")
    assert r.status_code == 200
    body = r.json()
    # /api/admin/papers 直接返回 list[dict], 不包一层 {papers:[...]}
    papers_list = body if isinstance(body, list) else body.get("papers", [])
    assert all(p.get("slug") != "crud-smoke" for p in papers_list)

    # create —— Go 契约: {ok: true, slug}
    r = client.post("/api/admin/papers", json={"slug": "crud-smoke", "name": "CRUD 试卷"})
    assert r.status_code in (200, 201)
    payload = r.json()
    assert payload.get("ok") is True or payload.get("success") is True
    meta = payload.get("paper") or payload
    assert meta["slug"] == "crud-smoke"

    # list 应含
    items = client.get("/api/admin/papers").json()
    items = items if isinstance(items, list) else items.get("papers", [])
    assert any(p["slug"] == "crud-smoke" for p in items)

    # get (full) —— 原始可编辑文档 {paper_id, name, exam_info, questions}
    full = client.get("/api/admin/papers/crud-smoke").json()
    assert full["paper_id"] == "crud-smoke"
    assert full["name"] == "CRUD 试卷"
    assert "questions" in full and isinstance(full["questions"], list)
    assert "exam_info" in full

    # 改名: Go 无 PATCH /meta 路由, 经 PUT 整卷更新
    full["name"] = "改名后的 CRUD"
    r = client.put("/api/admin/papers/crud-smoke", json=full)
    assert r.status_code in (200, 204)
    got = client.get("/api/admin/papers/crud-smoke").json()
    assert got["name"] == "改名后的 CRUD"

    # delete —— {ok: true}
    r = client.delete("/api/admin/papers/crud-smoke")
    assert r.status_code in (200, 204)
    body = r.json() or {}
    assert body.get("ok") is True or body.get("success") is True
    items = client.get("/api/admin/papers").json()
    items = items if isinstance(items, list) else items.get("papers", [])
    assert all(p["slug"] != "crud-smoke" for p in items)


@pytest.mark.xfail(reason="已知缺陷: GET /api/admin/papers/{slug} 缺 status/editable 字段, "
                          "前端 papers.js:188 依赖其实现'考试中锁定编辑器' (审计问题#11)",
                   strict=False)
def test_admin_paper_detail_reports_lock_status(client, paper_loaded):
    slug, _run = paper_loaded
    full = client.get(f"/api/admin/papers/{slug}").json()
    assert full["status"] in ("unpublished", "open", "closing", "closed")
    assert isinstance(full["editable"], bool)


# ---------------------------------------------------------------------------
# 通过 paper_loaded fixture 拉一张完整卷, admin preview = 完整数据 (含答案, 不脱敏)
# ---------------------------------------------------------------------------
def test_admin_paper_preview_includes_answers(client, paper_loaded):
    """admin preview 路由明确"返回完整数据包括答案", 故 answer 必须存在。"""
    slug, _run = paper_loaded
    r = client.get(f"/api/admin/papers/{slug}/preview")
    assert r.status_code == 200, getattr(r, "text", r.json())
    body = r.json()
    assert body.get("paper_id") == slug
    assert body.get("is_preview") is True
    questions = body.get("questions", [])
    assert isinstance(questions, list) and questions
    # 至少首题含 answer (admin 视角)
    has_any_answer = any("answer" in q for q in questions)
    assert has_any_answer, "admin preview 应保留答案"


# ---------------------------------------------------------------------------
# open/close run 流程
# ---------------------------------------------------------------------------
def test_admin_open_close_run(client, paper_loaded):
    slug, run = paper_loaded

    # 已有 round_no=1 的 open run (paper_loaded 已开考)。再 open 应 409 或翻到下一轮。
    r = client.post(f"/api/admin/papers/{slug}/open", json={})
    assert r.status_code in (200, 201, 409)
    if r.status_code in (200, 201):
        d = r.json()
        assert d.get("ok") is True or d.get("success") is True
        assert d["run_id"]

    # close —— Go 契约: {ok, slug}
    r = client.post(f"/api/admin/papers/{slug}/close", json={})
    assert r.status_code in (200, 201, 404, 409)
    if r.status_code in (200, 201):
        d = r.json()
        assert d.get("ok") is True or d.get("success") is True


# ---------------------------------------------------------------------------
# exams list + reset-rounds
# ---------------------------------------------------------------------------
def test_admin_exams_list_and_reset(client, paper_loaded):
    slug, _run = paper_loaded
    r = client.get("/api/admin/exams")
    assert r.status_code == 200
    # /api/admin/exams 直接返回 list[dict] (非 {items:[...]} / {exams:[...]})
    items = r.json()
    assert isinstance(items, list)
    assert any(it.get("paper_id") == slug or it.get("paper") == slug
               for it in items)

    # reset-rounds: 应幂等成功
    r = client.post("/api/admin/exams/reset-rounds", json={})
    assert r.status_code in (200, 204)
    if r.status_code == 200:
        # 返回 {}, "reset" 字段不强约束; 只校验 dict
        assert isinstance(r.json(), dict)


# ---------------------------------------------------------------------------
# stats
# ---------------------------------------------------------------------------
def test_admin_stats(client, paper_loaded):
    r = client.get("/api/admin/stats")
    assert r.status_code == 200
    body = r.json()
    # Python 基线 stats 形状较松; 只校验非空 dict 且有计数类 key
    assert isinstance(body, dict) and body
    count_keys = [k for k in body if isinstance(body[k], (int, float)) or
                  (isinstance(body[k], dict) and all(isinstance(v, (int, float)) for v in body[k].values()))]
    assert count_keys  # 至少一个聚合计数


# ---------------------------------------------------------------------------
# exam-link (admin 视角拿明文 token)
# ---------------------------------------------------------------------------
def test_admin_exam_link(client, paper_loaded):
    slug, _run = paper_loaded
    # query 形态 (旧式, 参数名 paper_slug)
    r = client.get("/api/admin/exam-link", params={"paper_slug": slug})
    assert r.status_code in (200, 404)
    # path 形态 (前端 papers.js 主路径)
    r2 = client.get(f"/api/admin/papers/{slug}/exam-link")
    assert r2.status_code in (200, 404)
    if r2.status_code == 200:
        d = r2.json()
        assert "url" in d
        # active run 必含 url + url 内嵌入 run=token
        url = d.get("url")
        if url:
            assert "run=" in url, "exam-link url 必含 run token 参数"
        assert d.get("qr_base64"), "exam-link 必含二维码 qr_base64"


# ---------------------------------------------------------------------------
# submissions list (空也可, 只校验 200 + 数组形态)
# ---------------------------------------------------------------------------
def test_admin_submissions_list_empty(client, paper_loaded):
    r = client.get("/api/admin/submissions")
    assert r.status_code == 200
    body = r.json()
    # 形态: 直接 list 或 {items: [...]} / {submissions: [...]}
    if isinstance(body, list):
        items = body
    else:
        items = body.get("items") if "items" in body else body.get("submissions")
    assert isinstance(items, list)
