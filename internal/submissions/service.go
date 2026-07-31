// Package submissions - service.go
//
// Service.Submit 是 plan Task 6 主等待编排:
//  1. 事务外只读 (pool 自动事务): session token hash lookup, run lookup (含 snapshot
//     path/hash), duplicate check, idempotent lookup.
//  2. 事务外 CPU 工作: papers.LoadSnapshot + objective grade -> PreparedSubmission
//     (含 objective_score / objective_max_score / 单题 detail JSON / question_total).
//  3. 短事务内 (Begin): FOR UPDATE session + FOR UPDATE run + INSERT pending
//     submission (含客观分预填, 主观题占位 score=0) + INSERT grading_jobs (generation=1)
//     + UPDATE session status active -> submitted. 全部写一致, 失败 Rollback.
//
// 与 sessions.SubmitManual 的关系: SubmitManual 是 Task 5 遗留 deprecated 编排 (Tx 内
// 才判分), 仅用于 sessions_test 的 disabled case 路径; HTTP 主路径 (student.go) 走 Submit.
package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/papers"
)

// JobEnqueuer 是入队抽象 (jobs.Service 实现). 解耦避免反向依赖 sessions.
type JobEnqueuer interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, submissionID string, payload []byte) error
}

// GraderFunc 是注入式客观题评分函数 (objective.Grade 的方法 receiver). 由 HTTP
// 层注入闭包, 便于测试替换.
type GraderFunc func(question map[string]any, studentAnswer any, partial bool) (map[string]any, error)

// SnapshotLoader 是注入式快照加载函数 (papers.LoadSnapshot signature).
type SnapshotLoader func(path, shaHex string) (*papers.Snapshot, error)

// Service 是 plan Task 6 主编排.零 IO 状态, 全部依赖 ctor 注入.
type Service struct {
	pool               *pgxpool.Pool
	repo               *Repository
	snapshotLoad       SnapshotLoader
	grade              GraderFunc
	jobs               JobEnqueuer
	GracePeriodSeconds int // plan Step 2: 超过 deadline + grace 后拒绝提交 (-1 = 不校验)
	// MultipleChoicePartial 对应 config scoring.multiple_choice_partial:
	// true 时多选按命中比例给分 (无错项前提), false 全对才给分. main 装配时注入.
	MultipleChoicePartial bool
}

// NewService 构造 Service. 任一必传依赖 nil 返回 nil (不允许 panic).
// gracePeriodSeconds < 0 表示不校验 deadline + grace.
func NewService(pool *pgxpool.Pool, repo *Repository, snap SnapshotLoader,
	grade GraderFunc, jobs JobEnqueuer, gracePeriodSeconds int) *Service {
	if pool == nil || repo == nil || snap == nil || grade == nil || jobs == nil {
		return nil
	}
	return &Service{
		pool:               pool,
		repo:               repo,
		snapshotLoad:       snap,
		grade:              grade,
		jobs:               jobs,
		GracePeriodSeconds: gracePeriodSeconds,
	}
}

// GetStatus 实现 GET /api/submission/{id}/status (plan Step 4): 返回最小 4 字段,
// 不暴露错误 / 身份 / 评分原始数组.
func (s *Service) GetStatus(ctx context.Context, submissionID string) (*StatusResult, error) {
	if s == nil || s.repo == nil || s.pool == nil {
		return nil, ErrNoRows
	}
	sub, err := s.repo.GetByID(ctx, s.pool, submissionID)
	if err != nil {
		return nil, err
	}
	return &StatusResult{
		SubmissionID:  sub.ID,
		Status:        deriveStatus(sub.GradingStatus, sub.ReviewStatus),
		GradingStatus: sub.GradingStatus,
		ReviewStatus:  sub.ReviewStatus,
	}, nil
}

// deriveStatus 把 (grading_status, review_status) -> status (Python 兼容映射).
//   - pending/grading  -> grading
//   - done             -> review_status (生命周期阶段, 如 auto_scored/submitted/...)
//   - failed           -> need_review
func deriveStatus(gs Status, rs ReviewStatus) Status {
	switch gs {
	case StatusPending, StatusGrading:
		return "grading"
	case StatusDone:
		return Status(rs) // review_status 即生命周期阶段
	case StatusFailed:
		return "need_review"
	}
	return "grading"
}

// Submit 是 HTTP 层主入口 (POST /api/session/submit). 编排拆三段:
//
//	(a) 事务外只读: token 校验 + run + dup + idempotent lookup.
//	(b) 事务外 CPU: LoadSnapshot + objective grade -> PreparedSubmission.
//	(c) 短事务: FOR UPDATE session + FOR UPDATE run + INSERT submission +
//	    INSERT grading_jobs (gen=1) + UPDATE session active -> submitted.
//
// 幂等: 同 (run_id, employee_id) 已存在则直接返回既有 submission id, 不重复入队.
// (与 sessions.SubmitManual 行为对齐; 兼容并发重试.)
func (s *Service) Submit(ctx context.Context, in SubmitRequest) (*SubmitResult, error) {
	if s == nil || s.pool == nil || s.repo == nil {
		return nil, ErrSubmitDisabled
	}
	if in.SessionToken == "" {
		return nil, ErrInvalidSessionToken
	}
	if len(in.Answers) == 0 {
		in.Answers = []byte("{}")
	}
	// auto_submit_reason 枚举校验 (与 Python Literal 对齐).
	if in.AutoSubmitReason != nil && *in.AutoSubmitReason != "" {
		switch *in.AutoSubmitReason {
		case "third_blur", "blur_timeout_30s":
		default:
			return nil, fmt.Errorf("%w: %s", ErrInvalidAutoSubmitReason, *in.AutoSubmitReason)
		}
	}
	// --- (a) 事务外只读 (pool 自动事务, 无锁) ---
	sess, err := s.repo.FindSessionByToken(ctx, s.pool, in.SessionToken)
	if err != nil {
		return nil, err
	}
	switch sess.Status {
	case "submitted":
		// 已提交; 幂等返回既有 submission id (避免重复入队).
		existing, ferr := s.repo.FindIDForRunEmployee(ctx, s.pool,
			sess.RunID, sess.EmployeeID)
		if ferr == nil && existing != "" {
			return &SubmitResult{SubmissionID: existing}, nil
		}
		return nil, ErrSubmissionClosed
	case "closed", "expired":
		return nil, ErrSubmissionClosed
	case "", "active":
		// 接受 (默认 active)
	default:
		return nil, ErrSubmissionClosed
	}
	// deadline + grace 校验: 超期一律拒绝. auto_submit_reason 来自请求体 (客户端可
	// 任意伪造), 不得作为绕过条件 —— 合法的自动交卷发生在 deadline 附近, grace 窗口
	// (默认 30s) 已覆盖其提交延迟.
	if s.GracePeriodSeconds >= 0 && sess.DeadlineAt != nil {
		deadlineWithGrace := sess.DeadlineAt.Add(time.Duration(s.GracePeriodSeconds) * time.Second)
		if time.Now().UTC().After(deadlineWithGrace) {
			return nil, ErrSubmissionClosed
		}
	}
	// --- (a) run + dup + idempotent ---
	run, err := s.repo.FindRunByID(ctx, s.pool, sess.RunID)
	if err != nil {
		return nil, err
	}
	dup, err := s.repo.DuplicateExists(ctx, s.pool, sess.RunID, sess.EmployeeID)
	if err != nil {
		return nil, err
	}
	if dup {
		existing, ferr := s.repo.FindIDForRunEmployee(ctx, s.pool,
			sess.RunID, sess.EmployeeID)
		if ferr == nil && existing != "" {
			return &SubmitResult{SubmissionID: existing}, nil
		}
		return nil, ErrDuplicateSubmission
	}
	// --- (b) 事务外 CPU 工作: LoadSnapshot + objective grade ---
	// 快照不可用时直接失败 (不静默按 0 分定稿); 学生可重试, 管理员可从错误日志定位.
	now := time.Now().UTC()
	objScore, _, objDetailJSON, _, hasSubjective, err := s.computeObjective(ctx, run, in.AnswersMap)
	if err != nil {
		return nil, err
	}

	// --- (c) 短事务写: FOR UPDATE session + FOR UPDATE run + INSERT submission + INSERT job + UPDATE session submitted ---
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("submissions.Submit: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// (c1) FOR UPDATE session: 校验状态没被并发关闭. 标准模式.
	if err := lockSessionForUpdate(ctx, tx, sess.SessionTokenHash); err != nil {
		return nil, err
	}
	// (c2) FOR UPDATE run: 校验 run 仍 open.
	if err := lockRunForUpdate(ctx, tx, sess.RunID); err != nil {
		return nil, err
	}
	// (c3) 再次 dup check (并发已提交, 走幂等路径).
	dup2, err := s.repo.DuplicateExists(ctx, tx, sess.RunID, sess.EmployeeID)
	if err != nil {
		return nil, err
	}
	if dup2 {
		existing, ferr := s.repo.FindIDForRunEmployee(ctx, tx, sess.RunID, sess.EmployeeID)
		if ferr != nil {
			return nil, ferr
		}
		return &SubmitResult{SubmissionID: existing}, nil
	}
	// (c4) INSERT pending submission.
	subID, err := s.repo.createSubmissionTx(ctx, tx, submissionInsertInput{
		Name:             sess.Name,
		EmployeeID:       sess.EmployeeID,
		PaperID:          run.PaperName, // run.PaperID 即 paper slug
		RunID:            sess.RunID,
		Department:       sess.Department,
		Answers:          in.Answers,
		ObjectiveScore:   objScore,
		ObjectiveDetail:  objDetailJSON,
		ClientIP:         in.ClientIP,
		UserAgent:        in.UserAgent,
		AutoSubmitReason: in.AutoSubmitReason,
		StartedAt:        sess.StartedAt,
		SubmittedAt:      now,
		HasSubjective:    hasSubjective,
	})
	if err != nil {
		return nil, err
	}
	// (c5) INSERT grading_jobs (generation=1). 纯客观卷跳过 (short-circuit done).
	if hasSubjective {
		jobPayload := encodeJobSpec(run.PaperName, sess.RunID, 1)
		if err := s.jobs.EnqueueTx(ctx, tx, subID, jobPayload); err != nil {
			return nil, err
		}
	}
	// (c6) UPDATE session status=active -> submitted.
	if err := markSessionSubmitted(ctx, tx, sess.SessionTokenHash); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("submissions.Submit: commit: %w", err)
	}
	return &SubmitResult{SubmissionID: subID}, nil
}

// ErrSubmitDisabled 表示依赖未注入 (Service 零值时 Submit 返回此错). HTTP 映射 503.
var ErrSubmitDisabled = errors.New("SUBMIT_DISABLED")

// ErrSnapshotUnavailable 表示 run 快照缺失或校验失败, 无法客观判分.
// Submit 主路径遇到它直接失败 (不静默按 0 分定稿). HTTP 映射 500.
var ErrSnapshotUnavailable = errors.New("SNAPSHOT_UNAVAILABLE")

// computeObjective 在事务外加载快照并评分, 返回 PreparedSubmission 的客观分字段.
// 主观题占位 score=0; Worker (Task 7) 异步补完.
//
// 快照缺失/校验失败 -> ErrSnapshotUnavailable (由 Submit 拒绝提交, 不静默 0 分入库).
// 单题评分器出错仍按占位处理 (reason 写进 detail), 不阻塞整卷.
// computeObjective 返回 (objScore, objMax, objDetailJSON, questionTotal,
// hasSubjective, err). hasSubjective=true 表示快照含主观题 (需异步 worker).
func (s *Service) computeObjective(ctx context.Context, run *RunLite,
	answersMap map[string]any) (score, max float64, detailJSON []byte, qTotal int,
	hasSubjective bool, err error) {
	if run.SnapshotPath == "" {
		return 0, 0, nil, 0, false,
			fmt.Errorf("%w: run %s has no snapshot path", ErrSnapshotUnavailable, run.ID)
	}
	if answersMap == nil {
		answersMap = map[string]any{}
	}
	snap, err := s.snapshotLoad(run.SnapshotPath, run.SnapshotHash)
	if err != nil {
		return 0, 0, nil, 0, false,
			fmt.Errorf("%w: %v", ErrSnapshotUnavailable, err)
	}
	questions, _ := snap.Doc["questions"].([]any)
	details := make([]map[string]any, 0, len(questions))
	var totalScore, totalMax float64
	for _, qraw := range questions {
		question, ok := qraw.(map[string]any)
		if !ok {
			continue
		}
		qTotal++
		qid := objQID(question)
		ans, ansOK := answersMap[qid]
		if !ansOK {
			ans = nil
		}
		if !isObjectiveType(question) {
			// 主观题占位 score=0
			hasSubjective = true
			details = append(details, objPlaceholder(question, ans, objMaxScoreOf(question), "subjective_pending"))
			continue
		}
		d, gerr := s.grade(question, ans, s.MultipleChoicePartial)
		if gerr != nil {
			details = append(details, objPlaceholder(question, ans, objMaxScoreOf(question), gerr.Error()))
			continue
		}
		details = append(details, d)
		if v, ok := d["final_score"].(float64); ok {
			totalScore += v
		}
		if v, ok := d["max_score"].(float64); ok {
			totalMax += v
		}
	}
	encoded := jsonMarshalArray(details)
	// 不做 int 截断: 题库有 1.5 分制客观题, 分数按 float 原样落库 (列 double precision).
	return totalScore, totalMax, encoded, qTotal, hasSubjective, nil
}

// jsonMarshalArray 局部封装; 出错时返回 [] (保证 SUBMIT 主路径不卡评分失败).
func jsonMarshalArray(a []map[string]any) []byte {
	b, err := json.Marshal(a)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// ----- (c) 阶段 helper stubs (后续小段逐个填充) -----

// submissionInsertInput 是 CreateFullTx 写入所需的扁平输入 (本地私有类型).
type submissionInsertInput struct {
	Name             string
	EmployeeID       string
	PaperID          string
	RunID            string
	Department       *string
	Answers          []byte
	ObjectiveScore   float64
	ObjectiveDetail  []byte
	ClientIP         *string
	UserAgent        *string
	AutoSubmitReason *string
	StartedAt        time.Time
	SubmittedAt      time.Time
	// HasSubjective=true 时 pending 入队 + review_status='grading';
	// false (纯客观) 时 grading_status='done' + review_status='auto_scored', 不入队.
	HasSubjective bool
}

// lockSessionForUpdate 在 tx 内 SELECT ... FOR UPDATE exam_sessions 重新校验 status.
// status 必须 'active' (状态未变并发关闭). 否则返回 ErrSubmissionClosed.
func lockSessionForUpdate(ctx context.Context, tx pgx.Tx, tokenHash string) error {
	const sql = `SELECT status FROM exam_sessions
		WHERE session_token_hash = $1 FOR UPDATE`
	var st string
	if err := tx.QueryRow(ctx, sql, tokenHash).Scan(&st); err != nil {
		if isPgNoRows(err) {
			return fmt.Errorf("%w: session vanished in tx", ErrSubmissionClosed)
		}
		return fmt.Errorf("submissions.lockSessionForUpdate: %w", err)
	}
	if st != "active" {
		return ErrSubmissionClosed
	}
	return nil
}

// lockRunForUpdate 在 tx 内 SELECT ... FOR UPDATE exam_runs 重新校验 status.
// status='closed' 表示 run 已结束, 拒绝提交; 'open'/'closing' 均允许 (closing 仍可短时间补提).
func lockRunForUpdate(ctx context.Context, tx pgx.Tx, runID string) error {
	const sql = `SELECT status FROM exam_runs WHERE id = $1 FOR UPDATE`
	var st string
	if err := tx.QueryRow(ctx, sql, runID).Scan(&st); err != nil {
		if isPgNoRows(err) {
			return fmt.Errorf("%w: run %s vanished in tx", ErrSubmissionClosed, runID)
		}
		return fmt.Errorf("submissions.lockRunForUpdate: %w", err)
	}
	if st == "closed" {
		return ErrSubmissionClosed
	}
	return nil
}

// createSubmissionTx 在 tx 内 INSERT submissions 一行, 返回 id::text.
// 不写 IO / 不算分 (Service 主路径事务外已算), 仅 INSERT. 客观分 -> objective_score;
// objective 改写 -> grading_detail_json (Worker Task 7 完成时按快照顺序覆盖).
func (r *Repository) createSubmissionTx(ctx context.Context, tx pgx.Tx,
	in submissionInsertInput) (string, error) {
	if len(in.ObjectiveDetail) == 0 {
		in.ObjectiveDetail = []byte("[]")
	}
	// 纯客观卷 short-circuit: grading_status='done', review_status='auto_scored', 不入队 job.
	// 混合卷 (含主观): grading_status='pending', review_status='grading', 异步入队 generation=1.
	gradingStatus := "pending"
	reviewStatus := "grading"
	generation := 1
	if !in.HasSubjective {
		gradingStatus = "done"
		reviewStatus = "auto_scored"
		generation = 0
	}
	const sql = `INSERT INTO submissions
		(name, employee_id, paper_id, paper_name, run_id, department,
		 answers_json, objective_score, total_score, grading_detail_json,
		 grading_status, review_status, grading_generation,
		 submitted_at, started_at,
		 client_ip, user_agent, auto_submit_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING id::text`
	row := tx.QueryRow(ctx, sql,
		in.Name, in.EmployeeID, in.PaperID, nil, // paper_name 列冗余, NULL
		in.RunID, nullablePtrStr(in.Department),
		in.Answers, in.ObjectiveScore, in.ObjectiveScore, in.ObjectiveDetail,
		gradingStatus, reviewStatus, generation,
		in.SubmittedAt, nullableTime(in.StartedAt),
		nullablePtrStr(in.ClientIP), nullablePtrStr(in.UserAgent),
		nullablePtrStr(in.AutoSubmitReason),
	)
	var id string
	if err := row.Scan(&id); err != nil {
		return "", fmt.Errorf("submissions.createSubmissionTx: %w", err)
	}
	return id, nil
}

// encodeJobSpec 构造 jobs.Service.EnqueueTx 的 payload JSON (JobSpec).
func encodeJobSpec(paperID, runID string, generation int) []byte {
	b, _ := json.Marshal(map[string]any{
		"paper_id":   paperID,
		"run_id":     runID,
		"generation": generation,
	})
	return b
}

// markSessionSubmitted 在 tx 内 UPDATE exam_sessions status='submitted'.
// 同时刷 updated_at 与 (submissions_grading_jobs 同事务), 一致原子.
func markSessionSubmitted(ctx context.Context, tx pgx.Tx, tokenHash string) error {
	const sql = `UPDATE exam_sessions SET status = 'submitted', updated_at = $2
		WHERE session_token_hash = $1 AND status = 'active'`
	res, err := tx.Exec(ctx, sql, tokenHash, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("submissions.markSessionSubmitted: %w", err)
	}
	if res.RowsAffected() != 1 {
		return fmt.Errorf("%w: expected 1 row updated, got %d",
			ErrSubmissionClosed, res.RowsAffected())
	}
	return nil
}
