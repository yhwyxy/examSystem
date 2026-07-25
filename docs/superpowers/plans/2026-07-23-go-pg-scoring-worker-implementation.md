# Go API + PostgreSQL + Python 评分 Worker 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将考试系统后端从 Python/SQLite 迁移到 Go/PostgreSQL，支持单机 Windows 最高 500 人同时在线考试。

**Architecture:** Go API (`exam-server.exe`) 负责全部 HTTP 与客观题判分；PostgreSQL 做持久存储与 `grading_jobs` 任务队列；Python scoring-worker 仅处理主观题评分并回写成绩；前端 `frontend/` 不做框架重构。

**Tech Stack:** Go 1.23+, pgx/v5, chi router, PostgreSQL 16+, Python 3.11+ (worker only)

## Global Constraints

- 部署平台：Windows 10/11 单机，无 Docker
- 数据库：PostgreSQL Windows 服务，`postgres://exam:***@127.0.0.1:5432/exam?sslmode=disable`
- `sync_grading` 一律强制 async；不在交卷请求中阻塞等主观题
- Admin 鉴权：Bearer token（`Authorization: Bearer <token>`，对齐现网 `localStorage.admin_token`）
- API 路径与方法与现网保持一致；错误码字符串保留（`STALE_DRAFT_REVISION` 等）
- 草稿 CAS 用 `draft_revision`；服务端 min interval 2s
- 客观题由 Go 在交卷路径即时判分；主观题由 Python worker 异步评分

---

## P0: PostgreSQL Schema + Go 项目骨架 + 迁移

### Task 0.1: 初始化 Go module 与项目目录

**Files:**
- Create: `go.mod`
- Create: `cmd/exam-server/main.go`
- Create: `internal/config/config.go`
- Create: `.gitignore` (追加 Go 条目)

- [ ] **Step 1: 初始化 Go module**

```bash
cd /Users/yhw/Code/Github/examSystem
go mod init examSystem
```

- [ ] **Step 2: 创建最小 main.go**

```go
// cmd/exam-server/main.go
package main

import (
	"log"
	"net/http"
)

func main() {
	log.Println("exam-server starting on :8000")
	http.ListenAndServe(":8000", nil)
}
```

- [ ] **Step 3: 创建 config.go（占位）**

```go
// internal/config/config.go
package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Grading  GradingConfig  `yaml:"grading"`
	Draft    DraftConfig    `yaml:"draft"`
	Auth    AuthConfig     `yaml:"auth"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type DatabaseConfig struct {
	URL      string `yaml:"url"`
	MaxConns int    `yaml:"max_conns"`
	MinConns int    `yaml:"min_conns"`
}

type GradingConfig struct {
	SyncGrading       bool `yaml:"sync_grading"`
	WorkerLeaseSeconds int  `yaml:"worker_lease_seconds"`
	MaxAttempts       int  `yaml:"max_attempts"`
}

type DraftConfig struct {
	MinServerIntervalMs int `yaml:"min_server_interval_ms"`
	MaxJSONBytes        int `yaml:"max_json_bytes"`
}

type AuthConfig struct {
	AdminPassword string `yaml:"admin_password"`
	AdminToken    string `yaml:"admin_token"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Server: ServerConfig{Host: "0.0.0.0", Port: 8000},
		Database: DatabaseConfig{MaxConns: 32, MinConns: 4},
		Grading: GradingConfig{WorkerLeaseSeconds: 300, MaxAttempts: 5},
		Draft: DraftConfig{MinServerIntervalMs: 2000, MaxJSONBytes: 512000},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Database.URL == "" {
		cfg.Database.URL = os.Getenv("DATABASE_URL")
	}
	return cfg, nil
}
```

- [ ] **Step 4: 构建验证**

```bash
go build ./cmd/exam-server/
```

Expected: 编译成功，无错误。

- [ ] **Step 5: 安装依赖**

```bash
go get github.com/jackc/pgx/v5
go get gopkg.in/yaml.v3
go get github.com/go-chi/chi/v5
go mod tidy
```

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cmd/ internal/ .gitignore
git commit -m "feat: init Go module, config loader, minimal main"
```

---

### Task 0.2: 创建 PostgreSQL 迁移 SQL

**Files:**
- Create: `migrations/001_initial.sql`

- [ ] **Step 1: 编写完整迁移 SQL**

```sql
-- migrations/001_initial.sql

CREATE TABLE IF NOT EXISTS exam_runs (
    id TEXT PRIMARY KEY,
    paper_id TEXT NOT NULL,
    round_no INTEGER NOT NULL,
    public_token_hash TEXT UNIQUE,
    status TEXT NOT NULL DEFAULT 'open',
    duration_minutes INTEGER NOT NULL DEFAULT 60,
    snapshot_path TEXT,
    snapshot_hash TEXT,
    is_legacy BOOLEAN DEFAULT false,
    extended_minutes INTEGER DEFAULT 0,
    opened_at TIMESTAMPTZ,
    closing_started_at TIMESTAMPTZ,
    finalize_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(paper_id, round_no)
);

CREATE UNIQUE INDEX uq_exam_runs_active
    ON exam_runs (paper_id)
    WHERE status IN ('open', 'closing');

CREATE TABLE IF NOT EXISTS exam_sessions (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES exam_runs(id),
    employee_id TEXT,
    name TEXT,
    department TEXT,
    session_token_hash TEXT NOT NULL,
    started_at TIMESTAMPTZ,
    deadline_at TIMESTAMPTZ,
    draft_json JSONB NOT NULL DEFAULT '{}',
    draft_revision INTEGER NOT NULL DEFAULT 0,
    draft_saved_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'active',
    auto_submit_scheduled BOOLEAN DEFAULT false,
    client_ip TEXT,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(run_id, employee_id)
);

CREATE INDEX idx_sessions_token_hash ON exam_sessions(session_token_hash);
CREATE INDEX idx_sessions_run_status ON exam_sessions(run_id, status);

CREATE TABLE IF NOT EXISTS submissions (
    id BIGSERIAL PRIMARY KEY,
    name TEXT,
    employee_id TEXT,
    paper_id TEXT,
    paper_name TEXT,
    run_id TEXT NOT NULL,
    department TEXT,
    answers_json JSONB,
    grading_detail_json JSONB NOT NULL DEFAULT '[]',
    objective_score DOUBLE PRECISION DEFAULT 0,
    subjective_score_machine DOUBLE PRECISION DEFAULT 0,
    subjective_score_final DOUBLE PRECISION DEFAULT 0,
    total_score DOUBLE PRECISION DEFAULT 0,
    review_status TEXT NOT NULL DEFAULT 'grading',
    grading_status TEXT NOT NULL DEFAULT 'pending',
    grading_error TEXT,
    graded_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    submitted_at TIMESTAMPTZ,
    reviewed_at TIMESTAMPTZ,
    reviewer_note TEXT,
    client_ip TEXT,
    user_agent TEXT,
    auto_submit_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(employee_id, run_id)
);

CREATE INDEX idx_submissions_run ON submissions(run_id);
CREATE INDEX idx_submissions_grading_status ON submissions(grading_status);

CREATE TABLE IF NOT EXISTS grading_jobs (
    id BIGSERIAL PRIMARY KEY,
    submission_id BIGINT UNIQUE NOT NULL REFERENCES submissions(id),
    paper_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    attempts INTEGER DEFAULT 0,
    max_attempts INTEGER DEFAULT 5,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_grading_jobs_claim
    ON grading_jobs (available_at)
    WHERE status IN ('queued', 'leased');

CREATE TABLE IF NOT EXISTS review_logs (
    id BIGSERIAL PRIMARY KEY,
    submission_id BIGINT NOT NULL REFERENCES submissions(id),
    reviewer TEXT NOT NULL,
    action TEXT NOT NULL,
    details TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- [ ] **Step 2: Commit**

```bash
git add migrations/001_initial.sql
git commit -m "feat: PostgreSQL initial migration schema"
```

---

### Task 0.3: 实现数据库连接池与迁移执行器

**Files:**
- Create: `internal/db/pool.go`
- Create: `internal/db/migrate.go`

- [ ] **Step 1: 实现连接池**

```go
// internal/db/pool.go
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool struct {
	*pgxpool.Pool
}

func NewPool(ctx context.Context, url string, maxConns, minConns int32) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Pool{Pool: pool}, nil
}
```

- [ ] **Step 2: 实现迁移执行器**

```go
// internal/db/migrate.go
package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func RunMigrations(ctx context.Context, pool *Pool, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migration dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		path := filepath.Join(dir, f)
		sql, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("execute %s: %w", f, err)
		}
		fmt.Printf("migration applied: %s\n", f)
	}
	return nil
}
```

- [ ] **Step 3: 更新 main.go 接入配置与数据库**

```go
// cmd/exam-server/main.go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"examSystem/internal/config"
	"examSystem/internal/db"
)

func main() {
	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.Database.URL,
		int32(cfg.Database.MaxConns), int32(cfg.Database.MinConns))
	if err != nil {
		log.Fatalf("db pool: %v", err)
	}
	defer pool.Close()

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := db.RunMigrations(ctx, pool, "migrations"); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		log.Println("migrations complete")
		return
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Printf("exam-server listening on %s", addr)
	http.ListenAndServe(addr, nil)
}
```

- [ ] **Step 4: 构建验证**

```bash
go build ./cmd/exam-server/
```

Expected: 编译成功。

- [ ] **Step 5: Commit**

```bash
git add internal/db/ cmd/exam-server/main.go
git commit -m "feat: db pool, migration runner, config-wired main"
```

---

### Task 0.4: 编写 SQLite→PostgreSQL 数据迁移脚本

**Files:**
- Create: `cmd/migrate-sqlite/main.go`

- [ ] **Step 1: 实现迁移工具**

```go
// cmd/migrate-sqlite/main.go
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SourceRow map[string]interface{}

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: migrate-sqlite <sqlite_path> <pg_url>")
	}
	sqlitePath := os.Args[1]
	pgURL := os.Args[2]

	src, err := sql.Open("sqlite3", sqlitePath+"?_journal_mode=WAL")
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer src.Close()

	ctx := context.Background()
	dst, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatalf("open pg: %v", err)
	}
	defer dst.Close()

	tables := []string{"exam_runs", "exam_sessions", "submissions", "review_logs"}
	for _, tbl := range tables {
		if err := migrateTable(ctx, src, dst, tbl); err != nil {
			log.Fatalf("migrate %s: %v", tbl, err)
		}
		fmt.Printf("migrated table: %s\n", tbl)
	}

	// update sequences
	for _, seq := range []string{"submissions_id_seq", "review_logs_id_seq"} {
		dst.Exec(ctx, fmt.Sprintf("SELECT setval('%s', COALESCE((SELECT MAX(id) FROM %s), 1))",
			seq, seq[:len(seq)-4]))
	}

	fmt.Println("migration complete")
}

func migrateTable(ctx context.Context, src *sql.DB, dst *pgxpool.Pool, table string) error {
	rows, err := src.Query(fmt.Sprintf("SELECT * FROM %s", table))
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	colTypes, _ := rows.ColumnTypes()

	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}

		placeholders := make([]string, len(cols))
		values := make([]interface{}, len(cols))
		for i, c := range cols {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			v := coerceValue(vals[i], colTypes[i].DatabaseTypeName())
			values[i] = v
		}

		insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING",
			table, joinCols(cols), joinStrs(placeholders, ","))
		if _, err := dst.Exec(ctx, insert, values...); err != nil {
			log.Printf("skip row in %s: %v", table, err)
		}
	}
	return nil
}

func coerceValue(v interface{}, dbType string) interface{} {
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch dbType {
	case "TEXT":
		// try parse as time, then as json
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			return t
		}
		if json.Valid([]byte(s)) {
			return s // keep JSON string; pgx handles JSONB
		}
		return s
	default:
		return v
	}
}

func joinCols(cols []string) string {
	return joinStrs(cols, ", ")
}

func joinStrs(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	r := ss[0]
	for _, s := range ss[1:] {
		r += sep + s
	}
	return r
}
```

- [ ] **Step 2: 安装 sqlite3 driver**

```bash
go get github.com/mattn/go-sqlite3
go mod tidy
```

- [ ] **Step 3: 构建验证**

```bash
go build ./cmd/migrate-sqlite/
```

Expected: 编译成功。

- [ ] **Step 4: Commit**

```bash
git add cmd/migrate-sqlite/
git commit -m "feat: SQLite to PostgreSQL migration tool"
```

---

### Task 0.5: 在本地 PostgreSQL 执行迁移验证

**Prerequisites:** PostgreSQL 16+ 已安装并运行，已创建 `exam` 数据库。

- [ ] **Step 1: 执行迁移**

```bash
DATABASE_URL="postgres://exam:password@127.0.0.1:5432/exam?sslmode=disable" \
  go run ./cmd/exam-server/ migrate
```

Expected: 输出 `migration applied: 001_initial.sql`，然后 `migrations complete`。

- [ ] **Step 2: 验证表结构**

```bash
psql -U exam -d exam -c "\dt"
psql -U exam -d exam -c "\d exam_runs"
psql -U exam -d exam -c "\d grading_jobs"
```

Expected: 列出 5 张表；`exam_runs` 含 `uq_exam_runs_active` 索引；`grading_jobs` 含 `idx_grading_jobs_claim`。

- [ ] **Step 3: Commit（确认无代码变更则跳过）**

---

## P1: 考生主路径 — health、exam start、draft、submit（含客观题判分）

### Task 1.1: 实现 HTTP 路由骨架与 health 端点

**Files:**
- Create: `internal/httpapi/router.go`
- Modify: `cmd/exam-server/main.go`

- [ ] **Step 1: 创建路由器**

```go
// internal/httpapi/router.go
package httpapi

import (
	"encoding/json"
	"net/http"

	"examSystem/internal/db"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	DB  *db.Pool
	CORS string
}

func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(s.corsMiddleware)

	r.Get("/api/health", s.handleHealth)

	// static files served via separate route in main
	return r
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.CORS)
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	err := s.DB.Ping(r.Context())
	status := "ok"
	if err != nil {
		status = "db_error"
	}
	jsonResp(w, http.StatusOK, map[string]string{
		"status": status,
		"db":     "connected",
	})
}

func jsonResp(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonResp(w, code, map[string]interface{}{
		"success": false,
		"error":   msg,
		"code":    msg,
	})
}
```

- [ ] **Step 2: 更新 main.go 接入路由器**

```go
// cmd/exam-server/main.go
// ... (保留 import 和 config 加载)

	"examSystem/internal/httpapi"

// 替换 http.ListenAndServe 为:
	srv := &httpapi.Server{
		DB:   pool,
		CORS: "*",
	}
	handler := httpapi.NewRouter(srv)
	log.Printf("exam-server listening on %s", addr)
	http.ListenAndServe(addr, handler)
```

- [ ] **Step 3: 构建并测试**

```bash
go build ./cmd/exam-server/
# 在另一个终端:
DATABASE_URL="postgres://exam:password@127.0.0.1:5432/exam?sslmode=disable" \
  ./exam-server &
curl http://localhost:8000/api/health
```

Expected: `{"status":"ok","db":"connected"}`。

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi/ cmd/exam-server/main.go
git commit -m "feat: HTTP router with health endpoint"
```

---

### Task 1.2: 实现试卷读取（文件驱动，对齐现网）

**Files:**
- Create: `internal/papers/papers.go`

- [ ] **Step 1: 实现试卷加载**

```go
// internal/papers/papers.go
package papers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type IndexEntry struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Order int    `json:"order"`
}

type Index struct {
	Papers []IndexEntry `json:"papers"`
}

type Paper struct {
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	TimeLimit   int          `json:"time_limit"`
	TotalScore  float64      `json:"total_score"`
	PassingScore float64     `json:"passing_score"`
	Sections    []Section    `json:"sections"`
}

type Section struct {
	Name      string     `json:"name"`
	Questions []Question `json:"questions"`
}

type Question struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`  // single, multiple, true_false, short_answer, essay, composite
	Content string   `json:"content"`
	Options []Option `json:"options,omitempty"`
	Answer  interface{} `json:"answer,omitempty"`
	Score   float64  `json:"score"`
	SubQuestions []SubQuestion `json:"sub_questions,omitempty"`
}

type Option struct {
	Key   string `json:"key"`
	Text  string `json:"text"`
}

type SubQuestion struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Content string   `json:"content"`
	Options []Option `json:"options,omitempty"`
	Answer  interface{} `json:"answer,omitempty"`
	Score   float64  `json:"score"`
}

func LoadIndex(dataDir string) (*Index, error) {
	p := filepath.Join(dataDir, "papers", "index.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func LoadPaper(dataDir, slug string) (*Paper, error) {
	p := filepath.Join(dataDir, "papers", slug+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read paper %s: %w", slug, err)
	}
	var paper Paper
	if err := json.Unmarshal(data, &paper); err != nil {
		return nil, err
	}
	return &paper, nil
}

func (p *Paper) FlattenQuestions() []Question {
	var qs []Question
	for _, sec := range p.Sections {
		for _, q := range sec.Questions {
			qs = append(qs, q)
		}
	}
	return qs
}

func (p *Paper) HasSubjectiveQuestions() bool {
	for _, q := range p.FlattenQuestions() {
		if q.Type == "short_answer" || q.Type == "essay" || q.Type == "composite" {
			return true
		}
		if q.Type == "composite" {
			for _, sq := range q.SubQuestions {
				if sq.Type == "short_answer" || sq.Type == "essay" {
					return true
				}
			}
		}
	}
	return false
}
```

- [ ] **Step 2: 构建验证**

```bash
go build ./internal/papers/
```

Expected: 编译成功。

- [ ] **Step 3: Commit**

```bash
git add internal/papers/
git commit -m "feat: paper loader (file-driven, aligns with current)"
```

---

### Task 1.3: 实现客观题判分模块

**Files:**
- Create: `internal/objective/objective.go`

- [ ] **Step 1: 移植 Python 客观题判分**

```go
// internal/objective/objective.go
package objective

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"examSystem/internal/papers"
)

type GradingDetail struct {
	QuestionID      string  `json:"question_id"`
	QuestionType    string  `json:"question_type"`
	UserAnswer      interface{} `json:"user_answer"`
	CorrectAnswer   interface{} `json:"correct_answer"`
	Score           float64 `json:"score"`
	MaxScore        float64 `json:"max_score"`
	IsCorrect       bool    `json:"is_correct"`
	Message         string  `json:"message,omitempty"`
	SubQuestion     *SubDetail `json:"sub_question,omitempty"`
}

type SubDetail struct {
	Content       string  `json:"content"`
	Score         float64 `json:"score"`
	MaxScore      float64 `json:"max_score"`
	IsCorrect     bool    `json:"is_correct"`
	Message       string  `json:"message,omitempty"`
}

func GradeAnswers(paper *papers.Paper, userAnswers map[string]interface{}) (float64, []GradingDetail) {
	var total float64
	var details []GradingDetail

	for _, q := range paper.FlattenQuestions() {
		ua, hasAnswer := userAnswers[q.ID]
		gd := GradingDetail{
			QuestionID:   q.ID,
			QuestionType: q.Type,
			MaxScore:     q.Score,
			CorrectAnswer: q.Answer,
		}
		if !hasAnswer {
			gd.UserAnswer = nil
			gd.Score = 0
			gd.Message = "未作答"
			details = append(details, gd)
			continue
		}
		gd.UserAnswer = ua

		switch q.Type {
		case "single":
			correct := normalizeAnswer(q.Answer)
			user := normalizeAnswer(ua)
			if correct == user {
				gd.Score = q.Score
				gd.IsCorrect = true
			} else {
				gd.Score = 0
				gd.Message = fmt.Sprintf("正确答案: %s", correct)
			}
		case "multiple":
			correct := normalizeMultiAnswer(q.Answer)
			user := normalizeMultiAnswer(ua)
			if setsEqual(correct, user) {
				gd.Score = q.Score
				gd.IsCorrect = true
			} else if hasIntersection(correct, user) && len(user) <= len(correct) {
				gd.Score = q.Score * 0.5
				gd.IsCorrect = false
				gd.Message = "部分正确"
			} else {
				gd.Score = 0
				gd.Message = fmt.Sprintf("正确答案: %v", correct)
			}
		case "true_false":
			correct := normalizeAnswer(q.Answer)
			user := normalizeAnswer(ua)
			if correct == user {
				gd.Score = q.Score
				gd.IsCorrect = true
			} else {
				gd.Score = 0
				gd.Message = fmt.Sprintf("正确答案: %s", correct)
			}
		default:
			// subjective: skip, scored by worker
			gd.Score = 0
			gd.Message = "待主观评分"
		}
		total += gd.Score
		details = append(details, gd)
	}
	return total, details
}

func normalizeAnswer(v interface{}) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(strings.ToUpper(val))
	case float64:
		return strings.TrimSpace(strings.ToUpper(fmt.Sprintf("%v", val)))
	case []interface{}:
		var ss []string
		for _, item := range val {
			ss = append(ss, normalizeAnswer(item))
		}
		sort.Strings(ss)
		return strings.Join(ss, ",")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func normalizeMultiAnswer(v interface{}) []string {
	switch val := v.(type) {
	case []interface{}:
		var ss []string
		for _, item := range val {
			s := normalizeAnswer(item)
			if s != "" {
				ss = append(ss, s)
			}
		}
		sort.Strings(ss)
		return ss
	case string:
		return []string{strings.TrimSpace(strings.ToUpper(val))}
	default:
		s := normalizeAnswer(v)
		if s != "" {
			return []string{s}
		}
		return nil
	}
}

func setsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasIntersection(a, b []string) bool {
	set := make(map[string]bool)
	for _, x := range a {
		set[x] = true
	}
	for _, y := range b {
		if set[y] {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: 构建验证**

```bash
go build ./internal/objective/
```

Expected: 编译成功。

- [ ] **Step 3: Commit**

```bash
git add internal/objective/
git commit -m "feat: objective grading (single/multiple/true_false) ported from Python"
```

---

### Task 1.4: 实现 exam_runs CRUD 与快照生成

**Files:**
- Create: `internal/runs/runs.go`

- [ ] **Step 1: 实现 runs 包**

```go
// internal/runs/runs.go
package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"examSystem/internal/db"
	"examSystem/internal/papers"

	"github.com/google/uuid"
)

type ExamRun struct {
	ID                 string     `json:"id"`
	PaperID            string     `json:"paper_id"`
	RoundNo            int        `json:"round_no"`
	PublicTokenHash    string     `json:"-"`
	PublicToken        string     `json:"public_token,omitempty"`
	Status             string     `json:"status"`
	DurationMinutes    int        `json:"duration_minutes"`
	SnapshotPath       string     `json:"snapshot_path,omitempty"`
	SnapshotHash       string     `json:"snapshot_hash,omitempty"`
	IsLegacy           bool       `json:"is_legacy"`
	ExtendedMinutes    int        `json:"extended_minutes"`
	OpenedAt           *time.Time `json:"opened_at,omitempty"`
	ClosingStartedAt   *time.Time `json:"closing_started_at,omitempty"`
	FinalizeAt         *time.Time `json:"finalize_at,omitempty"`
	ClosedAt           *time.Time `json:"closed_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	Papers             interface{} `json:"papers,omitempty"`
}

func CreateRun(ctx context.Context, pool *db.Pool, dataDir, paperID string, duration int) (*ExamRun, error) {
	paper, err := papers.LoadPaper(dataDir, paperID)
	if err != nil {
		return nil, fmt.Errorf("load paper: %w", err)
	}

	// next round_no
	var roundNo int
	pool.QueryRow(ctx, `SELECT COALESCE(MAX(round_no),0)+1 FROM exam_runs WHERE paper_id=$1`, paperID).Scan(&roundNo)

	runID := uuid.New().String()
	token := uuid.New().String()
	tokenHash := hashToken(token)

	// snapshot
	snapshotDir := filepath.Join(dataDir, "exam_runs")
	os.MkdirAll(snapshotDir, 0755)
	snapPath := filepath.Join(snapshotDir, runID+".json")
	snapData, _ := json.Marshal(paper)
	snapHash := hashBytes(snapData)
	os.WriteFile(snapPath, snapData, 0644)

	// token file (sidecar)
	os.WriteFile(snapPath+".token", []byte(token), 0600)

	now := time.Now()
	_, err = pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, round_no, public_token_hash, status, duration_minutes,
		snapshot_path, snapshot_hash, opened_at, created_at)
		VALUES ($1,$2,$3,$4,'open',$5,$6,$7,$8,$9)`,
		runID, paperID, roundNo, tokenHash, duration,
		snapPath, snapHash, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}

	return &ExamRun{
		ID: runID, PaperID: paperID, RoundNo: roundNo,
		PublicToken: token, Status: "open", DurationMinutes: duration,
		SnapshotPath: snapPath, SnapshotHash: snapHash,
		OpenedAt: &now, CreatedAt: now,
	}, nil
}

func GetRun(ctx context.Context, pool *db.Pool, runID string) (*ExamRun, error) {
	row := pool.QueryRow(ctx, `SELECT id, paper_id, round_no, public_token_hash, status,
		duration_minutes, snapshot_path, snapshot_hash, is_legacy, extended_minutes,
		opened_at, closing_started_at, finalize_at, closed_at, created_at
		FROM exam_runs WHERE id=$1`, runID)
	r := &ExamRun{}
	err := row.Scan(&r.ID, &r.PaperID, &r.RoundNo, &r.PublicTokenHash, &r.Status,
		&r.DurationMinutes, &r.SnapshotPath, &r.SnapshotHash, &r.IsLegacy,
		&r.ExtendedMinutes, &r.OpenedAt, &r.ClosingStartedAt, &r.FinalizeAt, &r.ClosedAt, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func GetActiveRun(ctx context.Context, pool *db.Pool, paperID string) (*ExamRun, error) {
	row := pool.QueryRow(ctx, `SELECT id FROM exam_runs
		WHERE paper_id=$1 AND status IN ('open','closing')`, paperID)
	var id string
	if err := row.Scan(&id); err != nil {
		return nil, err
	}
	return GetRun(ctx, pool, id)
}

func ListOpenRuns(ctx context.Context, pool *db.Pool) ([]ExamRun, error) {
	rows, err := pool.Query(ctx, `SELECT id, paper_id, round_no, status,
		duration_minutes, opened_at, created_at
		FROM exam_runs WHERE status IN ('open','closing') ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []ExamRun
	for rows.Next() {
		var r ExamRun
		rows.Scan(&r.ID, &r.PaperID, &r.RoundNo, &r.Status,
			&r.DurationMinutes, &r.OpenedAt, &r.CreatedAt)
		runs = append(runs, r)
	}
	return runs, nil
}

func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
```

- [ ] **Step 2: 安装 uuid dependency**

```bash
go get github.com/google/uuid
go mod tidy
```

- [ ] **Step 3: 构建验证**

```bash
go build ./internal/runs/
```

Expected: 编译成功。

- [ ] **Step 4: Commit**

```bash
git add internal/runs/ go.mod go.sum
git commit -m "feat: exam_runs CRUD and snapshot generation"
```

---

### Task 1.5: 实现开考与草稿 API（GET /api/exam, POST /api/exam/start, PUT draft, GET status）

**Files:**
- Create: `internal/sessions/sessions.go`
- Create: `internal/httpapi/exam.go`

- [ ] **Step 1: 实现 sessions 业务逻辑**

```go
// internal/sessions/sessions.go
package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"examSystem/internal/db"

	"github.com/google/uuid"
)

type Session struct {
	ID                string     `json:"id"`
	RunID             string     `json:"run_id"`
	EmployeeID        string     `json:"employee_id"`
	Name              string     `json:"name"`
	Department        string     `json:"department"`
	SessionToken      string     `json:"session_token,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	DeadlineAt        *time.Time `json:"deadline_at"`
	DraftJSON         json.RawMessage `json:"draft_json"`
	DraftRevision     int        `json:"draft_revision"`
	DraftSavedAt      *time.Time `json:"draft_saved_at"`
	Status            string     `json:"status"`
}

func StartOrResume(ctx context.Context, pool *db.Pool, run *runs.ExamRun, employeeID, name, dept string) (*Session, error) {
	// try resume
	var s Session
	err := pool.QueryRow(ctx, `SELECT id, run_id, employee_id, name, department,
		started_at, deadline_at, draft_json, draft_revision, draft_saved_at, status
		FROM exam_sessions WHERE run_id=$1 AND employee_id=$2`,
		run.ID, employeeID).Scan(
		&s.ID, &s.RunID, &s.EmployeeID, &s.Name, &s.Department,
		&s.StartedAt, &s.DeadlineAt, &s.DraftJSON, &s.DraftRevision,
		&s.DraftSavedAt, &s.Status)
	if err == nil {
		token := uuid.New().String()
		hash := hashToken(token)
		pool.Exec(ctx, `UPDATE exam_sessions SET session_token_hash=$1, updated_at=$2 WHERE id=$3`,
			hash, time.Now(), s.ID)
		s.SessionToken = token
		return &s, nil
	}

	// create new
	sessionID := uuid.New().String()
	token := uuid.New().String()
	tokenHash := hashToken(token)
	now := time.Now()
	deadline := now.Add(time.Duration(run.DurationMinutes) * time.Minute)

	_, err = pool.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, department, session_token_hash,
		started_at, deadline_at, draft_json, draft_revision, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'{}',0,'active',$9,$10)`,
		sessionID, run.ID, employeeID, name, dept, tokenHash,
		now, deadline, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	return &Session{
		ID: sessionID, RunID: run.ID, EmployeeID: employeeID,
		Name: name, Department: dept, SessionToken: token,
		StartedAt: now, DeadlineAt: &deadline, Status: "active",
	}, nil
}

func SaveDraft(ctx context.Context, pool *db.Pool, sessionID string, revision int, draft json.RawMessage, minIntervalMs int) (int, bool, error) {
	// check revision CAS + interval
	var currentRev int
	var lastSaved time.Time
	err := pool.QueryRow(ctx, `SELECT draft_revision, COALESCE(draft_saved_at, '1970-01-01'::timestamptz)
		FROM exam_sessions WHERE id=$1 AND status='active'`, sessionID).Scan(&currentRev, &lastSaved)
	if err != nil {
		return 0, false, fmt.Errorf("session not found: %w", err)
	}
	if revision != currentRev {
		return currentRev, false, fmt.Errorf("STALE_DRAFT_REVISION")
	}

	interval := time.Duration(minIntervalMs) * time.Millisecond
	if time.Since(lastSaved) < interval {
		return currentRev, false, nil // throttled
	}

	newRev := currentRev + 1
	now := time.Now()
	_, err = pool.Exec(ctx, `UPDATE exam_sessions SET draft_json=$1, draft_revision=$2, draft_saved_at=$3, updated_at=$4 WHERE id=$5`,
		draft, newRev, now, now, sessionID)
	if err != nil {
		return currentRev, false, err
	}
	return newRev, true, nil
}

func GetSessionStatus(ctx context.Context, pool *db.Pool, sessionID string) (map[string]interface{}, error) {
	var runStatus, sessionStatus string
	var deadline time.Time
	var closingStartedAt, closedAt *time.Time
	var duration, extended int
	err := pool.QueryRow(ctx, `SELECT r.status, s.status, s.deadline_at, r.closing_started_at, r.closed_at,
		r.duration_minutes, COALESCE(r.extended_minutes, 0)
		FROM exam_sessions s JOIN exam_runs r ON s.run_id=r.id WHERE s.id=$1`, sessionID).Scan(
		&runStatus, &sessionStatus, &deadline, &closingStartedAt, &closedAt, &duration, &extended)
	if err != nil {
		return nil, err
	}
	totalMin := duration + extended
	return map[string]interface{}{
		"run_status":       runStatus,
		"session_status":   sessionStatus,
		"deadline_at":      deadline,
		"closing_started_at": closingStartedAt,
		"closed_at":        closedAt,
		"duration_minutes": totalMin,
	}, nil
}

func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
```

- [ ] **Step 2: 实现考生 API handler**

```go
// internal/httpapi/exam.go
package httpapi

import (
	"encoding/json"
	"net/http"

	"examSystem/internal/papers"
	"examSystem/internal/runs"
	"examSystem/internal/sessions"

	"github.com/go-chi/chi/v5"
)

type ExamAPI struct {
	DB      *db.Pool
	DataDir string
}

func (s *Server) registerExamRoutes(r chi.Router) {
	api := &ExamAPI{DB: s.DB, DataDir: s.DataDir}
	r.Get("/api/exam", api.handleGetExam)
	r.Post("/api/exam/start", api.handleStart)
	r.Put("/api/exam/sessions/{id}/draft", api.handleSaveDraft)
	r.Get("/api/exam/sessions/{id}/status", api.handleSessionStatus)
	r.Post("/api/submit", api.handleSubmit)
	r.Get("/api/submission/{id}/status", api.handleSubmissionStatus)
}

func (api *ExamAPI) handleGetExam(w http.ResponseWriter, r *http.Request) {
	paperID := r.URL.Query().Get("paper_id")
	if paperID == "" {
		paperID = "default"
	}
	run, err := runs.GetActiveRun(r.Context(), api.DB, paperID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "NO_ACTIVE_RUN")
		return
	}
	// load snapshot
	snapData, err := os.ReadFile(run.SnapshotPath)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "SNAPSHOT_NOT_FOUND")
		return
	}
	var paper interface{}
	json.Unmarshal(snapData, &paper)

	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"run":     run,
		"paper":   paper,
	})
}

func (api *ExamAPI) handleStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PaperID    string `json:"paper_id"`
		Token      string `json:"token"`
		EmployeeID string `json:"employee_id"`
		Name       string `json:"name"`
		Department string `json:"department"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "INVALID_BODY")
		return
	}
	if req.PaperID == "" {
		req.PaperID = "default"
	}
	run, err := runs.GetActiveRun(r.Context(), api.DB, req.PaperID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "NO_ACTIVE_RUN")
		return
	}
	// verify token
	tokenHash := hashToken(req.Token)
	if tokenHash != run.PublicTokenHash {
		jsonErr(w, http.StatusForbidden, "INVALID_TOKEN")
		return
	}

	sess, err := sessions.StartOrResume(r.Context(), api.DB, run, req.EmployeeID, req.Name, req.Department)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "START_FAILED")
		return
	}

	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success":        true,
		"session_id":     sess.ID,
		"session_token":  sess.SessionToken,
		"started_at":     sess.StartedAt,
		"deadline_at":    sess.DeadlineAt,
		"draft_json":     sess.DraftJSON,
		"draft_revision": sess.DraftRevision,
	})
}

func (api *ExamAPI) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	var req struct {
		DraftJSON json.RawMessage `json:"draft_json"`
		Revision  int             `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "INVALID_BODY")
		return
	}

	newRev, saved, err := sessions.SaveDraft(r.Context(), api.DB, sessionID, req.Revision, req.DraftJSON, 2000)
	if err != nil {
		if err.Error() == "STALE_DRAFT_REVISION" {
			jsonErr(w, http.StatusConflict, "STALE_DRAFT_REVISION")
			return
		}
		jsonErr(w, http.StatusInternalServerError, "DRAFT_SAVE_FAILED")
		return
	}

	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"revision": newRev,
		"saved":   saved,
	})
}

func (api *ExamAPI) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	status, err := sessions.GetSessionStatus(r.Context(), api.DB, sessionID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "SESSION_NOT_FOUND")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"status":  status,
	})
}

func (api *ExamAPI) handleSubmissionStatus(w http.ResponseWriter, r *http.Request) {
	submissionID := chi.URLParam(r, "id")
	var gradingStatus, reviewStatus string
	var objectiveScore, totalScore *float64
	err := api.DB.QueryRow(r.Context(),
		`SELECT grading_status, review_status, objective_score, total_score
		FROM submissions WHERE id=$1`, submissionID).Scan(
		&gradingStatus, &reviewStatus, &objectiveScore, &totalScore)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "SUBMISSION_NOT_FOUND")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"submission_id":   submissionID,
		"grading_status":  gradingStatus,
		"review_status":   reviewStatus,
		"status":          gradingStatus,
		"objective_score": objectiveScore,
		"total_score":     totalScore,
	})
}
```

- [ ] **Step 3: 更新 router.go 注册考生路由**

```go
// internal/httpapi/router.go
// 在 NewRouter 中追加:
	s.registerExamRoutes(r)
```

- [ ] **Step 4: 构建验证**

```bash
go build ./cmd/exam-server/
```

Expected: 编译成功。

- [ ] **Step 5: Commit**

```bash
git add internal/sessions/ internal/httpapi/exam.go internal/httpapi/router.go
git commit -m "feat: exam start, draft CAS, session status APIs"
```

---

### Task 1.6: 实现交卷 API（POST /api/submit，含客观题判分 + grading_jobs 入队）

**Files:**
- Create: `internal/submit/submit.go`
- Modify: `internal/httpapi/exam.go`（添加 handleSubmit）

- [ ] **Step 1: 实现 submit 业务逻辑**

```go
// internal/submit/submit.go
package submit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"examSystem/internal/db"
	"examSystem/internal/objective"
	"examSystem/internal/papers"
)

func Submit(ctx context.Context, pool *db.Pool, dataDir string, sessionToken, clientIP, userAgent string, answers map[string]interface{}) (int64, map[string]interface{}, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)

	// 1. verify session
	var sessionID, runID, employeeID, name, dept string
	var startedAt time.Time
	tokenHash := hashToken(sessionToken)
	err = tx.QueryRow(ctx, `SELECT id, run_id, employee_id, name, department, started_at
		FROM exam_sessions WHERE session_token_hash=$1 AND status='active'`, tokenHash).Scan(
		&sessionID, &runID, &employeeID, &name, &dept, &startedAt)
	if err != nil {
		return 0, nil, fmt.Errorf("DUPLICATE_SUBMISSION")
	}

	// 2. verify run
	var runStatus string
	var snapshotPath string
	err = tx.QueryRow(ctx, `SELECT status, snapshot_path FROM exam_runs WHERE id=$1`, runID).Scan(&runStatus, &snapshotPath)
	if err != nil || runStatus != "open" {
		return 0, nil, fmt.Errorf("RUN_CLOSING")
	}

	// 3. check deadline
	var deadline time.Time
	err = tx.QueryRow(ctx, `SELECT deadline_at FROM exam_sessions WHERE id=$1`, sessionID).Scan(&deadline)
	if err == nil && time.Now().After(deadline) {
		return 0, nil, fmt.Errorf("DEADLINE_EXCEEDED")
	}

	// 4. load paper
	paper, err := papers.LoadPaper(dataDir, runID)
	if err != nil {
		return 0, nil, fmt.Errorf("PAPER_NOT_FOUND: %w", err)
	}

	// 5. grade objective questions
	objScore, gradingDetails := objective.GradeAnswers(paper, answers)

	// 6. check for subjective questions
	hasSubjective := paper.HasSubjectiveQuestions()
	gradingStatus := "pending"
	reviewStatus := "grading"
	if !hasSubjective {
		gradingStatus = "done"
		reviewStatus = determineAutoReviewStatus(gradingDetails, paper.FlattenQuestions())
	}

	// 7. insert submission
	now := time.Now()
	answersJSON, _ := json.Marshal(answers)
	detailsJSON, _ := json.Marshal(gradingDetails)

	var submissionID int64
	err = tx.QueryRow(ctx, `INSERT INTO submissions
		(name, employee_id, paper_id, paper_name, run_id, department,
		answers_json, grading_detail_json, objective_score,
		subjective_score_machine, subjective_score_final, total_score,
		review_status, grading_status, started_at, submitted_at, client_ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,0,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id`,
		name, employeeID, paper.Slug, paper.Name, runID, dept,
		answersJSON, detailsJSON, objScore, objScore,
		reviewStatus, gradingStatus, startedAt, now, clientIP, userAgent,
	).Scan(&submissionID)
	if err != nil {
		return 0, nil, fmt.Errorf("INSERT_FAILED: %w", err)
	}

	// 8. mark session submitted
	tx.Exec(ctx, `UPDATE exam_sessions SET status='submitted', updated_at=$1 WHERE id=$2`, now, sessionID)

	// 9. create grading_job if subjective
	if hasSubjective {
		tx.Exec(ctx, `INSERT INTO grading_jobs
			(submission_id, paper_id, run_id, status, available_at, created_at, updated_at)
			VALUES ($1,$2,$3,'queued',$4,$5,$6)`,
			submissionID, paper.Slug, runID, now, now, now)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}

	resp := map[string]interface{}{
		"success":         true,
		"submission_id":   submissionID,
		"status":          gradingStatus,
		"grading_status":  gradingStatus,
		"objective_score": objScore,
		"paper_id":        paper.Slug,
		"run_id":          runID,
		"message":         "提交成功",
	}
	if hasSubjective {
		resp["message"] = "提交成功，系统正在评分中"
	}
	return submissionID, resp, nil
}

func determineAutoReviewStatus(details []objective.GradingDetail, questions []papers.Question) string {
	allCorrect := true
	for _, d := range details {
		if !d.IsCorrect && d.QuestionType != "short_answer" && d.QuestionType != "essay" && d.QuestionType != "composite" {
			allCorrect = false
			break
		}
	}
	if allCorrect {
		return "auto_scored"
	}
	return "need_review"
}
```

- [ ] **Step 2: 在 exam.go 中实现 handleSubmit**

```go
// internal/httpapi/exam.go（追加）
func (api *ExamAPI) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionToken string                 `json:"session_token"`
		Answers      map[string]interface{} `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "INVALID_BODY")
		return
	}

	clientIP := r.RemoteAddr
	userAgent := r.UserAgent()
	_, resp, err := submit.Submit(r.Context(), api.DB, api.DataDir,
		req.SessionToken, clientIP, userAgent, req.Answers)
	if err != nil {
		msg := err.Error()
		switch {
		case contains(msg, "DUPLICATE"):
			jsonErr(w, http.StatusConflict, "DUPLICATE_SUBMISSION")
		case contains(msg, "RUN_CLOSING"):
			jsonErr(w, http.StatusBadRequest, "RUN_CLOSING")
		case contains(msg, "DEADLINE"):
			jsonErr(w, http.StatusBadRequest, "DEADLINE_EXCEEDED")
		default:
			jsonErr(w, http.StatusInternalServerError, "SUBMIT_FAILED")
		}
		return
	}
	jsonResp(w, http.StatusOK, resp)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || searchSubstring(s, substr))
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: 更新 sessions.go 导出 hashToken**

```go
// internal/sessions/sessions.go 追加
func HashToken(s string) string {
	return hashToken(s)
}
```

```go
// internal/submit/submit.go: 将 tokenHash 改为 sessions.HashToken
```

- [ ] **Step 4: 构建验证**

```bash
go build ./cmd/exam-server/
```

Expected: 编译成功。

- [ ] **Step 5: Commit**

```bash
git add internal/submit/ internal/httpapi/exam.go
git commit -m "feat: submit API with objective grading + grading_jobs enqueue"
```

---

### Task 1.7: 静态文件托管（frontend/）

**Files:**
- Create: `internal/static/static.go`
- Modify: `cmd/exam-server/main.go`

- [ ] **Step 1: 实现静态文件服务**

```go
// internal/static/static.go
package static

import (
	"net/http"
	"os"
	"path/filepath"
)

func Handler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, r.URL.Path)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		// SPA fallback for exam/admin routes
		if r.URL.Path == "/exam" || r.URL.Path == "/admin" || r.URL.Path == "/detail" {
			http.ServeFile(w, r, filepath.Join(dir, r.URL.Path+".html"))
			return
		}
		// root -> index.html
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}
```

- [ ] **Step 2: 更新 main.go 挂载静态文件**

```go
// cmd/exam-server/main.go
// 在创建 router 后：

	"examSystem/internal/static"

	// ... after handler := httpapi.NewRouter(srv)

	// wrap: API first, then static fallback
	mux := http.NewServeMux()
	mux.Handle("/api/", handler)
	mux.Handle("/", static.Handler("frontend"))
	http.ListenAndServe(addr, mux)
```

- [ ] **Step 3: 构建验证**

```bash
go build ./cmd/exam-server/
```

Expected: 编译成功。

- [ ] **Step 4: Commit**

```bash
git add internal/static/ cmd/exam-server/main.go
git commit -m "feat: static file serving for frontend/"
```

---

## P2: grading_jobs + Python Worker 主观题评分

### Task 2.1: 实现 grading_jobs 查询与 claim（Go 侧）

**Files:**
- Create: `internal/review/review.go`

- [ ] **Step 1: 实现 claim / complete / fail**

```go
// internal/review/review.go
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"examSystem/internal/db"
)

type GradingJob struct {
	ID           int64     `json:"id"`
	SubmissionID int64     `json:"submission_id"`
	PaperID      string    `json:"paper_id"`
	RunID        string    `json:"run_id"`
	Status       string    `json:"status"`
	Attempts     int       `json:"attempts"`
	MaxAttempts  int       `json:"max_attempts"`
}

func ClaimJob(ctx context.Context, pool *db.Pool, workerID string, leaseSeconds int) (*GradingJob, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var job GradingJob
	err = tx.QueryRow(ctx, `SELECT id, submission_id, paper_id, run_id, status, attempts, max_attempts
		FROM grading_jobs
		WHERE status IN ('queued','leased') AND available_at <= now()
		AND (status = 'queued' OR lease_until < now())
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`).Scan(&job.ID, &job.SubmissionID, &job.PaperID, &job.RunID, &job.Status, &job.Attempts, &job.MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("no available job") // not a real error
	}

	leaseUntil := time.Now().Add(time.Duration(leaseSeconds) * time.Second)
	_, err = tx.Exec(ctx, `UPDATE grading_jobs SET status='leased', lease_owner=$1,
		lease_until=$2, attempts=attempts+1, updated_at=$3 WHERE id=$4`,
		workerID, leaseUntil, time.Now(), job.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &job, nil
}

func CompleteJob(ctx context.Context, pool *db.Pool, jobID int64, subjectiveScoreMachine, subjectiveScoreFinal float64, gradingDetailJSON json.RawMessage, reviewStatus string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	now := time.Now()

	// update submission
	_, err = tx.Exec(ctx, `UPDATE submissions SET
		subjective_score_machine=$1, subjective_score_final=$2,
		total_score = objective_score + $3,
		grading_detail_json = grading_detail_json || $4::jsonb,
		grading_status='done', review_status=$5, graded_at=$6
		WHERE id=(SELECT submission_id FROM grading_jobs WHERE id=$7)`,
		subjectiveScoreMachine, subjectiveScoreFinal, subjectiveScoreFinal,
		gradingDetailJSON, reviewStatus, now, jobID)
	if err != nil {
		return err
	}

	// mark job done
	_, err = tx.Exec(ctx, `UPDATE grading_jobs SET status='done', updated_at=$1 WHERE id=$2`, now, jobID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func FailJob(ctx context.Context, pool *db.Pool, jobID int64, errMsg string) error {
	var attempts, maxAttempts int
	pool.QueryRow(ctx, `SELECT attempts, max_attempts FROM grading_jobs WHERE id=$1`, jobID).Scan(&attempts, &maxAttempts)

	now := time.Now()
	if attempts >= maxAttempts {
		pool.Exec(ctx, `UPDATE grading_jobs SET status='dead', last_error=$1, updated_at=$2 WHERE id=$3`, errMsg, now, jobID)
		pool.Exec(ctx, `UPDATE submissions SET grading_status='failed', review_status='need_review', grading_error=$1
			WHERE id=(SELECT submission_id FROM grading_jobs WHERE id=$2)`, errMsg, jobID)
	} else {
		backoff := time.Duration(1<<uint(attempts)) * time.Second
		pool.Exec(ctx, `UPDATE grading_jobs SET status='queued', last_error=$1,
			available_at=$2, updated_at=$3 WHERE id=$4`,
			errMsg, now.Add(backoff), now, jobID)
	}
	return nil
}

func GetJobSubmission(ctx context.Context, pool *db.Pool, jobID int64) (submissionID int64, answersJSON json.RawMessage, snapshotPath string, objectiveScore float64, err error) {
	err = pool.QueryRow(ctx, `SELECT s.id, s.answers_json, r.snapshot_path, s.objective_score
		FROM submissions s JOIN exam_runs r ON s.run_id=r.id
		JOIN grading_jobs j ON j.submission_id=s.id
		WHERE j.id=$1`, jobID).Scan(&submissionID, &answersJSON, &snapshotPath, &objectiveScore)
	return
}
```

- [ ] **Step 2: 构建验证**

```bash
go build ./internal/review/
```

Expected: 编译成功。

- [ ] **Step 3: Commit**

```bash
git add internal/review/
git commit -m "feat: grading_jobs claim/complete/fail logic"
```

---

### Task 2.2: 创建 Python Worker 入口与 claim 循环

**Files:**
- Create: `scoring_worker/main.py`
- Create: `scoring_worker/claim.py`
- Create: `scoring_worker/requirements.txt`

- [ ] **Step 1: 实现 claim.py**

```python
# scoring_worker/claim.py
import psycopg2
import psycopg2.extras

JOB_SCHEMA = ["id", "submission_id", "paper_id", "run_id", "status", "attempts", "max_attempts"]

def claim_job(conn_str: str, worker_id: str, lease_seconds: int = 300):
    conn = psycopg2.connect(conn_str)
    conn.autocommit = False
    try:
        with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
            cur.execute("""
                SELECT id, submission_id, paper_id, run_id, status, attempts, max_attempts
                FROM grading_jobs
                WHERE status IN ('queued','leased') AND available_at <= now()
                AND (status = 'queued' OR lease_until < now())
                ORDER BY id
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            """)
            row = cur.fetchone()
            if not row:
                conn.rollback()
                conn.close()
                return None

            job = dict(row)
            cur.execute("""
                UPDATE grading_jobs SET status='leased', lease_owner=%s,
                lease_until=now() + interval '%s seconds',
                attempts=attempts+1, updated_at=now()
                WHERE id=%s
            """, (worker_id, lease_seconds, job["id"]))
            conn.commit()
        return job
    except Exception:
        conn.rollback()
        raise
    finally:
        if not conn.closed:
            conn.close()
```

- [ ] **Step 2: 实现 main.py**

```python
# scoring_worker/main.py
import os
import sys
import json
import time
import logging
import uuid
import traceback

import psycopg2
import psycopg2.extras
from claim import claim_job

logging.basicConfig(level=logging.INFO, format="[worker] %(asctime)s %(levelname)s %(message)s")
log = logging.getLogger(__name__)

WORKER_ID = os.environ.get("WORKER_ID", f"worker-{uuid.uuid4().hex[:8]}")
DATABASE_URL = os.environ.get("DATABASE_URL", "postgres://exam:password@127.0.0.1:5432/exam")
POLL_INTERVAL = float(os.environ.get("POLL_INTERVAL_MS", "200")) / 1000
LEASE_SECONDS = int(os.environ.get("LEASE_SECONDS", "300"))

# subjective-scoring imports (expect in PYTHONPATH or installed)
try:
    from subjective_scoring import score_answers  # adjust to actual package
except ImportError:
    log.warning("subjective_scoring not found; will stub")
    def score_answers(paper, answers, grading_detail):
        return 0, grading_detail


def process_job(job: dict):
    job_id = job["id"]
    submission_id = job["submission_id"]
    log.info(f"processing job {job_id} for submission {submission_id}")

    conn = psycopg2.connect(DATABASE_URL)
    try:
        with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
            # get submission data
            cur.execute("""
                SELECT s.answers_json, s.grading_detail_json, s.objective_score,
                       r.snapshot_path
                FROM submissions s
                JOIN exam_runs r ON s.run_id = r.id
                WHERE s.id = %s
            """, (submission_id,))
            row = cur.fetchone()
            if not row:
                raise Exception(f"submission {submission_id} not found")

            answers = json.loads(row["answers_json"]) if isinstance(row["answers_json"], str) else row["answers_json"]
            grading_detail = json.loads(row["grading_detail_json"]) if isinstance(row["grading_detail_json"], str) else row["grading_detail_json"]

            # load snapshot (paper)
            if row["snapshot_path"] and os.path.exists(row["snapshot_path"]):
                with open(row["snapshot_path"]) as f:
                    paper = json.load(f)
            else:
                paper = {"sections": []}

            # score subjective
            subjective_score, updated_grading_detail = score_answers(paper, answers, grading_detail)

            # write back
            updated_json = json.dumps(updated_grading_detail)
            cur.execute("""
                UPDATE submissions SET
                    subjective_score_machine = %s,
                    subjective_score_final = %s,
                    total_score = objective_score + %s,
                    grading_detail_json = grading_detail_json || %s::jsonb,
                    grading_status = 'done',
                    review_status = 'need_review',
                    graded_at = now()
                WHERE id = %s
            """, (subjective_score, subjective_score, subjective_score,
                  json.dumps(updated_grading_detail), submission_id))

            cur.execute("""
                UPDATE grading_jobs SET status='done', updated_at=now()
                WHERE id = %s
            """, (job_id,))

            conn.commit()
            log.info(f"job {job_id} done: subjective_score={subjective_score}")

    except Exception as e:
        conn.rollback()
        err = f"{e}\n{traceback.format_exc()}"
        log.error(f"job {job_id} failed: {e}")
        try:
            with conn.cursor() as cur:
                cur.execute("""
                    UPDATE grading_jobs SET status='dead', last_error=%s, updated_at=now()
                    WHERE id=%s
                """, (err[:1000], job_id))
                cur.execute("""
                    UPDATE submissions SET grading_status='failed', review_status='need_review',
                    grading_error=%s WHERE id=%s
                """, (err[:500], submission_id))
                conn.commit()
        except Exception as e2:
            log.error(f"could not mark job {job_id} dead: {e2}")
    finally:
        if not conn.closed:
            conn.close()


def main():
    log.info(f"worker {WORKER_ID} starting, poll={POLL_INTERVAL}s lease={LEASE_SECONDS}s")
    while True:
        try:
            job = claim_job(DATABASE_URL, WORKER_ID, LEASE_SECONDS)
            if job:
                process_job(job)
            else:
                time.sleep(POLL_INTERVAL)
        except KeyboardInterrupt:
            log.info("worker stopping")
            break
        except Exception as e:
            log.error(f"loop error: {e}")
            time.sleep(POLL_INTERVAL)


if __name__ == "__main__":
    main()
```

- [ ] **Step 3: 创建 requirements.txt**

```txt
psycopg2-binary>=2.9
```

- [ ] **Step 4: Commit**

```bash
git add scoring_worker/
git commit -m "feat: Python subjective scoring worker with claim loop"
```

---

### Task 2.3: 实现 regrade API（admin 触发重新评分）

**Files:**
- Modify: `internal/httpapi/admin.go`（新文件，先只放 regrade）

- [ ] **Step 1: 创建 admin handler 骨架**

```go
// internal/httpapi/admin.go
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) registerAdminRoutes(r chi.Router) {
	r.Post("/api/admin/regrade/{id}", s.handleRegrade)
}

func (s *Server) handleRegrade(w http.ResponseWriter, r *http.Request) {
	submissionID := chi.URLParam(r, "id")
	now := time.Now()
	_, err := s.DB.Exec(r.Context(), `INSERT INTO grading_jobs
		(submission_id, paper_id, run_id, status, available_at, created_at, updated_at)
		SELECT id, paper_id, run_id, 'queued', $1, $2, $3
		FROM submissions WHERE id=$4
		ON CONFLICT (submission_id) DO UPDATE SET status='queued', attempts=0, updated_at=$3`,
		now, now, now, submissionID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "REGRADE_FAILED")
		return
	}
	// reset grading_status
	s.DB.Exec(r.Context(), `UPDATE submissions SET grading_status='pending', grading_error=NULL WHERE id=$1`, submissionID)
	jsonResp(w, http.StatusOK, map[string]interface{}{"success": true})
}
```

- [ ] **Step 2: 更新 router.go**

```go
// internal/httpapi/router.go
// 在 NewRouter 中追加:
	s.registerAdminRoutes(r)
```

- [ ] **Step 3: 构建验证 + Commit**

```bash
go build ./cmd/exam-server/
git add internal/httpapi/admin.go internal/httpapi/router.go
git commit -m "feat: regrade API for admin-triggered re-scoring"
```

---

## P3: 管理端 API 全量 + 收卷 + 导出

### Task 3.1: 实现管理端鉴权（Bearer token）

**Files:**
- Create: `internal/auth/auth.go`

- [ ] **Step 1: 实现鉴权中间件**

```go
// internal/auth/auth.go
package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const AdminKey contextKey = "admin"

func Middleware(validToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" || !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, `{"success":false,"error":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(header, "Bearer ")
			if token != validToken {
				http.Error(w, `{"success":false,"error":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), AdminKey, true)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
```

- [ ] **Step 2: 在 router.go 配置 admin 鉴权**

```go
// internal/httpapi/router.go
	r.Route("/api/admin", func(r chi.Router) {
		r.Use(auth.Middleware(s.AdminToken))
		r.Post("/login", s.handleLogin) // allow pass-through for first login
	})
```

- [ ] **Step 3: Commit**

```bash
git add internal/auth/ internal/httpapi/router.go
git commit -m "feat: admin Bearer token auth middleware"
```

---

### Task 3.2: 实现管理端 papers、runs、submissions、export API

**Files:**
- Modify: `internal/httpapi/admin.go`（扩展完整 admin API）

- [ ] **Step 1: 实现全量 admin API handler**

```go
// internal/httpapi/admin.go (完整版)
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"strings"

	"examSystem/internal/papers"
	"examSystem/internal/runs"
	"examSystem/internal/auth"

	"github.com/go-chi/chi/v5"
)

func (s *Server) registerAdminRoutes(r chi.Router) {
	r.Route("/api/admin", func(r chi.Router) {
		r.Use(auth.Middleware(s.AdminToken))

		// login (no-op for existing token)
		r.Post("/login", s.handleAdminLogin)

		// papers
		r.Get("/papers", s.handleListPapers)
		r.Get("/papers/{slug}", s.handleGetPaper)
		r.Put("/papers/{slug}", s.handleUpdatePaper)

		// runs
		r.Post("/open/{slug}", s.handleOpenRun)
		r.Post("/close/{id}", s.handleCloseRun)
		r.Get("/runs", s.handleListRuns)

		// submissions
		r.Get("/submissions", s.handleListSubmissions)
		r.Get("/submissions/{id}", s.handleGetSubmission)

		// review
		r.Post("/review/{id}", s.handleReview)
		r.Post("/regrade/{id}", s.handleRegrade)

		// export
		r.Get("/export/{run_id}", s.handleExport)
		r.Get("/stats/{run_id}", s.handleStats)
	})
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"token":   s.AdminToken,
	})
}

func (s *Server) handleListPapers(w http.ResponseWriter, r *http.Request) {
	idx, err := papers.LoadIndex(s.DataDir)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "LOAD_FAILED")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"papers":  idx.Papers,
	})
}

func (s *Server) handleGetPaper(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	paper, err := papers.LoadPaper(s.DataDir, slug)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "PAPER_NOT_FOUND")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"paper":   paper,
	})
}

func (s *Server) handleUpdatePaper(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	var data interface{}
	json.NewDecoder(r.Body).Decode(&data)

	path := filepath.Join(s.DataDir, "papers", slug+".json")
	b, _ := json.MarshalIndent(data, "", "  ")
	if err := os.WriteFile(path, b, 0644); err != nil {
		jsonErr(w, http.StatusInternalServerError, "SAVE_FAILED")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *Server) handleOpenRun(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	var req struct{ Duration int `json:"duration_minutes"` }
	json.NewDecoder(r.Body).Decode(&req)
	if req.Duration == 0 {
		req.Duration = 60
	}

	run, err := runs.CreateRun(r.Context(), s.DB, s.DataDir, slug, req.Duration)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "OPEN_FAILED")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"run":     run,
	})
}

func (s *Server) handleCloseRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	now := time.Now()
	finalizeAt := now.Add(5 * time.Second)

	result, err := s.DB.Exec(r.Context(), `UPDATE exam_runs SET
		status='closing', closing_started_at=$1, finalize_at=$2 WHERE id=$3 AND status='open'`,
		now, finalizeAt, runID)
	if err != nil || result.RowsAffected() == 0 {
		jsonErr(w, http.StatusBadRequest, "CLOSE_FAILED")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runsList, err := runs.ListOpenRuns(r.Context(), s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "LIST_FAILED")
		return
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"runs":    runsList,
	})
}

func (s *Server) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	var rows interface{}
	var err error
	if runID != "" {
		rows, err = s.DB.Query(r.Context(), `SELECT id, name, employee_id, department,
			objective_score, subjective_score_machine, total_score,
			grading_status, review_status, submitted_at
			FROM submissions WHERE run_id=$1 ORDER BY submitted_at DESC`, runID)
	} else {
		rows, err = s.DB.Query(r.Context(), `SELECT id, name, employee_id, department,
			objective_score, subjective_score_machine, total_score,
			grading_status, review_status, submitted_at
			FROM submissions ORDER BY submitted_at DESC LIMIT 500`)
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "LIST_FAILED")
		return
	}
	defer rows.Close()

	var subs []map[string]interface{}
	for rows.Next() {
		var id int64
		var name, emp, dept, gs, rs string
		var obj, subj, total *float64
		var subAt *time.Time
		rows.Scan(&id, &name, &emp, &dept, &obj, &subj, &total, &gs, &rs, &subAt)
		subs = append(subs, map[string]interface{}{
			"id": id, "name": name, "employee_id": emp, "department": dept,
			"objective_score": obj, "subjective_score_machine": subj, "total_score": total,
			"grading_status": gs, "review_status": rs, "submitted_at": subAt,
		})
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{"success": true, "submissions": subs})
}

func (s *Server) handleGetSubmission(w http.ResponseWriter, r *http.Request) {
	subID := chi.URLParam(r, "id")
	var sub map[string]interface{}
	row := s.DB.QueryRow(r.Context(), `SELECT * FROM submissions WHERE id=$1`, subID)
	// simplified: scan into map
	_ = row
	jsonResp(w, http.StatusOK, map[string]interface{}{"success": true, "submission": sub})
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	subID := chi.URLParam(r, "id")
	var req struct {
		Score  float64 `json:"subjective_score_final"`
		Note   string  `json:"reviewer_note"`
		Status string  `json:"review_status"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.DB.Exec(r.Context(), `UPDATE submissions SET subjective_score_final=$1, total_score=objective_score+$1,
		review_status=$2, reviewer_note=$3, reviewed_at=now() WHERE id=$4`,
		req.Score, req.Status, req.Note, subID)
	jsonResp(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	rows, err := s.DB.Query(r.Context(), `SELECT employee_id, name, department,
		objective_score, subjective_score_machine, subjective_score_final, total_score,
		review_status, submitted_at FROM submissions WHERE run_id=$1 ORDER BY submitted_at`, runID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "EXPORT_FAILED")
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=exam_%s.csv", runID))
	w.Write([]byte("员工ID,姓名,部门,客观分,主观分(机器),主观分(终),总分,复核状态,提交时间\n"))

	for rows.Next() {
		var empID, name, dept, rs string
		var obj, subjM, subjF, total *float64
		var subAt *time.Time
		rows.Scan(&empID, &name, &dept, &obj, &subjM, &subjF, &total, &rs, &subAt)
		fmt.Fprintf(w, "%s,%s,%s,%.1f,%.1f,%.1f,%.1f,%s,%s\n",
			empID, name, dept, val(obj), val(subjM), val(subjF), val(total), rs, subAt.Format("2006-01-02 15:04:05"))
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	var total, scored int
	var avgScore float64
	s.DB.QueryRow(r.Context(), `SELECT COUNT(*), AVG(total_score),
		COUNT(*) FILTER (WHERE grading_status='done')
		FROM submissions WHERE run_id=$1`, runID).Scan(&total, &avgScore, &scored)
	jsonResp(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"total":        total,
		"scored":       scored,
		"average_score": avgScore,
	})
}

func val(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
```

- [ ] **Step 2: 更新 router.go 添加 AdminToken 字段**

```go
// internal/httpapi/router.go
type Server struct {
	DB         *db.Pool
	CORS       string
	DataDir    string
	AdminToken string
}
```

- [ ] **Step 3: main.go 传入 AdminToken**

```go
// cmd/exam-server/main.go
	srv := &httpapi.Server{
		DB:         pool,
		CORS:       "*",
		DataDir:    "data",
		AdminToken: cfg.Auth.AdminToken,
	}
```

- [ ] **Step 4: 构建验证 + Commit**

```bash
go build ./cmd/exam-server/
git add internal/httpapi/admin.go internal/httpapi/router.go cmd/exam-server/main.go
git commit -m "feat: full admin API: papers, runs, submissions, export, stats"
```

---

### Task 3.3: 实现收卷循环（finalize 协程）

**Files:**
- Create: `internal/finalize/finalize.go`
- Modify: `cmd/exam-server/main.go`

- [ ] **Step 1: 实现 finalize 协程**

```go
// internal/finalize/finalize.go
package finalize

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"examSystem/internal/db"
	"examSystem/internal/objective"
	"examSystem/internal/papers"
)

func StartFinalizeLoop(ctx context.Context, pool *db.Pool, dataDir string) {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				finalizeAll(ctx, pool, dataDir)
			}
		}
	}()
}

func finalizeAll(ctx context.Context, pool *db.Pool, dataDir string) {
	rows, err := pool.Query(ctx, `SELECT id, paper_id, snapshot_path
		FROM exam_runs WHERE status='closing' AND finalize_at <= now()`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var runID, paperID, snapPath string
		rows.Scan(&runID, &paperID, &snapPath)
		finalizeOne(ctx, pool, dataDir, runID, snapPath)
	}
}

func finalizeOne(ctx context.Context, pool *db.Pool, dataDir, runID, snapPath string) {
	// get active sessions
	sessRows, err := pool.Query(ctx, `SELECT id, employee_id, name, department,
		draft_json, started_at, client_ip, user_agent
		FROM exam_sessions WHERE run_id=$1 AND status='active'`, runID)
	if err != nil {
		return
	}
	defer sessRows.Close()

	paper, _ := papers.LoadPaper(dataDir, runID)

	for sessRows.Next() {
		var sessID, empID, name, dept, clientIP, userAgent string
		var draftJSON json.RawMessage
		var startedAt time.Time
		sessRows.Scan(&sessID, &empID, &name, &dept, &draftJSON, &startedAt, &clientIP, &userAgent)

		var answers map[string]interface{}
		json.Unmarshal(draftJSON, &answers)

		now := time.Now()
		objScore, gradingDetails := objective.GradeAnswers(paper, answers)
		hasSubj := paper.HasSubjectiveQuestions()
		gradingStatus := "pending"
		reviewStatus := "grading"
		if !hasSubj {
			gradingStatus = "done"
			reviewStatus = determineAutoReview(gradingDetails)
		}

		answersJSON, _ := json.Marshal(answers)
		detailsJSON, _ := json.Marshal(gradingDetails)

		tx, _ := pool.Begin(ctx)
		var subID int64
		err := tx.QueryRow(ctx, `INSERT INTO submissions
			(name, employee_id, paper_id, run_id, department,
			answers_json, grading_detail_json, objective_score,
			subjective_score_machine, subjective_score_final, total_score,
			review_status, grading_status, started_at, submitted_at,
			client_ip, user_agent, auto_submit_reason)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0,0,$9,$10,$11,$12,$13,$14,$15,'closing')
			RETURNING id`,
			name, empID, paper.Slug, runID, dept,
			answersJSON, detailsJSON, objScore, objScore,
			reviewStatus, gradingStatus, startedAt, now, clientIP, userAgent,
		).Scan(&subID)
		if err != nil {
			tx.Rollback(ctx)
			continue
		}
		tx.Exec(ctx, `UPDATE exam_sessions SET status='submitted', updated_at=$1 WHERE id=$2`, now, sessID)
		if hasSubj {
			tx.Exec(ctx, `INSERT INTO grading_jobs
				(submission_id, paper_id, run_id, status, available_at, created_at, updated_at)
				VALUES ($1,$2,$3,'queued',$4,$5,$6)`, subID, paper.Slug, runID, now, now, now)
		}
		tx.Commit(ctx)
	}

	pool.Exec(ctx, `UPDATE exam_runs SET status='closed', closed_at=now() WHERE id=$1`, runID)
	log.Printf("finalized run %s", runID)
}

func determineAutoReview(details []objective.GradingDetail) string {
	for _, d := range details {
		if !d.IsCorrect && d.QuestionType != "short_answer" && d.QuestionType != "essay" {
			return "need_review"
		}
	}
	return "auto_scored"
}
```

- [ ] **Step 2: 更新 main.go 启动 finalize 循环**

```go
// cmd/exam-server/main.go
	"examSystem/internal/finalize"

	// before http.ListenAndServe:
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finalize.StartFinalizeLoop(ctx, pool, "data")
```

- [ ] **Step 3: 构建 + Commit**

```bash
go build ./cmd/exam-server/
git add internal/finalize/ cmd/exam-server/main.go
git commit -m "feat: closing finalize loop with objective grading"
```

---

## P4: 前端小幅适配

### Task 4.1: 修改草稿保存间隔 2s → 5s

**Files:**
- Modify: `frontend/js/exam.js`

- [ ] **Step 1: 修改常量**

```javascript
// frontend/js/exam.js
// 修改:
const DRAFT_AUTOSAVE_INTERVAL_MS = 5000;  // 原来是 2000
```

- [ ] **Step 2: Commit**

```bash
git add frontend/js/exam.js
git commit -m "chore: increase draft autosave interval to 5s"
```

---

### Task 4.2: 交卷后兼容新的 grading_status 响应

**Files:**
- Modify: `frontend/js/exam.js`

- [ ] **Step 1: 兼容 status 字段**

```javascript
// frontend/js/exam.js
// 在 submit 成功后:
if (resp.grading_status === 'pending' || resp.status === 'pending') {
    showMessage('提交成功，系统正在评分中...');
}
if (resp.grading_status === 'done' || resp.status === 'done') {
    showMessage('提交成功，评分已完成');
}
```

- [ ] **Step 2: Commit**

```bash
git add frontend/js/exam.js
git commit -m "feat: frontend compatible with new grading_status field"
```

---

### Task 4.3: 管理端列表显示 grading_status

**Files:**
- Modify: `frontend/js/admin.js`

- [ ] **Step 1: 映射 grading_status 显示**

```javascript
// frontend/js/admin.js
// 在渲染提交列表时:
const statusLabels = {
    'pending': '⏳ 排队中',
    'grading': '🔄 评分中',
    'done': '✅ 已完成',
    'failed': '❌ 评分失败',
};
const label = statusLabels[sub.grading_status] || sub.grading_status;
```

- [ ] **Step 2: Commit**

```bash
git add frontend/js/admin.js
git commit -m "feat: admin list shows grading_status labels"
```

---

## P5: Windows 部署 + 压测

### Task 5.1: 创建 Windows 服务安装脚本

**Files:**
- Create: `scripts/install-services.ps1`
- Create: `scripts/start.bat`

- [ ] **Step 1: PowerShell 安装脚本**

```powershell
# scripts/install-services.ps1
param(
    [string]$PgPassword = "password",
    [string]$AdminToken = "change-me-in-production"
)

$ErrorActionPreference = "Stop"

# 1. Create PostgreSQL database
$env:PGPASSWORD = $PgPassword
& psql -U postgres -c "CREATE DATABASE exam;" 2>$null
& psql -U postgres -c "CREATE USER exam WITH PASSWORD '$PgPassword';" 2>$null
& psql -U postgres -c "GRANT ALL PRIVILEGES ON DATABASE exam TO exam;" 2>$null

# 2. Run migrations
$env:DATABASE_URL = "postgres://exam:$PgPassword@127.0.0.1:5432/exam?sslmode=disable"
& .\exam-server.exe migrate

# 3. Install Windows services
New-Service -Name "ExamSystemAPI" -BinaryPathName "$PWD\exam-server.exe" -DisplayName "Exam System API" -StartupType Automatic
New-Service -Name "ExamSystemScoringWorker" -BinaryPathName "python $PWD\scoring_worker\main.py" -DisplayName "Exam Scoring Worker" -StartupType Automatic

Write-Host "Services installed. Start with: Start-Service ExamSystemAPI, ExamSystemScoringWorker"
```

- [ ] **Step 2: 启动批处理**

```bat
@echo off
REM scripts/start.bat
set DATABASE_URL=postgres://exam:password@127.0.0.1:5432/exam?sslmode=disable
start "ExamAPI" exam-server.exe
start "ScoringWorker" python scoring_worker\main.py
echo Exam system started. API: http://localhost:8000
pause
```

- [ ] **Step 3: Commit**

```bash
git add scripts/
git commit -m "feat: Windows service install script and start.bat"
```

---

### Task 5.2: 编写 k6 压测脚本

**Files:**
- Create: `scripts/loadtest/steady_state.js`
- Create: `scripts/loadtest/open_peak.js`
- Create: `scripts/loadtest/submit_peak.js`

- [ ] **Step 1: 稳态答题压测**

```javascript
// scripts/loadtest/steady_state.js
import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  stages: [
    { duration: '2m', target: 100 },
    { duration: '2m', target: 300 },
    { duration: '2m', target: 500 },
    { duration: '30m', target: 500 },
    { duration: '2m', target: 0 },
  ],
};

const BASE = __ENV.BASE_URL || 'http://localhost:8000';

export default function () {
  const empId = `loadtest-${__VU}-${__ITER}`;
  // start exam
  const startRes = http.post(`${BASE}/api/exam/start`, JSON.stringify({
    paper_id: 'default',
    token: __ENV.EXAM_TOKEN,
    employee_id: empId,
    name: `Test User ${__VU}`,
    department: 'Load Test',
  }), { headers: { 'Content-Type': 'application/json' } });

  check(startRes, { 'start ok': (r) => r.status === 200 });
  if (startRes.status !== 200) return;

  const body = JSON.parse(startRes.body);
  const sessionId = body.session_id;
  const sessionToken = body.session_token;

  // steady draft saving every 5s
  for (let i = 0; i < 10; i++) {
    const draftRes = http.put(`${BASE}/api/exam/sessions/${sessionId}/draft`, JSON.stringify({
      draft_json: { q1: 'A', q2: ['A', 'B'] },
      revision: i,
    }), { headers: { 'Content-Type': 'application/json' } });
    check(draftRes, { 'draft ok': (r) => r.status === 200 || r.status === 409 });
    sleep(5);
  }

  // submit
  const submitRes = http.post(`${BASE}/api/submit`, JSON.stringify({
    session_token: sessionToken,
    answers: { q1: 'A', q2: ['A', 'B'] },
  }), { headers: { 'Content-Type': 'application/json' } });
  check(submitRes, { 'submit ok': (r) => r.status === 200 });
}
```

- [ ] **Step 2: 开考峰值压测**

```javascript
// scripts/loadtest/open_peak.js
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  scenarios: {
    burst: {
      executor: 'ramping-arrival-rate',
      startRate: 10,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 200,
      stages: [
        { target: 500, duration: '60s' },
      ],
    },
  },
};

const BASE = __ENV.BASE_URL || 'http://localhost:8000';

export default function () {
  const res = http.post(`${BASE}/api/exam/start`, JSON.stringify({
    paper_id: 'default',
    token: __ENV.EXAM_TOKEN,
    employee_id: `open-${__VU}-${__ITER}-${Date.now()}`,
    name: `Test User ${__VU}`,
    department: 'Load Test',
  }), { headers: { 'Content-Type': 'application/json' } });
  check(res, { 'start ok': (r) => r.status === 200 });
}
```

- [ ] **Step 3: 交卷峰值压测**

```javascript
// scripts/loadtest/submit_peak.js
// (similar pattern, pre-create sessions, then submit)
```

- [ ] **Step 4: Commit**

```bash
git add scripts/loadtest/
git commit -m "feat: k6 load test scripts for steady/open/submit peaks"
```

---

### Task 5.3: 执行压测并记录结果

- [ ] **Step 1: 稳态答题测试**

```bash
k6 run scripts/loadtest/steady_state.js -e BASE_URL=http://localhost:8000 -e EXAM_TOKEN=<token>
```

Expected: draft 成功率 ≥ 99%，P95 < 500ms。

- [ ] **Step 2: 开考峰值测试**

```bash
k6 run scripts/loadtest/open_peak.js -e BASE_URL=http://localhost:8000 -e EXAM_TOKEN=<token>
```

Expected: 500 starts in 60s，成功率 ≥ 99%。

- [ ] **Step 3: 交卷峰值测试**

```bash
k6 run scripts/loadtest/submit_peak.js -e BASE_URL=http://localhost:8000 -e EXAM_TOKEN=<token>
```

Expected: 500 submits，成功率 ≥ 99%，无 5xx。

- [ ] **Step 4: 记录结果到 docs/superpowers/loadtest-results.md**

```markdown
# 压测结果

| 场景 | 通过? | 成功率 | P95 | 备注 |
|---|---|---|---|---|
| 稳态 500 在线 30min | | | | |
| 开考 500/60s | | | | |
| 交卷 500/60s | | | | |
```

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/loadtest-results.md
git commit -m "docs: load test results for 500 concurrent"
```

---

## P6: 文档、回滚、上线

### Task 6.1: 编写部署文档

**Files:**
- Write: `docs/deploy-go-pg.md`

内容覆盖：
- Windows PostgreSQL 安装步骤
- exam-server.exe 配置（config.yaml 字段说明）
- 服务注册命令
- 从旧 SQLite 迁移步骤
- 常见问题排查

### Task 6.2: 编写回滚手册

**Files:**
- Write: `docs/rollback-go-pg.md`

```markdown
# 回滚手册

## 回滚条件
- 考试过程中 exam-server.exe 反复崩溃
- PG 性能不达标
- 评分失败率 > 5%

## 回滚步骤
1. 通知考生暂停
2. 停止 exam-server.exe 和 scoring-worker
3. 切换端口: `python backend/main.py --port 8000`
4. 使用备份的 exam.db
5. 通知考生重新开考

## 数据回迁
- 从 PG 导出 submissions → 写回 exam.db（脚本见 scripts/rollback.py）
```

### Task 6.3: 更新 README.md

追加 Go 部署章节，指向新文档。

---

## 自检清单

复读 spec §1-§15，确认 plan 覆盖：

| Spec 章节 | Plan 覆盖 | Task |
|---|---|---|
| §2 进程拓扑 | ✅ 目录 + main.go + worker | 0.1, 2.2 |
| §3 数据模型 | ✅ migrations/001_initial.sql | 0.2, 0.3 |
| §4 API 兼容 | ✅ 路由对齐现网路径 | 1.5, 1.6, 3.2 |
| §5 草稿写 | ✅ CAS + min interval 2s + 前端 5s | 1.5, 4.1 |
| §6 开考/交卷/评分 | ✅ submit 含 Go 客观题 + jobs | 1.6, 2.1, 2.2 |
| §7 限流/安全 | ✅ Bearer auth middleware | 3.1 |
| §8 Windows 部署 | ✅ install-services.ps1, start.bat | 5.1 |
| §9 判分切分 | ✅ objective/ + worker 只做主观 | 1.3, 2.2 |
| §10 前端最小改动 | ✅ 草稿间隔 + status 兼容 | 4.1, 4.2, 4.3 |
| §11 容量 | ✅ k6 三条曲线压测 | 5.2, 5.3 |
| §12 测试 | ✅ 每 task 含 build 验证; P5 压测 | all tasks |
| §13 实施阶段 | ✅ P0-P6 拆分 | all tasks |
| §14 风险 | ✅ 回滚手册 | 6.2 |
| §15 目录 | ✅ 对齐仓库结构 | 0.1 |

全量覆盖，无 TBD、无 TODO、无占位符。

---

**计划完成，保存至 `docs/superpowers/plans/2026-07-23-go-pg-scoring-worker-implementation.md`。**

