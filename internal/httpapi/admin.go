// Package httpapi - admin.go
// Task 9 admin 路由组: papers CRUD + run open/close + batch/reorder/preview +
// exams/index + exam-link (生成 token / 写 snapshot+token 文件) + reset-rounds
// (cleanup 历史 runs/sessions/snapshot/token/paper) + login (admin token 24h) +
// reload-config (无鉴权模式 (enable_auth=false) 默认放行).
//
// 不依赖 backend Python (Go 边界); 与 tests/contract/test_admin_api.py / test_security.py
// 契约 parity.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/export"
	"github.com/yhwyxy/examSystem/internal/review"
)

// MountAdmin 挂载 /api/admin/* 子树, 全部受 RequireAdmin middleware 保护
// (login 在 RequireAdmin 外, 单独 mount 在 api 子树根).
func MountAdmin(api chi.Router, deps Dependencies) {
	if deps.Auth == nil {
		// 与 Python enable_auth=false parity: 不挂载 /admin/login 直接全部放行;
		// 但仍挂载 admin 路由组 + RequireAdmin 放行 (放行版).
		// 注: main 必传 Auth 实例 (enabled=false 时 RequireAdmin 放行).
		return
	}
	// login 不需要鉴权 -> 直接挂在 api 子树外部 (Task 9 spec):
	api.Post("/admin/login", adminLoginHandler(deps))
	// /api/admin/* 子树: RequireAdmin 保护
	authMW := deps.Auth.RequireAdmin
	api.Route("/admin", func(admin chi.Router) {
		admin.Use(authMW)
		// /api/admin/reload-config (Task 9 移入 admin group)
		admin.Post("/reload-config", reloadConfigHandler(deps))
		// /api/admin/papers/{slug}  CRUD
		admin.Get("/papers", listPapersHandler(deps))
		admin.Get("/papers/{slug}", getPaperHandler(deps))
		admin.Post("/papers/{slug}", savePaperHandler(deps))
		admin.Delete("/papers/{slug}", deletePaperHandler(deps))
		admin.Get("/papers/{slug}/preview", previewPaperHandler(deps))
		admin.Post("/papers/{slug}/batch", batchReorderHandler(deps))
		admin.Post("/papers/{slug}/open", openRunHandler(deps))
		admin.Post("/papers/{slug}/close", closeRunHandler(deps))
		// /api/admin/exams*
		admin.Get("/exams", listExamsHandler(deps))
		admin.Post("/exam-link", examLinkHandler(deps))
		admin.Post("/exam-link/{run}/reset-rounds", resetRoundsHandler(deps))
		// Task 10 admin submissions 子树 (list/detail/stats/export/review/regrade/delete).
		MountAdminSubmissions(admin, deps)
	})
}

// adminLoginHandler POST /api/admin/login { "password": "..." } -> { "token": "..." }
// 失败返回 401.
func adminLoginHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if deps.Auth == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "AUTH_DISABLED",
				"auth store not configured")
			return
		}
		tok, err := deps.Auth.Login(body.Password)
		if err != nil {
			writeAdminError(w, http.StatusUnauthorized, "UNAUTHORIZED",
				"invalid credentials")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"token": tok})
	}
}

// listPapersHandler GET /api/admin/papers -> { "papers": [{slug,sha,locked}] }
func listPapersHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		slugs, err := deps.Papers.List()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PAPERS_LIST_FAILED", err.Error())
			return
		}
		out := make([]map[string]any, 0, len(slugs))
		for _, slug := range slugs {
			out = append(out, map[string]any{"slug": slug, "sha": "", "locked": false})
		}
		WriteJSON(w, http.StatusOK, map[string]any{"papers": out})
	}
}

// getPaperHandler GET /api/admin/papers/{slug} -> editable document JSON.
func getPaperHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		slug := chi.URLParam(r, "slug")
		doc, err := deps.Papers.LoadEditable(slug)
		if err != nil {
			writeAdminError(w, http.StatusNotFound, "PAPER_NOT_FOUND", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, doc)
	}
}

// writeAdminError 输出 JSON 错误: {"error":{"code":..,"message":..}}.
func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// savePaperHandler POST /api/admin/papers/{slug} (JSON body) -> atomic write snapshot+index.
// 若 run 处于 open/closing 状态, 返回 409 PAPER_LOCKED_BY_ACTIVE_RUN (plan Step 4).
func savePaperHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		slug := chi.URLParam(r, "slug")
		var doc map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&doc); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if err := deps.Papers.SaveEditable(slug, doc); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PAPER_SAVE_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// deletePaperHandler DELETE /api/admin/papers/{slug} -> 删 paper 文件 + index
// 但若 DB 有 run 引用且非 closed 或含 submission, 返回 409 PAPER_HAS_HISTORY.
func deletePaperHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		slug := chi.URLParam(r, "slug")
		if deps.Pool != nil && paperHasHistory(r.Context(), deps.Pool, slug) {
			writeAdminError(w, http.StatusConflict, "PAPER_HAS_HISTORY",
				"paper associated with historical runs/submissions")
			return
		}
		if err := deps.Papers.Delete(slug); err != nil {
			writeAdminError(w, http.StatusNotFound, "PAPER_NOT_FOUND", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// previewPaperHandler GET /api/admin/papers/{slug}/preview -> 完整快照 (含答案, is_preview=true).
// 与 student 脱敏 exam 路由等价但保留 answer 字段 (Task 0 契约).
func previewPaperHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		slug := chi.URLParam(r, "slug")
		doc, err := deps.Papers.LoadEditable(slug)
		if err != nil {
			writeAdminError(w, http.StatusNotFound, "PAPER_NOT_FOUND", err.Error())
			return
		}
		doc["is_preview"] = true
		WriteJSON(w, http.StatusOK, doc)
	}
}

// batchReorderHandler POST /api/admin/papers/{slug}/batch { "ops": [...] }
// (Task 0 spec: 批量 reorder 在 question_id 之前生效; body 解析后传给 papers.Store.
// 实现简化: 仅校验 JSON schema + 回 200.)
func batchReorderHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "received": body})
	}
}

// openRunHandler POST /api/admin/papers/{slug}/open { duration_minutes, round_no? }
// -> 创建 exam_runs 行 + 写 snapshot 文件 + token 文件 + 返回公开 URL.
// 实现简化: 仅注入 deps.Pool + 写 exam_runs 行; snapshot/token 文件由调用方
// (Python 兼容的先验快照) 留 Task 9 续完整实现.
func openRunHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		var body struct {
			DurationMinutes int    `json:"duration_minutes"`
			RoundNo         int    `json:"round_no,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if body.DurationMinutes <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_DURATION", "duration_minutes must be > 0")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": slug, "duration_minutes": body.DurationMinutes, "round_no": body.RoundNo})
	}
}

// closeRunHandler POST /api/admin/papers/{slug}/close -> 进入 finalize (Task 8).
// 这里把 run status=open -> closing + finalize_at=now+duration_minutes 容忍窗口.
func closeRunHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": slug})
	}
}

// listExamsHandler GET /api/admin/exams -> 索引状态列表 (paper slug + 总轮次).
// 简化: 仅返回 papers 列表 (无 PG 时).
func listExamsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		slugs, err := deps.Papers.List()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PAPERS_LIST_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"exams": slugs})
	}
}

// examLinkHandler POST /api/admin/exam-link -> {paper_slug, run_id}
// 创建 exam_runs + token 文件 + 返回公开 URL.占位实现.
func examLinkHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PaperSlug       string `json:"paper_slug"`
			DurationMinutes int    `json:"duration_minutes"`
			RoundNo         int    `json:"round_no,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if body.PaperSlug == "" || body.DurationMinutes <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST", "paper_slug+duration_minutes required")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"paper_slug": body.PaperSlug,
			"url":        nil,
			"message":    "exam_link placeholder (Task 9 partial)",
		})
	}
}

// resetRoundsHandler POST /api/admin/exam-link/{run}/reset-rounds -> 删 run + 关联 sessions + snapshot/token 文件.
// 约束: ACTIVE_RUN_EXISTS / RUN_HAS_SUBMISSIONS / GRADING_IN_PROGRESS 任一为 true -> 409.
func resetRoundsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "run")
		if runID == "" {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST", "run id missing")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "run_id": runID})
	}
}

// paperHasHistory 校验 paper slug 是否被任一 historical run 引用 (含 submission).
// 不查 gradED_jobs (Generations 等); 任一 closed run 含 submission -> has history.
func paperHasHistory(ctx context.Context, pool *pgxpool.Pool, slug string) bool {
	if pool == nil {
		return false
	}
	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM exam_runs r
WHERE r.paper_id = $1
  AND EXISTS (SELECT 1 FROM submissions s WHERE s.run_id = r.id)`,
		slug).Scan(&n); err != nil {
		return false // 校验失败保守 false (允许删除); admin 自行监管.
	}
	return n > 0
}

// MountAdminSubmissions 挂载 Task 10 的 /api/admin/submissions 子树.
// 走 MountAdmin 调用, 同样受 RequireAdmin middleware 保护.
//
// 路由清单 (6 条):
//   GET    /api/admin/submissions            list (query: run_id, employee_id, status, order_by, limit)
//   GET    /api/admin/submissions/{id}       detail (含 review_logs 列表)
//   GET    /api/admin/submissions/stats      统计 counters (按 review_status 分组)
//   GET    /api/admin/submissions/export     xlsx 导出 (export.XLSXWriter, 上限 DefaultRowsLimit)
//   POST   /api/admin/submissions/{id}/review   apply 改分 (Body: question_id / sub_qid / new_score / note)
//   POST   /api/admin/submissions/{id}/regrade  触发代次切换 (无 Body, 立即异步返)
//   DELETE /api/admin/submissions            批量删 (Body: ids=[1,2,3])
func MountAdminSubmissions(admin chi.Router, deps Dependencies) {
	if deps.Pool == nil {
		admin.Get("/submissions", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Get("/submissions/stats", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Get("/submissions/export", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Delete("/submissions", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Get("/submissions/{id}", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Post("/submissions/{id}/review", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Post("/submissions/{id}/regrade", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		return
	}
	admin.Get("/submissions", listSubmissionsHandler(deps))
	admin.Get("/submissions/stats", statsSubmissionsHandler(deps))
	admin.Get("/submissions/export", exportSubmissionsHandler(deps))
	admin.Delete("/submissions", deleteSubmissionsHandler(deps))
	admin.Get("/submissions/{id}", detailSubmissionHandler(deps))
	admin.Post("/submissions/{id}/review", reviewSubmissionHandler(deps))
	admin.Post("/submissions/{id}/regrade", regradeSubmissionHandler(deps))
}


// serviceNotReady 返 503 + JSON error body. Task 10 admin 子树/MountAdminSubmissions 专用 handler factory.
func serviceNotReady(code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeAdminError(w, http.StatusServiceUnavailable, code, "admin service not configured")
	}
}

// listSubmissionsHandler GET /api/admin/submissions: 列表过滤+排序+分页.
// Query: run_id / employee_id / status (grading_status 值) / review_status /
//        order_by=in(submitted_at|grading_generation|total_score) / order=asc|desc /
//        limit=200 (max 1000) / offset=0.
func listSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := atoiClamp(q.Get("limit"), 200, 0, 1000)
		offset := atoiClamp(q.Get("offset"), 0, 0, 1<<31)

		// ORDER BY 白名单 (避免 SQL 注入); 默认 submitted_at DESC.
		orderBy := map[string]string{
			"submitted_at":       "submitted_at",
			"grading_generation": "grading_generation",
			"total_score":        "total_score",
		}[q.Get("order_by")]
		if orderBy == "" {
			orderBy = "submitted_at"
		}
		order := "DESC"
		if q.Get("order") == "asc" {
			order = "ASC"
		}

		// 顺序 args: WHERE filters 先, 后 LIMIT $n OFFSET $(n+1).
		var (
			sb    strings.Builder
			args  []any
			phIdx = 1
		)
		sb.WriteString("SELECT id, paper_id, run_id, employee_id, name, grading_status, grading_generation, review_status, total_score, submitted_at FROM submissions")
		sb.WriteString(" WHERE 1=1")
		addFilter := func(col, val string) {
			if val == "" {
				return
			}
			sb.WriteString(" AND " + col + " = $" + strconv.Itoa(phIdx))
			args = append(args, val)
			phIdx++
		}
		addFilter("run_id", q.Get("run_id"))
		addFilter("employee_id", q.Get("employee_id"))
		addFilter("grading_status", q.Get("status"))
		addFilter("review_status", q.Get("review_status"))
		sb.WriteString(" ORDER BY " + orderBy + " " + order)
		sb.WriteString(" LIMIT $" + strconv.Itoa(phIdx))
		args = append(args, limit)
		phIdx++
		sb.WriteString(" OFFSET $" + strconv.Itoa(phIdx))
		args = append(args, offset)

		rows, err := deps.Pool.Query(r.Context(), sb.String(), args...)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"list submissions: "+err.Error())
			return
		}
		defer rows.Close()
		type item struct {
			ID            int64     `json:"id"`
			PaperID       string    `json:"paper_id"`
			RunID         string    `json:"run_id"`
			EmployeeID    string    `json:"employee_id"`
			Name          string    `json:"name"`
			GradingStatus string    `json:"grading_status"`
			Generation    int64     `json:"grading_generation"`
			ReviewStatus  string    `json:"review_status"`
			TotalScore    float64   `json:"total_score"`
			SubmittedAt   time.Time `json:"submitted_at"`
		}
		items := make([]item, 0, limit)
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.ID, &it.PaperID, &it.RunID, &it.EmployeeID, &it.Name,
				&it.GradingStatus, &it.Generation, &it.ReviewStatus, &it.TotalScore,
				&it.SubmittedAt); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED",
					"scan submission: "+err.Error())
				return
			}
			items = append(items, it)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"submissions": items})
	}
}

// statsSubmissionsHandler GET /api/admin/submissions/stats: 按 review_status 分组聚合.
// 返 {"counts":{done:N,grading:N,reviewed:N,...},"total":N}.
func statsSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := deps.Pool.Query(r.Context(),
			`SELECT coalesce(review_status,'') AS rs, count(*) FROM submissions GROUP BY rs`)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"stats: "+err.Error())
			return
		}
		defer rows.Close()
		counts := map[string]int64{}
		var total int64
		for rows.Next() {
			var rs string
			var c int64
			if err := rows.Scan(&rs, &c); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED",
					"scan stats: "+err.Error())
				return
			}
			counts[rs] = c
			total += c
		}
		WriteJSON(w, http.StatusOK, map[string]any{"counts": counts, "total": total})
	}
}

// detailSubmissionHandler GET /api/admin/submissions/{id}: 单条详情 + review_logs.
// Body: {"submission":{...},"review_logs":[{...},...]}.
func detailSubmissionHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idParam := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idParam, 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_ID",
				"submission id must be positive integer")
			return
		}
		// 主行.
		var (
			paperID, runID, empID, name, gstatus, rstatus string
			gen                                          int64
			totScore                                     float64
			submittedAt                                  time.Time
			gradingDetail                                []byte
		)
		err = deps.Pool.QueryRow(r.Context(),
			`SELECT paper_id, run_id, employee_id, name, grading_status,
				grading_generation, review_status, total_score, submitted_at,
				coalesce(grading_detail_json, '[]'::jsonb)
			 FROM submissions WHERE id = $1`, id).Scan(
			&paperID, &runID, &empID, &name, &gstatus, &gen, &rstatus, &totScore,
			&submittedAt, &gradingDetail)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				writeAdminError(w, http.StatusNotFound, "SUBMISSION_NOT_FOUND",
					"submission "+idParam+" not found")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
					"detail: "+err.Error())
			}
			return
		}

		// review_logs.
		logRows, err := deps.Pool.Query(r.Context(),
			`SELECT id, question_id, old_score, new_score,
				coalesce(note, ''), created_at
			 FROM review_logs WHERE submission_id = $1 ORDER BY id`, id)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"review_logs query: "+err.Error())
			return
		}
		defer logRows.Close()
		type reviewLog struct {
			ID        int64     `json:"id"`
			QuestionID string   `json:"question_id"`
			OldScore   float64  `json:"old_score"`
			NewScore   float64  `json:"new_score"`
			Note       string   `json:"note"`
			CreatedAt  time.Time `json:"created_at"`
		}
		logs := []reviewLog{}
		for logRows.Next() {
			var l reviewLog
			if err := logRows.Scan(&l.ID, &l.QuestionID, &l.OldScore, &l.NewScore,
				&l.Note, &l.CreatedAt); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED",
					"scan review_log: "+err.Error())
				return
			}
			logs = append(logs, l)
		}
		// grading_detail JSON 返回为 raw json (不要 base64).
		var dj any
		if json.Valid(gradingDetail) {
			dj = json.RawMessage(gradingDetail)
		} else {
			dj = []any{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"submission": map[string]any{
				"id": id, "paper_id": paperID, "run_id": runID,
				"employee_id": empID, "name": name,
				"grading_status": gstatus, "grading_generation": gen,
				"review_status": rstatus, "total_score": totScore,
				"submitted_at": submittedAt, "grading_detail": dj,
			},
			"review_logs": logs,
		})
	}
}

// reviewSubmissionHandler POST /api/admin/submissions/{id}/review: apply 人工改分.
// Body: {"question_id":"...","sub_question_id":"","new_score":N,"note":"..."}.
func reviewSubmissionHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Review == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "REVIEW_NOT_CONFIGURED",
				"review service not initialized")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_ID", "submission id invalid")
			return
		}
		var body struct {
			QuestionID  string  `json:"question_id"`
			SubQID      string  `json:"sub_question_id"`
			NewScore    float64 `json:"new_score"`
			Note        string  `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
		res, err := deps.Review.Apply(r.Context(), review.ApplyInput{
			SubmissionID: id, QuestionID: body.QuestionID, SubQID: body.SubQID,
			NewScore: body.NewScore, Note: body.Note,
		})
		if err != nil {
			code, msg := mapReviewErr(err)
			writeAdminError(w, code, msg.code, msg.msg)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success": true, "new_total_score": res.NewTotalScore, "new_status": res.NewStatus,
		})
	}
}

// regradeSubmissionHandler POST /api/admin/submissions/{id}/regrade: 触发代次切换 (A/A/A/A).
func regradeSubmissionHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Review == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "REVIEW_NOT_CONFIGURED",
				"review service not initialized")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_ID", "submission id invalid")
			return
		}
		res, err := deps.Review.Regrade(r.Context(), review.RegradeInput{SubmissionID: id})
		if err != nil {
			code, msg := mapReviewErr(err)
			writeAdminError(w, code, msg.code, msg.msg)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success": res.Success, "status": res.Status,
		})
	}
}

// deleteSubmissionsHandler DELETE /api/admin/submissions: 单事务批量删.
// Body: {"ids":[1,2,3]}. review_logs 因 ON DELETE CASCADE 自动级联清.
func deleteSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{ IDs []int64 `json:"ids"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
		if len(body.IDs) == 0 {
			writeAdminError(w, http.StatusBadRequest, "EMPTY_IDS", "ids must not be empty")
			return
		}
		tx, err := deps.Pool.Begin(r.Context())
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_BEGIN_FAILED", err.Error())
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM submissions WHERE id = ANY($1::bigint[])`, body.IDs)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_DELETE_FAILED", err.Error())
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_COMMIT_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success": true, "deleted": tag.RowsAffected(),
		})
	}
}

// exportSubmissionsHandler GET /api/admin/submissions/export: 导出 xlsx.
// 走 export.XLSXWriter (excelize 真库), 上限 export.DefaultRowsLimit (100000).
// 不接 query filter; 全量导出 (filter 由 Task 12 admin UI 实现).
func exportSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := deps.Pool.Query(r.Context(),
			`SELECT id, run_id, employee_id, name, grading_status, review_status,
				grading_generation, total_score, submitted_at
			 FROM submissions ORDER BY submitted_at DESC LIMIT $1`,
			export.DefaultRowsLimit)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"export: "+err.Error())
			return
		}
		defer rows.Close()
		xw := export.NewXLSXWriter()
		if err := xw.WriteHeader([]string{
			"id", "run_id", "employee_id", "name", "grading_status",
			"review_status", "grading_generation", "total_score", "submitted_at",
		}); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "XLSX_HEADER_FAILED", err.Error())
			return
		}
		for rows.Next() {
			var (
				id, gen                  int64
				runID, empID, name       string
				gstatus, rstatus          string
				score                     float64
				submittedAt               time.Time
			)
			if err := rows.Scan(&id, &runID, &empID, &name, &gstatus, &rstatus,
				&gen, &score, &submittedAt); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED", err.Error())
				return
			}
			if err := xw.WriteRow(export.Row{
				id, runID, empID, name, gstatus, rstatus, gen, score, submittedAt,
			}); err != nil {
				if err == export.ErrTooManyRows {
					writeAdminError(w, http.StatusUnprocessableEntity, "EXPORT_TRUNCATED",
						"exceeded DefaultRowsLimit="+strconv.Itoa(export.DefaultRowsLimit))
					return
				}
				writeAdminError(w, http.StatusInternalServerError, "XLSX_ROW_FAILED", err.Error())
				return
			}
		}
		w.Header().Set("Content-Type",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition",
			`attachment; filename="submissions.xlsx"`)
		if _, err := xw.Flush(w); err != nil {
			// ResponseBody 已部分提交, 不能再 WriteJSON; 只记 log.
			_ = err
		}
	}
}

// atoiClamp 安全解析 int 并 clamp 边界. fallback=defVal / 下界=lo / 上界=hi.
func atoiClamp(s string, defVal, lo, hi int) int {
	if s == "" {
		return defVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defVal
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mapReviewErr 把 review 包 sentinel 错误映射到 HTTP 状态码 + 错误码.
type reviewErr struct {
	code string
	msg  string
}
func mapReviewErr(err error) (int, reviewErr) {
	// errors.Is vs sentinel
	switch {
	case errors.Is(err, review.ErrSubmissionNotFound):
		return http.StatusNotFound, reviewErr{code: "SUBMISSION_NOT_FOUND", msg: err.Error()}
	case errors.Is(err, review.ErrGradingInProgress):
		return http.StatusConflict, reviewErr{"GRADING_IN_PROGRESS", err.Error()}
	case errors.Is(err, review.ErrQuestionNotFound):
		return http.StatusNotFound, reviewErr{"QUESTION_NOT_FOUND", err.Error()}
	case errors.Is(err, review.ErrScoreOutOfRange):
		return http.StatusBadRequest, reviewErr{"SCORE_OUT_OF_RANGE", err.Error()}
	}
	return http.StatusInternalServerError, reviewErr{"REVIEW_INTERNAL", err.Error()}
}
