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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/papers"
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
	api.Get("/exam", getPublicExamHandler(deps))
	api.Post("/exam/start", startExamHandler(deps))
	api.Put("/exam/sessions/{id}/draft", putDraftHandler(deps))
	api.Get("/exam/sessions/{id}/status", getSessionStatusHandler(deps))
}


// getPublicExamHandler GET /api/exam?paper={slug}&run={run_token}
// 步骤: Find run by run_token -> LoadSnapshot(脱敏前的真快照) -> SanitizeForStudent.
// 返 {"paper_id","round_no","duration_minutes","deadline_at","questions":[...脱敏...]}.
func getPublicExamHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		runToken := q.Get("run")
		if runToken == "" || deps.Runs == nil || deps.Papers == nil {
			writeAdminError(w, http.StatusBadRequest, "MISSING_QUERY",
				"both paper slug & run token required")
			return
		}
		// Hash run_token (sha256 hex) 与 internal/sessions/service.go hashToken 一致
		// -> runs.FindByPublicTokenHash (repo 入口接 hash, 不接明文 token).
		tokenHash := hashRunToken(runToken)
		run, err := deps.Runs.FindByPublicTokenHash(r.Context(), nil, tokenHash)
		if err != nil {
			writeAdminError(w, http.StatusUnauthorized, "INVALID_RUN_TOKEN",
				"run_token lookup failed: "+err.Error())
			return
		}
		// run 必须含 snapshot_path + snapshot_hash (paper 包提供).
		snap, err := deps.Papers.LoadSnapshot(run.PaperID, run.SnapshotHash)
		if err != nil || snap == nil {
			writeAdminError(w, http.StatusInternalServerError, "SNAPSHOT_LOAD_FAILED",
				"load snapshot: "+err.Error())
			return
		}
		// SanitizeForStudent 统一脱敏 (Question=map[string]any; questions 切片).
		// 先按 []papers.Question 强断言; 不行则按 []any 安全转拷贝.
		var questions []papers.Question
		switch qs := snap.Doc["questions"].(type) {
		case []papers.Question:
			questions = qs
		case []any:
			questions = make([]papers.Question, 0, len(qs))
			for _, q := range qs {
				if qq, ok := q.(papers.Question); ok {
					questions = append(questions, qq)
				} else if qq, ok := q.(map[string]any); ok {
					questions = append(questions, qq)
				}
			}
		default:
			questions = []papers.Question{}
		}
		sanitized := papers.SanitizeForStudent(questions)
		// 响应 deadline_at 用 run.FinalizeAt (closing/closed 时是 finalize 时间).
		var deadlineAt any
		if run.FinalizeAt != nil {
			deadlineAt = run.FinalizeAt
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"paper_id":          run.PaperID,
			"round_no":          run.RoundNo,
			"duration_minutes":  run.DurationMinutes,
			"deadline_at":       deadlineAt,
			"questions":         sanitized,
		})
	}
}

// hashRunToken 用 sha256 hex (与 internal/sessions/service.go hashToken 一致,
// internal/runs/repository 传入 sha256(token) hex).
func hashRunToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
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
			"started_at":       res.Deadline,
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
// 响应{success, draft_revision, draft_saved_at, run_status, finalize_at}.
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
			writeAdminError(w, mapSessionErrCode(err), "SAVE_DRAFT_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success":         true,
			"draft_revision":  res.Revision,
			"draft_saved_at":  res.DraftSavedAt,
			"throttled":       res.Throttled,
		})
	}
}


// getSessionStatusHandler GET /api/exam/sessions/{id}/status?session_token=...
// 响应 sessions.StatusResult: {started_at, deadline_at, draft_revision, draft_saved_at,
//   finalize_at, run_status, session_status, answers, time_remaining, drift_seconds}.
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
			writeAdminError(w, mapSessionErrCode(err), "STATUS_FAILED", err.Error())
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
// ErrInvalidSessionToken -> 401; ErrRevisionMismatch (stale draft) -> 409;
// ErrRunClosed -> 409; 其他 -> 500.
func mapSessionErrCode(err error) int {
	if errors.Is(err, sessions.ErrInvalidSessionToken) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, sessions.ErrRunClosed) ||
		errors.Is(err, sessions.ErrActiveExists) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
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
