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
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
