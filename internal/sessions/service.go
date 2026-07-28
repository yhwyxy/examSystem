// Package sessions 实现考试会话编排: StartOrResume、SaveDraft(with CAS)、Status、
// SubmitManual. 与 runs/papers/submissions/jobs 协作, 由 HTTP 层注入.
//
// 与 Python backend/exam_run_service.start_exam/save_draft/get_session_status/submit_manual
// 对齐; token 用 sha256 hash 入库, 与 runs.Run.PublicTokenHash 同源.
package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Service 编排会话生命周期. 依赖:
//   - repo *Repository           : exam_sessions 直接读写
//   - runsLookup RunsLookup      : 由 run_token / run_id 取 Run (含 finalize_at / status)
//   - submission SubmissionStore : submit 时 insert (submissions package, 由 6 实现)
//   - jobEnqueue JobEnqueuer     : submit 后 enqueue grading job (jobs package, 由 6 实现)
//   - generateID / now           : 便于测试注入
//
// service 不直接持有 pgxpool — 每个方法以 pgx.Tx / pgxPool 开始一个 (可中断的) 事务;
// 跨 repo 调用在同一 tx 内, 保证 CAS + insert submission + mark submitted 原子.
type Service struct {
	repo           *Repository
	runsLookup     RunsLookup
	submission     SubmissionStore
	jobEnqueue     JobEnqueuer
	examDuration   time.Duration
	gracePeriod    time.Duration
	draftThrottle  time.Duration // 草稿保存最小间隔, plan 138 行 retry_after_ms
	now            func() time.Time
	generateID     func() string
	generateToken  func() (string, error)
}

// RunsLookup 是 runs.Service 暴露的最小接口, 避免循环 import.
// FindByToken(runToken) 与 FindByID(runID) 都返回 *RunLite (含 status/finalize_at 等).
type RunsLookup interface {
	FindByToken(ctx context.Context, runToken string) (*RunLite, error)
	FindByID(ctx context.Context, runID string) (*RunLite, error)
}

// RunLite 是 runs.Run 的裁剪视图, 仅含 sessions 关心的字段. 由 HTTP 层/adaptor 装箱.
type RunLite struct {
	ID           string
	PaperID      string
	PaperName    string // 等同 PaperID (exam_runs 无独立 paper_name 列); 备纸用
	Status       string // "open"/"closing"/"closed"
	Duration     time.Duration
	FinalizeAt   *time.Time
	RoundNo      int
	SnapshotPath string // 评分快照文件绝对路径 (Task 6 客观题即时判分所需)
	SnapshotHash string // 评分快照 sha256 hex (Task 6 校验用)
}

// SubmissionStore 是 submissions 包暴露的最小接口 (由 Task 6 实现).
// CreateFromSessionTx 在调用方 tx 内插一条 pending submission (含客观题即时判分
// 写入 grading_detail_json / objective_score / objective_max_score), 返回 submission id;
// DuplicateExists 在调用方 tx 内查 (run_id, employee_id) 是否已提交;
// FindIDForRunEmployee 在调用方 tx 内幂等取回 submission_id.
//
// Task 6 将接口签名扩展为显式传 pgx.Tx: 保证 submission INSERT / grading_jobs INSERT /
// session MarkSubmitted 三步在同一原子单元内完成 (plan Step 2 "短事务" 要求).
type SubmissionStore interface {
	CreateFromSessionTx(ctx context.Context, tx pgx.Tx, in SubmissionInput) (string, error)
	DuplicateExists(ctx context.Context, tx pgx.Tx, runID, employeeID string) (bool, error)
	FindIDForRunEmployee(ctx context.Context, tx pgx.Tx, runID, employeeID string) (string, error)
}

// SubmissionInput 是 sessions 给 submissions 的入参 (跨包 transport).
type SubmissionInput struct {
	Name             string
	EmployeeID       string
	PaperID          string
	RunID            string
	PaperName        string
	Department       *string
	Answers          []byte          // raw canonical answers json
	AnswersMap       map[string]any  // for objective grading input
	StartedAt        time.Time
	SubmittedAt      time.Time
	ClientIP         *string
	UserAgent        *string
	AutoSubmitReason *string

	// 客观题即时判分输入 (Task 6). 由 SubmitManual 从 run.PaperName + run.Snapshot*
	// 填充; submissions.CreateFromSessionTx 在事务内 papers.LoadSnapshot + grade.
	SnapshotPath string
	SnapshotHash string
}

// JobEnqueuer 是 jobs 包最小接口 (由 Task 6 实现). EnqueueTx 在调用方 tx 内.
type JobEnqueuer interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, submissionID string, payload []byte) error
}

// NewService 构造. runsLookup 必须非空; submission/jobEnqueue 在 Task 6 接入前
// 可为 nil (StartOrResume/SaveDraft 不依赖它们). examDuration/gracePeriod 若为 0
// 用默认 (60min / 30s).
func NewService(repo *Repository, runsLookup RunsLookup, opts ...Option) *Service {
	s := &Service{
		repo:          repo,
		runsLookup:    runsLookup,
		now:           timeNowDefault,
		generateID:    generateIDDefault,
		generateToken: generateTokenDefault,
		examDuration:  60 * time.Minute,
		gracePeriod:   30 * time.Second,
		draftThrottle: 0, // 默认不限流; HTTP 层可注入 ratelimit.Limiter
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Option 用于 NewService 配置注入 (Clamp duration / throttle / now / idGen).
type Option func(*Service)

// WithExamDuration 覆盖单次会话默认时长 (来自 config.Exam.DurationMinutes).
func WithExamDuration(d time.Duration) Option {
	return func(s *Service) { if d > 0 { s.examDuration = d } }
}

// WithGracePeriod 覆盖提交宽限 (config.Exam.GracePeriodSeconds).
func WithGracePeriod(d time.Duration) Option {
	return func(s *Service) { if d > 0 { s.gracePeriod = d } }
}

// WithDraftThrottle 覆盖草稿最小保存间隔; 0 表示不限流.
func WithDraftThrottle(d time.Duration) Option {
	return func(s *Service) { s.draftThrottle = d }
}

// WithSubmissionStore 注入 submissions store (Task 6 完成后由 HTTP 层注入).
func WithSubmissionStore(store SubmissionStore) Option {
	return func(s *Service) { s.submission = store }
}

// WithJobEnqueuer 注入 jobs enqueuer (Task 6 完成后由 HTTP 层注入).
func WithJobEnqueuer(e JobEnqueuer) Option {
	return func(s *Service) { s.jobEnqueue = e }
}

// WithClock 注入测试用 clock.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithIDGen 注入测试用 id 生成器.
func WithIDGen(g func() string) Option { return func(s *Service) { s.generateID = g } }

// WithTokenGen 注入测试用 token 生成器.
func WithTokenGen(g func() (string, error)) Option {
	return func(s *Service) { s.generateToken = g }
}

// ----- 默认实现 -----

func timeNowDefault() time.Time { return time.Now().UTC() }

// generateIDDefault 生成 22 字符 base64url id (16 字节熵, 与 Python new_token 同长).
func generateIDDefault() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// generateTokenDefault 生成 43 字符 base64url token (32 字节熵).
func generateTokenDefault() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken 用 sha256 hex (与 Python backend.database.hash_token 一致).
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

var _ = json.Marshal // placeholder; 后段 SaveDraft 会用 json. 防止 "imported and not used".

// StartResult 是 StartOrResume 的返回: 已有则 Resume, 新建则 Start.
type StartResult struct {
	SessionID        string
	SessionToken     string // 明文 token, 仅本次返回, 入库为 hash
	NewSession       bool   // true=本次新建; false=resume 既有
	RunID            string
	RunStatus        string
	StartedAt        time.Time
	Deadline         time.Time
	Draft            json.RawMessage // resume 时已有 draft (空 "{}" 若新建)
	DraftRevision    int
	Paper            json.RawMessage // sanitized paper json (供前端展示), 由调用方填回或调 Get
}

// StartOrResume 对齐 Python start_exam: 给定 run_token + employee 信息, 若 (run_id, employee_id,
// status='active') 已有就 resume (返回其深层 token 需前端持有, 这里 resume 不再发新 token),
// 否则建新 session 并发出新 token.
//
// 状态机约束 (与 Python 一致):
//   - run 不存在 / token 无效 -> ErrInvalidSessionToken (401 INVALID_RUN_TOKEN)
//   - run.status='closed' -> ErrRunClosed (409 RUN_CLOSED)
//   - 否则允许 open/closing (closing 仍允许开始, 实际由 grace_period 决定,
//     与 Python backend 同 — Python 允许 closing 走 start 流程, 在 deadline 后 submit_manual 检)
//   - duplicate (run, employee) 已 active: 返回 Resume, 不再发新 token (Python 旧表
//     copy_to_working 实际上 resume 时复用 session_token 即原 token)
//
// tx 可为 *pgxpool.Pool (隐式 tx) 或 pgx.Tx (HTTP 层显式 tx); 用 pgxExec 兼容.
// 注意: 出于各端 PRD 简化, 我们让 Resume 直接回读 session_token_hash 校验对应 session_id,
// 不再重发原 token —— 前端应在首次 Start 保存明文 token 并随保存 / 提交时附带.
// (若后续 PRD 要求 Resume 重发, 在此处理逻辑即可.)
func (s *Service) StartOrResume(ctx context.Context, q pgxExec,
	runToken, employeeID, name string, department *string,
	clientIP, userAgent *string) (*StartResult, error) {
	if runToken == "" {
		return nil, ErrInvalidSessionToken
	}
	if employeeID == "" {
		return nil, fmt.Errorf("sessions.StartOrResume: employeeID empty")
	}

	runLite, err := s.runsLookup.FindByToken(ctx, runToken)
	if err != nil {
		// 区分 token-not-found (401) vs other (500); runs 包目前返回 ErrTokenNotFound
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrInvalidSessionToken) {
			return nil, ErrInvalidSessionToken
		}
		return nil, fmt.Errorf("sessions.StartOrResume lookup run: %w", err)
	}
	if runLite.Status == string(RunClosed) {
		return nil, ErrRunClosed
	}
	// closing 仍允许, 与 Python 一致 (closing 的宽限由 grace_period 决定, 不在此阻)
	now := s.now()
	deadline := now.Add(s.examDuration)
	if runLite.Status == string(RunClosing) && runLite.FinalizeAt != nil {
		// closing 期 run 自带 finalize_at; 会话截止取 min(now+duration, finalize_at)
		if runLite.FinalizeAt.Before(deadline) {
			deadline = *runLite.FinalizeAt
		}
	}

	// 先查 (run, employee) 是否已有
	existing, err := s.repo.FindByRunEmployee(ctx, q, runLite.ID, employeeID)
	if err == nil && existing != nil {
		// Resume: 返回既有 session, 不发新 token (前端按本地存储发送回)
		return &StartResult{
			SessionID:     existing.ID,
			SessionToken:  "", // resume 不重发
			NewSession:    false,
			RunID:         runLite.ID,
			RunStatus:     runLite.Status,
			StartedAt:     existing.StartedAt,
			Deadline:      existing.DeadlineAt,
			Draft:         existing.DraftJSON,
			DraftRevision: existing.DraftRevision,
		}, nil
	}
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		return nil, fmt.Errorf("sessions.StartOrResume probe existing: %w", err)
	}

	// 新建
	sessionID := s.generateID()
	token, err := s.generateToken()
	if err != nil {
		return nil, fmt.Errorf("sessions.StartOrResume gen token: %w", err)
	}
	in := SessionCreateInput{
		SessionID:        sessionID,
		RunID:           runLite.ID,
		EmployeeID:      employeeID,
		Name:            name,
		Department:      department,
		SessionTokenHash: hashToken(token),
		StartedAt:       now,
		DeadlineAt:      deadline,
		ClientIP:        clientIP,
		UserAgent:       userAgent,
	}
	if err := s.repo.InsertIfAbsent(ctx, q, in); err != nil {
		if errors.Is(err, ErrActiveExists) {
			// 并发竞得: 立即 resume 一次, 取那行
			ex, err2 := s.repo.FindByRunEmployee(ctx, q, runLite.ID, employeeID)
			if err2 != nil {
				return nil, fmt.Errorf("sessions.StartOrResume race-rescue: %w", err2)
			}
			return &StartResult{
				SessionID:     ex.ID,
				SessionToken:  "",
				NewSession:    false,
				RunID:         runLite.ID,
				RunStatus:     runLite.Status,
				StartedAt:     ex.StartedAt,
				Deadline:      ex.DeadlineAt,
				Draft:         ex.DraftJSON,
				DraftRevision: ex.DraftRevision,
			}, nil
		}
		return nil, fmt.Errorf("sessions.StartOrResume insert: %w", err)
	}

	return &StartResult{
		SessionID:     sessionID,
		SessionToken:  token, // 仅新建时返回明文
		NewSession:    true,
		RunID:         runLite.ID,
		RunStatus:     runLite.Status,
		StartedAt:     now,
		Deadline:      deadline,
		Draft:         []byte("{}"),
		DraftRevision: 0,
	}, nil
}

// SaveDraftResult 给 HTTP 层反映 CAS 结果.
type SaveDraftResult struct {
	Revision    int
	DraftSavedAt time.Time
	Throttled   bool          // true 表示距离上次保存太近, 拒写 (rls 0 行无变化)
}

// SaveDraft 对齐 Python save_draft: 给定 session_token + 旧 revision + 新 draft,
// 用 CAS 把 draft_revision+1. 多重约束:
//   - token 校验 (Find by token hash) 错 -> ErrInvalidSessionToken
//   - session 不在 active -> ErrSessionSubmitted (已交卷)
//   - draft_revision 不匹配 -> ErrStaleDraftRevision (409 STALE_DRAFT)
//   - 已超 deadline -> ErrRunClosed (409 RUN_CLOSED; Python 用 deadline 已过)
//   - 距离上次 draft_saved_at 太近 (draftThrottle>0) -> Throttled=true, 写入被同 CAS
//     拒绝但要明确告诉前端 retry_after. 实现: 若 draftThrottle>0 且 draft_saved_at 不为空,
//     且 now - draft_saved_at < draftThrottle, 直接返回 Throttled=true (让 HTTP 层
//     回 429 + Retry-After), 不走 CAS.
//
// draftJSON 由 HTTP 层做 sanitize/类型校验; service 仅做 raw jsonb 落库 (与 Python 一致).
func (s *Service) SaveDraft(ctx context.Context, q pgxExec,
	sessionToken string, oldRevision int, draftJSON []byte) (*SaveDraftResult, error) {
	if sessionToken == "" {
		return nil, ErrInvalidSessionToken
	}
	if len(draftJSON) == 0 {
		draftJSON = []byte("{}")
	}

	// 先按 token hash 找 session
	// (Python save_draft 是按 token 校验 session_token_hash; 我们 repo 没直接定
	// FindByToken, 故用 inline SQL 在此轻量查 ID; 若后期 repo 增加 FindByToken 即抽走)
	row := q.QueryRow(ctx, `
SELECT `+sessionCols+` FROM exam_sessions WHERE session_token_hash = $1`,
		hashToken(sessionToken))
	sess, err := scanSession(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrInvalidSessionToken
		}
		return nil, fmt.Errorf("sessions.SaveDraft probe by token: %w", err)
	}

	// 状态机约束
	switch sess.Status {
	case StatusSubmitted:
		return nil, ErrSessionSubmitted
	case StatusActive:
		// ok
	default:
		return nil, fmt.Errorf("sessions.SaveDraft: unexpected status %q", sess.Status)
	}

	// 超期校验: Python 行为是 deadline 已过 -> 410 GONE / 行为按 RUN_CLOSED 报错
	if s.now().After(sess.DeadlineAt) {
		return nil, ErrRunClosed
	}

	// throttle 检查 (靠近上次写)
	if s.draftThrottle > 0 && sess.DraftSavedAt != nil {
		if elapsed := s.now().Sub(*sess.DraftSavedAt); elapsed < s.draftThrottle {
			return &SaveDraftResult{
				Revision:    sess.DraftRevision, // 不变
				DraftSavedAt: *sess.DraftSavedAt,
				Throttled:   true,
			}, nil
		}
	}

	// CAS
	res, err := s.repo.UpdateDraftCAS(ctx, q, sess.ID, oldRevision, draftJSON)
	if err != nil {
		return nil, err
	}
	return &SaveDraftResult{
		Revision:     res.Revision,
		DraftSavedAt: derefTime(res.DraftSavedAt),
		Throttled:    false,
	}, nil
}

// derefTime 把 *time.Time 解为 time.Time; nil 当 zero, 仅用于返回结构体.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// StatusResult 是 Status 调用结果.
type StatusResult struct {
	SessionID     string
	RunID         string
	EmployeeID    string
	Name          string
	Status        SessionStatus
	StartedAt     time.Time
	DeadlineAt    time.Time
	DraftRevision int
	DraftSavedAt  *time.Time
	Draft         json.RawMessage
}

// Status 对齐 Python get_session_status: 用 token hash 查会话; 返回草案 / 截止 / revision.
// 错误: token 不匹配 -> ErrInvalidSessionToken; session 不存在 -> ErrSessionNotFound.
func (s *Service) Status(ctx context.Context, q pgxExec, sessionToken string) (*StatusResult, error) {
	if sessionToken == "" {
		return nil, ErrInvalidSessionToken
	}
	row := q.QueryRow(ctx, `
SELECT `+sessionCols+` FROM exam_sessions WHERE session_token_hash = $1`,
		hashToken(sessionToken))
	sess, err := scanSession(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("sessions.Status probe by token: %w", err)
	}
	return &StatusResult{
		SessionID:     sess.ID,
		RunID:         sess.RunID,
		EmployeeID:    sess.EmployeeID,
		Name:          sess.Name,
		Status:        sess.Status,
		StartedAt:     sess.StartedAt,
		DeadlineAt:    sess.DeadlineAt,
		DraftRevision: sess.DraftRevision,
		DraftSavedAt:  sess.DraftSavedAt,
		Draft:         sess.DraftJSON,
	}, nil
}

// SubmitManualRequest 是 SubmitManual 的入参. answers / answers_map 由 HTTP 层解析
// 后传入; answersMap 仅用于注入 objective grader 内部 cache.
type SubmitManualRequest struct {
	SessionToken    string
	ClientIP        *string
	UserAgent       *string
	Answers         []byte         // canonical json bytes
	AnswersMap      map[string]any // for objective grader (optional)
	AutoSubmitReason *string       // 非 nil 表示系统自动交卷 (deadline / admin close)
}

// SubmitManualResult 是 SubmitManual 的成功结果.
type SubmitManualResult struct {
	SubmissionID string
	SessionID    string
	New          bool // false 表示幂等命中已有提交
}

// SubmitManual 对齐 Python submit_manual:
//  1. token hash 查 session; 不在 active -> ErrSessionSubmitted; 已超 deadline + closing
//     期允许 -> 处理; closed -> ErrRunClosed
//  2. (run_id, employee_id) 查 submissions; 已存在 submission -> 幂等返回 New=false
//     + 已有 submission_id
//  3. create submission (tx 内); submit 元数据 + 客观题分数同步 insert review_logs
//  4. EnqueueTx job (payload=answers); 失败回滚
//  5. mark session submitted
//
// 依赖 submissions + jobs; 在 Task 6 之前 submission/jobEnqueue 可能为 nil,
// 此时应返回 ErrSubmitDisabled 而非 panic.
//
// 注: 此处仅做骨架, 真正跨包协作逻辑由 Task 6 完成后 (submissions/jobs 实装) 调通;
// 此段先注入依赖校验 + token/session/state machine 完成"开门"部分, 后续把链路接强.
func (s *Service) SubmitManual(ctx context.Context, q pgxExec, req SubmitManualRequest) (*SubmitManualResult, error) {
	if s.submission == nil || s.jobEnqueue == nil {
		return nil, ErrSubmitDisabled
	}
	// Plan Step 2: 全部只读 + 写入必须落在同一 pgx.Tx 内 (HTTP 层 Begin/Commit/Rollback).
	// pgx.Tx 实现 pgxExec; *pgxpool.Pool 也实现 pgxExec 但不是 pgx.Tx - 拒绝以防止
	// auto-tx 让 INSERT submission / INSERT job / MarkSubmitted 三步失去原子性.
	tx, ok := q.(pgx.Tx)
	if !ok {
		return nil, fmt.Errorf("sessions.SubmitManual: %w", ErrSubmitRequiresTx)
	}
	if req.SessionToken == "" {
		return nil, ErrInvalidSessionToken
	}
	if len(req.Answers) == 0 {
		req.Answers = []byte("{}")
	}

	// 1. session lookup by token hash
	row := q.QueryRow(ctx, `
SELECT `+sessionCols+` FROM exam_sessions WHERE session_token_hash = $1`,
		hashToken(req.SessionToken))
	sess, err := scanSession(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("sessions.SubmitManual probe by token: %w", err)
	}
	if sess.Status == StatusSubmitted {
		// 幂等: 已提交, 取回 submission_id 当 New=false
		subID, err := s.submission.FindIDForRunEmployee(ctx, tx, sess.RunID, sess.EmployeeID)
		if err != nil {
			return nil, fmt.Errorf("sessions.SubmitManual idempotent lookup: %w", err)
		}
		return &SubmitManualResult{
			SubmissionID: subID,
			SessionID:    sess.ID,
			New:          false,
		}, nil
	}

	// 2. run state 跟踪 (closing 仍允许, closed 拒绝)
	run, err := s.runsLookup.FindByID(ctx, sess.RunID)
	if err != nil {
		return nil, fmt.Errorf("sessions.SubmitManual lookup run: %w", err)
	}
	now := s.now()
	if run.Status == string(RunClosed) {
		return nil, ErrRunClosed
	}
	if run.Status == string(RunClosing) && run.FinalizeAt != nil && now.After(*run.FinalizeAt) {
		// closing 已过 final_at -> 视作 closed
		return nil, ErrRunClosed
	}

	// 3. duplicate submission check (submissions.DuplicateExists on run+employee)
	dup, err := s.submission.DuplicateExists(ctx, tx, sess.RunID, sess.EmployeeID)
	if err != nil {
		return nil, fmt.Errorf("sessions.SubmitManual dup-check: %w", err)
	}
	if dup {
		subID, err2 := s.submission.FindIDForRunEmployee(ctx, tx, sess.RunID, sess.EmployeeID)
		if err2 != nil {
			return nil, fmt.Errorf("sessions.SubmitManual dup-id lookup: %w", err2)
		}
		// idempotent 返回
		return &SubmitManualResult{SubmissionID: subID, SessionID: sess.ID, New: false}, nil
	}

	// 4. create submission + enqueue + mark submitted (此步要求 q 为 pgx.Tx;
	// HTTP 层必须显式传 tx, 而不是 *pgxpool.Pool auto-tx — 否则无法整体回滚.
	// 我们这里通过 Assertion 检测: pgx.Tx 同时实现 pgxExec, 但 *pgxpool.Pool 也是.
	// 由于 CreateFromSessionTx 内部会在同一 Exec/QueryRow 上写 submissions / review_logs / jobs,
	// HTTP 层负责 Begin/Commit/Rollback, Service 不感知 tx 类型.)

	subID, err := s.submission.CreateFromSessionTx(ctx, tx, SubmissionInput{
		Name:             sess.Name,
		EmployeeID:       sess.EmployeeID,
		PaperID:          run.PaperID,
		RunID:            run.ID,
		PaperName:        run.PaperName,
		SnapshotPath:     run.SnapshotPath,
		SnapshotHash:     run.SnapshotHash,
		Department:       sess.Department,
		Answers:          req.Answers,
		AnswersMap:       req.AnswersMap,
		StartedAt:        sess.StartedAt,
		SubmittedAt:      now,
		ClientIP:         req.ClientIP,
		UserAgent:        req.UserAgent,
		AutoSubmitReason: req.AutoSubmitReason,
	})
	if err != nil {
		return nil, fmt.Errorf("sessions.SubmitManual create submission: %w", err)
	}
	// 构造 grading job spec payload: paper_id / run_id / generation=1.
	// jobs.Service.EnqueueTx 解析 JSON 字段; 缺失字段容错用默认 generation=1.
	jobPayload := mustJobSpecJSON(run.PaperID, sess.RunID, 1)
	if err := s.jobEnqueue.EnqueueTx(ctx, tx, subID, jobPayload); err != nil {
		return nil, fmt.Errorf("sessions.SubmitManual enqueue: %w", err)
	}
	if _, err := s.repo.MarkSubmitted(ctx, q, sess.ID); err != nil {
		return nil, fmt.Errorf("sessions.SubmitManual mark submitted: %w", err)
	}

	return &SubmitManualResult{
		SubmissionID: subID,
		SessionID:    sess.ID,
		New:          true,
	}, nil
}

// mustJobSpecJSON 构造 jobs.Service.EnqueueTx 期望的 payload JSON.
// Marshal 出错时返回空 []byte{} (容错: EnqueueTx 见空 payload 仍按默认
// generation=1 入队, 但 paper_id/run_id 会缺失 -> 仅作兜底).
func mustJobSpecJSON(paperID, runID string, generation int) []byte {
	b, _ := json.Marshal(map[string]any{
		"paper_id":   paperID,
		"run_id":     runID,
		"generation": generation,
	})
	return b
}


// ErrSubmitDisabled 是 submission/job 依赖未注入时的合约错误 (HTTP 映射 503).
var ErrSubmitDisabled = errors.New("sessions: SUBMIT_DISABLED")

// ErrSubmitRequiresTx 表示 SubmitManual 收到的 q 不是 pgx.Tx (调用方传入 *pgxpool.Pool
// auto-tx 时无法整体回滚, 拒绝). HTTP 映射 500.
var ErrSubmitRequiresTx = errors.New("sessions: SUBMIT_REQUIRES_TX")

