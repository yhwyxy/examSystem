package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/ratelimit"
	"github.com/yhwyxy/examSystem/internal/review"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
	"github.com/yhwyxy/examSystem/internal/submissions"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dependencies 是 NewRouter 的依赖注入容器。
// Task 1 只需最小依赖, 后续 Task 通过扩展它注入 DBPool、ReviewService、Worker 等。
type Dependencies struct {
	Config     *config.Config
	StaticRoot string
	// ReloadConfig 是 reload-config 路由的刷新回调;
	// 返回 (success, message)。message 会原样回给 client。
	ReloadConfig func() (ok bool, message string)

	// Pool 供 student 路径 (Task 6) 使用: submissions.Service 等底层需 *pgxpool.Pool.
	Pool *pgxpool.Pool
	// SubmissionsService 是 POST /api/session/submit / GET /api/submission/{id}/status
	// 的核心依赖. 任一缺失时对应路由返 503 (plan Files 参考 Task 6 Step 5).
	SubmissionsService *submissions.Service

	// Task 9 admin 路径.
	Auth   *auth.Store      // nil 时所有 admin 路由放行 (与 Python enable_auth=false parity)
	Papers *papers.Store    // 试卷文件写入入口 (per-slug mutex + atomic write)
	Runs   *runs.Repository // 用作 exam-link/reset-rounds/auth CHECK 等的 PG 关系校验
	// RunService 是 admin "开考/关考/exam-link" 路径的真业务依赖, Open/BeginClose
	// + token 侧车文件管理. nil 时对应路由返 503.
	RunService *runs.Service

	// Task 10 admin submissions/review 路径.
	Review *review.Service // Apply / Regrade 跨事务, nil 时对应路由返 503

	// Task 12a student /api/exam/* 8 端点 (公开 + token 鉴权), nil 时路由返 503.
	Sessions *sessions.Service // GetPaper / StartOrResume / SaveDraft / Status

	// RateLimiter 是热点路由限流器 (login/submit/start/draft/status).
	// nil (单测常态) 时全部直通.
	RateLimiter *ratelimit.Limiter
}

// NewRouter 构造 examSystem 的 HTTP router。
//
// Task 1 仅挂载:
//   - middleware.RequestID + middleware.Logger + middleware.Recoverer
//   - CORS 中间件 (按 server.allow_origins)
//   - GET  /api/health
//   - POST /api/reload-config
//   - GET  / + 静态页 (/exam /admin /detail /js/* /css/*)
//   - /api/* 未知路径返 JSON 404 (不回退)
func NewRouter(deps Dependencies) http.Handler {
	cfg := deps.Config
	if cfg == nil {
		// 没传 config 时给空壳避免 panic; main 启动会用 ValidateRequired 阻止
		cfg = &config.Config{}
	}

	r := chi.NewRouter()

	// chi 内置中间件 (trim 前导/末尾空白, 与 Python 段统一)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// CORS (按 server.allow_origins)
	corsMW := corsMiddleware(cfg.Server.AllowOrigins)
	r.Use(corsMW)

	// /api/* 子树: API 行为
	r.Route("/api", func(api chi.Router) {
		api.Get("/health", healthHandler())
		// reload-config 是运维操作, 不得匿名可达: Auth 注入时套 RequireAdmin
		// (enable_auth=false 的开发态 RequireAdmin 本身放行, 行为不变).
		if deps.Auth != nil {
			api.With(deps.Auth.RequireAdmin).Post("/reload-config", reloadConfigHandler(deps))
		} else {
			api.Post("/reload-config", reloadConfigHandler(deps))
		}

		// Task 6 student 提交 + 状态查询.提交路由 (POST /api/session/submit)
		// 接收 raw JSON, 调 submissions.Service.Submit 主路径编排.
		// 注: 不用 if nil gate (与 admin.RunService nil gate 一致, 路由always mount, handler 内返503).
		// 这样无 SubmissionsService 注入时 UI/CI 路由仍可达, handler 友好返 503 而非 404
		// (避免 UI 误判 "不支持此功能" 而非 "服务暂不可用")
		submitH := rateLimitByIP(deps.RateLimiter, ratelimit.PresetPublic, "submit",
			submitHandler(deps))
		api.Post("/session/submit", submitH)
		// UI exam.js:746 实际调 POST /api/submit (非 /api/session/submit);
		// 路径 mismatch bug 2026-07-26 核查发现, 加 /api/submit 等价 handler 适配 UI,
		// 旧 /api/session/submit 保留向后兼容.
		api.Post("/submit", submitH)
		api.Get("/submission/{id}/status", submissionStatusHandler(deps))

		// Task 9 admin 路由组 (/api/admin/*): papers/open/close/batch/reorder/preview/
		// exams/exam-link/reset-rounds/login/reload-config. RequireAdmin middleware.
		MountAdmin(api, deps)

		// Task 12a student /api/exam/* 4 端点.
		mountStudentExam(api, deps)

		// /api/* 未知 -> JSON 404
		MountAPI404(api)
	})

	// 静态资源 (页面 + js/css)
	root := deps.StaticRoot
	if root == "" {
		root = StaticRoot
	}
	MountStatic(r, StaticConfig{Root: root, NoStore: true})

	// 根 root 的 404: 不回退 exam.html
	// (已被 MountStatic 的 "/" path 命中 / 404 到这里)
	r.NotFound(NotFoundJSON)
	r.MethodNotAllowed(MethodNotAllowedJSON)

	return r
}

// healthHandler 返回 {ok, time, version, build}。
// Task 1 极简版: ok=true, time=RFC3339, version 待 main 注入。
// Go 专属深字段 (db.connected / worker_alive / grading_status) 由后续 Task 加。
func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"time":    time.Now().UTC().Format(time.RFC3339),
			"version": versionInfo(),
		})
	}
}

// reloadConfigHandler 调 reload 回调并把 result.message 回给 client。
// 规范要求: 若 CORS 配置变更, 必须在 message 中提示重启生效。
func reloadConfigHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ReloadConfig == nil {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"success": false,
				"message": "reload-config not available",
			})
			return
		}
		ok, msg := deps.ReloadConfig()
		WriteJSON(w, statusFor(ok), map[string]any{
			"success": ok,
			"message": msg,
		})
	}
}

func statusFor(ok bool) int {
	if ok {
		return http.StatusOK
	}
	return http.StatusConflict
}

// corsMiddleware 实现简单 CORS:
//   - Origin 在 allow_origins 之中 -> 回显 Origin
//   - Methods: GET POST PUT PATCH DELETE OPTIONS
//   - Headers: Authorization Content-Type X-Requested-With
//   - Credentials: false
//   - Preflight OPTIONS: 204 no body
func corsMiddleware(allow []string) func(http.Handler) http.Handler {
	allowAny := len(allow) == 0
	allowed := map[string]bool{}
	for _, o := range allow {
		allowed[strings.TrimSpace(o)] = true
	}
	header := "Authorization, Content-Type, X-Requested-With"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowAny || allowed[origin]) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", header)
			w.Header().Set("Access-Control-Allow-Credentials", "false")

			// Preflight: 不回正文, 直接 204
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BuildVersion Info: Task 1 占位; main 启动时通过 linker flag 注入。
var (
	buildCommit  = "dev"
	buildTimeRaw = "unknown"
)

func versionInfo() map[string]string {
	return map[string]string{
		"version":    buildCommit,
		"build_time": buildTimeRaw,
	}
}

// contextKey 用于 ctx 注入; 备 Task 各阶段使用。
type contextKey string

func (c contextKey) String() string { return "httpapi.ctx." + string(c) }

// 注入 context 帮助函数 (后续扩展)
var _ = context.Background
var _ = json.Marshal
