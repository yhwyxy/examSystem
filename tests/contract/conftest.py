"""Task 0 契约测试共享夹具。

设计目标:
1. 同时支持两种被测对象 —— Python TestClient 基线 与 Go HTTP 黑盒 URL；
2. tmp 目录隔离 papers / sqlite / exam_runs, 不污染工作区 data/;
3. 注入确定性 fake scorer, 不依赖真实 LLM 服务;
4. 固定题库 fixtures 来自 tests/fixtures/contract/。

环境变量:
- EXAM_CONTRACT_BASE_URL: 指向 Go 服务 (http://127.0.0.1:8000); 不设则用 Python TestClient
- EXAM_CONTRACT_EXPECT_GO=1: 测试里 Go 专属增强字段(grading_status / draft 节流 / health 等)断言才开启
- EXAM_FAKE_WORKER_DONE_TOPIC / EXAM_FAKE_WORKER_RESULT_* : 给 fake_worker_entry.py 用, conftest 默认不依赖
"""
from __future__ import annotations

import json
import os
import threading
import time
from pathlib import Path
from typing import Any, Callable, Iterator

import pytest

# 让 tests/contract 可导入顶层 backend 包
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

# 旧 Python backend 已从仓库删除; 本套件的夹具(python_env/paper_loaded)仍依赖它做
# 双跑基准, 待改造为"经 Go admin API 注入数据"的纯黑盒模式后恢复运行。
try:
    import backend  # noqa: F401
except ImportError:
    pytest.skip(
        "legacy Python backend removed; contract suite pending Go-only refactor",
        allow_module_level=True,
    )

FIXTURES_DIR = Path(__file__).resolve().parents[1] / "fixtures" / "contract"
GO_BASE_URL = os.environ.get("EXAM_CONTRACT_BASE_URL", "").rstrip("/")
EXPECT_GO = os.environ.get("EXAM_CONTRACT_EXPECT_GO", "0") == "1"


# ---------------------------------------------------------------------------
# Fake Scoring Service —— 确定性主观题评分, 不调真实模型
# ---------------------------------------------------------------------------
def _build_fake_result(req: Any) -> Any:
    """按学生答案长度 / 参考答案长度比例, 构造确定性 ScoringResult。

    使用真实 SubjectiveScoringService 同接口的 ScoringResult 实例, 字段严格匹配:
    question_id / score / max_score / scoring_mode / track / confidence /
    need_manual_review / review_level, 其余用默认值。
    """
    from subjective_scoring import ScoringResult, ScoringMode, ReviewLevel

    max_score = float(getattr(req, "max_score", 0) or 0)
    reference = str(getattr(req, "reference_answer", "") or "")
    student = str(getattr(req, "student_answer", "") or "")
    ratio = (len(student) / len(reference)) if reference else 0.0
    score_ratio = max(0.0, min(1.0, ratio))
    score = round(max_score * score_ratio, 2)

    # mode 透传请求里的 (可能为 None); 给个默认 TEXT
    mode = getattr(req, "scoring_mode", None) or ScoringMode.TEXT

    return ScoringResult(
        question_id=str(getattr(req, "question_id", "?")),
        score=score,
        max_score=max_score,
        scoring_mode=mode,
        track="FakeSubjectiveService",
        confidence=0.95,
        need_manual_review=False,
        review_level=ReviewLevel.AUTO_PASS,
    )


class _FakeSubjectiveService:
    """确定性主观题评分: 按答案长度比例给分, 保证可重复。"""

    def __init__(self) -> None:
        self.calls: list[Any] = []
        self._lock = threading.Lock()

    # 与真实 SubjectiveScoringService 同接口: .score(req) -> result
    def score(self, req: Any) -> Any:
        with self._lock:
            self.calls.append(req)
        return _build_fake_result(req)

    def close(self) -> None:  # noqa: D401
        return None


# ---------------------------------------------------------------------------
# Python 后端环境启动 (tmp 隔离)
# ---------------------------------------------------------------------------
@pytest.fixture
def python_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """初始化 Python 后端到 tmp 目录: papers/sqlite/runs + 注入 fake scorer。"""
    # 导入顺序敏感: 先改路径再 init
    from backend import question_loader as ql
    from backend import database
    from backend import exam_run_service
    from backend import grader
    from backend import config as cfg_module

    papers = tmp_path / "papers"
    papers.mkdir()
    (papers / "index.json").write_text(json.dumps({"papers": []}), encoding="utf-8")
    backups = tmp_path / "backups" / "papers"
    backups.mkdir(parents=True)
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
    # 禁用评分回调调度(测试内不依赖后台 worker), None 是 set_grading_scheduler 合法值
    exam_run_service.set_grading_scheduler(None)

    # admin 关鉴权, 契约测试更稳定
    # config.yaml 默认 enable_auth=False; 全程使用 frozen dataclass, 不在此改写

    # 注入 fake scorer
    fake = _FakeSubjectiveService()
    grader.set_subjective_service(fake)

    yield tmp_path
    # teardown: 停后台线程
    try:
        exam_run_service.stop_finalize_loop()
    except Exception:
        pass
    grader.set_subjective_service(None)


# ---------------------------------------------------------------------------
# 固定题库 fixtures -> 注入到 python_env
# ---------------------------------------------------------------------------
def _load_paper(name: str) -> dict[str, Any]:
    return json.loads((FIXTURES_DIR / name).read_text(encoding="utf-8"))


@pytest.fixture
def paper_smoke() -> dict[str, Any]:
    """paper.json —— 含全题型 + 深脱敏字段的固定卷。"""
    return _load_paper("paper.json")


@pytest.fixture
def objective_cases() -> dict[str, Any]:
    return _load_paper("objective_cases.json")


@pytest.fixture
def sanitized_exam_golden() -> dict[str, Any]:
    return _load_paper("sanitized_exam.json")


@pytest.fixture
def paper_loaded(python_env: Path, paper_smoke: dict[str, Any]) -> str:
    """把固定卷写进 tmp papers 目录, 返回 slug。"""
    from backend import paper_store
    from backend import question_loader as ql
    ql.clear_question_cache()
    slug = paper_smoke["paper_id"]
    paper_store.create_paper(slug=slug, name=paper_smoke["name"])
    paper_store.save_paper(slug, paper_smoke)
    # 初始化一个开放 run
    from backend import exam_run_service
    run = exam_run_service.open_run(slug)
    yield slug, run


@pytest.fixture(autouse=True)
def _disable_rate_limit(monkeypatch: pytest.MonkeyPatch):
    """契约测试全程关闭速率限制, 否则 wait_until 轮询会撞 60/窗口触发 429。"""
    from backend import main as main_module
    monkeypatch.setattr(main_module, "_check_rate_limit",
                       lambda *a, **k: None)
    with main_module._rate_lock:
        main_module._rate_store.clear()
    yield
    with main_module._rate_lock:
        main_module._rate_store.clear()


# ---------------------------------------------------------------------------
# HTTP 客户端 —— 双跑核心
# ---------------------------------------------------------------------------
class _GoHTTPClient:
    """对 Go 服务发真实 HTTP 请求的黑盒客户端 (基于 httpx, 与 TestClient 同栈)。"""

    def __init__(self, base_url: str) -> None:
        import httpx
        self._base = base_url

    def _url(self, path: str) -> str:
        return f"{self._base}{path if path.startswith('/') else '/' + path}"

    def _client(self) -> Any:
        import httpx
        return httpx.Client(base_url=self._base, timeout=10.0)

    def _do(self, method: str, path: str, json_body: Any | None, **kw: Any) -> Any:
        import httpx
        # 兼容 kw 与显式 json_body 同时存在 (旧 fixture bug: 同时 json=json_body + **kw
        # 在测试用 json=... kwargs 时撞名 "multiple values for keyword argument 'json'").
        # 优先级: 显式 json_body 覆盖 kw.json; 同时清洗 kw 内 json.
        body = json_body
        if body is None and "json" in kw:
            body = kw.pop("json")
        elif "json" in kw:
            kw.pop("json")
        with httpx.Client(base_url=self._base, timeout=10.0) as cli:
            r = cli.request(method, path, json=body, **kw)
        return _GoResponse(r)

    def get(self, path: str, **kw: Any) -> Any:
        return self._do("GET", path, None, **kw)

    def post(self, path: str, json_body: Any | None = None, **kw: Any) -> Any:
        return self._do("POST", path, json_body, **kw)

    def put(self, path: str, json_body: Any | None = None, **kw: Any) -> Any:
        return self._do("PUT", path, json_body, **kw)

    def delete(self, path: str, **kw: Any) -> Any:
        return self._do("DELETE", path, None, **kw)


class _GoResponse:
    def __init__(self, resp: Any) -> None:
        self.status_code = resp.status_code
        try:
            self._json = resp.json()
        except (ValueError, Exception):
            self._json = None

    def json(self) -> Any:
        return self._json

    @property
    def ok(self) -> bool:
        return 200 <= self.status_code < 300


@pytest.fixture
def client(python_env: Path) -> Iterator[Any]:
    """默认返回 Python TestClient; 设了 EXAM_CONTRACT_BASE_URL 则走 Go 黑盒。"""
    if GO_BASE_URL:
        yield _GoHTTPClient(GO_BASE_URL)
        return

    # Python 基线路径
    from fastapi.testclient import TestClient
    from backend import main as main_module
    # 重置进程级速率限制器, 测试执行前后都清, 避免跨用例 429 污染
    with main_module._rate_lock:
        main_module._rate_store.clear()
    try:
        with TestClient(main_module.app) as c:
            yield c
    finally:
        with main_module._rate_lock:
            main_module._rate_store.clear()


@pytest.fixture
def admin_headers(client: Any) -> dict[str, str]:
    """enable_auth=False 时返回空 headers。Go 路径同样标注。"""
    return {}


# ---------------------------------------------------------------------------
# 辅助: 等待 Go 服务的 grading_status 收敛 (Go 专属, 测试内部轮询)
# ---------------------------------------------------------------------------
def wait_until(predicate: Callable[[], bool], timeout: float = 5.0, interval: float = 0.05) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return False
