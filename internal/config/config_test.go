package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig 写一份临时 yaml 给 Task 1 的最小可用结构, 含 4 段新段。
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

const minimalYAML = `
server:
  host: "127.0.0.1"
  port: 8000
  allow_origins: []
exam:
  title: "契约校验"
  duration_minutes: 90
  auto_submit: true
  grace_period_seconds: 30
  allow_duplicate_submit: false
  duplicate_key: "employee_id"
  enable_global_time_window: false
  start_time: null
  end_time: null
scoring:
  multiple_choice_partial: true
  wrong_choice_penalty: false
  score_precision: 1
review:
  high_confidence_threshold: 0.75
  need_review_threshold: 0.5
  low_confidence_threshold: 0.35
grading:
  sync_grading: true
model:
  reranker: "BAAI/bge-reranker-v2-m3"
admin:
  enable_auth: false
  password: null
export:
  format: "xlsx"
database:
  url: "postgres://exam:exam@127.0.0.1:5432/exam?sslmode=disable"
  max_conns: 32
  min_conns: 4
  connect_timeout_seconds: 5
  statement_timeout_seconds: 10
draft:
  min_server_interval_ms: 2000
  max_json_bytes: 512000
worker:
  poll_interval_ms: 200
  concurrency: 2
  lease_seconds: 300
  heartbeat_seconds: 60
  max_attempts: 5
logging:
  directory: "logs"
`

func TestLoad_FromYAML(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, minimalYAML)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// server
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 8000 {
		t.Errorf("server got %s:%d", cfg.Server.Host, cfg.Server.Port)
	}
	// exam
	if cfg.Exam.DurationMinutes != 90 || !cfg.Exam.AutoSubmit {
		t.Errorf("exam shape mismatch: %+v", cfg.Exam)
	}
	// scoring / review / grading / model / admin / export
	if !cfg.Scoring.MultipleChoicePartial {
		t.Errorf("scoring.partial should be true")
	}
	if cfg.Review.HighConfidenceThreshold != 0.75 {
		t.Errorf("review.high threshold mismatch")
	}
	if !cfg.Grading.SyncGrading {
		t.Errorf("grading.sync expected true")
	}
	if cfg.Model.Reranker == "" {
		t.Errorf("model.reranker empty")
	}
	if cfg.Admin.EnableAuth {
		t.Errorf("admin.enable_auth expected false")
	}
	if cfg.Export.Format != "xlsx" {
		t.Errorf("export.format expected xlsx")
	}
	// Go 新增 4 段
	if cfg.Database.URL == "" {
		t.Errorf("database.url empty")
	}
	if cfg.Database.MaxConns != 32 || cfg.Database.MinConns != 4 {
		t.Errorf("database conn bounds mismatch: %+v", cfg.Database)
	}
	if cfg.Database.ConnectTimeoutSeconds != 5 || cfg.Database.StatementTimeoutSeconds != 10 {
		t.Errorf("database timeout fields mismatch: %+v", cfg.Database)
	}
	if cfg.Draft.MinServerIntervalMs != 2000 || cfg.Draft.MaxJSONBytes != 512000 {
		t.Errorf("draft fields mismatch: %+v", cfg.Draft)
	}
	if cfg.Worker.PollIntervalMs != 200 || cfg.Worker.Concurrency != 2 {
		t.Errorf("worker fields mismatch: %+v", cfg.Worker)
	}
	if cfg.Worker.LeaseSeconds != 300 || cfg.Worker.HeartbeatSeconds != 60 {
		t.Errorf("worker lease/heartbeat mismatch: %+v", cfg.Worker)
	}
	if cfg.Worker.MaxAttempts != 5 {
		t.Errorf("worker.max_attempts expected 5")
	}
	if cfg.Logging.Directory != "logs" {
		t.Errorf("logging.directory expected 'logs'")
	}
}

// env override: EXAM_DATABASE_URL / EXAM_ADMIN_PASSWORD
func TestLoad_EnvOverrides(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, minimalYAML)
	t.Setenv("EXAM_DATABASE_URL", "postgres://override:pw@127.0.0.1:5432/exam?sslmode=disable")
	t.Setenv("EXAM_ADMIN_PASSWORD", "secret-pw")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.URL != "postgres://override:pw@127.0.0.1:5432/exam?sslmode=disable" {
		t.Errorf("EXAM_DATABASE_URL override failed: got %q", cfg.Database.URL)
	}
	if cfg.Admin.Password == nil || *cfg.Admin.Password != "secret-pw" {
		t.Errorf("EXAM_ADMIN_PASSWORD override failed: got %+v", cfg.Admin.Password)
	}
}

// sync_grading=true 仅警告, 不应失败 (Python 基线默认)
func TestLoad_SyncGradingWarningOnly(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, minimalYAML) // grading.sync_grading: true
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("sync_grading=true should not return error, got %v", err)
	}
	if !cfg.Grading.SyncGrading {
		t.Errorf("sync_grading should be true")
	}
}

// 空 url 在走 Load 时不一定报错 (可由 serve/preflight 子命令做 final 校验);
// 但若数据库 url 与 yaml 都为空, 加载时声明 present=false.
func TestLoad_EmptyDatabaseURL(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(minimalYAML,
		`url: "postgres://exam:exam@127.0.0.1:5432/exam?sslmode=disable"`, "url: \"\"", 1)
	p := writeConfig(t, dir, body)
	t.Setenv("EXAM_DATABASE_URL", "")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load empty url should succeed (defer to subcommand): %v", err)
	}
	if cfg.Database.URL != "" {
		t.Errorf("empty url expected, got %q", cfg.Database.URL)
	}
	// ValidateRequired 应明确指出 database.url 缺失
	if err := cfg.ValidateRequired(); err == nil {
		t.Errorf("ValidateRequired should fail on empty database.url")
	} else if !strings.Contains(err.Error(), "database.url") {
		t.Errorf("error should mention database.url, got %v", err)
	}
}

// ValidateRequired: 完整配置应通过
func TestValidateRequired_OK(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, minimalYAML)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateRequired(); err != nil {
		t.Errorf("ValidateRequired should pass on full config, got %v", err)
	}
}
