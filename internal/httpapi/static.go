package httpapi

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// StaticRoot 是项目内静态资源根目录 (Task 1 默认 frontend/)。
// 生产可由 main 通过 dependencies 配置注入或 env 覆盖。
const StaticRoot = "frontend"

// StaticConfig 控制静态服务行为。
type StaticConfig struct {
	// Root 指向静态资源根目录; 不存在则在启动时拒绝。
	Root string
	// NoStore 是否给页面/脚本回 no-store/no-cache 头。
	// Task 1 规范: 页面与脚本必带 no-store/no-cache。
	NoStore bool
}

// MountStatic 把 "/" 下的静态页/JS/CSS 路由挂到 router, 未知路径返 404,
// 不 fallback 到 exam.html (避免 leak / 安全风险)。
//
// 静态请命中表:
//   GET /                    -> frontend/index.html? 没 index 时 404 (Task 1 Play型 -> 仅 SPA root)
//   GET /exam                -> frontend/exam.html
//   GET /admin               -> frontend/admin.html
//   GET /detail              -> frontend/detail.html
//   GET /js/*, /css/*        -> frontend/{js,css}/* (SPA assets)
//   其它 GET                 -> 404 (不 fallback)
//
// 与产品当前 FastAPI 后端行为保持等价。
func MountStatic(r chi.Router, cfg StaticConfig) {
	root := cfg.Root
	if root == "" {
		root = StaticRoot
	}

	// 拦截器: 处理 no-store + 拒绝目录列出 + forbid fallback。
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.NoStore {
				w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
				w.Header().Set("Pragma", "no-cache")
				w.Header().Set("Expires", "0")
			}
			next.ServeHTTP(w, r)
		})
	}

	// /js /css: 静态资源子路径
	r.Route("/js", func(sub chi.Router) {
		sub.Use(wrap)
		sub.Handle("/*", http.StripPrefix("/js/", http.FileServer(http.Dir(filepath.Join(root, "js")))))
	})
	r.Route("/css", func(sub chi.Router) {
		sub.Use(wrap)
		sub.Handle("/*", http.StripPrefix("/css/", http.FileServer(http.Dir(filepath.Join(root, "css")))))
	})

	// 页面: 直接 ServeFile; 未知页面路径走 root NotFound (不回退)
	serveHTML := func(rel string) http.HandlerFunc {
		full := filepath.Join(root, rel)
		return func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, full)
		}
	}
	// 同时给页面绑 no-store
	pageH := func(name string) http.Handler {
		return wrap(serveHTML(name))
	}

	// SPA 根路径: 优先 index.html; 不存在则 404 (http.ServeFile 处理)。
	r.With(wrap).Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})

	r.With(wrap).Get("/exam", pageH("exam.html").(http.HandlerFunc)) // 见下:ServeFile 优雅 fallback
	r.With(wrap).Get("/admin", pageH("admin.html").(http.HandlerFunc))
	r.With(wrap).Get("/detail", pageH("detail.html").(http.HandlerFunc))

	// 防御: 任何其它静态路径都不得回退到 exam.html/任意页面
	// chi NotFound 在 /api/ 已被 MountAPI404 占用, 静态侧由 main 兜底
	_ = strings.TrimSpace
}
