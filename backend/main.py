"""FastAPI 入口。"""

from __future__ import annotations

import hashlib
import logging
import secrets
import threading
from concurrent.futures import ThreadPoolExecutor
import time
from collections import defaultdict
from datetime import datetime, timezone
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, JSONResponse
from pydantic import BaseModel, Field
from fastapi.concurrency import run_in_threadpool

from . import database, exporter, grader, question_loader
from .config import get_config, reload_config
from .utils import generate_qr_base64, get_lan_ip, parse_iso

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
app.add_middleware(
    CORSMiddleware,
    allow_origins=cfg.allow_origins,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
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
    expire = _admin_tokens.get(token)
    if expire is None:
        raise HTTPException(status_code=401, detail={"code": "INVALID_TOKEN", "message": "Token 无效或已过期"})
    
    if time.time() > expire:
        _admin_tokens.pop(token, None)
        raise HTTPException(status_code=401, detail={"code": "TOKEN_EXPIRED", "message": "Token 已过期，请重新登录"})

# ---------------------------------------------------------------------------
# 简易内存速率限制器
# ---------------------------------------------------------------------------
_rate_lock = threading.Lock()
_rate_store: dict[str, list[float]] = defaultdict(list)

def _check_rate_limit(ip: str, max_requests: int = 60, window_seconds: int = 60) -> None:
    """每 IP 每窗口最多 max_requests 次请求，超出返回 429。"""
    now = time.monotonic()
    with _rate_lock:
        timestamps = _rate_store[ip]
        # 清理过期记录
        _rate_store[ip] = [t for t in timestamps if now - t < window_seconds]
        if len(_rate_store[ip]) >= max_requests:
            raise HTTPException(status_code=429, detail="请求过于频繁，请稍后再试")
        _rate_store[ip].append(now)

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
    department: str | None = None
    answers: dict[str, Any] = Field(default_factory=dict)
    started_at: str | None = None  # 保留字段，但不再用于时间校验


class ExamStartRequest(BaseModel):
    employee_id: str


class ReviewRequest(BaseModel):
    submission_id: int
    question_id: str
    new_score: float
    note: str | None = None


# 评分并发上限：防止「提交即新建线程 + 新建事件循环」导致
# 百人并发时 OOM / 事件循环爆炸 / LLM 服务被打满。
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


def schedule_grading(submission_id: int, answers: dict[str, Any]) -> None:
    """后台线程执行评分，保持提交接口快速返回。

    使用全局有界线程池（默认 4 workers）+ 复用单个事件循环模式，
    避免每次提交新建线程/事件循环导致资源��尽。
    队满时提交请求排队等待，不会丢失。
    """
    def _background_grade(sub_id: int, submitted_answers: dict[str, Any]) -> None:
        # 复用调用线程的事件循环：grader.grade_submission 是 async，
        # 但在线程池 worker 中无 running loop，新建一个并立即关闭。
        # 相比旧实现，线程数受 max_workers 约束，避免无界增长。
        import asyncio
        loop = asyncio.new_event_loop()
        try:
            result = loop.run_until_complete(grader.grade_submission(submitted_answers))
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
            logger.info("后台评分完成: submission_id=%d, status=%s", sub_id, result.review_status)
        except Exception:
            logger.exception("后台评分失败: submission_id=%d", sub_id)
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
        _get_grading_executor().submit(_background_grade, submission_id, answers)
    except RuntimeError:
        # executor 已关闭（如进程退出），降级同步评分避免丢任务
        logger.warning("评分线程池不可用，降级同步评分: submission_id=%d", submission_id)
        _background_grade(submission_id, answers)


# ---------------------------------------------------------------------------
# 中间件：全局速率限制
# ---------------------------------------------------------------------------

@app.middleware("http")
async def rate_limit_middleware(request: Request, call_next):
    try:
        _check_rate_limit(request.client.host if request.client else "unknown")
    except HTTPException:
        return JSONResponse(status_code=429, content={"detail": "请求过于频繁，请稍后再试"})
    return await call_next(request)


# ---------------------------------------------------------------------------
# 考试端
# ---------------------------------------------------------------------------

@app.get("/api/exam")
def get_exam(request: Request) -> dict[str, Any]:
    _assert_global_time_window()
    cfg = get_config().exam
    data = question_loader.public_exam_payload()
    server_time = datetime.now(timezone.utc).isoformat()
    return {
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
    now = datetime.now(timezone.utc)
    database.upsert_exam_session(employee_id=employee_id, started_at=now.isoformat())
    return {"started_at": now.isoformat(), "server_time": now.isoformat()}


@app.post("/api/submit")
def submit(req: SubmitRequest, request: Request) -> dict[str, Any]:
    _assert_global_time_window()
    cfg = get_config().exam

    # 使用服务器记录的开始时间进行超时校验，不信任客户端 started_at
    employee_id = req.employee_id.strip()
    started_at_iso = database.pop_exam_session(employee_id)
    if started_at_iso is None:
        raise _error(403, "EXAM_NOT_STARTED", "请先开始考试")
    server_start = parse_iso(started_at_iso)

    now = datetime.now(timezone.utc)
    elapsed = (now - server_start).total_seconds()
    if elapsed < -60:
        raise _error(403, "EXAM_TIMEOUT", "考试时间异常，提交被拒绝")
    if elapsed > cfg.duration_minutes * 60 + cfg.grace_period_seconds:
        raise _error(403, "EXAM_TIMEOUT", "考试已超时，提交被拒绝")

    # 直接 INSERT，依赖数据库 UNIQUE 约束防止重复提交
    try:
        submission_id = database.insert_submission_pending(
            name=req.name.strip(),
            employee_id=employee_id,
            department=req.department.strip() if req.department else None,
            answers=req.answers,
            started_at=req.started_at,
            client_ip=request.client.host if request.client else None,
            user_agent=request.headers.get("user-agent"),
        )
    except Exception as e:
        if "UNIQUE" in str(e).upper():
            raise _error(409, "DUPLICATE_SUBMISSION", "该员工已提交，不能重复提交")
        logger.exception("提交保存失败")
        raise _error(500, "SUBMIT_FAILED", "提交失败，请联系管理员")

    schedule_grading(submission_id, req.answers)

    return {
        "success": True,
        "submission_id": submission_id,
        "status": "grading",
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
        raise HTTPException(status_code=500, detail={"code": "NO_PASSWORD", "message": "管理员密码未配置"})
    
    # 验证密码（支持明文和 SHA-256 哈希）
    pwd_hash = hashlib.sha256(req.password.encode()).hexdigest()
    if req.password != cfg.password and pwd_hash != cfg.password:
        raise HTTPException(status_code=401, detail={"code": "WRONG_PASSWORD", "message": "密码错误"})
    
    # 生成 Token
    token = secrets.token_urlsafe(32)
    _admin_tokens[token] = time.time() + _TOKEN_TTL
    return {"success": True, "token": token}


@app.get("/api/admin/exam-link", dependencies=[Depends(require_admin)])
def exam_link(request: Request) -> dict[str, Any]:
    """返回考试链接和二维码，供管理员发布考试。"""
    cfg = get_config().server
    host = request.url.hostname or get_lan_ip()
    if host in {"127.0.0.1", "localhost"}:
        host = get_lan_ip()
    url = f"http://{host}:{cfg.port}/exam"
    return {"url": url, "qr_base64": generate_qr_base64(url)}


@app.get("/api/admin/stats", dependencies=[Depends(require_admin)])
def admin_stats() -> dict[str, Any]:
    return database.get_stats()


@app.get("/api/admin/submissions", dependencies=[Depends(require_admin)])
def admin_submissions(
    keyword: str | None = None,
    review_status: str | None = None,
    sort_by: str | None = None,
    order: str | None = None,
    limit: int = 100,
    offset: int = 0,
) -> list[dict[str, Any]]:
    """支持分页的成绩列表。"""
    limit = max(1, min(limit, 500))
    offset = max(0, offset)
    return database.list_submissions(
        keyword=keyword, review_status=review_status,
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
    result = database.apply_review(submission_id=req.submission_id, question_id=req.question_id, new_score=req.new_score, note=req.note)
    if not result.get("success"):
        raise _error(400, result.get("code", "REVIEW_FAILED"), result.get("message", "复核失败"))
    return result


@app.post("/api/admin/regrade/{submission_id}", dependencies=[Depends(require_admin)])
def admin_regrade(submission_id: int) -> dict[str, Any]:
    submission = database.get_submission(submission_id)
    if not submission:
        raise _error(404, "NOT_FOUND", "提交记录不存在")

    answers = submission.get("answers") or {}
    cfg = get_config()

    import asyncio
    loop = asyncio.new_event_loop()
    try:
        result = loop.run_until_complete(grader.grade_submission(answers))
    finally:
        loop.close()

    existing_detail = submission.get("grading_detail") or []
    manual_map = {(d.get("question_id")): d for d in existing_detail if d.get("reviewed_by")}

    merged: list[dict[str, Any]] = []
    for detail in result.grading_detail:
        qid = detail.get("question_id")
        if qid in manual_map:
            detail = {**detail, **manual_map[qid]}
        merged.append(detail)

    database.save_grading_result(submission_id, {
        "objective_score": result.objective_score,
        "subjective_score_machine": result.subjective_score_machine,
        "subjective_score_final": result.subjective_score_final,
        "total_score": result.total_score,
        "review_status": result.review_status,
        "grading_detail": merged,
        "pending_ids": [d.get("question_id") for d in merged if d.get("review_status") == "pending"],
        "low_confidence_count": sum(1 for d in merged if d.get("low_confidence")),
    })

    return {
        "success": True,
        "submission_id": submission_id,
        "objective_score": result.objective_score,
        "subjective_score_machine": result.subjective_score_machine,
        "total_score": result.total_score,
    }


@app.get("/api/admin/export", dependencies=[Depends(require_admin)])
def admin_export() -> Any:
    export_cfg = get_config().export
    if export_cfg.format == "xlsx":
        from fastapi.responses import Response
        content = exporter.export_submissions_xlsx()
        return Response(
            content=content,
            media_type="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
            headers={"Content-Disposition": "attachment; filename=exam_results.xlsx"},
        )
    return {"message": "仅支持 xlsx 导出"}


@app.post("/api/admin/reload-questions", dependencies=[Depends(require_admin)])
def reload_questions_api() -> dict[str, Any]:
    try:
        data = question_loader.reload_questions()
        return {"success": True, "count": len(data["questions"])}
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
    return FileResponse("frontend/exam.html")


@app.get("/exam")
def exam_page():
    return FileResponse("frontend/exam.html")


@app.get("/admin")
def admin_page():
    return FileResponse("frontend/admin.html")


@app.get("/detail")
def detail_page():
    return FileResponse("frontend/detail.html")


from starlette.staticfiles import StaticFiles
app.mount("/css", StaticFiles(directory="frontend/css"), name="css")
app.mount("/js", StaticFiles(directory="frontend/js"), name="js")
