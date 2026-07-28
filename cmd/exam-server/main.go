// Package main 是 examSystem Go 服务的入口 (Task 1)。
//
// 三个子命令 (claude-style flag dispatch):
//
//	exam-server serve       --config config.yaml [--bind :8000] [--static frontend]
//	  启动 HTTP 服务, 加载 config, 跑 NewRouter; 未挂业务路由的全部 404,
//	  /api/health 上线, reload-config 接通配置热刷新回调。
//	  database.url 必填, 实际 DB 连接在 Task 2 之后启用; Task 1 preflight 仅校验非空。
//
//	exam-server migrate     --config config.yaml
//	  对 PostgreSQL 跑 migrations 目录下未应用的迁移; Task 1 仅占位, Task 2 落地。
//
//	exam-server preflight   --config config.yaml
//	  启动前自检: 配置完整 / database.url 非空 / 静态目录可读。
//	  失败立刻非零退出, 便于 docker/k8s readinessProbe 失败 quick-fail。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"github.com/jackc/pgx/v5/pgxpool"
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
	// Task 12 Step 1: 日志轮转 (20MB × 10 保留) 通过 lumberjack 写文件; stdout
	// 同时保留 (MultiWriter) 便于 staging 容器化看进程输出. 不输出 token/密码
	// (stdout 仍是原型 fmt.Fprintf 形, token 等敏感值本就未打印; 日志文件层零侵入).
	appLogger := initLogger(cfg)
	_ = appLogger // appLogger 暂用于将来替换 fmt.Fprintf 入口; 现仅 MultiWriter 副作用
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
	// dataRoot: 试卷 / 快照 / token 数据根目录, 默认 "data"
	dataRoot := cfg.Server.DataRoot
	if dataRoot == "" {
		dataRoot = "data"
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
	papersStore, err := papers.NewStore(filepath.Join(dataRoot, "papers"))
	if err != nil {
		return fmt.Errorf("open papers store: %w", err)
	}

	// Task 10: review 服务 (Apply / Regrade 跨表事务 + 借 jobs.EnqueueTx 异步接班).
	jobsRepo := jobs.NewRepository()
	reviewSvc := review.NewService(pool, jobsRepo)

	// Task 12a: sessions.Service (student /api/exam/* 公开 + 鉴权端点
	//   依赖 sessions.RunsLookup 接口). 用 adapter 包装 runs.Repository,
	//   适配 hash_token 形态 (FindByPublicTokenHash) + RunLite 字段裁剪.
	runsLookup := &runsLookupAdapter{repo: runsRepo, pool: pool}
	sessionsSvc := sessions.NewService(sessionsRepo, runsLookup,
		sessions.WithGracePeriod(time.Duration(cfg.Exam.GracePeriodSeconds)*time.Second))

	// Task 14: runs.Service (admin 开考/关考/exam-link 真业务依赖). 注入 :=
	//   RunService 给 admin 4 stub 替换用; token 侧车目录 cfg.Logging.RunTokenDir
	//   (空 = exam-link GET 无法重建 URL, 文档已记遗留). pubRoot 用 dataRoot/exam_runs.
	examRunsDir := filepath.Join(dataRoot, "exam_runs")
	_ = os.MkdirAll(examRunsDir, 0o700)
	runService := runs.NewService(runsRepo, papersStore, examRunsDir)
	if cfg.Logging.RunTokenDir != "" {
		runService = runService.WithTokenDir(cfg.Logging.RunTokenDir)
		_ = os.MkdirAll(cfg.Logging.RunTokenDir, 0o700)
	} else {
		// 默认 token 侧车目录: data/exam_runs (与 Python 旧栈 EXAM_RUNS_DIR 兼容)
		runService = runService.WithTokenDir(examRunsDir)
	}
	runService = runService.WithExamAutoSubmit(cfg.Exam.AutoSubmit) // 2026-07-26 不硬编码

	router := httpapi.NewRouter(httpapi.Dependencies{
		Config:             cfg,
		StaticRoot:         static,
		ReloadConfig:       reloadFn,
		Pool:               pool,
		SubmissionsService: subSvc,
		Auth:               authStore,
		Papers:             papersStore,
		Runs:               runsRepo,
		RunService:         runService, // Task 14: 真 admin 开考 / 关考 / exam-link
		Review:             reviewSvc,
		Sessions:           sessionsSvc,
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

// runsLookupAdapter 桥 runs.Repository (生产 PG) 与 sessions.RunsLookup interface.
// 注: runs.Repository 的 FindBy* 第 2 参 tx 必填, adapter 持 pgxpool.Pool 引入有事务
// 路径 (此处只用 pool 即可, 无需开事务).
type runsLookupAdapter struct {
	repo *runs.Repository
	pool *pgxpool.Pool
}

// FindByToken 把 run_token (raw) hash 后调 runs.FindByPublicTokenHash 并裁剪成 RunLite.
// 用 Begin-Tx 桥 (runs.Repository.FindBy* 强接 pgx.Tx), defer Rollback 释放.
func (a *runsLookupAdapter) FindByToken(ctx context.Context, runToken string) (*sessions.RunLite, error) {
	h := sha256.Sum256([]byte(runToken))
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("runsLookupAdapter.FindByToken: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := a.repo.FindByPublicTokenHash(ctx, tx, hex.EncodeToString(h[:]))
	return runToLite(run, err)
}

// FindByID 通过 run_id 调 runs.FindByID 并裁剪成 RunLite.
func (a *runsLookupAdapter) FindByID(ctx context.Context, runID string) (*sessions.RunLite, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("runsLookupAdapter.FindByID: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := a.repo.FindByID(ctx, tx, runID)
	return runToLite(run, err)
}

// runToLite 错误处理 + runs.Run -> sessions.RunLite 字段裁剪.
func runToLite(run *runs.Run, err error) (*sessions.RunLite, error) {
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, nil
	}
	return &sessions.RunLite{
		ID: run.ID, PaperID: run.PaperID, RoundNo: run.RoundNo,
		Status: string(run.Status), Duration: time.Duration(run.DurationMinutes) * time.Minute,
		SnapshotPath: run.SnapshotPath, SnapshotHash: run.SnapshotHash,
		FinalizeAt: run.FinalizeAt,
	}, nil
}

// initLogger 用 lumberjack 配置日志轮转 (Task 12 Step 1: 20MB×10 保留)
// 并把 stdout/stderr 重定向到 MultiWriter(stdout + 文件), 保留原 print 行为不动.
// 用 defer redir 在 main 退出时关闭日志文件句柄.
func initLogger(cfg *config.Config) *log.Logger {
	logDir := cfg.Logging.Directory
	if logDir == "" {
		logDir = "logs"
	}
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "exam-server.log")
	lj := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    cfg.Logging.MaxSizeMB,
		MaxBackups: cfg.Logging.MaxBackups,
		MaxAge:     cfg.Logging.MaxAgeDays,
		Compress:   true,
	}
	multi := io.MultiWriter(os.Stdout, lj)
	// os.Stdout 是 *os.File, 我们不替换 fd; 用 stdLog 走 multi 触发文件写入, 自检可写.
	stdLog := log.New(multi, "", log.LstdFlags|log.LUTC)
	stdLog.Printf("log init: %s (max=%dMB backups=%d age=%dd)",
		logPath, cfg.Logging.MaxSizeMB, cfg.Logging.MaxBackups, cfg.Logging.MaxAgeDays)
	return stdLog
}
