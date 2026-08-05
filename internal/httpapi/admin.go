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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skip2/go-qrcode"
	"github.com/yhwyxy/examSystem/internal/export"
	"github.com/yhwyxy/examSystem/internal/ratelimit"
	"github.com/yhwyxy/examSystem/internal/review"
	"github.com/yhwyxy/examSystem/internal/runs"
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
	// login 不需要鉴权 -> 直接挂在 api 子树外部 (Task 9 spec);
	// 但必须限流 (10/min burst 3 per IP), 否则口令可无限暴破.
	api.Post("/admin/login", rateLimitByIP(deps.RateLimiter, ratelimit.PresetStart,
		"admin-login", adminLoginHandler(deps)))
	// /api/admin/* 子树: RequireAdmin 保护
	authMW := deps.Auth.RequireAdmin
	api.Route("/admin", func(admin chi.Router) {
		admin.Use(authMW)
		// /api/admin/reload-config (Task 9 移入 admin group)
		admin.Post("/reload-config", reloadConfigHandler(deps))
		// /api/admin/papers/{slug}  CRUD
		admin.Get("/papers", listPapersHandler(deps))
		// UI papers.js createPaper() 真 调 POST /papers (裸路径, 无 slug) 创建新专业.
		admin.Post("/papers", createPaperHandler(deps))
		admin.Get("/papers/{slug}", getPaperHandler(deps))
		// UI papers.js savePaper() 真 用 PUT; 保留 POST 向后兼容.
		admin.Put("/papers/{slug}", savePaperHandler(deps))
		admin.Post("/papers/{slug}", savePaperHandler(deps))
		admin.Delete("/papers/{slug}", deletePaperHandler(deps))
		admin.Get("/papers/{slug}/preview", previewPaperHandler(deps))
		admin.Post("/papers/{slug}/batch", batchReorderHandler(deps))
		// 单 paper 开/关: 保留向后兼容, UI 实际用 batch/open-批量
		admin.Post("/papers/{slug}/open", openRunHandler(deps))
		admin.Post("/papers/{slug}/close", closeRunHandler(deps))
		// UI 真 调用 (papers.js): 批量开/关, body={slug: list[str]}
		admin.Post("/papers/batch/open", batchOpenHandler(deps))
		admin.Post("/papers/batch/close", batchCloseHandler(deps))
		// /api/admin/exams*
		admin.Get("/exams", listExamsHandler(deps))
		// UI 用 GET, Python 旧栈也用 GET (main.py 行 520); 兼容旧 POST 保留
		admin.Get("/exam-link", examLinkHandlerGET(deps))
		// UI papers.js:546 / 779 实际调 GET /papers/{slug}/exam-link (路径形态).
		// handler 内自动识别 chi.URLParam(slug) 优先, 回退 query paper_slug (旧式),
		// 复用 examLinkHandlerGET, 两种形态同 handler.
		admin.Get("/papers/{slug}/exam-link", examLinkHandlerGET(deps))
		admin.Post("/exam-link", examLinkHandler(deps))
		// UI papers.js:1027 真 调用 POST /exams/reset-rounds
		admin.Post("/exams/reset-rounds", resetRoundsHandler(deps))
		// 保留向后兼容旧路径 (exam-link/{run}/reset-rounds), 实际指向同 handler
		admin.Post("/exam-link/{run}/reset-rounds", resetRoundsHandler(deps))
		// Task 10 (path C 修正): stats / regrade 是顶层 admin 路径 (与 Python 基线
		// test_admin_api.py 契约一致), 不挂在 /submissions 子树下.
		if deps.Pool == nil {
			admin.Get("/stats", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
			admin.Post("/regrade/{id}", serviceNotReady("REVIEW_NOT_CONFIGURED"))
		} else {
			admin.Get("/stats", statsSubmissionsHandler(deps))
			admin.Post("/regrade/{id}", regradeSubmissionHandler(deps))
		}
		// Task 10 admin submissions 子树 (list/detail/export/review/delete).
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
		items := make(map[string]map[string]any, len(slugs))
		paperIDs := make([]string, 0, len(slugs))
		for _, slug := range slugs {
			if slug == "index" {
				continue // 跳过索引文件 (data/papers/index.json 是旧结构清单, 非试卷)
			}
			item := map[string]any{
				"slug":             slug,
				"name":             slug,
				"status":           "unpublished",
				"question_count":   0,
				"total_score":      0,
				"submission_count": 0,
			}
			// 尝试加载试卷元数据
			if doc, err := deps.Papers.LoadEditable(slug); err == nil {
				if name, ok := doc["name"].(string); ok && name != "" {
					item["name"] = name
				}
				if qs, ok := doc["questions"].([]any); ok {
					item["question_count"] = len(qs)
				}
				if ei, ok := doc["exam_info"].(map[string]any); ok {
					if ts, ok := ei["total_score"].(float64); ok {
						item["total_score"] = int(ts)
					}
				}
			}
			items[slug] = item
			paperIDs = append(paperIDs, slug)
			out = append(out, item)
		}
		// 查询运行状态: 一次批量查询覆盖所有 slug, 避免 N+1 (runs.Repository 无
		// 单 slug "Latest" 方法, ListExamsOverview 已聚合 open/closing/closed/unpublished).
		if deps.Runs != nil && deps.Pool != nil && len(paperIDs) > 0 {
			var overview []runs.ExamOverview
			err := withTx(r.Context(), deps.Pool, func(tx pgx.Tx) error {
				var err error
				overview, err = deps.Runs.ListExamsOverview(r.Context(), tx, paperIDs)
				return err
			})
			if err == nil {
				for _, ov := range overview {
					if item, ok := items[ov.PaperID]; ok {
						item["status"] = ov.Status
					}
				}
			}
		}
		WriteJSON(w, http.StatusOK, out)
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

// writeAdminError 输出 JSON 错误: {"error":{"code":..,"message":..},"detail":{...}}.
// "detail" 字段是兼容层: 前端 (papers.js/admin.js/exam.js) 统一读 data?.detail?.message
// 展示真实失败原因; "error" 字段保留给可能仍读旧形态的调用方.
func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := map[string]any{"code": code, "message": message}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  body,
		"detail": body,
	})
}

// savePaperHandler PUT/POST /api/admin/papers/{slug} (JSON body) -> atomic write snapshot+index.
// 若 run 处于 open/closing 状态, 返回 409 PAPER_LOCKED_BY_ACTIVE_RUN (plan Step 4).
// UI (frontend/js/papers.js savePaper()) payload 只含 {name, exam_info, questions},
// 不带 paper_id/slug (URL 路径已含 slug); Go validate.go 要求 doc["paper_id"] 必填,
// 故此处兼容补写 paper_id=slug (若前端已显式传, 保留前端值不覆盖).
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
		if _, ok := doc["paper_id"]; !ok {
			doc["paper_id"] = slug
		}
		if err := deps.Papers.SaveEditable(slug, doc); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PAPER_SAVE_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// createPaperHandler POST /api/admin/papers { "slug": "...", "name": "..." } -> 建空白试卷.
// UI (frontend/js/papers.js createPaper()) 真调用. 建最小合法 Document (与
// papers/validate.go 契约一致: exam_info + 至少一道题), 之后 admin 走编辑器补全.
func createPaperHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED", "papers store missing")
			return
		}
		var body struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if body.Slug == "" || body.Name == "" {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST", "slug and name required")
			return
		}
		if deps.Papers.Exists(body.Slug) {
			writeAdminError(w, http.StatusConflict, "PAPER_ALREADY_EXISTS", "slug already exists: "+body.Slug)
			return
		}
		// papers.ValidateDocument (与 Python question_loader 1 对 1) 要求 questions
		// 非空 + exam_info.description 非空; 建空白试卷用占位单选题, 待 admin 在
		// 编辑器内补全/替换.
		doc := map[string]any{
			"paper_id": body.Slug,
			"name":     body.Name,
			"exam_info": map[string]any{
				"title":         body.Name,
				"description":   body.Name,
				"total_score":   1.0,
				"passing_score": 0,
			},
			"questions": []any{
				map[string]any{
					"id":       "q1",
					"type":     "single_choice",
					"question": "占位题目，请在编辑器中修改",
					"score":    1.0,
					"options": []any{
						map[string]any{"key": "A", "text": "选项 A"},
						map[string]any{"key": "B", "text": "选项 B"},
					},
					"answer": "A",
				},
			},
		}
		if err := deps.Papers.SaveEditable(body.Slug, doc); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PAPER_SAVE_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": body.Slug})
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
// 现状: UI 未启用此端点 (frontend/js/* 无 /batch 调用真 用), handler 仅校验 JSON + 回 200.
// 真 实现 (调 papers.Store.ApplyBatchOps) 待 UI 启用此功能时再补, 文档记遗留.)
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
// UI (frontend/js/papers.js setPaperStatus) 单个开考时不带任何 body (无 Content-Type),
// 故 body 允许缺省/空, 缺省时取 deps.Config.Exam.DurationMinutes 默认值.
// 真实现: 调 deps.RunService.Open 创建 run + snapshot + token 侧车, 返回公开 URL.
func openRunHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		if deps.RunService == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "RUN_SERVICE_NOT_CONFIGURED", "run service missing")
			return
		}
		var body struct {
			DurationMinutes int `json:"duration_minutes"`
			RoundNo         int `json:"round_no,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if body.DurationMinutes <= 0 {
			body.DurationMinutes = 90
			if deps.Config != nil && deps.Config.Exam.DurationMinutes > 0 {
				body.DurationMinutes = deps.Config.Exam.DurationMinutes
			}
		}
		ctx := r.Context()
		res, err := callOpenRun(ctx, deps, slug, body.DurationMinutes, body.RoundNo)
		if err != nil {
			status, code := mapRunOpenErr(err)
			writeAdminError(w, status, code, err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"ok": true, "paper_slug": slug, "run_id": res.Run.ID,
			"public_token":     res.PublicToken,
			"url":              buildExamURL(r, slug, res.PublicToken),
			"duration_minutes": body.DurationMinutes, "round_no": res.Run.RoundNo,
		})
	}
}

// closeRunHandler POST /api/admin/papers/{slug}/close -> 进入 finalize: open -> closing.
func closeRunHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		if deps.RunService == nil || deps.Pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "RUN_SERVICE_NOT_CONFIGURED", "run service or pool missing")
			return
		}
		var body struct {
			GraceSeconds int `json:"grace_seconds,omitempty"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
		grace := time.Duration(body.GraceSeconds) * time.Second
		if grace == 0 {
			grace = 30 * time.Second // 与 Python 默认 30s 一致
		}
		ctx := r.Context()
		err := withTx(ctx, deps.Pool, func(tx pgx.Tx) error {
			run, rerr := deps.RunService.FindOpenBySlug(ctx, tx, slug)
			if rerr != nil {
				return rerr
			}
			if run == nil {
				return errRunNotOpen
			}
			_, rerr = deps.RunService.BeginClose(ctx, tx, run.ID, grace)
			return rerr
		})
		if err != nil {
			status, code := mapRunCloseErr(err)
			writeAdminError(w, status, code, err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": slug})
	}
}

// batchOpenHandler POST /api/admin/papers/batch/open {slugs: [str], duration_minutes?}
// UI papers.js batchOpen() 真调用, 只传 {slugs} (无 duration_minutes) -> 缺省时取
// deps.Config.Exam.DurationMinutes. 循环调 RunService.Open, 单个失败不影响其它
// slug (continue-on-error), 响应 {updated, requested, errors:[{slug,message,code}]}
// 与 UI 读取形态一致 (batchOpen()/batchClose() 均读 data.updated/requested/errors).
func batchOpenHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.RunService == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "RUN_SERVICE_NOT_CONFIGURED", "run service missing")
			return
		}
		var body struct {
			Slugs           []string `json:"slugs"`
			DurationMinutes int      `json:"duration_minutes,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if len(body.Slugs) == 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST", "slugs required")
			return
		}
		if body.DurationMinutes <= 0 {
			body.DurationMinutes = 90
			if deps.Config != nil && deps.Config.Exam.DurationMinutes > 0 {
				body.DurationMinutes = deps.Config.Exam.DurationMinutes
			}
		}
		ctx := r.Context()
		updated := 0
		errs := make([]map[string]any, 0)
		for _, slug := range body.Slugs {
			_, err := callOpenRun(ctx, deps, slug, body.DurationMinutes, 0)
			if err != nil {
				_, code := mapRunOpenErr(err)
				errs = append(errs, map[string]any{"slug": slug, "code": code, "message": err.Error()})
				continue
			}
			updated++
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"updated": updated, "requested": len(body.Slugs), "errors": errs,
		})
	}
}

// batchCloseHandler POST /api/admin/papers/batch/close {slugs: [str], grace_seconds?}
// UI papers.js batchClose() 读 {updated, requested, errors} (同 batchOpen 形态).
func batchCloseHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.RunService == nil || deps.Pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "RUN_SERVICE_NOT_CONFIGURED", "run service or pool missing")
			return
		}
		var body struct {
			Slugs        []string `json:"slugs"`
			GraceSeconds int      `json:"grace_seconds,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if len(body.Slugs) == 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST", "slugs required")
			return
		}
		grace := time.Duration(body.GraceSeconds) * time.Second
		if grace == 0 {
			grace = 30 * time.Second
		}
		ctx := r.Context()
		updated := 0
		errs := make([]map[string]any, 0)
		for _, slug := range body.Slugs {
			err := withTx(ctx, deps.Pool, func(tx pgx.Tx) error {
				run, rerr := deps.RunService.FindOpenBySlug(ctx, tx, slug)
				if rerr != nil {
					return rerr
				}
				if run == nil {
					return nil // 已关闭, 幂等成功
				}
				_, rerr = deps.RunService.BeginClose(ctx, tx, run.ID, grace)
				return rerr
			})
			if err != nil {
				_, code := mapRunCloseErr(err)
				errs = append(errs, map[string]any{"slug": slug, "code": code, "message": err.Error()})
				continue
			}
			updated++
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"updated": updated, "requested": len(body.Slugs), "errors": errs,
		})
	}
}

// examLinkHandlerGET GET /api/admin/exam-link?paper_slug=<slug>
//
//	兼容 GET /api/admin/papers/{slug}/exam-link (UI papers.js:546/779 真 用路径形态)
//
// 重建当前最近一条 open run 的公开 URL. 从 snapshot 文件读 hash + token 侧车重建.
// qr_base64 由 makeQRDataURL 真生成 (Task 14 fix 分支已补 QR 库).
func examLinkHandlerGET(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.RunService == nil || deps.Pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "RUN_SERVICE_NOT_CONFIGURED", "run service or pool missing")
			return
		}
		// slug 源: 优先 chi path param {slug} (UI papers/{slug}/exam-link 形态),
		// 回退 query string paper_slug (兼容旧 ?paper_slug= 形态)
		slug := chi.URLParam(r, "slug")
		if slug == "" {
			slug = r.URL.Query().Get("paper_slug")
		}
		if slug == "" {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST", "paper_slug (path or query) required")
			return
		}
		ctx := r.Context()
		var run *runs.Run
		var token string
		err := withTx(ctx, deps.Pool, func(tx pgx.Tx) (err error) {
			run, err = deps.RunService.FindOpenBySlug(ctx, tx, slug)
			return err
		})
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "OPEN_RUN_QUERY_FAILED", err.Error())
			return
		}
		if run == nil {
			writeAdminError(w, http.StatusNotFound, "RUN_NOT_OPEN", "paper has no open run")
			return
		}
		token, err = deps.RunService.LoadPublicToken(run.ID)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "TOKEN_SIDECAR_READ_FAILED", err.Error())
			return
		}
		if token == "" {
			writeAdminError(w, http.StatusNotFound, "TOKEN_NOT_FOUND",
				"token sidecar 未持久化 (run 在 tokenDir 配置前创建?)")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"paper_slug": slug, "run_id": run.ID,
			"url":       buildExamURL(r, slug, token),
			"qr_base64": makeQRDataURL(buildExamURL(r, slug, token)),
		})
	}
}

// listExamsHandler GET /api/admin/exams -> 直接返 Array[ExamOverview].
// UI papers.js:738 loadExams 期望 Array (非 {exams:...} 包装), 每项含状态 + counter.
func listExamsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Papers == nil || deps.RunService == nil || deps.Pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "DEPS_NOT_CONFIGURED",
				"papers/run-service/pool missing")
			return
		}
		slugs, err := deps.Papers.List()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PAPERS_LIST_FAILED", err.Error())
			return
		}
		ctx := r.Context()
		var overviews []runs.ExamOverview
		err = withTx(ctx, deps.Pool, func(tx pgx.Tx) error {
			var ierr error
			overviews, ierr = deps.RunService.ListExamsOverview(ctx, tx, slugs)
			return ierr
		})
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "LIST_EXAMS_FAILED", err.Error())
			return
		}
		// UI 期望裸数组; 无 run 的 paper -> 返 "unpublished" 状态条目, 保证每 paper 都在列表里.
		seen := make(map[string]bool, len(overviews))
		for _, o := range overviews {
			seen[o.PaperID] = true
		}
		for _, slug := range slugs {
			if !seen[slug] {
				overviews = append(overviews, runs.ExamOverview{
					PaperID: slug, Name: slug, Status: "unpublished",
				})
			}
		}
		// 专业显示名: 优先 papers store 中试卷文档的中文 name, 回退 slug.
		resolver := newPaperNameResolver(deps)
		for i := range overviews {
			overviews[i].Name = resolver.resolve(overviews[i].PaperID, overviews[i].Name)
		}
		WriteJSON(w, http.StatusOK, overviews)
	}
}

// examLinkHandler POST /api/admin/exam-link 旧 POST 形态保留向后兼容; 实为调 Open
// (legacy Python POST 创建新 run; 现真实现同 batchOpen 单 slug).
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
		if deps.RunService == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "RUN_SERVICE_NOT_CONFIGURED", "run service missing")
			return
		}
		ctx := r.Context()
		res, err := callOpenRun(ctx, deps, body.PaperSlug, body.DurationMinutes, body.RoundNo)
		if err != nil {
			status, code := mapRunOpenErr(err)
			writeAdminError(w, status, code, err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"paper_slug":   body.PaperSlug,
			"run_id":       res.Run.ID,
			"public_token": res.PublicToken,
			"url":          buildExamURL(r, body.PaperSlug, res.PublicToken),
		})
	}
}

// resetRoundsHandler POST /api/admin/exams/reset-rounds {slugs, all?} -> 软重置.
// UI papers.js:1017 真 用, 期望:
//   - slugs=[] 表示所有 paper (UI 默认传 [])
//   - 删: 各专业轮次记录 (exam_runsid token sidecar + papers JSON) + 未交卷会话 (active)
//   - 跳过: 考试中/收卷中 (open/closing) 的 run (防误删)
//   - 保留: 已提交的历史成绩 (submissions 已 submitted, RUN_NOT_DELETABLE 因 FK RESTRICT)
//
// 真实实现作"软重置":
//   - exam_sessions WHERE status='active' 的会话全删 (SUBMIT 可走 finalize 时已做了, 这里只清遗留)
//   - 关闭的 run 行不删 (FK RESTRICT + 历史保留), 但删 token 侧车文件 (失效旧入场链接)
//   - 不删 papers JSON (admin 仍可重用配置), 仅清 token 侧车
//   - 任一 active run -> 整批拒 (409 ACTIVE_RUN_EXISTS, UI 提示先关考)
func resetRoundsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.RunService == nil || deps.Pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "DEPS_NOT_CONFIGURED",
				"run-service/pool missing")
			return
		}
		var body struct {
			Slugs []string `json:"slugs"`
			All   bool     `json:"all,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		slugs := body.Slugs
		if len(slugs) == 0 && !body.All {
			// UI 默认传 slugs=[] 语义 = 全部 paper; 兼容此项, 转为 all=true 继续处理
			body.All = true
		}
		if len(slugs) == 0 && !body.All {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"slugs required (or all=true)")
			return
		}
		ctx := r.Context()
		// 找待处理 paper slug 列表
		if len(slugs) == 0 && body.All {
			if deps.Papers == nil {
				writeAdminError(w, http.StatusServiceUnavailable, "PAPERS_NOT_CONFIGURED",
					"papers store missing")
				return
			}
			var err error
			slugs, err = deps.Papers.List()
			if err != nil {
				writeAdminError(w, http.StatusInternalServerError, "PAPERS_LIST_FAILED", err.Error())
				return
			}
		}
		deleted := make([]string, 0)
		skipped := make([]map[string]any, 0) // {slug, reason}
		cleaningErr := withTx(ctx, deps.Pool, func(tx pgx.Tx) error {
			for _, slug := range slugs {
				// 取该 paper 最近一条 run (--closed/--open)
				run, err := deps.RunService.FindOpenBySlug(ctx, tx, slug)
				if err != nil {
					return err
				}
				if run != nil {
					// open 状态存在 -> 拒 (UI 文案说"考试中/收卷中跳过", 但我们更严:
					// 让 admin 先关考再重置, 防 active session 数据丢)
					skipped = append(skipped, map[string]any{
						"slug": slug, "reason": "ACTIVE_RUN_EXISTS"})
					continue
				}
				// 删该 paper 所有 active session + 关联 token 侧车 + closed run 的 token 侧车
				_, err = tx.Exec(ctx, `
					DELETE FROM exam_sessions
					WHERE run_id IN (
						SELECT id FROM exam_runs WHERE paper_id = $1 AND status IN ('open','closing','closed')
					) AND status = 'active'`, slug)
				if err != nil {
					return fmt.Errorf("resetRounds delete active sessions: %w", err)
				}
				// 删该 paper 所有 closed run 的 token 侧车文件
				rows, err := tx.Query(ctx,
					`SELECT id FROM exam_runs WHERE paper_id = $1 AND status = 'closed'`, slug)
				if err != nil {
					return fmt.Errorf("resetRounds query closed runs: %w", err)
				}
				for rows.Next() {
					var runID string
					if err := rows.Scan(&runID); err != nil {
						rows.Close()
						return err
					}
					_ = deps.RunService.RemovePublicToken(runID)
				}
				if err := rows.Err(); err != nil {
					return err
				}
				deleted = append(deleted, slug)
			}
			return nil
		})
		if cleaningErr != nil {
			writeAdminError(w, http.StatusInternalServerError, "RESET_ROUNDS_FAILED",
				cleaningErr.Error())
			return
		}
		errs := make([]map[string]any, 0, len(skipped))
		for _, sk := range skipped {
			errs = append(errs, map[string]any{
				"slug": sk["slug"], "code": sk["reason"], "message": "考试中/收卷中，已跳过",
			})
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"updated": len(deleted), "requested": len(slugs), "errors": errs,
			"deleted": deleted, "skipped": skipped,
			"note": "软重置 (历史 submissions 保留; token 侧车失效; 仍需重新关考后 open 新轮次)",
		})
	}
}

// withTx 工具: pgxpool.Pool 不自带 WithTx; 用 Begin/Commit/Rollback.
func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// callOpenRun 共享: 调 RunService.Open 在事务内创建 run.
// 注: runs.Service.Open 真实签名 (ctx, tx, slug, durationMinutes) 不接 roundNo,
// round 由 service 内自增 (论文 Task 10 代次切换). roundNo 仅 handler 用于响应回显.
func callOpenRun(ctx context.Context, deps Dependencies, slug string, durationMin, roundNo int) (*runs.OpenResult, error) {
	var res *runs.OpenResult
	err := withTx(ctx, deps.Pool, func(tx pgx.Tx) error {
		var err error
		res, err = deps.RunService.Open(ctx, tx, slug, durationMin)
		return err
	})
	return res, err
}

// buildExamURL 重构公开考试 URL. frontend/js/exam.js 的 getPaperIdFromUrl/
// getRunTokenFromUrl 分别读 URL query 的 "paper" / "run" 参数 (页面路径 /exam),
// 而非旧的 "paper_slug" / "run_token" —— 此处必须与其保持一致.
func buildExamURL(r *http.Request, slug, token string) string {
	host := r.Host
	if host == "" {
		host = "127.0.0.1:18080"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/exam?paper=%s&run=%s", scheme, host, slug, token)
}

// makeQRDataURL 把 url 编码成 PNG 二维码 -> base64 data URI.
// 给 admin UI <img src="..."> 直接渲染 (admin.js:329 期望 qr_base64 字段).
// 失败 (空 url / 编码失败) 返 "" (UI fallback 不渲染 <img>).
func makeQRDataURL(url string) string {
	if url == "" {
		return ""
	}
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// errRunNotOpen 表示 paper 无 open run (admin close 调幂等用).
var errRunNotOpen = errors.New("no open run for paper")

// mapRunOpenErr 把 RunService.Open 错误映射到 HTTP.
func mapRunOpenErr(err error) (int, string) {
	if errors.Is(err, errRunNotOpen) {
		return http.StatusConflict, "RUN_NOT_OPEN"
	}
	if errors.Is(err, runs.ErrActiveRunExists) {
		return http.StatusConflict, "ACTIVE_RUN_EXISTS"
	}
	if strings.Contains(err.Error(), "does not exist") {
		return http.StatusNotFound, "PAPER_NOT_FOUND"
	}
	return http.StatusInternalServerError, "OPEN_RUN_FAILED"
}

// mapRunCloseErr close/beginClose 错误.
func mapRunCloseErr(err error) (int, string) {
	if errors.Is(err, errRunNotOpen) {
		return http.StatusOK, "NO_OP" // 幂等成功
	}
	if errors.Is(err, runs.ErrAlreadyClosed) {
		return http.StatusConflict, "RUN_ALREADY_CLOSED"
	}
	return http.StatusInternalServerError, "CLOSE_RUN_FAILED"
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
// 路由清单 (5 条; stats/regrade 在 path C 修正后已顶层挂, 不在本子树):
//
//	GET    /api/admin/submissions            list (query: run_id, employee_id, status, order_by, limit)
//	GET    /api/admin/submissions/{id}       detail (含 review_logs 列表)
//	GET    /api/admin/submissions/export     xlsx 导出 (query: paper_id 可选过滤; 上限 DefaultRowsLimit)
//	                                        表头: 专业/工号/姓名 + 每题"作答内容 - 得分" + 总分/时间
//	POST   /api/admin/submissions/{id}/review   apply 改分 (Body: question_id / sub_qid / new_score / note)
//	DELETE /api/admin/submissions            批量删 (Body: ids=[1,2,3])
func MountAdminSubmissions(admin chi.Router, deps Dependencies) {
	if deps.Pool == nil {
		admin.Get("/submissions", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Get("/submissions/export", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Delete("/submissions", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Get("/submissions/{id}", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		admin.Post("/submissions/{id}/review", serviceNotReady("SUBMISSIONS_POOL_NOT_CONFIGURED"))
		return
	}
	admin.Get("/submissions", listSubmissionsHandler(deps))
	admin.Get("/submissions/export", exportSubmissionsHandler(deps))
	admin.Delete("/submissions", deleteSubmissionsHandler(deps))
	admin.Get("/submissions/{id}", detailSubmissionHandler(deps))
	admin.Post("/submissions/{id}/review", reviewSubmissionHandler(deps))
}

// serviceNotReady 返 503 + JSON error body. Task 10 admin 子树/MountAdminSubmissions 专用 handler factory.
func serviceNotReady(code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeAdminError(w, http.StatusServiceUnavailable, code, "admin service not configured")
	}
}

// listSubmissionsHandler GET /api/admin/submissions: 列表过滤+排序+分页.
// Query: run_id / employee_id / status (grading_status 值) / review_status /
//
//	keyword (name/employee_id 模糊) / paper_id /
//	order_by=in(submitted_at|grading_generation|total_score) / order=asc|desc /
//	limit=200 (max 1000) / offset=0.
//
// LEFT JOIN exam_runs 取 round_no; run 行缺失 (旧栈遗留数据) -> run_is_legacy=true.
// 返回字段与前端 admin.js renderRows 一一对应 (轮次/部门/客观分/主观分等列).
func listSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := atoiClamp(q.Get("limit"), 200, 0, 1000)
		offset := atoiClamp(q.Get("offset"), 0, 0, 1<<31)

		// ORDER BY 白名单 (避免 SQL 注入); 默认 submitted_at DESC.
		orderBy := map[string]string{
			"submitted_at":       "s.submitted_at",
			"grading_generation": "s.grading_generation",
			"total_score":        "s.total_score",
		}[q.Get("order_by")]
		if orderBy == "" {
			orderBy = "s.submitted_at"
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
		sb.WriteString(`SELECT s.id, s.paper_id, s.paper_name, s.run_id,
			s.employee_id, s.name, s.department,
			s.grading_status, s.grading_generation, s.review_status,
			s.objective_score, s.subjective_score_final, s.total_score,
			s.auto_submit_reason, s.submitted_at,
			r.round_no, (r.id IS NULL) AS run_is_legacy
		FROM submissions s
		LEFT JOIN exam_runs r ON r.id = s.run_id`)
		sb.WriteString(" WHERE 1=1")
		addFilter := func(col, val string) {
			if val == "" {
				return
			}
			sb.WriteString(" AND " + col + " = $" + strconv.Itoa(phIdx))
			args = append(args, val)
			phIdx++
		}
		addFilter("s.run_id", q.Get("run_id"))
		addFilter("s.employee_id", q.Get("employee_id"))
		addFilter("s.grading_status", q.Get("status"))
		addFilter("s.review_status", q.Get("review_status"))
		addFilter("s.paper_id", q.Get("paper_id"))
		if kw := strings.TrimSpace(q.Get("keyword")); kw != "" {
			sb.WriteString(" AND (s.name ILIKE $" + strconv.Itoa(phIdx) +
				" OR s.employee_id ILIKE $" + strconv.Itoa(phIdx) + ")")
			args = append(args, "%"+kw+"%")
			phIdx++
		}
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
			ID                   int64     `json:"id"`
			PaperID              string    `json:"paper_id"`
			PaperName            *string   `json:"paper_name"`
			RunID                string    `json:"run_id"`
			EmployeeID           string    `json:"employee_id"`
			Name                 string    `json:"name"`
			Department           *string   `json:"department"`
			GradingStatus        string    `json:"grading_status"`
			Generation           int64     `json:"grading_generation"`
			ReviewStatus         string    `json:"review_status"`
			ObjectiveScore       float64   `json:"objective_score"`
			SubjectiveScoreFinal float64   `json:"subjective_score_final"`
			TotalScore           float64   `json:"total_score"`
			AutoSubmitReason     *string   `json:"auto_submit_reason"`
			SubmittedAt          time.Time `json:"submitted_at"`
			RoundNo              *int      `json:"round_no"`
			RunIsLegacy          bool      `json:"run_is_legacy"`
		}
		items := make([]item, 0, limit)
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.ID, &it.PaperID, &it.PaperName, &it.RunID,
				&it.EmployeeID, &it.Name, &it.Department,
				&it.GradingStatus, &it.Generation, &it.ReviewStatus,
				&it.ObjectiveScore, &it.SubjectiveScoreFinal, &it.TotalScore,
				&it.AutoSubmitReason, &it.SubmittedAt,
				&it.RoundNo, &it.RunIsLegacy); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED",
					"scan submission: "+err.Error())
				return
			}
			items = append(items, it)
		}
		// 专业显示名: 优先 papers store 中文 name, 回退已有 paper_name /
		// paper_id (前端 paper_name || paper_id 兜底, 保持非空).
		resolver := newPaperNameResolver(deps)
		for i := range items {
			name := resolver.resolve(items[i].PaperID, ptrOrEmpty(items[i].PaperName))
			items[i].PaperName = &name
			// 展示分数: 保留两位小数, 多余位直接舍弃.
			items[i].ObjectiveScore = trunc2(items[i].ObjectiveScore)
			items[i].SubjectiveScoreFinal = trunc2(items[i].SubjectiveScoreFinal)
			items[i].TotalScore = trunc2(items[i].TotalScore)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"submissions": items})
	}
}

// statsSubmissionsHandler GET /api/admin/stats: 返回总览页需要的聚合值，并保留状态分组。
func statsSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			submittedCount     int64
			avgScore           float64
			maxScore           float64
			minScore           float64
			pendingReview      int64
			lowConfidenceCount int64
		)
		err := deps.Pool.QueryRow(r.Context(), `
			SELECT COUNT(*),
				COALESCE(ROUND(AVG(total_score)::numeric, 2)::float8, 0),
				COALESCE(MAX(total_score), 0),
				COALESCE(MIN(total_score), 0),
				COUNT(*) FILTER (WHERE review_status IN ('pending', 'need_review', 'low_confidence')),
				-- 低置信卷数: worker 整卷状态里没有 'low_confidence' 值 (它写
				-- need_review/partial_manual/high_confidence/reviewed), 低置信体现在
				-- grading_detail_json 单题的 low_confidence 标志上, 按"含任一低置信题"计数.
				-- 兼容三种条目形态: 顶层 low_confidence / 顶层 lowest_confidence_flagged
				-- (worker 旧格式) / review_status='low_confidence' 字符串.
				COUNT(*) FILTER (WHERE jsonb_typeof(grading_detail_json) = 'array' AND EXISTS (
					SELECT 1 FROM jsonb_array_elements(grading_detail_json) e
					WHERE (e ->> 'low_confidence')::boolean IS TRUE
					   OR (e ->> 'lowest_confidence_flagged')::boolean IS TRUE
					   OR e ->> 'review_status' = 'low_confidence'))
			FROM submissions`).Scan(
			&submittedCount, &avgScore, &maxScore, &minScore,
			&pendingReview, &lowConfidenceCount)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"stats: "+err.Error())
			return
		}

		rows, err := deps.Pool.Query(r.Context(),
			`SELECT coalesce(review_status,'') AS rs, count(*) FROM submissions GROUP BY rs`)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"stats by status: "+err.Error())
			return
		}
		defer rows.Close()
		counts := map[string]int64{}
		for rows.Next() {
			var rs string
			var count int64
			if err := rows.Scan(&rs, &count); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED",
					"scan stats: "+err.Error())
				return
			}
			counts[rs] = count
		}
		if err := rows.Err(); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"iterate stats: "+err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"submitted_count":      submittedCount,
			"avg_score":            trunc2(avgScore),
			"max_score":            trunc2(maxScore),
			"min_score":            trunc2(minScore),
			"pending_review":       pendingReview,
			"low_confidence_count": lowConfidenceCount,
			"counts":               counts,
			"total":                submittedCount,
		})
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
			gen                                           int64
			totScore                                      float64
			submittedAt                                   time.Time
			gradingDetail                                 []byte
			gradingErr                                    string
		)
		err = deps.Pool.QueryRow(r.Context(),
			`SELECT paper_id, run_id, employee_id, name, grading_status,
				grading_generation, review_status, total_score, submitted_at,
				coalesce(grading_error, ''),
				coalesce(grading_detail_json, '[]'::jsonb)
			 FROM submissions WHERE id = $1`, id).Scan(
			&paperID, &runID, &empID, &name, &gstatus, &gen, &rstatus, &totScore,
			&submittedAt, &gradingErr, &gradingDetail)
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
			ID         int64     `json:"id"`
			QuestionID string    `json:"question_id"`
			OldScore   float64   `json:"old_score"`
			NewScore   float64   `json:"new_score"`
			Note       string    `json:"note"`
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
				"grading_error": gradingErr,
				"submitted_at":  submittedAt, "grading_detail": dj,
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
			QuestionID string  `json:"question_id"`
			SubQID     string  `json:"sub_question_id"`
			NewScore   float64 `json:"new_score"`
			Note       string  `json:"note"`
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
		var body struct {
			IDs []int64 `json:"ids"`
		}
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

// paperNameResolver 缓存 paper_id -> 专业展示名, 避免每行/每请求重复读试卷 JSON.
// 展示名来源: papers store 中试卷文档顶层 name (中文名) > 已有 paper_name >
// paper_id 兜底. deps.Papers 为 nil (单测/未配置) 时直接回退 stored, 保持原行为.
type paperNameResolver struct {
	deps  Dependencies
	cache map[string]string
}

func newPaperNameResolver(deps Dependencies) *paperNameResolver {
	return &paperNameResolver{deps: deps, cache: make(map[string]string)}
}

// resolve 返回 paperID 的专业展示名; stored 为已有显示值 (paper_name 或 slug).
func (r *paperNameResolver) resolve(paperID, stored string) string {
	if name, ok := r.cache[paperID]; ok {
		return name
	}
	name := stored
	if r.deps.Papers != nil {
		if doc, err := r.deps.Papers.LoadEditable(paperID); err == nil {
			if n, ok := doc["name"].(string); ok && n != "" {
				name = n
			}
		}
	}
	r.cache[paperID] = name
	return name
}

// exportSubmissionsHandler GET /api/admin/submissions/export: 导出 xlsx.
// 表头: 专业(paper_name, NULL 时回退 paper_id)/工号/姓名/部门 + 每题
// "n. 作答内容 - 得分"列 + 总分/时间. 每题列按
// grading_detail_json 顺序 (composite 展平为子题列), 列集合由实际数据首次出现
// 顺序 union 得到, 故先做轻量列发现, 再流式写行. 上限 export.DefaultRowsLimit
// (100000); 过滤: ids (逗号分隔的提交 id, 优先) / paper_id / employee_id /
// keyword (姓名或工号模糊), 与 listSubmissionsHandler 同模式.
func exportSubmissionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		colQIDs, err := discoverExportColumns(r.Context(), deps, q)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"export columns: "+err.Error())
			return
		}
		// Excel 硬上限 16384 列; 预留 6 个固定列 (专业/工号/姓名/部门/总分/时间).
		if len(colQIDs) > 16384-6 {
			writeAdminError(w, http.StatusUnprocessableEntity, "EXPORT_TOO_MANY_COLUMNS",
				fmt.Sprintf("columns=%d exceed xlsx limit", len(colQIDs)+6))
			return
		}

		where, args := exportSubmissionsQuery(q)
		sb := strings.Builder{}
		sb.WriteString(`SELECT paper_id, coalesce(paper_name, paper_id), employee_id, name,
			department, total_score, submitted_at,
			coalesce(grading_detail_json, '[]'::jsonb) `)
		sb.WriteString(where)
		rows, err := deps.Pool.Query(r.Context(), sb.String(), args...)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "DB_QUERY_FAILED",
				"export: "+err.Error())
			return
		}
		defer rows.Close()

		header := []string{"专业", "工号", "姓名", "部门"}
		for i := range colQIDs {
			header = append(header, fmt.Sprintf("%d. 作答内容 - 得分", i+1))
		}
		header = append(header, "总分", "时间")

		// 专业显示名: 优先 papers store 中文 name, 回退已有 paper_name /
		// paper_id (SQL COALESCE 已兜底).
		resolver := newPaperNameResolver(deps)
		xw := export.NewXLSXWriter()
		if err := xw.WriteHeader(header); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "XLSX_HEADER_FAILED", err.Error())
			return
		}
		for rows.Next() {
			var (
				paperID     string
				paperName   string
				empID, name string
				department  *string
				score       float64
				submittedAt time.Time
				detailRaw   []byte
			)
			if err := rows.Scan(&paperID, &paperName, &empID, &name, &department, &score, &submittedAt,
				&detailRaw); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "DB_SCAN_FAILED", err.Error())
				return
			}
			cells := parseExportCells(detailRaw)
			byQID := make(map[string]string, len(cells))
			for _, c := range cells {
				byQID[c.qid] = c.value
			}
			row := export.Row{resolver.resolve(paperID, paperName), empID, name,
				ptrOrEmpty(department)}
			for _, qid := range colQIDs {
				row = append(row, byQID[qid])
			}
			row = append(row, trunc2(score), submittedAt)
			if err := xw.WriteRow(row); err != nil {
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

// exportSubmissionsQuery 构建 export 查询的 WHERE/ORDER/LIMIT 片段 (列发现与
// 数据查询共用同一过滤, 保证列集合与行一致). 支持 ids (逗号分隔提交 id) /
// paper_id / employee_id 精确过滤 + keyword (姓名或工号) 模糊过滤.
func exportSubmissionsQuery(q url.Values) (string, []any) {
	var (
		sb    strings.Builder
		args  []any
		phIdx = 1
	)
	sb.WriteString(" FROM submissions WHERE 1=1")
	addFilter := func(col, val string) {
		if val == "" {
			return
		}
		sb.WriteString(" AND " + col + " = $" + strconv.Itoa(phIdx))
		args = append(args, val)
		phIdx++
	}
	if ids := strings.TrimSpace(q.Get("ids")); ids != "" {
		var idNums []int64
		for _, p := range strings.Split(ids, ",") {
			if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
				idNums = append(idNums, n)
			}
		}
		if len(idNums) > 0 {
			sb.WriteString(" AND id = ANY($" + strconv.Itoa(phIdx) + "::bigint[])")
			args = append(args, idNums)
			phIdx++
		}
	}
	addFilter("paper_id", strings.TrimSpace(q.Get("paper_id")))
	addFilter("employee_id", strings.TrimSpace(q.Get("employee_id")))
	if kw := strings.TrimSpace(q.Get("keyword")); kw != "" {
		sb.WriteString(" AND (name ILIKE $" + strconv.Itoa(phIdx) +
			" OR employee_id ILIKE $" + strconv.Itoa(phIdx) + ")")
		args = append(args, "%"+kw+"%")
		phIdx++
	}
	sb.WriteString(" ORDER BY submitted_at DESC LIMIT $" + strconv.Itoa(phIdx))
	args = append(args, export.DefaultRowsLimit)
	return sb.String(), args
}

// discoverExportColumns 轻量读每行 grading_detail_json, 按首次出现顺序 union
// 出"作答内容 - 得分"列的 qid 序列 (composite 子题展平为 parent:sub).
func discoverExportColumns(ctx context.Context, deps Dependencies, q url.Values) ([]string, error) {
	where, args := exportSubmissionsQuery(q)
	sb := strings.Builder{}
	sb.WriteString("SELECT coalesce(grading_detail_json, '[]'::jsonb)")
	sb.WriteString(where)

	rows, err := deps.Pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		cols []string
		seen = make(map[string]bool)
	)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		for _, c := range parseExportCells(raw) {
			if !seen[c.qid] {
				seen[c.qid] = true
				cols = append(cols, c.qid)
			}
		}
	}
	return cols, rows.Err()
}

// exportCell 是一道题 (或 composite 子题) 的导出单元格.
type exportCell struct {
	qid   string
	value string
}

// parseExportCells 解析 grading_detail_json 为有序单元格; composite 展平为子题.
// 单元格文本 = "作答内容（final/max）".
func parseExportCells(raw []byte) []exportCell {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var detail []map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil
	}
	var out []exportCell
	for _, d := range detail {
		qid, _ := d["question_id"].(string)
		if qid == "" {
			continue
		}
		if subs, ok := d["sub_results"].([]any); ok && len(subs) > 0 {
			for _, s := range subs {
				sm, ok := s.(map[string]any)
				if !ok {
					continue
				}
				sid, _ := sm["sub_question_id"].(string)
				out = append(out, exportCell{
					qid:   qid + ":" + sid,
					value: exportCellText(sm),
				})
			}
			continue
		}
		out = append(out, exportCell{qid: qid, value: exportCellText(d)})
	}
	return out
}

// exportCellText 组装单元格文本 "作答内容（x/y）".
func exportCellText(m map[string]any) string {
	answer := answerText(m["student_answer"])
	if answer == "" {
		answer = "未作答"
	}
	score := numText(m["final_score"])
	if score == "" {
		score = numText(m["score"])
	}
	max := numText(m["max_score"])
	switch {
	case score == "":
		return answer
	case max == "":
		return fmt.Sprintf("%s（%s）", answer, score)
	default:
		return fmt.Sprintf("%s（%s/%s）", answer, score, max)
	}
}

// answerText 把 student_answer (string/[]/bool/数值) 转导出文本.
func answerText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, answerText(e))
		}
		return strings.Join(parts, ",")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// numText 把 JSON number/string 转文本.
func numText(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(trunc2(t), 'f', -1, 64)
	case string:
		return t
	}
	return ""
}

// trunc2 保留两位小数, 多余位数直接舍弃 (非四舍五入).
func trunc2(f float64) float64 {
	return math.Trunc(f*100) / 100
}

// ptrOrEmpty 把 *string 兜底为空串 (department 可空).
func ptrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
