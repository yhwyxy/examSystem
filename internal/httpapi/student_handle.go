//go:build !no_student_exam

// student_handle.go 提供 /api/exam/* 4 个公开 + 鉴权端点 handler (Task 12a).
//
// Plan 6-10 已实现 sessions.Service 完整业务编排 (StartOrResume / SaveDraft /
// Status / SubmitManual), 但 HTTP 层只挂了 /api/session/submit 与
// /api/submission/{id}/status 两条 post-submit 路径. 公开 student API 缺:
//
//   GET  /api/exam?paper={slug}&run={run_token}        取脱敏卷形 (papers.SanitizeForStudent)
//   POST /api/exam/start                                开始 / 幂等续接会话 (StartOrResume)
//   PUT  /api/exam/sessions/{id}/draft                 草稿保存 (SaveDraft, 学生成 PUT 路径)
//   GET  /api/exam/sessions/{id}/status                会话 + run 状态轮询 (Status)
//
// 业务编排已在 sessions 包内完成 (Task 6-7 fencing 含), 这里仅 HTTP 层薄包装.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/yhwyxy/examSystem/internal/ratelimit"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
)


// mountStudentExam 挂 /api/exam/* 4 个公开 + 鉴权路由到 chi.Router 上.
// 调用方: NewRouter 的 api subtree 内, 在 health / reload-config 之后.
func mountStudentExam(api chi.Router, deps Dependencies) {
	if deps.Sessions == nil {
		// 503 占位, 防止路由 404 误导前端
		api.Get("/exam", serviceNotReady("SESSIONS_NOT_CONFIGURED"))
		api.Post("/exam/start", serviceNotReady("SESSIONS_NOT_CONFIGURED"))
		api.Put("/exam/sessions/{id}/draft", serviceNotReady("SESSIONS_NOT_CONFIGURED"))
		api.Get("/exam/sessions/{id}/status", serviceNotReady("SESSIONS_NOT_CONFIGURED"))
		return
	}
	api.Get("/exam", rateLimitByIP(deps.RateLimiter, ratelimit.PresetPublic,
		"exam-get", getPublicExamHandler(deps)))
	api.Post("/exam/start", rateLimitByIP(deps.RateLimiter, ratelimit.PresetPublic,
		"exam-start", startExamHandler(deps)))
	api.Put("/exam/sessions/{id}/draft", rateLimitBySessionID(deps.RateLimiter,
		ratelimit.PresetDraftStatus, "draft", putDraftHandler(deps)))
	api.Get("/exam/sessions/{id}/status", rateLimitBySessionID(deps.RateLimiter,
		ratelimit.PresetDraftStatus, "status", getSessionStatusHandler(deps)))
}


// getPublicExamHandler GET /api/exam?paper={slug}&run={run_token}
// 步骤: FindByToken -> runs.GetPublicExam (读 run 冻结快照 + hash 校验 + 脱敏).
// 必须走 run 快照而非 papers.LoadSnapshot(可编辑卷): 开考后管理员编辑试卷不得
// 影响进行中的 run, 否则可编辑卷字节变化 -> hash 校验失败 -> 全体学生取卷 500.
func getPublicExamHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		runToken := q.Get("run")
		if runToken == "" || deps.RunService == nil || deps.Pool == nil {
			writeAdminError(w, http.StatusBadRequest, "MISSING_QUERY",
				"both paper slug & run token required")
			return
		}
		var exam *runs.PublicExamResult
		err := withTx(r.Context(), deps.Pool, func(tx pgx.Tx) error {
			run, err := deps.RunService.FindByToken(r.Context(), tx, runToken)
			if err != nil {
				return err
			}
			exam, err = deps.RunService.GetPublicExam(r.Context(), tx, run)
			return err
		})
		if err != nil {
			if errors.Is(err, runs.ErrTokenNotFound) {
				writeAdminError(w, http.StatusUnauthorized, "INVALID_RUN_TOKEN",
					"run token invalid")
				return
			}
			writeAdminError(w, http.StatusInternalServerError, "EXAM_LOAD_FAILED",
				"load exam: "+err.Error())
			return
		}
		var finalizeAt any
		if exam.FinalizeAt != nil {
			finalizeAt = exam.FinalizeAt
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"paper_id":         exam.PaperID,
			"run_id":           exam.RunID,
			"round_no":         exam.RoundNo,
			"paper_name":       exam.PaperName,
			"run_status":       exam.RunStatus,
			"closed":           exam.Closed,
			"duration_minutes": exam.DurationMinutes,
			"finalize_at":      finalizeAt,
			"deadline_at":      finalizeAt,
			"server_time":      exam.ServerTime,
			"auto_submit":      exam.AutoSubmit,
			"exam_info":        exam.ExamInfo,
			"questions":        exam.Questions,
		})
	}
}


// startExamHandler POST /api/exam/start
// Body: {run_token, employee_id, name, department?}; http 取 X-Forwarded-For / User-Agent.
// 响应{session_id, session_token, started_at, deadline_at, draft_revision,
//        answers, run_status, session_status, created}.
// 幂等: 同 run+employee 已有活动 session 不另发 token, session_token 返 null
// (与 Python 基线 test_save_draft_idempotent_resume 契约一致).
func startExamHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RunToken   string `json:"run_token"`
			EmployeeID string `json:"employee_id"`
			Name       string `json:"name"`
			Department string `json:"department"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
		if body.RunToken == "" || body.EmployeeID == "" {
			writeAdminError(w, http.StatusBadRequest, "MISSING_FIELD",
				"run_token and employee_id required")
			return
		}
		clientIP := r.Header.Get("X-Forwarded-For")
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}
		ua := r.UserAgent()
		res, err := deps.Sessions.StartOrResume(r.Context(), deps.Pool,
			body.RunToken, body.EmployeeID, body.Name,
			nullableString(body.Department),
			nullableString(clientIP),
			nullableString(ua))
		if err != nil {
			writeAdminError(w, mapStartErrCode(err), "START_FAILED", err.Error())
			return
		}
		// session_token 可能为空 (恢复时, Python 基线显式返 null).
		var stok any
		if res.SessionToken != "" {
			stok = res.SessionToken
		}
		// deadline_at 用 res.Deadline; answers 用 res.Draft 原始 JSON (前端 schema-free 解析).
		var answersRaw any
		if json.Valid(res.Draft) {
			answersRaw = json.RawMessage(res.Draft)
		}
		// run_status 来自 res.RunStatus; session_status 从 RunStatus 派生:
		//   open/closing=active->"active" / closed->"closed".
		sessStatus := "active"
		if res.RunStatus == "closed" {
			sessStatus = "closed"
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"session_id":      res.SessionID,
			"session_token":   stok,
			"started_at":       res.StartedAt,
			"deadline_at":      res.Deadline,
			"draft_revision":   res.DraftRevision,
			"answers":          answersRaw,
			"run_status":       res.RunStatus,
			"session_status":   sessStatus,
			"created":          res.NewSession,
		})
	}
}


// putDraftHandler PUT /api/exam/sessions/{id}/draft Body={session_token, revision, answers}.
// revision 是"客户端新版本号" (本地 revision+1); 服务端 CAS 要求当前 == revision-1.
// 响应{success, draft_revision, draft_saved_at, run_status, session_status, finalize_at}.
// CAS 失败回 409 STALE_DRAFT_REVISION + detail.current_revision (前端自愈通道).
func putDraftHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionToken string          `json:"session_token"`
			Revision     int             `json:"revision"`
			Answers      json.RawMessage `json:"answers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
		if body.SessionToken == "" {
			writeAdminError(w, http.StatusUnauthorized, "INVALID_SESSION_TOKEN",
				"session_token required")
			return
		}
		res, err := deps.Sessions.SaveDraft(r.Context(), deps.Pool,
			body.SessionToken, body.Revision, body.Answers)
		if err != nil {
			if errors.Is(err, sessions.ErrStaleDraftRevision) {
				cur := 0
				if res != nil {
					cur = res.Revision
				}
				writeStaleDraftError(w, cur)
				return
			}
			writeAdminError(w, mapSessionErrCode(err), sessionErrCodeName(err), err.Error())
			return
		}
		var finalizeAt any
		if res.FinalizeAt != nil {
			finalizeAt = res.FinalizeAt
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success":        !res.Throttled,
			"draft_revision": res.Revision,
			"draft_saved_at": res.DraftSavedAt,
			"run_status":     res.RunStatus,
			"session_status": res.SessionStatus,
			"finalize_at":    finalizeAt,
			"throttled":      res.Throttled,
		})
	}
}

// writeStaleDraftError 输出 409 STALE_DRAFT_REVISION + current_revision.
// 前端 exam.js 读 detail.code == 'STALE_DRAFT_REVISION' 与 detail.current_revision
// 同步本地 revision 后下轮重发 —— 这是草稿乐观锁的自愈通道.
func writeStaleDraftError(w http.ResponseWriter, currentRevision int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	body := map[string]any{
		"code":             "STALE_DRAFT_REVISION",
		"message":          "draft revision stale; sync current_revision and retry",
		"current_revision": currentRevision,
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body, "detail": body})
}


// getSessionStatusHandler GET /api/exam/sessions/{id}/status?session_token=...
// 响应 sessions.StatusResult (json tag 已对齐前端 snake_case 契约):
// {session_id, session_status, run_status, started_at, deadline_at, draft_revision,
//  draft_saved_at, finalize_at, submission_id, answers, server_time}.
func getSessionStatusHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stok := r.URL.Query().Get("session_token")
		if stok == "" {
			writeAdminError(w, http.StatusUnauthorized, "INVALID_SESSION_TOKEN",
				"session_token query required")
			return
		}
		res, err := deps.Sessions.Status(r.Context(), deps.Pool, stok)
		if err != nil {
			writeAdminError(w, mapSessionErrCode(err), sessionErrCodeName(err), err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}


// mapStartErrCode 把 sessions.StartOrResume 错误映射到 HTTP 状态码.
// ErrInvalidSessionToken/r -> 401; ErrRunClosed -> 409; 其他 -> 500.
func mapStartErrCode(err error) int {
	if errors.Is(err, sessions.ErrInvalidSessionToken) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, sessions.ErrRunClosed) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}


// mapSessionErrCode 把 sessions.SaveDraft / Status 错误映射到 HTTP 状态码.
// ErrInvalidSessionToken -> 401; ErrStaleDraftRevision / ErrRunClosed /
// ErrSessionSubmitted -> 409; ErrSessionNotFound -> 404; 其他 -> 500.
func mapSessionErrCode(err error) int {
	if errors.Is(err, sessions.ErrInvalidSessionToken) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, sessions.ErrStaleDraftRevision) ||
		errors.Is(err, sessions.ErrRunClosed) ||
		errors.Is(err, sessions.ErrSessionSubmitted) ||
		errors.Is(err, sessions.ErrActiveExists) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// sessionErrCodeName 把 sessions 包 sentinel 错误映射到前端可识别的错误码字符串.
// 前端 exam.js 依赖 RUN_CLOSED / RUN_CLOSING / SESSION_SUBMITTED 等 code 做锁卷.
func sessionErrCodeName(err error) string {
	switch {
	case errors.Is(err, sessions.ErrInvalidSessionToken):
		return "INVALID_SESSION_TOKEN"
	case errors.Is(err, sessions.ErrSessionNotFound):
		return "SESSION_NOT_FOUND"
	case errors.Is(err, sessions.ErrSessionSubmitted):
		return "SESSION_SUBMITTED"
	case errors.Is(err, sessions.ErrStaleDraftRevision):
		return "STALE_DRAFT_REVISION"
	case errors.Is(err, sessions.ErrRunClosed):
		return "RUN_CLOSED"
	}
	return "SESSION_INTERNAL"
}


// nullableString 把 non-empty string -> *string (空 string -> nil). 用于 sessions
// StartOrResume/SaveDraft 入参 *string 形.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
