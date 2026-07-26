// Package submissions - types.go
// 公开类型与 enums。
package submissions

import (
	"errors"
	"time"
)

// Status 是 submissions.grading_status 枚举字面量（与 SQL CHECK 对齐）。
type Status string

const (
	StatusPending Status = "pending" // 已入队, 待 worker 抢占 (Task 6 初始)
	StatusGrading Status = "grading" // worker 处理中 (Task 7)
	StatusDone    Status = "done"    // 抄表完成
	StatusFailed  Status = "failed"  // worker 多次失败
)

// ReviewStatus 是 submissions.review_status 枚举字面量。
type ReviewStatus string

const (
	ReviewGrading   ReviewStatus = "grading" // 初始: 待评审批阅
	ReviewSubmitted ReviewStatus = "submitted"
	ReviewApproved  ReviewStatus = "approved"
	ReviewRejected  ReviewStatus = "rejected"
	ReviewReopened  ReviewStatus = "reopened"
)

// Session 是 Submit 编排所需的最小会话读字段（事务外只读用）。
type Session struct {
	ID                 string
	Name               string
	EmployeeID         string
	RunID              string
	Department         *string
	SessionTokenHash   string
	StartedAt          time.Time
	DeadlineAt         *time.Time // 考试截止时间; +grace_period_seconds 之外仍可容许提交.
	Status             string // active/submitted/closed/expired
}

// RunLite 是 Submit 编排所需的最小 run 读字段（含快照路径与 hash）。
type RunLite struct {
	ID           string
	PaperName    string
	SnapshotPath string
	SnapshotHash string
}

// SubmitRequest 是 HTTP 层向 Submit 提交的请求体。
type SubmitRequest struct {
	SessionToken    string         // 客户端原始 token; 由包内 hash 后比对
	Answers         []byte         // raw canonical answers json
	AnswersMap      map[string]any // objective grading input
	ClientIP        *string
	UserAgent       *string
	AutoSubmitReason *string       // 非空表示系统自动提交（exceeded max-timeout）
}

// PreparedSubmission 是事务外计算得到的事务写输入。短事务内只写它。
type PreparedSubmission struct {
	Name              string
	EmployeeID        string
	PaperID           string
	RunID             string
	PaperName         string
	Department        *string
	Answers           []byte         // canonical answers (raw)
	StartedAt         time.Time
	SubmittedAt       time.Time
	ClientIP          *string
	UserAgent         *string
	AutoSubmitReason  *string

	// 客观题即时判分结果 (事务外算好)。主观题请求占位为 0, 待 worker 填充。
	ObjectiveScore     int
	ObjectiveMaxScore   int
	ObjectiveDetailJSON []byte // 单题判分明细 (array of objective.GradeResult 字段)
	QuestionTotal       int
}

// SubmitResult 是 Submit 返回的最小元信息。
type SubmitResult struct {
	SubmissionID string
}

// StatusResult 是 GET /api/submission/{id}/status 的公开 response body.
// 注意: plan Step 4 要求只露 4 字段 (submission_id, status, grading_status,
// review_status); 不返回分数 / 错误文本 / 身份 / 答案.
// status 由 grading_status + review_status 派生 (兼容 Python 旧 status):
//   - pending / grading      -> grading
//   - done                   -> review_status (submitted/approved/auto_scored/...)
//   - failed                 -> need_review
type StatusResult struct {
	SubmissionID  string      `json:"submission_id"`
	Status        Status      `json:"status"`
	GradingStatus Status      `json:"grading_status"`
	ReviewStatus  ReviewStatus `json:"review_status"`
}


// Submission 是 GetByID 返回的内部持久行 (供 StatusResult 等进一步转换).
type Submission struct {
	ID                string
	GradingStatus     Status
	ReviewStatus      ReviewStatus
	ObjectiveScore    int
}

// ErrSubmissionClosed 表示 session / run 已关闭，不允许提交。
var ErrSubmissionClosed = errors.New("SUBMISSION_CLOSED")

// ErrDuplicateSubmission 表示同 (run_id, employee_id) 已存在 submission。
var ErrDuplicateSubmission = errors.New("DUPLICATE_SUBMISSION")

// ErrInvalidSessionToken 与 sessions 包 ErrInvalidSessionToken 同义。
var ErrInvalidSessionToken = errors.New("INVALID_SESSION_TOKEN")

// ErrInvalidAnswers 表示 answers json 解析失败或为空。
var ErrInvalidAnswers = errors.New("INVALID_ANSWERS")
// ErrInvalidAutoSubmitReason 表示 auto_submit_reason 不在 {third_blur, blur_timeout_30s} 枚举内.
var ErrInvalidAutoSubmitReason = errors.New("INVALID_AUTO_SUBMIT_REASON")


