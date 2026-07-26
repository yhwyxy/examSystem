// Package main 是 examSystem Go 服务的入口 (Task 1)。
//
// 三个子命令 (claude-style flag dispatch):
//
//   exam-server serve       --config config.yaml [--bind :8000] [--static frontend]
//     启动 HTTP 服务, 加载 config, 跑 NewRouter; 未挂业务路由的全部 404,
//     /api/health 上线, reload-config 接通配置热刷新回调。
//     database.url 必填, 实际 DB 连接在 Task 2 之后启用; Task 1 preflight 仅校验非空。
//
//   exam-server migrate     --config config.yaml
//     对 PostgreSQL 跑 migrations 目录下未应用的迁移; Task 1 仅占位, Task 2 落地。
//
//   exam-server preflight   --config config.yaml
//     启动前自检: 配置完整 / database.url 非空 / 静态目录可读。
//     失败立刻非零退出, 便于 docker/k8s readinessProbe 失败 quick-fail。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/finalize"
	"github.com/yhwyxy/examSystem/internal/httpapi"
	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/objective"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/review"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
	"github.com/yhwyxy/examSystem/internal/submissions"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "exam-server:", err)
		os.Exit(1)
	}
}

// run 拆出方便测试与子命令 dispatch。
func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stdout)
		return errors.New("no subcommand given")
	}

	// 子命令分发: 第一个参数决定执行分支
	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "serve":
		return cmdServe(rest)
	case "migrate":
		return cmdMigrate(rest)
	case "preflight":
		return cmdPreflight(rest)
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return nil
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("unknown subcommand: %s", cmd)
	}
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, `exam-system Go server

Usage:
  exam-server serve       --config config.yaml [--bind :8000] [--static frontend]
  exam-server migrate     --config config.yaml
  exam-server preflight   --config config.yaml

Subcommands:
  serve      Start HTTP server. /api/health + static pages online.
  migrate    Apply pending PostgreSQL migrations (stub in Task 1; Task 2 落地).
  preflight  Validate config + database.url + static dir, fail fast on missing.`)
}

// ----------------------- serve -----------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	bind := fs.String("bind", "", "override server bind address (host:port); empty=use config")
	staticRoot := fs.String("static", "", "static root directory; empty=config-derived default (frontend)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// 强制最终校验 (database.url 必非空)
	if err := cfg.ValidateRequired(); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	listen := *bind
	if listen == "" {
		listen = fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	}

	// 构造 router 依赖
	static := *staticRoot
	if static == "" {
		static = httpapi.StaticRoot // 默认 frontend
	}

	// reload-config 回调: 当前实现 reload YAML 并刷新后续路由要消费的值;
	// CORS allow_origins 变更只对新连接生效, 旧 worker 不动;
	// message 主动提示 "CORS change requires restart"。
	reloadFn := func() (bool, string) {
		newCfg, err := config.Load(*cfgPath)
		if err != nil {
			return false, fmt.Sprintf("reload failed: %v", err)
		}
		if err := newCfg.ValidateRequired(); err != nil {
			return false, fmt.Sprintf("reload invalid: %v", err)
		}
		// 提示 CORS 变更需要重启进程才完全生效
		return true, "config reloaded; CORS change requires restart"
	}

	// Task 6: 打开 pool 以供 submit / status 路径使用; 失败直接终止进程.
	ctxOpen, cancelOpen := context.WithTimeout(context.Background(), 15*time.Second)
	pool, err := db.Open(ctxOpen, cfg)
	cancelOpen()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()

	// 构造 student 路径所需 services: submissions + jobs; 注 objective.Grade + papers.LoadSnapshot.
	subRepo := submissions.NewRepository()
	jobsSvc := jobs.NewService(nil)
	// GracePeriodSeconds < 0 表示不校验 deadline + grace; cfg.Exam.GracePeriodSeconds 默认 30s.
	// auto_submit (AutoSubmitReason 非空) 路径会跳过 deadline 校验 (见 Service.Submit).
	grace := -1
	if cfg.Exam.AutoSubmit {
		grace = cfg.Exam.GracePeriodSeconds
	}
	subSvc := submissions.NewService(pool, subRepo, papers.LoadSnapshot,
		func(q map[string]any, ans any, partial bool) (map[string]any, error) {
			return objective.Grade(q, ans, partial)
		}, jobsSvc, grace)

	// Task 8: finalize 服务 - 收卷轮询 (ScanDue 每 1s) + FinalizeRun 原子事务.
	runsRepo := runs.NewRepository()
	sessionsRepo := sessions.NewRepository()
	finalizeSvc := finalize.NewService(pool, runsRepo, sessionsRepo, subRepo,
		cfg.Exam.GracePeriodSeconds)

	// Task 9: auth + papers + 注入 admin 路由. enable_auth=false -> RequireAdmin 放行.
	authStore := auth.NewStore(cfg.Admin.EnableAuth,
		func() (string, error) {
			if cfg.Admin.Password == nil {
				return "", auth.ErrInvalidCredentials
			}
			return *cfg.Admin.Password, nil
		})
	papersStore, err := papers.NewStore(static)
	if err != nil {
		return fmt.Errorf("open papers store: %w", err)
	}

	// Task 10: review 服务 (Apply / Regrade 跨表事务 + 借 jobs.EnqueueTx 异步接班).
	jobsRepo := jobs.NewRepository()
	reviewSvc := review.NewService(pool, jobsRepo)

	router := httpapi.NewRouter(httpapi.Dependencies{
		Config:             cfg,
		StaticRoot:         static,
		ReloadConfig:       reloadFn,
		Pool:               pool,
		SubmissionsService: subSvc,
		Auth:               authStore,
		Papers:             papersStore,
		Runs:               runsRepo,
		Review:             reviewSvc,
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅关闭: SIGINT/SIGTERM 7s 内清理
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Task 8: finalize ticker. 每 1s 调 ScanDue; graceful shutdown 等
	// 当前 ScanDue 结束 (最长 7s 与 srv.Shutdown 并行).
	doneFinalize := make(chan struct{})
	go func() {
		defer close(doneFinalize)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nR, nS, ferr := finalizeSvc.ScanDue(ctx)
				if ferr != nil && !errors.Is(ferr, context.Canceled) {
					fmt.Fprintf(os.Stderr, "finalize.ScanDue err: %v\n", ferr)
				}
				if nR > 0 || nS > 0 {
					fmt.Fprintf(os.Stdout, "finalize: closed %d runs, auto-submitted %d sessions\n", nR, nS)
				}
			}
		}
	}()

	go func() {
		fmt.Fprintf(os.Stdout, "exam-server serve listening on %s (config=%s static=%s)\n", listen, *cfgPath, static)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "listen error: %v\n", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Fprintln(os.Stdout, "exam-server shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// 等 finalize ticker 退出 (ctx.Done 触发 return; 现同窗口最长 7s).
	select {
	case <-doneFinalize:
	case <-time.After(7 * time.Second):
		fmt.Fprintln(os.Stderr, "finalize ticker did not stop within 7s")
	}
	fmt.Fprintln(os.Stdout, "exam-server stopped.")
	return nil
}

// ----------------------- migrate ----------------------

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.ValidateRequired(); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	// Task 2: 真实迁移
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()

	res, err := db.Migrate(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	if len(res.Applied) == 0 {
		fmt.Fprintln(os.Stdout, "migrate: database schema is current, nothing to apply.")
	} else {
		fmt.Fprintf(os.Stdout, "migrate: applied %d migration(s):\n", len(res.Applied))
		for _, v := range res.Applied {
			fmt.Fprintf(os.Stdout, "  + %s\n", v)
		}
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintf(os.Stdout, "migrate: %d already applied (skipped):\n", len(res.Skipped))
		for _, v := range res.Skipped {
			fmt.Fprintf(os.Stdout, "  = %s\n", v)
		}
	}
	fmt.Fprintf(os.Stdout, "migrate: db url %s\n", maskedDBURL(cfg.Database.URL))
	return nil
}

// ----------------------- preflight --------------------

func cmdPreflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	staticRoot := fs.String("static", "", "static root directory (override)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var problems []string
	if err := cfg.ValidateRequired(); err != nil {
		problems = append(problems, err.Error())
	}
	static := *staticRoot
	if static == "" {
		static = httpapi.StaticRoot
	}
	if fi, err := os.Stat(static); err != nil || !fi.IsDir() {
		problems = append(problems, fmt.Sprintf("static dir not accessible: %s", static))
	}

	if len(problems) > 0 {
		fmt.Fprintln(os.Stderr, "preflight failed:")
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return fmt.Errorf("preflight: %d problem(s)", len(problems))
	}

	fmt.Fprintf(os.Stdout, "preflight ok\n  config:    %s\n  db url:    %s\n  static:    %s\n",
		*cfgPath, maskedDBURL(cfg.Database.URL), static)
	return nil
}

// maskedDBURL 隐藏密码, 便于日志输出。
// 任何 host:port/db 保留; 密码段一律打码。
func maskedDBURL(s string) string {
	if s == "" {
		return "(empty)"
	}
	const k = "://"
	idx := indexString(s, k)
	if idx < 0 {
		return s
	}
	host := s[idx+len(k):]
	at := indexByte(host, '@')
	if at < 0 {
		return s
	}
	userPass := host[:at]
	rest := host[at:]
	colon := indexByte(userPass, ':')
	if colon < 0 {
		return s // 没密码不必打码
	}
	return s[:idx+len(k)] + userPass[:colon+1] + "****" + rest + " (masked)"
}

// indexString / indexByte 不依赖 strings 以避免 Task 1 引入更多包 (已经 import 字符串链路)。
func indexString(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
