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
from pydantic import BaseModel, Field, field_validator
from fastapi.concurrency import run_in_threadpool

from . import database, exam_run_service, exporter, grader, paper_store, question_loader, review_service
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
# 模型
# ---------------------------------------------------------------------------

class SubmitRequest(BaseModel):
    session_id: str
    session_token: str
    answers: dict[str, Any] = Field(default_factory=dict)
    # 仅接受已知自动交卷原因；旧前端可能误把 submit Event 序列化成
    # {"isTrusted": true}，这类非字符串输入按“非自动交卷”处理为 None。
    auto_submit_reason: Literal["third_blur", "blur_timeout_30s"] | None = None

    @field_validator("auto_submit_reason", mode="before")
    @classmethod
    def _normalize_auto_submit_reason(cls, value: Any):
        if value is None or value == "":
            return None
        if isinstance(value, str):
            return value
        return None


class ExamStartRequest(BaseModel):
    paper_id: str
    run_token: str
    name: str
    employee_id: str
    department: str | None = None


class DraftRequest(BaseModel):
    session_token: str
    revision: int
    answers: dict[str, Any] = Field(default_factory=dict)


class BatchSlugsRequest(BaseModel):
    slugs: list[str] = Field(min_length=1)


class ResetRoundsRequest(BaseModel):
    """slugs 为空或省略时重置全部可重置专业。"""
    slugs: list[str] = Field(default_factory=list)


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


def schedule_grading(
    submission_id: int,
    answers: dict[str, Any],
    paper_id: str,
    run_id: str | None = None,
) -> None:
    """后台线程执行评分，保持提交接口快速返回。"""
    def _background_grade(
        sub_id: int,
        submitted_answers: dict[str, Any],
        pid: str,
        rid: str | None,
    ) -> None:
        import asyncio
        loop = asyncio.new_event_loop()
        try:
            result = loop.run_until_complete(
                grader.grade_submission(submitted_answers, paper_id=pid, run_id=rid)
            )
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
            logger.info(
                "后台评分完成: submission_id=%d paper=%s run=%s status=%s",
                sub_id, pid, rid, result.review_status,
            )
        except Exception:
            logger.exception("后台评分失败: submission_id=%d paper=%s run=%s", sub_id, pid, rid)
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
        _get_grading_executor().submit(_background_grade, submission_id, answers, paper_id, run_id)
    except RuntimeError:
        logger.warning("评分线程池不可用，降级同步评分: submission_id=%d", submission_id)
        _background_grade(submission_id, answers, paper_id, run_id)


def _shutdown_runtime() -> None:
    """停止收卷循环，等待后台评分结束后关闭远端评分连接。"""
    global _grading_executor
    try:
        exam_run_service.stop_finalize_loop()
    except Exception:
        logger.exception("停止收卷循环失败")
    executor = _grading_executor
    _grading_executor = None
    try:
        if executor is not None:
            executor.shutdown(wait=True, cancel_futures=False)
    finally:
        grader.close_subjective_service()


def _startup_runtime() -> None:
    """启动收卷循环并恢复未完成任务。"""
    exam_run_service.set_grading_scheduler(schedule_grading)
    exam_run_service.resume_on_startup()
    exam_run_service.start_finalize_loop()


app.router.add_event_handler("startup", _startup_runtime)
app.router.add_event_handler("shutdown", _shutdown_runtime)


# ---------------------------------------------------------------------------
# 请求校验错误日志（便于排查 422）
# ---------------------------------------------------------------------------

from fastapi.exceptions import RequestValidationError


@app.exception_handler(RequestValidationError)
async def request_validation_exception_handler(request: Request, exc: RequestValidationError):
    logger.warning(
        "请求校验失败 422 path=%s body_errors=%s",
        request.url.path,
        exc.errors(),
    )
    return JSONResponse(status_code=422, content={"detail": exc.errors()})


@app.middleware("http")
async def no_cache_exam_assets(request: Request, call_next):
    """考试页与脚本禁止缓存，避免修复后仍命中旧 exam.js。"""
    response = await call_next(request)
    path = request.url.path
    if path in {"/", "/exam", "/admin", "/detail"} or path.startswith("/js/") or path.startswith("/css/"):
        response.headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
        response.headers["Pragma"] = "no-cache"
    return response




# ---------------------------------------------------------------------------
# 中间件：全局速率限制
# ---------------------------------------------------------------------------

@app.middleware("http")
async def rate_limit_middleware(request: Request, call_next):
    # 管理端单独配额；草稿/状态按会话限流，避免与 IP 共用
    path = request.url.path
    is_admin = path.startswith("/api/admin") or path.startswith("/admin")
    ip = request.client.host if request.client else "unknown"
    if "/api/exam/sessions/" in path and ("/draft" in path or path.rstrip("/").endswith("/status")):
        parts = path.strip("/").split("/")
        sid = parts[3] if len(parts) >= 4 else "unknown"
        key = f"session:{sid}"
        max_req = 60
    elif is_admin:
        key = f"admin:{ip}"
        max_req = 120
    else:
        key = ip
        max_req = 60
    try:
        _check_rate_limit(key, max_requests=max_req)
    except HTTPException:
        return JSONResponse(status_code=429, content={"detail": "请求过于频繁，请稍后再试"})
    return await call_next(request)


# ---------------------------------------------------------------------------
# 考试端
# ---------------------------------------------------------------------------

def _service_call(fn, *args, **kwargs):
    try:
        return fn(*args, **kwargs)
    except exam_run_service.ServiceError as e:
        raise e.as_http() from e


@app.get("/api/exam")
def get_exam(paper: str | None = None, run: str | None = None) -> dict[str, Any]:
    """获取指定轮次的脱敏试卷。需要 paper + run token。"""
    _assert_global_time_window()
    if not paper or not str(paper).strip():
        raise _error(400, "PAPER_REQUIRED", "请使用管理员发放的专业考试链接（缺少 paper 参数）")
    if not run or not str(run).strip():
        raise _error(400, "RUN_REQUIRED", "请使用管理员发放的考试链接（缺少 run 参数）")
    return _service_call(exam_run_service.get_public_exam, str(paper).strip(), str(run).strip())


@app.post("/api/exam/start")
def exam_start(req: ExamStartRequest, request: Request) -> dict[str, Any]:
    """开始或恢复考试会话。"""
    _assert_global_time_window()
    return _service_call(
        exam_run_service.start_or_resume_session,
        paper_id=req.paper_id,
        run_token=req.run_token,
        name=req.name,
        employee_id=req.employee_id,
        department=req.department,
        client_ip=request.client.host if request.client else None,
        user_agent=request.headers.get("user-agent"),
    )


@app.put("/api/exam/sessions/{session_id}/draft")
def save_draft(session_id: str, req: DraftRequest) -> dict[str, Any]:
    _assert_global_time_window()
    return _service_call(
        exam_run_service.save_draft,
        session_id,
        session_token=req.session_token,
        revision=req.revision,
        answers=req.answers,
    )


@app.get("/api/exam/sessions/{session_id}/status")
def exam_session_status(session_id: str, session_token: str) -> dict[str, Any]:
    _assert_global_time_window()
    return _service_call(
        exam_run_service.get_session_status,
        session_id,
        session_token,
    )


@app.post("/api/submit")
def submit(req: SubmitRequest, request: Request) -> dict[str, Any]:
    _assert_global_time_window()
    return _service_call(
        exam_run_service.submit_manual,
        session_id=req.session_id,
        session_token=req.session_token,
        answers=req.answers or {},
        auto_submit_reason=req.auto_submit_reason,
    )


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
    """返回当前活动轮次或最近轮次链接。legacy 无 token 时仅返回状态。"""
    if not paper or not str(paper).strip():
        raise _error(400, "PAPER_REQUIRED", "请指定专业 paper 参数")
    slug = question_loader.validate_slug(paper)
    meta = question_loader.get_paper_meta(slug)
    if not meta:
        raise _error(404, "PAPER_NOT_FOUND", f"试卷不存在: {slug}")
    active = database.get_active_run_for_paper(slug)
    latest = active or database.get_latest_run_for_paper(slug)
    base = _exam_base_url(request)
    status = exam_run_service.derived_status_for_paper(slug)
    if not latest or latest.get("is_legacy") or not latest.get("public_token_hash"):
        return {
            "paper_id": slug,
            "paper_name": meta.get("name"),
            "status": status,
            "url": None,
            "qr_base64": "",
            "round_no": latest.get("round_no") if latest else None,
            "run_id": latest.get("id") if latest else None,
            "message": "当前无可用考试链接，请先发布新轮次",
        }
    token = exam_run_service.get_run_public_token(str(latest["id"]))
    if not token:
        return {
            "paper_id": slug,
            "paper_name": meta.get("name"),
            "status": status,
            "url": None,
            "qr_base64": "",
            "round_no": latest.get("round_no"),
            "run_id": latest.get("id"),
            "message": "本轮链接密钥不可用，请发布新轮次",
            "base_url": base,
        }
    url = exam_run_service.build_exam_url(base, slug, token)
    return {
        "paper_id": slug,
        "paper_name": meta.get("name"),
        "status": status,
        "url": url,
        "qr_base64": generate_qr_base64(url),
        "round_no": latest.get("round_no"),
        "run_id": latest.get("id"),
        "public_token": token,
        "base_url": base,
    }


@app.get("/api/admin/stats", dependencies=[Depends(require_admin)])
def admin_stats(paper_id: str | None = None) -> dict[str, Any]:
    return database.get_stats(paper_id=paper_id)


@app.get("/api/admin/submissions", dependencies=[Depends(require_admin)])
def admin_submissions(
    keyword: str | None = None,
    review_status: str | None = None,
    paper_id: str | None = None,
    run_id: str | None = None,
    sort_by: str = "submitted_at",
    order: str = "desc",
    limit: int = 50,
    offset: int = 0,
) -> list[dict[str, Any]]:
    rows = database.list_submissions(
        keyword=keyword,
        review_status=review_status,
        paper_id=paper_id,
        run_id=run_id,
        sort_by=sort_by,
        order=order,
        limit=limit,
        offset=offset,
    )
    # 列表不返回大字段
    for r in rows:
        r.pop("answers_json", None)
        r.pop("grading_detail_json", None)
    return rows


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
        if slug:
            try:
                item["status"] = exam_run_service.derived_status_for_paper(slug)
            except Exception:
                pass
            active = database.get_active_run_for_paper(slug)
            if active:
                item["active_sessions"] = database.count_active_sessions_for_run(str(active["id"]))
                item["round_no"] = active.get("round_no")
                item["run_id"] = active.get("id")
            else:
                latest = database.get_latest_run_for_paper(slug)
                item["active_sessions"] = 0
                item["round_no"] = latest.get("round_no") if latest else None
                item["run_id"] = latest.get("id") if latest else None
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


# 批量路由必须注册在 /papers/{slug}/open|close 之前，
# 否则 FastAPI 会把 path 段 "batch" 当成 slug，返回 试卷不存在: batch。
@app.post("/api/admin/papers/batch/open", dependencies=[Depends(require_admin)])
def admin_batch_open(req: BatchSlugsRequest, request: Request) -> dict[str, Any]:
    result = exam_run_service.batch_open(req.slugs)
    base = _exam_base_url(request)
    for p in result.get("papers") or []:
        token = p.get("public_token")
        if token:
            url = exam_run_service.build_exam_url(base, p["slug"], token)
            p["url"] = url
            p["qr_base64"] = generate_qr_base64(url)
    return result


@app.post("/api/admin/papers/batch/close", dependencies=[Depends(require_admin)])
def admin_batch_close(req: BatchSlugsRequest) -> dict[str, Any]:
    return exam_run_service.batch_close(req.slugs)


@app.post("/api/admin/papers/{slug}/open", dependencies=[Depends(require_admin)])
def admin_open_paper(slug: str, request: Request) -> dict[str, Any]:
    run = _service_call(exam_run_service.open_run, slug)
    base = _exam_base_url(request)
    url = exam_run_service.build_exam_url(base, slug, run["public_token"])
    return {
        "success": True,
        "status": run["status"],
        "run_id": run["id"],
        "round_no": run["round_no"],
        "duration_minutes": run["duration_minutes"],
        "public_token": run["public_token"],
        "url": url,
        "qr_base64": generate_qr_base64(url),
        "paper": {
            "slug": slug,
            "name": run.get("paper_name"),
            "status": "open",
        },
    }


@app.post("/api/admin/papers/{slug}/close", dependencies=[Depends(require_admin)])
def admin_close_paper(slug: str) -> dict[str, Any]:
    result = _service_call(exam_run_service.begin_close, slug)
    return result


@app.get("/api/admin/exams", dependencies=[Depends(require_admin)])
def admin_list_exams() -> list[dict[str, Any]]:
    # 管理端轮询时顺带收卷到期轮次，避免仅依赖后台线程时 UI 长时间停在「收卷中」
    try:
        exam_run_service.scan_and_finalize_due_runs()
    except Exception:
        logger.exception("管理端列表触发收卷扫描失败")
    return exam_run_service.list_exam_summaries()


@app.post("/api/admin/exams/reset-rounds", dependencies=[Depends(require_admin)])
def admin_reset_rounds(req: ResetRoundsRequest | None = None) -> dict[str, Any]:
    """清除考试轮次（已关闭/未发布专业）。slugs 为空则重置全部可重置专业。"""
    slugs = list(req.slugs) if req and req.slugs else None
    return exam_run_service.reset_rounds(slugs)


@app.get("/api/admin/papers/{slug}/exam-link", dependencies=[Depends(require_admin)])
def admin_paper_exam_link(slug: str, request: Request) -> dict[str, Any]:
    return exam_link(request, paper=slug)


@app.get("/api/admin/papers/{slug}/preview", dependencies=[Depends(require_admin)])
def admin_preview_paper(slug: str) -> dict[str, Any]:
    """管理员预览试卷（跳过开考验证，返回完整数据包括答案）"""
    paper_id = question_loader.validate_slug(slug)
    data = question_loader.load_questions(paper_id)
    if not data.get("questions"):
        raise _error(400, "EMPTY_QUESTION_BANK", "该专业试卷暂无题目")

    cfg = get_config().exam
    return {
        "paper_id": paper_id,
        "paper_name": data.get("paper_name"),
        "exam_info": data.get("exam_info", {}),
        "questions": data["questions"],
        "is_preview": True,
        "config": {
            "duration_minutes": cfg.duration_minutes,
            "auto_submit": cfg.auto_submit,
        },
    }


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
