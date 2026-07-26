#!/usr/bin/env python3
"""Task 13 k6 容量测试 prepare.py (路径 C: 直连 PG 绕 admin openRun stub).

产物 (全部不入 git - 见 .gitignore):
- loadtest/frontend/{start,draft,submit}.json - 三份脱敏 paper (admin savePaper API 写)
- loadtest/scenarios/{start-users,draft-sessions,submit-sessions}.json - k6 注入数据
- 直插 loadtest.exam_runs 3 行 (admin openRun stub 不可用, 直连绕)

启动顺序: 先 migrate + serve 起来, 再跑 prepare.py.
"""
from __future__ import annotations
import hashlib, json, os, secrets, sys, time
from pathlib import Path
import urllib.request

BASE_URL = os.environ.get("EXAM_LT_BASE_URL", "http://127.0.0.1:18080")
STATIC_ROOT = Path(os.environ.get("EXAM_LT_STATIC", "loadtest/frontend"))
SCENARIO_DIR = Path(os.environ.get("EXAM_LT_SCENARIOS", "loadtest/scenarios"))
DB_URL = os.environ["EXAM_LT_DB_URL"]   # search_path=loadtest DSN

def _post(path: str, body: dict) -> dict:
    data = json.dumps(body).encode()
    req = urllib.request.Request(f"{BASE_URL}{path}", data=data, method="POST",
                                  headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read())

def _sha256_hex(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()

def _build_paper(slug: str, n_questions: int = 10) -> dict:
    """最小脱敏 paper JSON (无敏感内容; 全客观题 + 1 主观题)."""
    qs = []
    for i in range(1, n_questions + 1):
        if i <= n_questions - 1:
            qs.append({
                "id": f"q{i}", "type": "single_choice", "score": 1,
                "question": f"loadtest-{slug} 选答 {i}",
                "options": [{"key": "A", "text": "aa"}, {"key": "B", "text": "bb"},
                            {"key": "C", "text": "cc"}, {"key": "D", "text": "dd"}],
                "answer": "A",
            })
        else:
            qs.append({
                "id": f"q{n_questions}", "type": "short_answer", "score": 10,
                "question": f"loadtest-{slug} 简答", "answer": "标准答案",
                "scoring_rubric": "命中关键词:5", "scoring_mode": "text",
            })
    return {"version": 1, "paper_id": slug, "name": slug, "title": slug,
            "duration_minutes": 60,
            "questions": qs, "allow_duplicate_submit": True}

def _save_paper(slug: str, paper: dict) -> str:
    """走 admin POST /api/admin/papers/{slug} (handler 非 stub); 返回 sha256(paper_json)."""
    _post(f"/api/admin/papers/{slug}", paper)
    blob = json.dumps(paper, sort_keys=True, ensure_ascii=False).encode()
    return _sha256_hex(blob)

def _insert_run(paper_id: str, paper_sha: str, duration_min: int = 60) -> str:
    """直插 loadtest.exam_runs 一行, 绕过 admin openRun stub.
    run_token 明文返回; DB 存 sha256(token) hex (与 start API hashRunToken 一致).
    真实列名: paper_id (非 paper_slug) / opened_at (非 created_at).
    约束: is_legacy=false 时 public_token_hash + snapshot_path + snapshot_hash 都 NOT NULL."""
    import psycopg
    token = f"lt-{paper_id}-{secrets.token_hex(8)}"
    token_hash = _sha256_hex(token.encode())
    run_id = f"run-{paper_id}-{secrets.token_hex(4)}"
    snapshot_path = f"{paper_id}.json"   # LoadSnapshot 用 root + slug.json, path 仅作记录
    with psycopg.connect(DB_URL) as conn, conn.cursor() as cur:
        cur.execute("""
            INSERT INTO exam_runs (id, paper_id, round_no, public_token_hash,
                                   status, duration_minutes,
                                   snapshot_path, snapshot_hash, is_legacy,
                                   opened_at, created_at)
            VALUES (%s, %s, 1, %s, 'open', %s, %s, %s, false, now(), now())
        """, (run_id, paper_id, token_hash, duration_min, snapshot_path, paper_sha))
        conn.commit()
    print(f"  run inserted: paper_id={paper_id} run_id={run_id} token={token[:32]}...")
    return token

def _make_users(n: int = None) -> list[dict]:
    if n is None:
        n = int(os.environ.get("EXAM_LT_USERS", "500"))
    return [{"employee_id": f"lt-u{i:04d}", "name": f"员工{i:04d}",
             "department": "loadtest"} for i in range(1, n + 1)]

def _start_session(run_token: str, user: dict) -> dict | None:
    """调 start API 建一个 session; 返回 {session_id, session_token, user}."""
    try:
        r = _post("/api/exam/start",
                  {"run_token": run_token,
                   "employee_id": user["employee_id"],
                   "name": user["name"], "department": user["department"]})
        return {"session_id": r.get("session_id"),
                "session_token": r.get("session_token"),
                "employee_id": user["employee_id"]}
    except Exception as e:
        print(f"  start FAIL user={user['employee_id']}: {e}", file=sys.stderr)
        return None

def _gen_answers(paper: dict) -> dict:
    """从 paper 生成全 A 的草稿答案 (任意合法 JSON)."""
    return {q["id"]: {"key": "A"} if q["type"] == "single_choice"
            else {"text": "答 loadtest"} for q in paper["questions"]}

def prepare_start_users(users: list[dict]) -> list[dict]:
    """start_peak: 仅给 run_token + user, 不预 start (plan 行 1739)."""
    return users   # k6 脚本自带 run_token, users 就是注入数据

def prepare_draft_sessions(run_token: str, users: list[dict],
                           paper: dict, out_path: Path) -> list[dict]:
    """draft_steady: 预 start + 写 draft-sessions.json.
    each entry: {session_id, session_token, employee_id, answers, revision}."""
    out = []
    for u in users:
        s = _start_session(run_token, u)
        if s and s.get("session_token"):
            out.append({**s, "answers": _gen_answers(paper), "revision": 1})
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(out, ensure_ascii=False))
    print(f"  draft-sessions: wrote {len(out)} -> {out_path}")
    return out

def prepare_submit_sessions(run_token: str, users: list[dict],
                            paper: dict, out_path: Path) -> list[dict]:
    """submit_peak: 预 start + 写 submit-sessions.json.
    each entry: {session_id, session_token, employee_id, answers}."""
    out = []
    for u in users:
        s = _start_session(run_token, u)
        if s and s.get("session_token"):
            out.append({**s, "answers": _gen_answers(paper)})
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(out, ensure_ascii=False))
    print(f"  submit-sessions: wrote {len(out)} -> {out_path}")
    return out

def main() -> int:
    SCENARIO_DIR.mkdir(parents=True, exist_ok=True)
    STATIC_ROOT.mkdir(parents=True, exist_ok=True)
    users = _make_users(500)
    papers, tokens = {}, {}
    for slug in ("loadtest-start", "loadtest-draft", "loadtest-submit"):
        paper = _build_paper(slug)
        sha = _save_paper(slug, paper)
        tok = _insert_run(slug, sha)
        papers[slug], tokens[slug] = paper, tok
        print(f"  paper {slug}: ok (sha={sha[:12]}..., tok={tok[:24]}...)")
    # 1) start-users.json - 不预 start, k6 自己调 start
    (SCENARIO_DIR / "start-users.json").write_text(
        json.dumps({"run_token": tokens["loadtest-start"], "users": users},
                   ensure_ascii=False))
    print(f"  start-users.json: {len(users)} users")
    # 2) draft-sessions.json - 预 start 500 (耗时, 容错)
    ds = prepare_draft_sessions(tokens["loadtest-draft"], users,
                                papers["loadtest-draft"],
                                SCENARIO_DIR / "draft-sessions.json")
    # 3) submit-sessions.json - 预 start 500 (耗时, 容错)
    prepare_submit_sessions(tokens["loadtest-submit"], users,
                           papers["loadtest-submit"],
                           SCENARIO_DIR / "submit-sessions.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
