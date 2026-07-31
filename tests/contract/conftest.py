"""契约测试共享夹具 —— 纯 Go 黑盒模式。

设计目标:
1. 被测对象是真实运行的 Go exam-server (HTTP 黑盒), 不依赖任何 Python 后端;
2. 测试数据(试卷/run)全部经 Go admin API 注入, teardown 同样经 API 清理;
3. 数据隔离: 独立 PostgreSQL schema (contract_test, 每会话重建) + tmp data_root;
4. 固定题库 fixtures 来自 tests/fixtures/contract/。

运行方式:
- 默认: conftest 自动 `go build` 并启动 exam-server (需本机可达 PostgreSQL,
  DSN 取 EXAM_CONTRACT_DATABASE_URL, 缺省用 config.dev.yaml 的开发库);
- 或设 EXAM_CONTRACT_BASE_URL 指向已运行的 Go 服务 (跳过自启动, 数据不隔离,
  仅建议临时调试用)。

环境变量:
- EXAM_CONTRACT_BASE_URL:      外部 Go 服务地址 (可选)
- EXAM_CONTRACT_DATABASE_URL:  自启动模式的 PostgreSQL DSN (可选, 不含 search_path)
- EXAM_CONTRACT_EXPECT_GO:     Go 增强字段断言开关, 默认开 (test_go_enhancements)
"""
from __future__ import annotations

import itertools
import json
import os
import socket
import subprocess
import time
from pathlib import Path
from typing import Any, Callable, Iterator

import pytest

ROOT = Path(__file__).resolve().parents[2]
FIXTURES_DIR = Path(__file__).resolve().parents[1] / "fixtures" / "contract"

DEFAULT_DSN = "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable"
CONTRACT_SCHEMA = "contract_test"

_slug_counter = itertools.count(1)


# ---------------------------------------------------------------------------
# Go 服务生命周期 (session 级)
# ---------------------------------------------------------------------------
def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _reset_schema(dsn: str) -> None:
    """DROP + CREATE contract_test schema, 保证每个测试会话从空库开始。"""
    import psycopg

    with psycopg.connect(dsn, autocommit=True) as conn:
        conn.execute(f"DROP SCHEMA IF EXISTS {CONTRACT_SCHEMA} CASCADE")
        conn.execute(f"CREATE SCHEMA {CONTRACT_SCHEMA}")


def _schema_dsn(dsn: str) -> str:
    sep = "&" if "?" in dsn else "?"
    return f"{dsn}{sep}options=-c%20search_path%3D{CONTRACT_SCHEMA}"


@pytest.fixture(scope="session")
def go_base_url(tmp_path_factory: pytest.TempPathFactory) -> Iterator[str]:
    external = os.environ.get("EXAM_CONTRACT_BASE_URL", "").rstrip("/")
    if external:
        yield external
        return

    base_dsn = os.environ.get("EXAM_CONTRACT_DATABASE_URL", DEFAULT_DSN)
    try:
        _reset_schema(base_dsn)
    except Exception as exc:  # noqa: BLE001
        pytest.skip(f"契约测试需要可达的 PostgreSQL ({exc})", allow_module_level=False)
    dsn = _schema_dsn(base_dsn)

    work = tmp_path_factory.mktemp("go-contract")
    data_root = work / "data"
    (data_root / "papers").mkdir(parents=True)
    (data_root / "exam_runs").mkdir()
    cfg_path = work / "config.yaml"
    cfg_path.write_text(
        "\n".join(
            [
                "server:",
                '  host: "127.0.0.1"',
                f'  data_root: "{data_root}"',
                "admin:",
                "  enable_auth: false",
                # 与生产 config.yaml 对齐: 多选按命中比例给分 (无错项前提)
                "scoring:",
                "  multiple_choice_partial: true",
                "database:",
                f'  url: "{dsn}"',
                "",
            ]
        ),
        encoding="utf-8",
    )

    binary = work / "exam-server"
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/exam-server"],
        cwd=ROOT, capture_output=True, text=True, timeout=180,
    )
    assert build.returncode == 0, f"go build 失败:\n{build.stderr}"

    migrate = subprocess.run(
        [str(binary), "migrate", "--config", str(cfg_path)],
        cwd=ROOT, capture_output=True, text=True, timeout=60,
    )
    assert migrate.returncode == 0, f"migrate 失败:\n{migrate.stdout}\n{migrate.stderr}"

    port = _free_port()
    log = (work / "server.log").open("w")
    proc = subprocess.Popen(
        [str(binary), "serve", "--config", str(cfg_path),
         "--bind", f"127.0.0.1:{port}", "--static", "frontend"],
        cwd=ROOT, stdout=log, stderr=subprocess.STDOUT,
    )
    base = f"http://127.0.0.1:{port}"
    try:
        import httpx

        deadline = time.time() + 15
        last_err: Exception | None = None
        while time.time() < deadline:
            if proc.poll() is not None:
                raise AssertionError(
                    f"exam-server 提前退出:\n{(work / 'server.log').read_text()}")
            try:
                r = httpx.get(f"{base}/api/health", timeout=1.0, trust_env=False)
                if r.status_code == 200:
                    break
            except Exception as exc:  # noqa: BLE001
                last_err = exc
            time.sleep(0.1)
        else:
            raise AssertionError(f"exam-server 健康检查超时: {last_err}")
        yield base
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()


# ---------------------------------------------------------------------------
# HTTP 客户端 (黑盒; trust_env=False 隔离本机代理设置)
# ---------------------------------------------------------------------------
class _GoResponse:
    def __init__(self, resp: Any) -> None:
        self.status_code = resp.status_code
        self.text = resp.text
        try:
            self._json = resp.json()
        except ValueError:
            self._json = None

    def json(self) -> Any:
        return self._json

    @property
    def ok(self) -> bool:
        return 200 <= self.status_code < 300


class _GoHTTPClient:
    def __init__(self, base_url: str) -> None:
        import httpx

        self._cli = httpx.Client(base_url=base_url, timeout=10.0, trust_env=False)

    def _do(self, method: str, path: str, **kw: Any) -> _GoResponse:
        return _GoResponse(self._cli.request(method, path, **kw))

    def get(self, path: str, **kw: Any) -> _GoResponse:
        return self._do("GET", path, **kw)

    def post(self, path: str, **kw: Any) -> _GoResponse:
        return self._do("POST", path, **kw)

    def put(self, path: str, **kw: Any) -> _GoResponse:
        return self._do("PUT", path, **kw)

    def patch(self, path: str, **kw: Any) -> _GoResponse:
        return self._do("PATCH", path, **kw)

    def delete(self, path: str, **kw: Any) -> _GoResponse:
        return self._do("DELETE", path, **kw)

    def close(self) -> None:
        self._cli.close()


@pytest.fixture
def client(go_base_url: str) -> Iterator[_GoHTTPClient]:
    c = _GoHTTPClient(go_base_url)
    yield c
    c.close()


@pytest.fixture
def admin_headers() -> dict[str, str]:
    """enable_auth=false, 管理端免鉴权。"""
    return {}


# ---------------------------------------------------------------------------
# 固定题库 fixtures -> 经 Go admin API 注入
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
def paper_loaded(client: _GoHTTPClient, paper_smoke: dict[str, Any]
                 ) -> Iterator[tuple[str, dict[str, Any]]]:
    """经 admin API 上传固定卷并开考, 返回 (slug, run)。

    每个测试用唯一 slug, 避免 run 状态跨用例串扰; teardown 关考 + 删卷。
    """
    slug = f"go-contract-p{next(_slug_counter)}"
    doc = dict(paper_smoke)
    doc["paper_id"] = slug

    r = client.put(f"/api/admin/papers/{slug}", json=doc)
    assert r.ok, f"上传试卷失败 [{r.status_code}]: {r.text}"

    r = client.post(f"/api/admin/papers/{slug}/open", json={})
    assert r.ok, f"开考失败 [{r.status_code}]: {r.text}"
    opened = r.json()
    run = {
        "id": opened.get("run_id"),
        "paper_id": slug,
        "round_no": opened.get("round_no"),
        "public_token": opened.get("public_token"),
    }
    assert run["public_token"], f"open 响应缺 public_token: {opened}"

    yield slug, run

    client.post(f"/api/admin/papers/{slug}/close", json={})
    client.delete(f"/api/admin/papers/{slug}")


# ---------------------------------------------------------------------------
# 辅助: 轮询等待
# ---------------------------------------------------------------------------
def wait_until(predicate: Callable[[], bool], timeout: float = 5.0,
               interval: float = 0.05) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return False
