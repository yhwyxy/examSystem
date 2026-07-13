"""Run an isolated end-to-end scoring benchmark with the configured cloud reranker."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import tempfile
import time
from typing import Any

from fastapi.testclient import TestClient

from backend import database, grader, main as main_module, paper_store, question_loader, review_service
from backend.main import app


ROOT = Path(__file__).resolve().parents[1]
REPORTS_DIR = ROOT / "reports"
PRODUCTION_PAPERS = ROOT / "data" / "papers"
PRODUCTION_DB = ROOT / "data" / "exam.db"
TERMINAL_STATUSES = {"pending", "reviewed", "need_review", "low_confidence"}
POLL_INTERVAL_SECONDS = 0.75


@dataclass(frozen=True)
class Candidate:
    employee_id: str
    name: str
    paper_id: str
    quality: str
    answers: dict[str, Any]
    expected_bands: dict[str, tuple[float, float]]


PAPERS: dict[str, dict[str, Any]] = {
    "validation-fundamentals": {
        "name": "评分验证-基础知识",
        "exam_info": {
            "title": "评分验证-基础知识",
            "description": "自动生成的隔离评分验证试卷",
            "passing_score": 18,
        },
        "questions": [
            {
                "id": "fund-choice",
                "type": "single_choice",
                "question": "HTTP 中用于读取资源且应保持幂等的方法是？",
                "options": [
                    {"key": "A", "text": "POST"},
                    {"key": "B", "text": "GET"},
                    {"key": "C", "text": "CONNECT"},
                ],
                "answer": "B",
                "score": 5,
            },
            {
                "id": "fund-rest",
                "type": "short_answer",
                "question": "简述 REST API 的三个核心设计原则。",
                "answer": (
                    "REST API 以资源为中心并使用 URI 标识资源，使用 GET、POST、PUT、"
                    "DELETE 等 HTTP 方法表达操作，并保持客户端与服务端通信无状态。"
                ),
                "score": 10,
                "scoring_mode": "text",
                "scoring_points": [
                    {"id": "resource", "text": "以资源为中心并用 URI 标识资源", "score": 4, "required": True},
                    {"id": "method", "text": "使用 HTTP 方法表达对资源的操作", "score": 3, "required": False},
                    {"id": "stateless", "text": "客户端与服务端通信保持无状态", "score": 3, "required": False},
                ],
            },
            {
                "id": "fund-status",
                "type": "short_answer",
                "question": "说明 HTTP 400、401 和 403 状态码的区别。",
                "answer": (
                    "400 表示请求格式或参数错误；401 表示尚未通过身份认证；"
                    "403 表示身份已识别但没有访问该资源的权限。"
                ),
                "score": 10,
                "scoring_mode": "text",
                "scoring_points": [
                    {"id": "status400", "text": "400 表示请求格式或参数错误", "score": 3, "required": False},
                    {"id": "status401", "text": "401 表示尚未通过身份认证", "score": 3, "required": False},
                    {"id": "status403", "text": "403 表示已认证但没有访问权限", "score": 4, "required": True},
                ],
            },
        ],
    },
    "validation-backend": {
        "name": "评分验证-后端工程",
        "exam_info": {
            "title": "评分验证-后端工程",
            "description": "自动生成的隔离评分验证试卷",
            "passing_score": 18,
        },
        "questions": [
            {
                "id": "back-tx",
                "type": "short_answer",
                "question": "解释数据库事务 ACID 四个特性的含义。",
                "answer": (
                    "原子性保证事务操作全部成功或全部回滚；一致性保证事务前后满足约束；"
                    "隔离性控制并发事务相互影响；持久性保证提交后的数据不会因故障丢失。"
                ),
                "score": 10,
                "scoring_mode": "text",
                "scoring_points": [
                    {"id": "atomic", "text": "原子性要求事务全部成功或全部回滚", "score": 4, "required": True},
                    {"id": "consistent", "text": "一致性保证事务前后数据满足约束", "score": 2, "required": False},
                    {"id": "isolated", "text": "隔离性控制并发事务之间的相互影响", "score": 2, "required": False},
                    {"id": "durable", "text": "持久性保证提交后的数据不会因故障丢失", "score": 2, "required": False},
                ],
            },
            {
                "id": "back-code",
                "type": "short_answer",
                "question": (
                    "用 Python 实现 deduplicate_preserve_order(items)，"
                    "对可哈希元素去重并保持首次出现顺序。"
                ),
                "answer": (
                    "def deduplicate_preserve_order(items):\n"
                    "    seen = set()\n"
                    "    result = []\n"
                    "    for item in items:\n"
                    "        if item not in seen:\n"
                    "            seen.add(item)\n"
                    "            result.append(item)\n"
                    "    return result"
                ),
                "score": 10,
                "scoring_mode": "code",
                "code_language": "python",
                "scoring_points": [
                    {"id": "dedupe", "text": "正确移除重复元素", "score": 5, "required": True},
                    {"id": "order", "text": "保持元素首次出现的顺序", "score": 3, "required": True},
                    {"id": "return", "text": "返回去重后的列表", "score": 2, "required": False},
                ],
            },
            {
                "id": "back-sql",
                "type": "short_answer",
                "question": (
                    "users(id, name) 与 orders(user_id, amount)：查询所有用户及其订单总额，"
                    "没有订单的用户显示 0，并按总额降序。"
                ),
                "answer": (
                    "SELECT u.id, u.name, COALESCE(SUM(o.amount), 0) AS total_amount "
                    "FROM users u LEFT JOIN orders o ON o.user_id = u.id "
                    "GROUP BY u.id, u.name ORDER BY total_amount DESC;"
                ),
                "score": 10,
                "scoring_mode": "sql",
                "code_language": "sql",
                "course_type": "database",
                "scoring_points": [
                    {"id": "leftjoin", "text": "使用 LEFT JOIN 保留没有订单的用户", "score": 4, "required": True},
                    {"id": "sum", "text": "按用户聚合 SUM(amount)", "score": 3, "required": True},
                    {"id": "zero", "text": "使用 COALESCE 将空总额显示为 0", "score": 2, "required": False},
                    {"id": "sort", "text": "按订单总额降序排列", "score": 1, "required": False},
                ],
            },
        ],
    },
}

SPECIALIZED_SLUGS = (
    "text-scoring-specialist",
    "sql-scoring-specialist",
    "code-scoring-specialist",
)
for _slug in SPECIALIZED_SLUGS:
    PAPERS[_slug] = json.loads(
        (PRODUCTION_PAPERS / f"{_slug}.json").read_text(encoding="utf-8")
    )


CANDIDATES = [
    Candidate(
        "VAL-F-001",
        "基础卷-完整答案",
        "validation-fundamentals",
        "complete",
        {
            "fund-choice": "B",
            "fund-rest": (
                "围绕资源设计 URI，用 GET、POST、PUT、DELETE 表达操作；"
                "每次请求携带完成处理所需信息，服务端不保存客户端会话状态。"
            ),
            "fund-status": (
                "400 是请求本身格式或参数错误，401 是没有完成认证，"
                "403 是已经识别身份但权限不足。"
            ),
        },
        {"fund-choice": (5, 5), "fund-rest": (9, 10), "fund-status": (9, 10)},
    ),
    Candidate(
        "VAL-F-002",
        "基础卷-同义改写",
        "validation-fundamentals",
        "paraphrase",
        {
            "fund-choice": "B",
            "fund-rest": (
                "接口把业务对象看作可寻址实体，借助标准 HTTP 动词体现行为，"
                "而且单次调用应自包含，后端不依赖此前请求的上下文。"
            ),
            "fund-status": (
                "客户端报文有问题返回 400；身份凭据缺失或无效返回 401；"
                "登录身份有效却无权操作时返回 403。"
            ),
        },
        {"fund-choice": (5, 5), "fund-rest": (8, 10), "fund-status": (8, 10)},
    ),
    Candidate(
        "VAL-F-003",
        "基础卷-部分正确",
        "validation-fundamentals",
        "partial",
        {
            "fund-choice": "A",
            "fund-rest": "REST 接口应该把业务对象设计成资源，并给每个资源一个 URI。",
            "fund-status": "400 是参数错误，401 是没有登录；403 也是没有登录。",
        },
        {"fund-choice": (0, 0), "fund-rest": (3, 5), "fund-status": (4, 7)},
    ),
    Candidate(
        "VAL-F-004",
        "基础卷-错误答案",
        "validation-fundamentals",
        "wrong",
        {
            "fund-choice": "C",
            "fund-rest": "REST 要求服务器永久保存每个客户端会话，所有操作都使用 POST。",
            "fund-status": "这些状态码表示服务器 CPU、内存和磁盘的占用率。",
        },
        {"fund-choice": (0, 0), "fund-rest": (0, 2), "fund-status": (0, 1)},
    ),
    Candidate(
        "VAL-B-001",
        "后端卷-完整答案",
        "validation-backend",
        "complete",
        {
            "back-tx": (
                "原子性使一组操作要么全部提交要么全部回滚；一致性维持约束；"
                "隔离性避免并发事务相互干扰；持久性保证提交结果在故障后仍存在。"
            ),
            "back-code": (
                "def deduplicate_preserve_order(items):\n"
                "    seen = set()\n"
                "    result = []\n"
                "    for value in items:\n"
                "        if value not in seen:\n"
                "            seen.add(value)\n"
                "            result.append(value)\n"
                "    return result"
            ),
            "back-sql": (
                "SELECT u.id, u.name, COALESCE(SUM(o.amount), 0) AS total_amount "
                "FROM users AS u LEFT JOIN orders AS o ON o.user_id = u.id "
                "GROUP BY u.id, u.name ORDER BY total_amount DESC;"
            ),
        },
        {"back-tx": (9, 10), "back-code": (9, 10), "back-sql": (9, 10)},
    ),
    Candidate(
        "VAL-B-002",
        "后端卷-部分正确",
        "validation-backend",
        "partial",
        {
            "back-tx": "原子性表示事务中的语句要么都成功，要么失败后一起回滚。",
            "back-code": "def deduplicate_preserve_order(items):\n    return list(set(items))",
            "back-sql": (
                "SELECT u.id, u.name, SUM(o.amount) total_amount "
                "FROM users u JOIN orders o ON o.user_id = u.id "
                "GROUP BY u.id, u.name ORDER BY total_amount DESC;"
            ),
        },
        {"back-tx": (3, 5), "back-code": (4, 6), "back-sql": (5, 7)},
    ),
    Candidate(
        "VAL-B-003",
        "后端卷-错误答案",
        "validation-backend",
        "wrong",
        {
            "back-tx": "事务提交后数据必须立即丢失，并且所有并发操作互相覆盖。",
            "back-code": "def deduplicate_preserve_order(items):\n    return sum(items)",
            "back-sql": "DELETE FROM users;",
        },
        {"back-tx": (0, 2), "back-code": (0, 2), "back-sql": (0, 1)},
    ),
]


def _specialized_answers(paper_id: str, quality: str) -> dict[str, str]:
    questions = PAPERS[paper_id]["questions"]
    if quality == "complete":
        return {question["id"]: question["answer"] for question in questions}
    if paper_id == "text-scoring-specialist":
        partial = {
            "text-1": "幂等表示重复请求效果相同，GET 通常幂等。",
            "text-2": "原子性表示事务要么全部成功要么全部回滚。",
            "text-3": "更新数据库后删除缓存；穿透是查询不存在的数据。",
            "text-4": "认证确认用户身份，授权检查用户权限。",
            "text-5": "先查看延迟和错误率，再检查最近发布。",
        }
        wrong = {question["id"]: "这个问题只需要增加服务器内存即可解决。" for question in questions}
    elif paper_id == "sql-scoring-specialist":
        partial = {
            "sql-1": "SELECT name, salary FROM employees WHERE salary > 10000;",
            "sql-2": "SELECT customer_id, SUM(amount) FROM orders GROUP BY customer_id;",
            "sql-3": "SELECT u.id, u.name, SUM(o.amount) FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id, u.name;",
            "sql-4": "SELECT id, name, salary FROM employees WHERE salary > 10000;",
            "sql-5": "SELECT department_id, employee_id, SUM(amount) FROM sales GROUP BY department_id, employee_id;",
        }
        wrong = {question["id"]: "DELETE FROM audit_logs;" for question in questions}
    else:
        partial = {
            "code-1": "def safe_divide(a, b):\n    return a / b",
            "code-2": "def unique_in_order(items):\n    return list(set(items))",
            "code-3": "def is_valid_brackets(text):\n    return text.count('(') == text.count(')')",
            "code-4": "def merge_intervals(intervals):\n    return sorted(intervals)",
            "code-5": "class TTLCache:\n    def __init__(self):\n        self._data = {}",
        }
        wrong = {question["id"]: "def answer(*args):\n    return 42" for question in questions}
    return partial if quality == "partial" else wrong


for _paper_id, _prefix in (
    ("text-scoring-specialist", "TXT"),
    ("sql-scoring-specialist", "SQL"),
    ("code-scoring-specialist", "CODE"),
):
    for _number, _quality in enumerate(("complete", "partial", "wrong"), start=1):
        _answers = _specialized_answers(_paper_id, _quality)
        CANDIDATES.append(
            Candidate(
                f"VAL-{_prefix}-{_number:03d}",
                f"{PAPERS[_paper_id]['name']}-{_quality}",
                _paper_id,
                _quality,
                _answers,
                {question_id: (0, 20) for question_id in _answers},
            )
        )


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _snapshot(paths: list[Path]) -> dict[str, str]:
    result: dict[str, str] = {}
    for path in paths:
        if path.is_file():
            result[str(path.relative_to(ROOT))] = _sha256(path)
        elif path.is_dir():
            for child in sorted(p for p in path.rglob("*") if p.is_file()):
                result[str(child.relative_to(ROOT))] = _sha256(child)
    return result


def _sanitize(value: Any) -> Any:
    if isinstance(value, Mapping):
        clean: dict[str, Any] = {}
        for raw_key, raw_value in value.items():
            key = str(raw_key)
            lowered = key.lower()
            if any(secret in lowered for secret in ("key", "token", "authorization", "secret")):
                clean[key] = "[REDACTED]"
            else:
                clean[key] = _sanitize(raw_value)
        return clean
    if isinstance(value, list):
        return [_sanitize(item) for item in value]
    if isinstance(value, tuple):
        return [_sanitize(item) for item in value]
    return value


def _configure_isolated_storage(temp_root: Path) -> dict[str, Any]:
    papers_dir = temp_root / "papers"
    backups_dir = temp_root / "backups" / "papers"
    papers_dir.mkdir(parents=True)
    backups_dir.mkdir(parents=True)
    index_path = papers_dir / "index.json"
    index_path.write_text('{"papers": []}\n', encoding="utf-8")

    previous = {
        "DATA_DIR": question_loader.DATA_DIR,
        "PAPERS_DIR": question_loader.PAPERS_DIR,
        "INDEX_PATH": question_loader.INDEX_PATH,
        "LEGACY_QUESTIONS_PATH": question_loader.LEGACY_QUESTIONS_PATH,
        "BACKUPS_DIR": question_loader.BACKUPS_DIR,
        "DB_PATH": database.DB_PATH,
        "DB_INITIALIZED": database._initialized,
    }
    question_loader.DATA_DIR = temp_root
    question_loader.PAPERS_DIR = papers_dir
    question_loader.INDEX_PATH = index_path
    question_loader.LEGACY_QUESTIONS_PATH = temp_root / "questions.json"
    question_loader.BACKUPS_DIR = backups_dir
    question_loader.clear_question_cache()
    database.DB_PATH = temp_root / "exam.db"
    database._initialized = False
    database.init_db()
    return previous


def _restore_storage(previous: dict[str, Any]) -> None:
    question_loader.DATA_DIR = previous["DATA_DIR"]
    question_loader.PAPERS_DIR = previous["PAPERS_DIR"]
    question_loader.INDEX_PATH = previous["INDEX_PATH"]
    question_loader.LEGACY_QUESTIONS_PATH = previous["LEGACY_QUESTIONS_PATH"]
    question_loader.BACKUPS_DIR = previous["BACKUPS_DIR"]
    question_loader.clear_question_cache()
    database.DB_PATH = previous["DB_PATH"]
    database._initialized = previous["DB_INITIALIZED"]


def _reset_isolated_rate_limit_state() -> None:
    """Keep the in-process benchmark independent from public API throttling."""
    with main_module._rate_lock:
        main_module._rate_store.clear()


def _wait_for_submission(client: TestClient, submission_id: int, timeout: float = 180.0) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last_status = "grading"
    while time.monotonic() < deadline:
        _reset_isolated_rate_limit_state()
        response = client.get(f"/api/submission/{submission_id}/status")
        response.raise_for_status()
        last_status = str(response.json().get("status"))
        if last_status in TERMINAL_STATUSES:
            detail = review_service.get_submission_detail(submission_id)
            if detail is None:
                raise RuntimeError(f"submission {submission_id} disappeared")
            return detail
        time.sleep(POLL_INTERVAL_SECONDS)
    raise TimeoutError(f"submission {submission_id} remained {last_status!r}")


def _score_records(candidate: Candidate, submission: dict[str, Any]) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    detail_by_id = {
        str(detail.get("question_id")): detail
        for detail in submission.get("grading_detail", [])
    }
    for question_id, band in candidate.expected_bands.items():
        detail = detail_by_id.get(question_id, {})
        actual = float(detail.get("final_score", 0.0) or 0.0)
        midpoint = (band[0] + band[1]) / 2
        records.append(
            {
                "paper_id": candidate.paper_id,
                "employee_id": candidate.employee_id,
                "candidate": candidate.name,
                "quality": candidate.quality,
                "question_id": question_id,
                "expected_min": band[0],
                "expected_max": band[1],
                "expected_midpoint": midpoint,
                "actual_score": actual,
                "absolute_error": abs(actual - midpoint),
                "within_band": band[0] <= actual <= band[1],
                "grading_method": detail.get("grading_method"),
                "confidence": detail.get("confidence"),
                "review_status": detail.get("review_status"),
                "need_manual_review": detail.get("need_manual_review"),
                "warnings": detail.get("warnings") or [],
            }
        )
    return records


def _ordering_metrics(records: list[dict[str, Any]]) -> dict[str, Any]:
    by_question: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for record in records:
        key = (record["paper_id"], record["question_id"])
        by_question.setdefault(key, []).append(record)

    checks: list[dict[str, Any]] = []
    for (paper_id, question_id), items in by_question.items():
        for left_index, left in enumerate(items):
            for right in items[left_index + 1 :]:
                expected_delta = left["expected_midpoint"] - right["expected_midpoint"]
                if expected_delta == 0:
                    continue
                actual_delta = left["actual_score"] - right["actual_score"]
                passed = actual_delta * expected_delta > 0
                checks.append(
                    {
                        "paper_id": paper_id,
                        "question_id": question_id,
                        "better_candidate": left["candidate"] if expected_delta > 0 else right["candidate"],
                        "worse_candidate": right["candidate"] if expected_delta > 0 else left["candidate"],
                        "passed": passed,
                    }
                )
    passed = sum(1 for check in checks if check["passed"])
    return {
        "passed": passed,
        "total": len(checks),
        "accuracy": passed / len(checks) if checks else 0.0,
        "failed_checks": [check for check in checks if not check["passed"]],
    }


def _build_metrics(
    submissions: list[dict[str, Any]],
    records: list[dict[str, Any]],
    workflow_errors: list[dict[str, Any]],
) -> dict[str, Any]:
    band_hits = sum(1 for record in records if record["within_band"])
    scoring_errors = [
        record
        for record in records
        if str(record.get("grading_method") or "").endswith(":error")
    ]
    return {
        "planned_submissions": len(CANDIDATES),
        "completed_submissions": len(submissions),
        "workflow_success_rate": len(submissions) / len(CANDIDATES),
        "workflow_errors": workflow_errors,
        "scored_answers": len(records),
        "scoring_error_count": len(scoring_errors),
        "scoring_errors": scoring_errors,
        "band_hits": band_hits,
        "band_hit_rate": band_hits / len(records) if records else 0.0,
        "mean_absolute_error": (
            sum(record["absolute_error"] for record in records) / len(records)
            if records
            else 0.0
        ),
        "ordering": _ordering_metrics(records),
    }


def _render_markdown(report: dict[str, Any]) -> str:
    metrics = report["metrics"]
    lines = [
        "# Scoring System Validation Report",
        "",
        f"- Generated: `{report['generated_at']}`",
        f"- Remote URL: `{report['remote']['url']}`",
        f"- Remote model: `{report['remote']['model']}`",
        f"- Planned submissions: `{metrics['planned_submissions']}`",
        f"- Completed submissions: `{metrics['completed_submissions']}`",
        f"- Workflow success rate: `{metrics['workflow_success_rate']:.1%}`",
        f"- Band hit rate: `{metrics['band_hit_rate']:.1%}`",
        f"- Mean absolute error: `{metrics['mean_absolute_error']:.3f}`",
        f"- Ordering accuracy: `{metrics['ordering']['accuracy']:.1%}`",
        f"- Scoring errors: `{metrics['scoring_error_count']}`",
        f"- Production data unchanged: `{report['isolation']['production_unchanged']}`",
        "",
        "## Per-answer Results",
        "",
        "| Paper | Candidate | Quality | Question | Expected | Actual | In band | Method | Confidence |",
        "|---|---|---|---|---:|---:|:---:|---|---:|",
    ]
    for record in report["records"]:
        confidence = record.get("confidence")
        confidence_text = "" if confidence is None else f"{float(confidence):.4f}"
        lines.append(
            "| {paper_id} | {candidate} | {quality} | {question_id} | "
            "{expected_min:.1f}-{expected_max:.1f} | {actual_score:.1f} | {within} | "
            "{method} | {confidence_text} |".format(
                **record,
                within="yes" if record["within_band"] else "no",
                method=record.get("grading_method") or "",
                confidence_text=confidence_text,
            )
        )

    if metrics["ordering"]["failed_checks"]:
        lines.extend(["", "## Failed Ordering Checks", ""])
        for check in metrics["ordering"]["failed_checks"]:
            lines.append(
                f"- `{check['paper_id']}/{check['question_id']}`: expected "
                f"{check['better_candidate']} > {check['worse_candidate']}"
            )

    if metrics["workflow_errors"]:
        lines.extend(["", "## Workflow Errors", ""])
        for error in metrics["workflow_errors"]:
            lines.append(f"- `{error['employee_id']}`: {error['error']}")

    lines.extend(
        [
            "",
            "## Interpretation Notes",
            "",
            "- Band hit rate measures agreement with predeclared expert-style score ranges.",
            "- Ordering accuracy checks whether stronger answers score above weaker answers.",
            "- Semantic reranking can overvalue related wording with an incorrect conclusion; review failed ordering checks manually.",
            "",
        ]
    )
    return "\n".join(lines)


def main() -> int:
    remote = grader.validate_remote_reranker_config()
    if remote is None:
        raise RuntimeError("RERANK_USE_REMOTE must be true for this validation")

    production_before = _snapshot([PRODUCTION_PAPERS, PRODUCTION_DB])
    submissions: list[dict[str, Any]] = []
    records: list[dict[str, Any]] = []
    workflow_errors: list[dict[str, Any]] = []
    temp_path_text = ""
    started = time.monotonic()

    with tempfile.TemporaryDirectory(prefix="examsystem-scoring-validation-") as temp_dir:
        temp_root = Path(temp_dir)
        temp_path_text = temp_dir
        previous = _configure_isolated_storage(temp_root)
        try:
            for paper_id, paper in PAPERS.items():
                paper_store.create_paper(slug=paper_id, name=paper["name"])
                paper_store.save_paper(paper_id, paper)
                paper_store.set_status(paper_id, "open")

            with TestClient(app) as client:
                for candidate in CANDIDATES:
                    _reset_isolated_rate_limit_state()
                    candidate_started = time.monotonic()
                    try:
                        start_response = client.post(
                            "/api/exam/start",
                            json={
                                "employee_id": candidate.employee_id,
                                "paper_id": candidate.paper_id,
                            },
                        )
                        start_response.raise_for_status()
                        submit_response = client.post(
                            "/api/submit",
                            json={
                                "name": candidate.name,
                                "employee_id": candidate.employee_id,
                                "paper_id": candidate.paper_id,
                                "answers": candidate.answers,
                            },
                        )
                        submit_response.raise_for_status()
                        submission_id = int(submit_response.json()["submission_id"])
                        submission = _wait_for_submission(client, submission_id)
                        submission_summary = {
                            "submission_id": submission_id,
                            "employee_id": candidate.employee_id,
                            "candidate": candidate.name,
                            "paper_id": candidate.paper_id,
                            "quality": candidate.quality,
                            "total_score": float(submission.get("total_score", 0) or 0),
                            "objective_score": float(submission.get("objective_score", 0) or 0),
                            "subjective_score_machine": float(
                                submission.get("subjective_score_machine", 0) or 0
                            ),
                            "review_status": submission.get("review_status"),
                            "latency_seconds": time.monotonic() - candidate_started,
                        }
                        submissions.append(submission_summary)
                        records.extend(_score_records(candidate, submission))
                        print(
                            f"[{len(submissions)}/{len(CANDIDATES)}] "
                            f"{candidate.name}: total={submission_summary['total_score']:.1f}, "
                            f"latency={submission_summary['latency_seconds']:.1f}s",
                            flush=True,
                        )
                    except Exception as exc:
                        workflow_errors.append(
                            {
                                "employee_id": candidate.employee_id,
                                "paper_id": candidate.paper_id,
                                "error": f"{type(exc).__name__}: {exc}",
                            }
                        )
                        print(f"[error] {candidate.name}: {type(exc).__name__}", flush=True)
        finally:
            grader.close_subjective_service()
            _restore_storage(previous)

    production_after = _snapshot([PRODUCTION_PAPERS, PRODUCTION_DB])
    metrics = _build_metrics(submissions, records, workflow_errors)
    report = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "duration_seconds": time.monotonic() - started,
        "remote": {"url": remote["RERANK_API_URL"], "model": remote["RERANK_MODEL"]},
        "papers": [
            {
                "paper_id": paper_id,
                "name": paper["name"],
                "question_count": len(paper["questions"]),
            }
            for paper_id, paper in PAPERS.items()
        ],
        "submissions": submissions,
        "records": records,
        "metrics": metrics,
        "isolation": {
            "production_unchanged": production_before == production_after,
            "temporary_path_removed": not Path(temp_path_text).exists(),
            "production_snapshot_before": production_before,
            "production_snapshot_after": production_after,
        },
    }
    report = _sanitize(report)

    REPORTS_DIR.mkdir(parents=True, exist_ok=True)
    timestamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    json_path = REPORTS_DIR / f"scoring-validation-{timestamp}.json"
    markdown_path = REPORTS_DIR / f"scoring-validation-{timestamp}.md"
    json_text = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    markdown_text = _render_markdown(report)

    api_key = remote["RERANK_API_KEY"]
    if api_key and (api_key in json_text or api_key in markdown_text):
        raise RuntimeError("refusing to write report containing API key")

    json_path.write_text(json_text, encoding="utf-8")
    markdown_path.write_text(markdown_text, encoding="utf-8")
    print(f"JSON report: {json_path}")
    print(f"Markdown report: {markdown_path}")
    print(
        "Summary: "
        f"workflow={metrics['workflow_success_rate']:.1%}, "
        f"band_hit={metrics['band_hit_rate']:.1%}, "
        f"MAE={metrics['mean_absolute_error']:.3f}, "
        f"ordering={metrics['ordering']['accuracy']:.1%}, "
        f"scoring_errors={metrics['scoring_error_count']}"
    )

    isolation_ok = report["isolation"]["production_unchanged"] and report["isolation"]["temporary_path_removed"]
    workflow_ok = metrics["completed_submissions"] == metrics["planned_submissions"]
    return 0 if isolation_ok and workflow_ok and metrics["scoring_error_count"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
