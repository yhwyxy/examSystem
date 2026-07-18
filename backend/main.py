"""FastAPI 入口。"""

from __future__ import annotations

import hashlib
import logging
import os
import secrets
import threading
from concurrent.futures import ThreadPoolExecutor
import time
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Literal

from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, JSONResponse
from pydantic import BaseModel, Field
from fastapi.concurrency import run_in_threadpool

from . import database, exporter, grader, paper_store, question_loader, review_service
from .config import get_config, reload_config
from .utils import generate_qr_base64, get_lan_ip, parse_iso

# 项目根目录基准：所有静态文件路径以 PROJECT_ROOT 为准，
# 避免从非项目根目录启动 uvicorn 时出现 404。
PROJECT_ROOT = Path(__file__).resolve().parent.parent
FRONTEND_DIR = PROJECT_ROOT / "frontend"

logger = logging.getLogger(__name__)
logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

# Token 存储 {token: expire_timestamp}
_admin_tokens: dict[str, float] = {}
_TOKEN_TTL = 86400  # 24小时

app = FastAPI(title="考试判分系统", version="1.0.0")

# ---------------------------------------------------------------------------
# CORS
# ---------------------------------------------------------------------------
cfg = get_config().server
# 注意：本系统 Token 通过 Authorization header 传递（非 cookie），
# 因此不需要 allow_credentials=True。关闭 credentials 可避免
# 「allow_origins=* + credentials」组合下的 CSRF 凭证泄漏风险。
# 当 allow_origins 包含 "*" 时浏览器会拒绝 credentials，故显式关掉。
app.add_middleware(
    CORSMiddleware,
    allow_origins=cfg.allow_origins,
    allow_credentials=False,
    allow_methods=["GET", "POST", "DELETE", "PUT", "PATCH", "OPTIONS"],
    allow_headers=["Authorization", "Content-Type", "X-Requested-With"],
)

# ---------------------------------------------------------------------------
# 管理员认证
# ---------------------------------------------------------------------------
async def require_admin(request: Request):
    """管理员认证依赖。enable_auth=false 时跳过验证。"""
    cfg = get_config().admin
    if not cfg.enable_auth:
        return
    
    auth = request.headers.get("authorization", "")
    if not auth.startswith("Bearer "):
        raise HTTPException(status_code=401, detail={"code": "UNAUTHORIZED", "message": "请先登录"})
    
    token = auth[7:]
    _cleanup_admin_tokens()  # 惰性清理过期 token，避免 _admin_tokens 长期累积
    expire = _admin_tokens.get(token)
    if expire is None:
        raise HTTPException(status_code=401, detail={"code": "INVALID_TOKEN", "message": "Token 无效或已过期"})
    
    if time.time() > expire:
        _admin_tokens.pop(token, None)
        raise HTTPException(status_code=401, detail={"code": "TOKEN_EXPIRED", "message": "Token 已过期，请重新登录"})

# ---------------------------------------------------------------------------
# 简易内存速率限制器
# ---------------------------------------------------------------------------
# 注意：_rate_store 有上限，防止恶意构造大量不同 IP 造成内存膨胀。
# 本系统刻意不引入 Redis 等外部依赖，单进程内存限流即可满足内网考试场景。
_rate_lock = threading.Lock()
_rate_store: dict[str, list[float]] = defaultdict(list)
_RATE_STORE_MAX_IPS = 10_000  # 最多跟踪 1 万个 IP，超出时整体清理


def _check_rate_limit(ip: str, max_requests: int = 60, window_seconds: int = 60) -> None:
    """每 IP 每窗口最多 max_requests 次请求，超出返回 429。

    - 窗口内滑动计数（清理过期 timestamp 后判断）
    - _rate_store 有 IP 数量上限，防止内存无界增长
    """
    now = time.monotonic()
    with _rate_lock:
        # 容量保护：超过上限时先整体清理过期记录
        if len(_rate_store) > _RATE_STORE_MAX_IPS:
            stale_keys = [
                k for k, ts in _rate_store.items()
                if not ts or now - ts[-1] >= window_seconds
            ]
            for k in stale_keys:
                _rate_store.pop(k, None)
        timestamps = _rate_store[ip]
        # 清理过期记录
        _rate_store[ip] = [t for t in timestamps if now - t < window_seconds]
        if len(_rate_store[ip]) >= max_requests:
            raise HTTPException(status_code=429, detail="请求过于频繁，请稍后再试")
        _rate_store[ip].append(now)


def _admin_rate_key(request: Request) -> str:
    """管理端使用独立限流 key，避免与考生互相影响。"""
    ip = request.client.host if request.client else "unknown"
    return f"admin:{ip}"


def _cleanup_admin_tokens() -> None:
    """惰性清理过期 token，避免 _admin_tokens 长期累积。"""
    now_ts = time.time()
    expired = [t for t, exp in _admin_tokens.items() if now_ts > exp]
    for t in expired:
        _admin_tokens.pop(t, None)

# ---------------------------------------------------------------------------
# 全局时间窗口校验（仅依赖服务器时间）
# ---------------------------------------------------------------------------

def _assert_global_time_window() -> None:
    cfg = get_config().exam
    if not cfg.enable_global_time_window:
        return
    now = datetime.now(timezone.utc)
    if cfg.start_time and now < cfg.start_time:
        raise _error(403, "EXAM_NOT_STARTED", "考试尚未开始")
    if cfg.end_time and now > cfg.end_time:
        raise _error(403, "EXAM_ENDED", "考试已结束")


def _error(status: int, code: str, message: str) -> HTTPException:
    return HTTPException(status_code=status, detail={"code": code, "message": message})


# ---------------------------------------------------------------------------
# 考试开始时间记录（服务器端，防篡改）
# ---------------------------------------------------------------------------
# 考试「开始时间」改为 DB 持久化（exam_sessions 表），避免：
#   1) 进程重启导致已开始考生无法提交
#   2) 全局内存字典无界增长（长期未提交的记录堆积）
# 详见 database.upsert_exam_session / pop_exam_session / cleanup_exam_sessions
# ---------------------------------------------------------------------------


# ---------------------------------------------------------------------------
# 模型
# ---------------------------------------------------------------------------

class SubmitRequest(BaseModel):
    name: str
    employee_id: str
    paper_id: str
    department: str | None = None
    answers: dict[str, Any] = Field(default_factory=dict)
    started_at: str | None = None  # 保留字段，但不再用于时间校验
    auto_submit_reason: Literal["third_blur", "blur_timeout_30s"] | None = None


class ExamStartRequest(BaseModel):
    employee_id: str
    paper_id: str


class CreatePaperRequest(BaseModel):
    slug: str
    name: str


class SavePaperRequest(BaseModel):
    name: str | None = None
    exam_info: dict[str, Any] = Field(default_factory=dict)
    questions: list[dict[str, Any]] = Field(default_factory=list)


class UpdatePaperMetaRequest(BaseModel):
    name: str | None = None


class ReorderQuestionsRequest(BaseModel):
    ids: list[str]


class ReviewRequest(BaseModel):
    submission_id: int
    question_id: str
    new_score: float
    note: str | None = None
    sub_question_id: str | None = None


# 评分并发上限：防止「提交即新建线程 + 新建事件循环」导致
# 百人并发时 OOM / 事件循环爆炸 / Embedding 服务被打满。
# 单进程内所有评分共享一个有界线程池，超出时提交排队等待。
_GRADING_MAX_WORKERS = 4
_grading_executor: ThreadPoolExecutor | None = None


def _get_grading_executor() -> ThreadPoolExecutor:
    """惰性创建全局有界线程池。进程级别单例。"""
    global _grading_executor
    if _grading_executor is None:
        _grading_executor = ThreadPoolExecutor(
            max_workers=_GRADING_MAX_WORKERS,
            thread_name_prefix="grader",
        )
    return _grading_executor


def schedule_grading(submission_id: int, answers: dict[str, Any], paper_id: str) -> None:
    """后台线程执行评分，保持提交接口快速返回。

    使用全局有界线程池（默认 4 workers）+ 复用单个事件循环模式，
    避免每次提交新建线程/事件循环导致资源耗尽。
    队满时提交请求排队等待，不会丢失。
    """
    def _background_grade(sub_id: int, submitted_answers: dict[str, Any], pid: str) -> None:
        # 复用调用线程的事件循环：grader.grade_submission 是 async，
        # 但在线程池 worker 中无 running loop，新建一个并立即关闭。
        # 相比旧实现，线程数受 max_workers 约束，避免无界增长。
        import asyncio
        loop = asyncio.new_event_loop()
        try:
            result = loop.run_until_complete(grader.grade_submission(submitted_answers, paper_id=pid))
            database.update_submission_grading_result(
                submission_id=sub_id,
                grading_detail=result.grading_detail,
                scores={
                    "objective_score": result.objective_score,
                    "subjective_score_machine": result.subjective_score_machine,
                    "subjective_score_final": result.subjective_score_final,
                    "total_score": result.total_score,
                },
                review_status=result.review_status,
            )
            logger.info("后台评分完成: submission_id=%d paper=%s status=%s", sub_id, pid, result.review_status)
        except Exception:
            logger.exception("后台评分失败: submission_id=%d paper=%s", sub_id, pid)
            try:
                database.update_submission_grading_result(
                    submission_id=sub_id,
                    grading_detail=[],
                    scores={"objective_score": 0, "subjective_score_machine": 0,
                            "subjective_score_final": 0, "total_score": 0},
                    review_status="need_review",
                )
            except Exception:
                logger.exception("更新评分失败状态也失败: submission_id=%d", sub_id)
        finally:
            loop.close()

    try:
        _get_grading_executor().submit(_background_grade, submission_id, answers, paper_id)
    except RuntimeError:
        # executor 已关闭（如进程退出），降级同步评分避免丢任务
        logger.warning("评分线程池不可用，降级同步评分: submission_id=%d", submission_id)
        _background_grade(submission_id, answers, paper_id)


def _shutdown_runtime() -> None:
    """等待后台评分结束后关闭远端评分连接。"""
    global _grading_executor
    executor = _grading_executor
    _grading_executor = None
    try:
        if executor is not None:
            executor.shutdown(wait=True, cancel_futures=False)
    finally:
        grader.close_subjective_service()


app.router.add_event_handler("shutdown", _shutdown_runtime)


# ---------------------------------------------------------------------------
# 中间件：全局速率限制
# ---------------------------------------------------------------------------

@app.middleware("http")
async def rate_limit_middleware(request: Request, call_next):
    # 管理端单独配额，避免导出等大请求被考生流量挤占
    path = request.url.path
    is_admin = path.startswith("/api/admin") or path.startswith("/admin")
    ip = request.client.host if request.client else "unknown"
    key = f"admin:{ip}" if is_admin else ip
    try:
        # 管理端 120/min，考生端 60/min
        _check_rate_limit(key, max_requests=120 if is_admin else 60)
    except HTTPException:
        return JSONResponse(status_code=429, content={"detail": "请求过于频繁，请稍后再试"})
    # 不再吞掉其他异常：仅捕获限流 HTTPException，其余异常向上抛由 FastAPI 处理
    return await call_next(request)


# ---------------------------------------------------------------------------
# 考试端
# ---------------------------------------------------------------------------

@app.get("/api/exam")
def get_exam(paper: str | None = None) -> dict[str, Any]:
    """获取指定专业的脱敏试卷。必须提供 paper 参数。"""
    _assert_global_time_window()
    if not paper or not str(paper).strip():
        raise _error(400, "PAPER_REQUIRED", "请使用管理员发放的专业考试链接（缺少 paper 参数）")
    paper_id = question_loader.validate_slug(paper)
    meta = question_loader.assert_paper_open(paper_id)
    cfg = get_config().exam
    data = question_loader.public_exam_payload(paper_id)
    if not data.get("questions"):
        raise _error(400, "EMPTY_QUESTION_BANK", "该专业试卷暂无题目")
    server_time = datetime.now(timezone.utc).isoformat()
    return {
        "paper_id": paper_id,
        "paper_name": data.get("paper_name") or meta.get("name"),
        "exam_info": data["exam_info"],
        "questions": data["questions"],
        "server_time": server_time,
        "duration_minutes": cfg.duration_minutes,
        "auto_submit": cfg.auto_submit,
        "config": {
            "duration_minutes": cfg.duration_minutes,
            "auto_submit": cfg.auto_submit,
        },
    }


@app.post("/api/exam/start")
def exam_start(req: ExamStartRequest, request: Request) -> dict[str, Any]:
    """考生点击「开始答题」时调用，服务器记录开始时间并返回。"""
    _assert_global_time_window()
    employee_id = req.employee_id.strip()
    if not employee_id:
        raise _error(400, "INVALID_EMPLOYEE_ID", "请提供工号")
    paper_id = question_loader.validate_slug(req.paper_id)
    question_loader.assert_paper_open(paper_id)
    now = datetime.now(timezone.utc)
    database.upsert_exam_session(
        employee_id=employee_id, paper_id=paper_id, started_at=now.isoformat()
    )
    return {
        "started_at": now.isoformat(),
        "server_time": now.isoformat(),
        "paper_id": paper_id,
    }


@app.post("/api/submit")
def submit(req: SubmitRequest, request: Request) -> dict[str, Any]:
    _assert_global_time_window()
    cfg = get_config().exam

    employee_id = req.employee_id.strip()
    paper_id = question_loader.validate_slug(req.paper_id)
    meta = question_loader.assert_paper_open(paper_id)
    paper_name = meta.get("name")

    started_at_iso = database.pop_exam_session(employee_id, paper_id)
    if started_at_iso is None:
        raise _error(403, "EXAM_NOT_STARTED", "请先开始考试")
    server_start = parse_iso(started_at_iso)

    now = datetime.now(timezone.utc)
    elapsed = (now - server_start).total_seconds()
    if elapsed < -60:
        raise _error(403, "EXAM_TIMEOUT", "考试时间异常，提交被拒绝")
    if elapsed > cfg.duration_minutes * 60 + cfg.grace_period_seconds:
        raise _error(403, "EXAM_TIMEOUT", "考试已超时，提交被拒绝")

    full = question_loader.load_questions(paper_id)
    qmap = {str(q["id"]): q for q in full.get("questions", [])}
    for qid, ans in (req.answers or {}).items():
        q = qmap.get(str(qid))
        if not q:
            continue
        try:
            question_loader.validate_answer_shape(q, ans)
        except ValueError as e:
            message = str(e)
            code = "INVALID_CODE_LANGUAGE" if message.startswith("INVALID_CODE_LANGUAGE") else "INVALID_ANSWER_SHAPE"
            raise _error(422, code, message) from e

    try:
        submission_id = database.insert_submission_pending(
            name=req.name.strip(),
            employee_id=employee_id,
            paper_id=paper_id,
            paper_name=paper_name,
            department=req.department.strip() if req.department else None,
            answers=req.answers,
            started_at=req.started_at,
            client_ip=request.client.host if request.client else None,
            user_agent=request.headers.get("user-agent"),
            auto_submit_reason=req.auto_submit_reason,
        )
    except Exception as e:
        if "UNIQUE" in str(e).upper():
            raise _error(409, "DUPLICATE_SUBMISSION", "该员工已在本专业提交，不能重复提交")
        logger.exception("提交保存失败")
        raise _error(500, "SUBMIT_FAILED", "提交失败，请联系管理员")

    schedule_grading(submission_id, req.answers, paper_id)

    return {
        "success": True,
        "submission_id": submission_id,
        "status": "grading",
        "paper_id": paper_id,
        "message": "提交成功，系统正在评分中",
    }


@app.get("/api/submission/{submission_id}/status")
def submission_status(submission_id: int) -> dict[str, Any]:
    """前端轮询接口：查询评分状态。"""
    info = database.get_submission_status(submission_id)
    if not info:
        raise _error(404, "SUBMISSION_NOT_FOUND", "提交记录不存在")
    return {
        "submission_id": info["id"],
        "status": info["review_status"],
    }


# ---------------------------------------------------------------------------
# 管理端
# ---------------------------------------------------------------------------

@app.get("/api/health")
def health() -> dict[str, Any]:
    return {"ok": True, "time": datetime.now(timezone.utc).isoformat()}


class LoginRequest(BaseModel):
    password: str

@app.post("/api/admin/login")
def admin_login(req: LoginRequest) -> dict[str, Any]:
    """管理员登录端点。"""
    cfg = get_config().admin
    if not cfg.enable_auth:
        return {"success": True, "token": "auth_disabled", "message": "认证未启用"}
    
    if not cfg.password:
        # 配置错误（非代码异常）：用 503 明确表示服务未就绪，而非 500 内部错误
        logger.error("管理员认证已启用但未配置密码（admin.enable_auth=true 且 password 为空）")
        raise HTTPException(
            status_code=503,
            detail={"code": "ADMIN_PASSWORD_NOT_CONFIGURED",
                    "message": "管理员认证已启用但未配置密码，请在 config.yaml 中设置 admin.password"},
        )
    
    # 验证密码（支持明文和 SHA-256 哈希）
    pwd_hash = hashlib.sha256(req.password.encode()).hexdigest()
    if req.password != cfg.password and pwd_hash != cfg.password:
        raise HTTPException(status_code=401, detail={"code": "WRONG_PASSWORD", "message": "密码错误"})
    
    # 生成 Token
    token = secrets.token_urlsafe(32)
    _admin_tokens[token] = time.time() + _TOKEN_TTL
    return {"success": True, "token": token}


def _exam_base_url(request: Request) -> str:
    """根据管理员当前访问地址生成考试链接基址。

    不固定拼接配置端口，避免通过 HTTPS、域名、反向代理、隧道或非默认端口
    访问管理端时，发布出的考试链接跳到错误地址。
    """
    scheme = (request.headers.get("x-forwarded-proto") or request.url.scheme or "http").split(",")[0].strip()
    host = (request.headers.get("x-forwarded-host") or request.headers.get("host") or request.url.netloc).split(",")[0].strip()
    if not host:
        cfg = get_config().server
        host = f"{get_lan_ip()}:{cfg.port}"

    hostname = request.url.hostname
    if hostname in {"127.0.0.1", "localhost"}:
        port = request.url.port or get_config().server.port
        host = f"{get_lan_ip()}:{port}"

    return f"{scheme}://{host}"


@app.get("/api/admin/exam-link", dependencies=[Depends(require_admin)])
def exam_link(request: Request, paper: str | None = None) -> dict[str, Any]:
    """返回考试链接和二维码；建议传 paper 生成专业专属链接。"""
    base = _exam_base_url(request)
    if paper:
        paper_id = question_loader.validate_slug(paper)
        meta = question_loader.get_paper_meta(paper_id)
        if not meta:
            raise _error(404, "PAPER_NOT_FOUND", f"试卷不存在: {paper_id}")
        url = f"{base}/exam?paper={paper_id}"
        return {
            "url": url,
            "qr_base64": generate_qr_base64(url),
            "paper_id": paper_id,
            "paper_name": meta.get("name"),
            "status": meta.get("status"),
        }
    # 兼容：无 paper 时返回提示性入口（不可直接作答）
    url = f"{base}/exam"
    return {"url": url, "qr_base64": generate_qr_base64(url), "message": "请为具体专业生成链接"}


@app.get("/api/admin/stats", dependencies=[Depends(require_admin)])
def admin_stats(paper_id: str | None = None) -> dict[str, Any]:
    return database.get_stats(paper_id=paper_id)


@app.get("/api/admin/submissions", dependencies=[Depends(require_admin)])
def admin_submissions(
    keyword: str | None = None,
    review_status: str | None = None,
    paper_id: str | None = None,
    sort_by: str | None = None,
    order: str | None = None,
    limit: int = 100,
    offset: int = 0,
) -> list[dict[str, Any]]:
    """支持分页的成绩列表。"""
    limit = max(1, min(limit, 500))
    offset = max(0, offset)
    return database.list_submissions(
        keyword=keyword, review_status=review_status, paper_id=paper_id,
        sort_by=sort_by or "submitted_at",
        order=order or "desc",
        limit=limit, offset=offset,
    )


@app.get("/api/admin/submissions/{submission_id}", dependencies=[Depends(require_admin)])
def admin_submission_detail(submission_id: int) -> dict[str, Any]:
    item = database.get_submission(submission_id)
    if not item:
        raise _error(404, "NOT_FOUND", "提交记录不存在")
    return item


class DeleteRequest(BaseModel):
    ids: list[int]


@app.delete("/api/admin/submissions", dependencies=[Depends(require_admin)])
def admin_delete_submissions(req: DeleteRequest) -> dict[str, Any]:
    if not req.ids:
        raise _error(400, "INVALID_IDS", "请提供待删除的记录 ID 列表")
    deleted = database.delete_submissions(req.ids)
    return {"success": True, "deleted": deleted}


@app.post("/api/admin/review", dependencies=[Depends(require_admin)])
def admin_review(req: ReviewRequest) -> dict[str, Any]:
    result = database.apply_review(submission_id=req.submission_id, question_id=req.question_id, new_score=req.new_score, note=req.note, sub_question_id=req.sub_question_id)
    if not result.get("success"):
        raise _error(400, result.get("code", "REVIEW_FAILED"), result.get("message", "复核失败"))
    return result


@app.post("/api/admin/regrade/{submission_id}", dependencies=[Depends(require_admin)])
def admin_regrade(submission_id: int) -> dict[str, Any]:
    result = review_service.regrade_submission(submission_id)
    if not result.get("success"):
        status = 404 if result.get("code") == "SUBMISSION_NOT_FOUND" else 400
        raise _error(
            status,
            result.get("code", "REGRADE_FAILED"),
            result.get("message", "重新判分失败"),
        )
    return {**result, "submission_id": submission_id}


@app.get("/api/admin/export", dependencies=[Depends(require_admin)])
def admin_export(paper_id: str | None = None) -> Any:
    export_cfg = get_config().export
    if export_cfg.format == "xlsx":
        from fastapi.responses import Response
        content = exporter.export_submissions_xlsx(paper_id=paper_id)
        fname = f"exam_results_{paper_id}.xlsx" if paper_id else "exam_results.xlsx"
        return Response(
            content=content,
            media_type="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
            headers={"Content-Disposition": f"attachment; filename={fname}"},
        )
    return {"message": "仅支持 xlsx 导出"}


# ---------------------------------------------------------------------------
# 管理端：多专业试卷
# ---------------------------------------------------------------------------

@app.get("/api/admin/papers", dependencies=[Depends(require_admin)])
def admin_list_papers() -> list[dict[str, Any]]:
    papers = paper_store.list_papers_with_status()
    out = []
    for p in papers:
        item = dict(p)
        slug = str(p.get("slug") or "")
        item["submission_count"] = database.submission_count(slug) if slug else 0
        out.append(item)
    return out


@app.post("/api/admin/papers", dependencies=[Depends(require_admin)])
def admin_create_paper(req: CreatePaperRequest) -> dict[str, Any]:
    meta = paper_store.create_paper(slug=req.slug, name=req.name)
    return {"success": True, "paper": meta}


@app.get("/api/admin/papers/{slug}", dependencies=[Depends(require_admin)])
def admin_get_paper(slug: str) -> dict[str, Any]:
    return paper_store.get_paper_full(slug)


@app.put("/api/admin/papers/{slug}", dependencies=[Depends(require_admin)])
def admin_save_paper(slug: str, req: SavePaperRequest) -> dict[str, Any]:
    return paper_store.save_paper(slug, {
        "name": req.name,
        "exam_info": req.exam_info,
        "questions": req.questions,
    })


@app.patch("/api/admin/papers/{slug}/meta", dependencies=[Depends(require_admin)])
def admin_patch_paper_meta(slug: str, req: UpdatePaperMetaRequest) -> dict[str, Any]:
    return paper_store.update_meta(slug, name=req.name)


@app.delete("/api/admin/papers/{slug}", dependencies=[Depends(require_admin)])
def admin_delete_paper(slug: str) -> dict[str, Any]:
    has = database.submission_count(slug) > 0
    paper_store.delete_paper(slug, has_submissions=has)
    return {"success": True}


@app.post("/api/admin/papers/{slug}/questions", dependencies=[Depends(require_admin)])
def admin_add_question(slug: str, question: dict[str, Any]) -> dict[str, Any]:
    q = paper_store.add_question(slug, question)
    return {"success": True, "question": q}


@app.put("/api/admin/papers/{slug}/questions/{question_id}", dependencies=[Depends(require_admin)])
def admin_update_question(slug: str, question_id: str, question: dict[str, Any]) -> dict[str, Any]:
    q = paper_store.update_question(slug, question_id, question)
    return {"success": True, "question": q}


@app.delete("/api/admin/papers/{slug}/questions/{question_id}", dependencies=[Depends(require_admin)])
def admin_delete_question(slug: str, question_id: str) -> dict[str, Any]:
    paper_store.delete_question(slug, question_id)
    return {"success": True}


@app.put("/api/admin/papers/{slug}/questions/reorder", dependencies=[Depends(require_admin)])
def admin_reorder_questions(slug: str, req: ReorderQuestionsRequest) -> dict[str, Any]:
    return paper_store.reorder_questions(slug, req.ids)


@app.post("/api/admin/papers/{slug}/open", dependencies=[Depends(require_admin)])
def admin_open_paper(slug: str) -> dict[str, Any]:
    meta = paper_store.set_status(slug, question_loader.PAPER_STATUS_OPEN)
    return {"success": True, "paper": meta}


@app.post("/api/admin/papers/{slug}/close", dependencies=[Depends(require_admin)])
def admin_close_paper(slug: str) -> dict[str, Any]:
    meta = paper_store.set_status(slug, question_loader.PAPER_STATUS_CLOSED)
    return {"success": True, "paper": meta}


@app.get("/api/admin/papers/{slug}/exam-link", dependencies=[Depends(require_admin)])
def admin_paper_exam_link(slug: str, request: Request) -> dict[str, Any]:
    return exam_link(request, paper=slug)


@app.post("/api/admin/reload-questions", dependencies=[Depends(require_admin)])
def reload_questions_api(paper: str | None = None) -> dict[str, Any]:
    try:
        if paper:
            data = question_loader.reload_questions(paper)
            return {"success": True, "paper_id": paper, "count": len(data.get("questions") or [])}
        question_loader.reload_questions()
        papers = question_loader.list_papers()
        return {"success": True, "papers": len(papers)}
    except HTTPException:
        raise
    except Exception:
        logger.exception("题库重载失败")
        raise _error(500, "RELOAD_FAILED", "题库重载失败，请检查题库文件格式")


@app.post("/api/admin/reload-config", dependencies=[Depends(require_admin)])
def reload_config_api() -> dict[str, Any]:
    try:
        reload_config()
        return {"success": True, "message": "配置已重载"}
    except Exception:
        logger.exception("配置重载失败")
        raise _error(500, "RELOAD_FAILED", "配置重载失败，请检查配置文件格式")


# ---------------------------------------------------------------------------
# 页面路由
# ---------------------------------------------------------------------------

@app.get("/")
def index():
    return FileResponse(FRONTEND_DIR / "exam.html")


@app.get("/exam")
def exam_page():
    return FileResponse(FRONTEND_DIR / "exam.html")


@app.get("/admin")
def admin_page():
    return FileResponse(FRONTEND_DIR / "admin.html")


@app.get("/detail")
def detail_page():
    return FileResponse(FRONTEND_DIR / "detail.html")


from starlette.staticfiles import StaticFiles
app.mount("/css", StaticFiles(directory=str(FRONTEND_DIR / "css")), name="css")
app.mount("/js", StaticFiles(directory=str(FRONTEND_DIR / "js")), name="js")


def _preflight_check() -> None:
    """启动时预检关键配置。

    将「运行时才崩」的配置错误（如 admin 启用认证但未设密码）
    前置到启动日志。云端 Reranker 配置不完整时阻止启动，其他现有
    检查仍只记录日志。
    """
    grader.validate_remote_reranker_config()
    grader.get_subjective_service()

    try:
        cfg = get_config()
    except Exception:
        logger.exception("配置加载失败，将使用默认配置可能行为异常")
        return

    # admin 认证与密码一致性
    admin = cfg.admin
    if admin.enable_auth and not admin.password:
        logger.warning(
            "配置预检：admin.enable_auth=true 但 password 为空 —— "
            "管理员将无法登录。请在 config.yaml 设置 admin.password 或关闭 enable_auth"
        )

    # 考试时间窗口合理性
    exam = getattr(cfg, "exam", None)
    if exam is not None and getattr(exam, "enable_global_time_window", False):
        try:
            start = parse_iso(exam.start_time)
            end = parse_iso(exam.end_time)
            if end <= start:
                logger.warning("配置预检：考试时间窗口结束时间早于开始时间")
        except Exception:
            logger.warning("配置预检：考试时间窗口时间格式无法解析")

    # 试卷目录 / 旧卷迁移
    try:
        question_loader.ensure_papers_layout()
        logger.info("试卷目录就绪，共 %d 份专业卷", len(question_loader.list_papers()))
    except Exception:
        logger.exception("配置预检：试卷目录初始化失败")

    # DB 初始化
    try:
        database.init_db()
    except Exception:
        logger.exception("配置预检：数据库初始化失败")

    logger.info("配置预检完成")


# 模块加载时执行预检（uvicorn 启动即触发）
_preflight_check()
