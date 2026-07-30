// Package httpapi - student.go
// Task 6 学生提交 HTTP 路径:
//   POST /api/session/submit         -> submitHandler (调 submissions.Service.Submit)
//   GET  /api/submission/{id}/status -> submissionStatusHandler (调 GetByID)
//
// 编排权威在 submissions.Service.Submit; handler 仅做 request/response + façade-level
// 错误映射 (validation / 未注入 -> 400/503; 业务错误按 submissions.* 错).
package httpapi

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/submissions"
)

// submitHandler 处理 POST /api/session/submit.
// Body 规范 (plan Task 0): { "session_token": "...", "answers": { ... canonical answers ... }  }.
// cliente ip / user-agent 由 server 提取; auto_submit_reason 在自动提交时由 server 设.
func submitHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := deps.SubmissionsService
		if svc == nil {
			writeStudentError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "submissions service not configured")
			return
		}
		var body struct {
			SessionToken     string          `json:"session_token"`
			Answers          json.RawMessage `json:"answers"`
			AutoSubmitReason *string         `json:"auto_submit_reason,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeStudentError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		answersMap, err := parseAnswersMap(body.Answers)
		if err != nil {
			writeStudentError(w, http.StatusBadRequest, "INVALID_ANSWERS", err.Error())
			return
		}
		clientIP := clientIPFromRequest(r)
		ua := userAgentFromRequest(r)

		res, err := svc.Submit(r.Context(), submissions.SubmitRequest{
			SessionToken:      body.SessionToken,
			Answers:           body.Answers,
			AnswersMap:        answersMap,
			ClientIP:          clientIP,
			UserAgent:         ua,
			AutoSubmitReason:  body.AutoSubmitReason,
		})
		if err != nil {
			submitErrorToHTTP(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"submission_id": res.SubmissionID,
		})
	}
}

// submissionStatusHandler 处理 GET /api/submission/{id}/status (plan Step 4: 仅 4 字段).
// 响应: { submission_id, grading_status, review_status, objective_score }.
// 不暴露评分原始数组 / 错误 / 身份.
func submissionStatusHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := deps.SubmissionsService
		if svc == nil {
			writeStudentError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "submissions service not configured")
			return
		}
		id := chi.URLParam(r, "id")
		if id == "" {
			writeStudentError(w, http.StatusBadRequest, "INVALID_ID", "submission id missing")
			return
		}
		status, err := svc.GetStatus(r.Context(), id)
		if err != nil {
			submitErrorToHTTP(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(status)
	}
}

// parseAnswersMap 把 raw answers JSON 解码为 map[string]any (供 objective.Grade 用).
// 空 / 非对象 (数组 / 字符串) 视为 INVALID_ANSWERS.
func parseAnswersMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// clientIPFromRequest 抽取 X-Forwarded-For 第一个或 RemoteAddr (去端口).
func clientIPFromRequest(r *http.Request) *string {
	ip := r.Header.Get("X-Forwarded-For")
	if i := strings.IndexByte(ip, ','); i >= 0 {
		ip = ip[:i]
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		ip = r.RemoteAddr
		if h, _, err := net.SplitHostPort(ip); err == nil {
			ip = h
		}
	}
	if ip == "" {
		return nil
	}
	return &ip
}

// userAgentFromRequest 抽取 User-Agent (空即 nil).
func userAgentFromRequest(r *http.Request) *string {
	ua := r.UserAgent()
	if ua == "" {
		return nil
	}
	return &ua
}

// submitErrorToHTTP 把 submissions 包业务错映射到 HTTP 响应 (plan Step 5 详细定义).
func submitErrorToHTTP(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, submissions.ErrInvalidSessionToken):
		writeStudentError(w, http.StatusUnauthorized, "INVALID_SESSION_TOKEN", "session token invalid")
	case errors.Is(err, submissions.ErrSubmissionClosed):
		writeStudentError(w, http.StatusConflict, "SUBMISSION_CLOSED", "session or run already closed")
	case errors.Is(err, submissions.ErrDuplicateSubmission):
		writeStudentError(w, http.StatusConflict, "DUPLICATE_SUBMISSION", "submission already exists for this run+employee")
	case errors.Is(err, submissions.ErrInvalidAnswers):
		writeStudentError(w, http.StatusBadRequest, "INVALID_ANSWERS", "answers JSON invalid")
	case errors.Is(err, submissions.ErrInvalidAutoSubmitReason):
		writeStudentError(w, http.StatusBadRequest, "INVALID_AUTO_SUBMIT_REASON", "auto_submit_reason not allowed")
	case errors.Is(err, submissions.ErrSubmitDisabled):
		writeStudentError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "submissions service disabled")
	case errors.Is(err, submissions.ErrSnapshotUnavailable):
		writeStudentError(w, http.StatusInternalServerError, "SNAPSHOT_UNAVAILABLE", "grading snapshot unavailable; submission not recorded, please retry")
	case errors.Is(err, submissions.ErrNoRows):
		writeStudentError(w, http.StatusNotFound, "NOT_FOUND", "submission not found")
	default:
		writeStudentError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}

// writeStudentError 输出 JSON 错误: {"error":{...}} 兼容现有 WriteError 风格.
func writeStudentError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

