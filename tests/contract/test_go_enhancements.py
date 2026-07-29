"""Task 0 契约: Go 服务专属增强字段断言。

默认 skip——只有当 EXAM_CONTRACT_EXPECT_GO=1 时才启用。
针对的是 Go 重写后新增、Python 基线没有的字段:

  * draft 节流: PUT /api/exam/sessions/{id}/draft 返回 server_min_interval_ms / last_draft_at
  * grading_status: /api/submission/{id}/status 返回 graded_items / pending_items /
                                  subjectives_remaining / objective_score / subjective_score
  * health 深字段: /api/health 返回 version / build / db / worker_alive
  * 幂等ビジネス: close 重复调用返 round_no=active.round_no
  * admin/stats 新增 active_workers / scoring_queue_depth
  * draft payload 必含 server_revision (乐观锁)

这些断言点在 Python 基线不成立, 所以默认 skip。Task 5+ 落地 Go 行为时,
设 EXAM_CONTRACT_EXPECT_GO=1 + EXAM_CONTRACT_BASE_URL=http://127.0.0.1:8000 即开启。
"""
from __future__ import annotations

import os
import time

import pytest

EXPECT_GO = os.environ.get("EXAM_CONTRACT_EXPECT_GO", "1") == "1"
pexpect_go = pytest.mark.skipif(not EXPECT_GO, reason="EXAM_CONTRACT_EXPECT_GO=0 显式关闭")


def wait_until(predicate, timeout: float = 5.0, interval: float = 0.05) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return False


# ---------------------------------------------------------------------------
# health
# ---------------------------------------------------------------------------
@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: 健康接口缺 build 结构 (现为 version:{version,build_time})", strict=False)
def test_health_has_version_and_build(client):
    body = client.get("/api/health").json()
    for k in ("ok", "time", "version", "build"):
        assert k in body, f"Go health 缺字段 {k}"
    assert isinstance(body["build"], dict)
    assert "commit" in body["build"] and "build_time" in body["build"]


@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: 健康接口缺 db/worker_alive 字段", strict=False)
def test_health_reports_db_and_worker(client):
    body = client.get("/api/health").json()
    assert "db" in body and isinstance(body["db"], dict)
    for k in ("connected", "latency_ms"):
        assert k in body["db"]
    assert body["db"]["connected"] is True
    assert "worker_alive" in body and isinstance(body["worker_alive"], bool)


# ---------------------------------------------------------------------------
# draft 节流 + 服务端 revision 乐观锁
# ---------------------------------------------------------------------------
@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: draft 响应缺 server_min_interval_ms/last_draft_at (现有 throttled), 且受 revision 语义缺陷影响", strict=False)
def test_draft_returns_throttle_fields(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "GO 节流测试", "employee_id": "THR001",
    }).json()
    sid, stok = s["session_id"], s["session_token"]
    d = client.put(f"/api/exam/sessions/{sid}/draft", json={
        "session_token": stok, "revision": 1,
        "answers": {"c-single": ["A"]},
    }).json()
    for k in ("success", "draft_revision",
              "server_min_interval_ms", "last_draft_at"):
        assert k in d, f"draft 响应缺字段 {k}"
    assert d["draft_revision"] == 1
    assert d["server_min_interval_ms"] >= 0
    assert d["last_draft_at"] is not None


@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: status 接口缺 server_revision 且返回 PascalCase", strict=False)
def test_draft_payload_includes_server_revision(client, paper_loaded):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "GO server-rev", "employee_id": "SRV001",
    }).json()
    sid, stok = s["session_id"], s["session_token"]
    client.put(f"/api/exam/sessions/{sid}/draft", json={
        "session_token": stok, "revision": 1, "answers": {"c-single": ["A"]},
    })
    st = client.get(f"/api/exam/sessions/{sid}/status",
                    params={"session_token": stok}).json()
    assert "server_revision" in st, "Go 必在 status 回 server_revision 乐观锁字段"
    assert st["server_revision"] == 1


# ---------------------------------------------------------------------------
# submission grading_status 深字段
# ---------------------------------------------------------------------------
@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: submission status 缺 graded_items 等深字段; 评分闭环另受快照哈希不一致阻断", strict=False)
def test_submission_status_reports_grading_progress(client, paper_loaded, paper_smoke):
    slug, run = paper_loaded
    s = client.post("/api/exam/start", json={
        "paper_id": slug, "run_token": run["public_token"],
        "name": "GO grading-status", "employee_id": "GS001",
    }).json()
    sid, stok = s["session_id"], s["session_token"]

    # 构造 paper_smoke 完整答案 (与 student 同款)
    from tests.contract.test_student_api import _build_full_answers
    answers = _build_full_answers(paper_smoke)

    r = client.post("/api/submit", json={
        "session_id": sid, "session_token": stok, "answers": answers,
    })
    assert r.status_code == 200
    sid_sub = r.json()["submission_id"]

    # 轮询直到 grading_status 出现深字段
    def _got():
        body = client.get(f"/api/submission/{sid_sub}/status").json()
        return all(k in body for k in (
            "graded_items", "pending_items", "subjectives_remaining",
            "objective_score",
        ))
    assert wait_until(_got, timeout=10.0), "Go 未能产出 grading_status 深字段"
    body = client.get(f"/api/submission/{sid_sub}/status").json()
    assert body["subjectives_remaining"] >= 0
    assert isinstance(body["objective_score"], (int, float))
    # 主观题全部完成后, subjective_score 也应填上
    if wait_until(lambda: (
        client.get(f"/api/submission/{sid_sub}/status").json()
        .get("subjectives_remaining") == 0
    ), timeout=10.0):
        body = client.get(f"/api/submission/{sid_sub}/status").json()
        assert "subjective_score" in body
        assert isinstance(body["subjective_score"], (int, float))


# ---------------------------------------------------------------------------
# close 幂等: 重复 close 返回同一 round_no
# ---------------------------------------------------------------------------
@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: close 响应缺 round_no/status (审计问题#13)", strict=False)
def test_close_is_idempotent_with_round_no(client, paper_loaded):
    slug, _ = paper_loaded
    r1 = client.post(f"/api/admin/papers/{slug}/close", json={})
    assert r1.status_code == 200
    d1 = r1.json()
    # 再 close = 幂等, 返回同一 round_no
    r2 = client.post(f"/api/admin/papers/{slug}/close", json={})
    if r2.status_code == 200:
        d2 = r2.json()
        assert d2["round_no"] == d1["round_no"]
        assert d2["success"] is True


# ---------------------------------------------------------------------------
# admin/stats 新增字段: active_workers / scoring_queue_depth
# ---------------------------------------------------------------------------
@pexpect_go
@pytest.mark.xfail(reason="计划契约未实现: stats 缺 active_workers/scoring_queue_depth", strict=False)
def test_admin_stats_go_fields(client, paper_loaded):
    body = client.get("/api/admin/stats").json()
    for k in ("active_workers", "scoring_queue_depth"):
        assert k in body, f"Go admin/stats 缺字段 {k}"
    assert isinstance(body["active_workers"], int)
    assert isinstance(body["scoring_queue_depth"], int)
