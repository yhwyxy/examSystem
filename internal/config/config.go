// Package config 加载并校验 examSystem 配置。
//
// 设计要点 (Task 1):
//   - 现有 Python 端 8 段结构 (server/exam/scoring/review/grading/model/admin/export)
//     与 Go 端完全字段兼容;  yaml tag 与 Python 字段名一致。
//   - 额外新增 4 段 (database/draft/worker/logging) 仅供 Go 服务消费,
//     Python _allowed 白名单已扩展, 不影响 Python 启动。
//   - 环境变量 EXAM_DATABASE_URL / EXAM_ADMIN_PASSWORD 优先级最高,
//     便于容器化部署注入。
//   - sync_grading=true 仅警告, 不阻断 Load (保留 Python 基线行为);
//     最终校验由子命令 (serve/preflight/migrate) 调 ValidateRequired 完成。
package config

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config 是 examSystem 的运行配置.  字段名 / yaml tag 与 Python 端 backend/config.py 对齐.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Exam     ExamConfig     `yaml:"exam"`
	Scoring  ScoringConfig  `yaml:"scoring"`
	Review   ReviewConfig   `yaml:"review"`
	Grading  GradingConfig  `yaml:"grading"`
	Model    ModelConfig    `yaml:"model"`
	Admin    AdminConfig    `yaml:"admin"`
	Export   ExportConfig   `yaml:"export"`
	Database DatabaseConfig `yaml:"database"`
	Draft    DraftConfig    `yaml:"draft"`
	Worker   WorkerConfig   `yaml:"worker"`
	Logging  LoggingConfig  `yaml:"logging"`

	loadedOnce sync.Once `yaml:"-"`
	path       string    `yaml:"-"`
}

type ServerConfig struct {
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	AllowOrigins []string `yaml:"allow_origins"`
}

type ExamConfig struct {
	Title                  string `yaml:"title"`
	DurationMinutes        int    `yaml:"duration_minutes"`
	AutoSubmit             bool   `yaml:"auto_submit"`
	GracePeriodSeconds     int    `yaml:"grace_period_seconds"`
	AllowDuplicateSubmit   bool   `yaml:"allow_duplicate_submit"`
	DuplicateKey           string `yaml:"duplicate_key"`
	EnableGlobalTimeWindow bool   `yaml:"enable_global_time_window"`
	StartTime              string `yaml:"start_time"`
	EndTime                string `yaml:"end_time"`
}

type ScoringConfig struct {
	MultipleChoicePartial bool `yaml:"multiple_choice_partial"`
	WrongChoicePenalty    bool `yaml:"wrong_choice_penalty"`
	ScorePrecision        int  `yaml:"score_precision"`
}

type ReviewConfig struct {
	HighConfidenceThreshold float64 `yaml:"high_confidence_threshold"`
	NeedReviewThreshold     float64 `yaml:"need_review_threshold"`
	LowConfidenceThreshold  float64 `yaml:"low_confidence_threshold"`
}

type GradingConfig struct {
	SyncGrading bool `yaml:"sync_grading"`
}

type ModelConfig struct {
	Reranker string `yaml:"reranker"`
}

type AdminConfig struct {
	EnableAuth bool    `yaml:"enable_auth"`
	Password   *string `yaml:"password"`
}

type ExportConfig struct {
	Format string `yaml:"format"`
}

// ----- Go 服务新增 4 段 -----

type DatabaseConfig struct {
	URL                     string `yaml:"url"`
	MaxConns                int32  `yaml:"max_conns"`
	MinConns                int32  `yaml:"min_conns"`
	ConnectTimeoutSeconds   int    `yaml:"connect_timeout_seconds"`
	StatementTimeoutSeconds int    `yaml:"statement_timeout_seconds"`
}

type DraftConfig struct {
	MinServerIntervalMs int `yaml:"min_server_interval_ms"`
	MaxJSONBytes        int `yaml:"max_json_bytes"`
}

type WorkerConfig struct {
	PollIntervalMs   int `yaml:"poll_interval_ms"`
	Concurrency      int `yaml:"concurrency"`
	LeaseSeconds     int `yaml:"lease_seconds"`
	HeartbeatSeconds int `yaml:"heartbeat_seconds"`
	MaxAttempts      int `yaml:"max_attempts"`
}

type LoggingConfig struct {
	Directory  string `yaml:"directory"`
	MaxSizeMB  int    `yaml:"max_size_mb"`  // Task 12: 日志轮转单文件大小上限 MB; 0 -> 默认 20
	MaxBackups int    `yaml:"max_backups"`  // Task 12: 日志保留份数; 0 -> 默认 10
	MaxAgeDays int    `yaml:"max_age_days"` // Task 12: 日志保留天数; 0 -> 不删除 (按份数 MaxBackups)
	// RunTokenDir 是 admin 开考时明文 token 侧车文件目录 (0600), 与 Python 旧栈
	// exam_run_service.save_public_token parity (exam_run_service.py 行 115).
	// 空: admin exam-link GET 无法重建 URL (run 在 tokenDir 配置前创建).
	// 注: token 不入 DB (DB 仅存 sha256 hash). admin UI 重建 URL 靠此 sidecar.
	RunTokenDir string `yaml:"run_token_dir"` // Task 14: admin tail 补
}

// Load 从 yaml 路径加载配置; 应用环境变量覆盖; 不做强制校验,
// 强制校验交给子命令 (serve/preflight/migrate) 调 ValidateRequired。
//
// env 覆盖:
//   - EXAM_DATABASE_URL  -> Database.URL  (即使 yaml 中为空也接管)
//   - EXAM_ADMIN_PASSWORD -> Admin.Password
func Load(path string) (*Config, error) {
	cfg := &Config{path: path}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		// 配置默认值: yaml.Unmarshal 会用 yaml tag 顶替; 此处补充默认
		applyDefaults(cfg)
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	} else {
		applyDefaults(cfg)
	}

	// env 覆盖 (优先级最高)
	if v := strings.TrimSpace(os.Getenv("EXAM_DATABASE_URL")); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("EXAM_ADMIN_PASSWORD"); v != "" {
		s := v
		cfg.Admin.Password = &s
	}

	// sync_grading=true 仅警告, 不阻断 Load (保留 Python 基线行为)
	if cfg.Grading.SyncGrading {
		fmt.Fprintln(os.Stderr,
			"⚠️  grading.sync_grading=true 已被 Go 服务忽略; Go 端评分始终经 DB 异步队列.")
	}

	return cfg, nil
}

// applyDefaults 设置与 Python 端相同的默认值, 防 yaml 缺字段时空零值不可用.
func applyDefaults(cfg *Config) {
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 8000
	cfg.Exam.DurationMinutes = 90
	cfg.Exam.GracePeriodSeconds = 30
	cfg.Exam.AutoSubmit = true
	cfg.Exam.DuplicateKey = "employee_id"
	cfg.Scoring.MultipleChoicePartial = true
	cfg.Scoring.ScorePrecision = 1
	cfg.Model.Reranker = "BAAI/bge-reranker-v2-m3"
	cfg.Export.Format = "xlsx"
	cfg.Database.MaxConns = 32
	cfg.Database.MinConns = 4
	cfg.Database.ConnectTimeoutSeconds = 5
	cfg.Database.StatementTimeoutSeconds = 10
	cfg.Draft.MinServerIntervalMs = 2000
	cfg.Draft.MaxJSONBytes = 512000
	cfg.Worker.PollIntervalMs = 200
	cfg.Worker.Concurrency = 2
	cfg.Worker.LeaseSeconds = 300
	cfg.Worker.HeartbeatSeconds = 60
	cfg.Worker.MaxAttempts = 5
	cfg.Logging.Directory = "logs"
	if cfg.Logging.MaxSizeMB == 0 {
		cfg.Logging.MaxSizeMB = 20 // Task 12 plan: 单文件 20MB
	}
	if cfg.Logging.MaxBackups == 0 {
		cfg.Logging.MaxBackups = 10 // Task 12 plan: 保留 10 份
	}
	// MaxAgeDays=0 保留默认 (按份数); 显式配了则按天数清理
}

// ValidateRequired 校验子命令 (serve/preflight/migrate) 启动前的必填项.
// 当前仅要求 database.url 非空; 后续 Task 加 server.port 范围 / worker 范围等.
func (c *Config) ValidateRequired() error {
	var missing []string
	if strings.TrimSpace(c.Database.URL) == "" {
		missing = append(missing, "database.url")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Path 返回配置文件加载路径, 空 = 默认.
func (c *Config) Path() string { return c.path }
