package finalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
)

// 错误语义别名 (HTTP 层映射).
var (
	ErrNotConfigured = errors.New("finalize: NOT_CONFIGURED")
	ErrNotClosing    = errors.New("finalize: NOT_CLOSING")
)

// activeRow 是 FinalizeRun 内从 exam_sessions JOIN exam_runs 拉取的活动 session 行
// (含 run 维度的 paper_id / snapshot_path / snapshot_hash; 给 CreateFromSessionTx 用).
type activeRow struct {
	ID               string
	RunID            string
	EmployeeID       string
	Name             string
	Department       *string
	SessionTokenHash string
	StartedAt        time.Time
	DeadlineAt       *time.Time
	Status           string
	DraftJSON        []byte
	DraftRevision    int
	DraftSavedAt     *time.Time
	// run 维度字段 (JOIN 取).
	PaperID      string
	SnapshotPath string
	SnapshotHash string
}

// RunnerAlloc 把 finalize 包与 sessions.SubmissionStore 解耦: finalize 调
// CreateFromSessionTx 时不再直接 import submissions 包, 而用 sessions.SubmissionStore
// (Task 6 已扩展为传 pgx.Tx) 抽象.
type RunnerAlloc interface {
	CreateFromSessionTx(ctx context.Context, tx pgx.Tx, in sessions.SubmissionInput) (submissionID string, needsGrading bool, err error)
}

// Service: 收卷编排 (ScanDue 轮询 + FinalizeRun 原子事务).
type Service struct {
	pool             *pgxpool.Pool
	runsRepo         *runs.Repository
	sessionsRepo     *sessions.Repository
	alloc            RunnerAlloc // Task 6 SubmissionStore 实例 (注入)
	GraceDurationSec int         // admin_closed 自动提交时给 session 草稿现值不变, 仅为提醒
}

// NewService 构造 finalize Service.
//   - pool        PG 连接池
//   - runsRepo    runs 层 (FindDueClosing + FinalizeRun)
//   - sessionsRepo sessions 层 (FindActiveByRunTxForAutoSubmit)
//   - alloc       Task 6 SubmissionStore 实例 (注入)
//   - graceDurationSec 自动提交时给 admin 的 grace 提示 (实质未使用)
func NewService(pool *pgxpool.Pool, runsRepo *runs.Repository,
	sessionsRepo *sessions.Repository, alloc RunnerAlloc,
	graceDurationSec int) *Service {
	return &Service{
		pool:             pool,
		runsRepo:         runsRepo,
		sessionsRepo:     sessionsRepo,
		alloc:            alloc,
		GraceDurationSec: graceDurationSec,
	}
}

// finalizeRawReason 是 admin_closed 时复用的 reason 字面值.
const finalizeRawReason = "admin_closed"

const individualTimeoutReason = "individual_timeout"

// FinalizeRun 在单事务内: lock run (closing) -> 对剩余 active sessions 调
// Task 6 CreateFromSessionTx (auto_submit_reason=admin_closed) -> run status
// closing->closed -> commit. 任一错全滚, run 保持 closing 由下次 ScanDue 重试.
//
//   - runID: exam_runs.id
//   - variant: "grace" -> finalize=true 但不强制收卷完成 (留 closing); 其他不变
//   - returns: (nClosedSessions int, runClosed bool, err error)
func (s *Service) FinalizeRun(ctx context.Context, runID string) (int, bool, error) {
	if s == nil || s.pool == nil || s.alloc == nil {
		return 0, false, ErrNotConfigured
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("finalize.FinalizeRun begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	// 1) lock run with FOR UPDATE; 校验 status='closing'.
	var status string
	err = tx.QueryRow(ctx, `
SELECT status FROM exam_runs WHERE id = $1 FOR UPDATE`,
		runID).Scan(&status)
	if err != nil {
		err = fmt.Errorf("finalize.FinalizeRun lock run: %w", err)
		return 0, false, err
	}
	if status != string(runs.StatusClosing) {
		err = fmt.Errorf("%w: actual=%q want %q", ErrNotClosing, status,
			runs.StatusClosing)
		return 0, false, err
	}
	// 找 pending active sessions 的所有 (id + 草稿 + employee) 字段;
	// 注意: 必须在该事务内查, 防并发 submit 在事务外插队.
	activeRows, qerr := tx.Query(ctx, `
SELECT s.id, s.run_id, s.employee_id, s.name, s.department, s.session_token_hash,
       s.started_at, s.deadline_at, s.status, s.draft_json, s.draft_revision, s.draft_saved_at,
       r.paper_id, r.snapshot_path, r.snapshot_hash
FROM exam_sessions s
JOIN exam_runs r ON r.id = s.run_id
WHERE s.run_id = $1 AND s.status = 'active'
ORDER BY s.id
FOR UPDATE OF s`,
		runID)
	if qerr != nil {
		err = fmt.Errorf("finalize.FinalizeRun query active sessions: %w", qerr)
		return 0, false, qerr
	}
	nClosed := 0
	// 1) collect 全部 active rows (含 FOR UPDATE 锁); 之后 close rows 释放 conn,
	// 让 CreateFromSessionTx 在同 tx 上发出 INSERT 时不再 "conn busy".
	var collected []activeRow
	for activeRows.Next() {
		var sess activeRow
		if cerr := activeRows.Scan(&sess.ID, &sess.RunID, &sess.EmployeeID,
			&sess.Name, &sess.Department, &sess.SessionTokenHash,
			&sess.StartedAt, &sess.DeadlineAt, &sess.Status,
			&sess.DraftJSON, &sess.DraftRevision, &sess.DraftSavedAt,
			&sess.PaperID, &sess.SnapshotPath, &sess.SnapshotHash); cerr != nil {
			err = fmt.Errorf("finalize.FinalizeRun scan active row: %w", cerr)
			return 0, false, cerr
		}
		collected = append(collected, sess)
	}
	activeRows.Close()
	if cerr := activeRows.Err(); cerr != nil {
		err = fmt.Errorf("finalize.FinalizeRun rows.Err: %w", cerr)
		return 0, false, cerr
	}
	// 2) 逐行对每 session 调 Task 6 CreateFromSessionTx (auto_submit_reason=admin_closed).
	closedIDs := make([]string, 0, len(collected))
	for _, sess := range collected {
		input := buildAutoSubmitted(sess)
		subID, needsGrading, cerr := s.alloc.CreateFromSessionTx(ctx, tx, input)
		if cerr != nil {
			err = fmt.Errorf("finalize.FinalizeRun auto-submit session %s: %w", sess.ID, cerr)
			return 0, false, cerr
		}
		if needsGrading {
			submissionID, parseErr := strconv.ParseInt(subID, 10, 64)
			if parseErr != nil {
				err = fmt.Errorf("finalize.FinalizeRun invalid submission id %q: %w", subID, parseErr)
				return 0, false, err
			}
			if cerr := jobs.NewRepository().EnqueueTx(ctx, tx, jobs.JobSpec{
				SubmissionID: submissionID,
				PaperID:      sess.PaperID,
				RunID:        sess.RunID,
				Generation:   1,
			}); cerr != nil {
				err = fmt.Errorf("finalize.FinalizeRun enqueue session %s: %w", sess.ID, cerr)
				return 0, false, cerr
			}
		}
		closedIDs = append(closedIDs, sess.ID)
		nClosed++
	}
	// 2b) 自动收卷的 session 置 submitted (同事务): 否则学生端状态轮询永远等不到
	// session_status=submitted / submission_id, 收卷后卡在"正在自动收卷"页.
	if len(closedIDs) > 0 {
		if _, cerr := tx.Exec(ctx, `
UPDATE exam_sessions SET status = 'submitted', updated_at = $2
WHERE id = ANY($1) AND status = 'active'`,
			closedIDs, time.Now().UTC()); cerr != nil {
			err = fmt.Errorf("finalize.FinalizeRun mark sessions submitted: %w", cerr)
			return 0, false, cerr
		}
	}
	// 2) update run status=closing -> closed. 注意: exam_runs 没 updated_at, 用
	// closed_at = now 替代 (与 Python close_run parity).
	if _, cerr := tx.Exec(ctx, `
UPDATE exam_runs SET status = $2, closed_at = $3
WHERE id = $1 AND status = $4`,
		runID, string(runs.StatusClosed), time.Now().UTC(),
		string(runs.StatusClosing)); cerr != nil {
		err = fmt.Errorf("finalize.FinalizeRun update run closed: %w", cerr)
		return 0, false, cerr
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		err = fmt.Errorf("finalize.FinalizeRun commit: %w", cerr)
		return 0, false, cerr
	}
	return nClosed, true, nil
}

// buildAutoSubmitted 构造 SubmissionInput 用于 finalize 自动提交.
// 不要求 looks ups re-fetch run; 使用 sess 字段 + 草稿 answers 作为 answers (从 draft_json 解析 student_answers).
// 注意 run-level snapshot_path/hash 由 Task 6 CreateFromSessionTx 经 input 字段传入.
func buildAutoSubmitted(sess activeRow) sessions.SubmissionInput {
	reason := finalizeRawReason
	rawDraft := sess.DraftJSON
	if len(rawDraft) == 0 {
		rawDraft = []byte("{}")
	}
	return sessions.SubmissionInput{
		Name:             sess.Name,
		EmployeeID:       sess.EmployeeID,
		PaperID:          sess.PaperID,
		PaperName:        sess.PaperID, // paper_name = paper slug (Task 6 注释)
		RunID:            sess.RunID,
		Department:       sess.Department,
		Answers:          rawDraft,
		AnswersMap:       parseAnswersMapSafe(rawDraft),
		StartedAt:        sess.StartedAt,
		SubmittedAt:      time.Now().UTC(),
		AutoSubmitReason: &reason,
		SnapshotPath:     sess.SnapshotPath,
		SnapshotHash:     sess.SnapshotHash,
	}
}

// parseAnswersMapSafe 把 draft_json 解码为 map[string]any 给 Task 6 客观题即时判分用.
// 失败时返回空 map (避免 finalize 路径因 JSON 解析失败 panic).
func parseAnswersMapSafe(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := jsonDecodeObject(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// jsonDecodeObject 包装 json.Unmarshal 避免外部 import json 散乱.
func jsonDecodeObject(raw []byte, dst *map[string]any) error {
	return json.Unmarshal(raw, dst)
}

// ScanDue 是 Plan Step 4 的轮询入口: 在 PG 池上事务内调用 FindDueClosingTx, 对每条
// 收卷 id 调 FinalizeRun; 返回 (nRunsClosed int, nSessionsAutoSubmitted int, err).
//
// 调用方 (cmd/exam-server) 每秒 ticker 调一次 ScanDue. 任一 run finalize 失败记
// log + 继续下一 run 不中断整体循环 (run 仍 closing 等下次 ScanDue 自然重试).
func (s *Service) ScanDue(ctx context.Context) (int, int, error) {
	if s == nil || s.pool == nil {
		return 0, 0, ErrNotConfigured
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("finalize.ScanDue begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids, err := s.runsRepo.FindDueClosingTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	if len(ids) == 0 {
		return 0, 0, nil
	}
	nRuns, nSess := 0, 0
	if cerr := tx.Commit(ctx); cerr != nil {
		return 0, 0, fmt.Errorf("finalize.ScanDue commit: %w", cerr)
	}
	// 现在 ids 持久可见; 各 run 在新 tx 内逐个 finalize.
	for _, id := range ids {
		nS, closed, ferr := s.FinalizeRun(ctx, id)
		if ferr != nil {
			// 不中断循环; 任一 run finalize 失败兜下次 ScanDue.
			continue
		}
		if closed {
			nRuns++
			nSess += nS
		}
	}
	return nRuns, nSess, nil
}

// ScanExpiredSessions 自动提交个人截止时间已过且仍处于 active 的会话。
// 为保留客户端在提交宽限期内的补提机会，截止时间按 deadline_at + grace 计算。
// 每个 session 使用独立事务，session 行锁与提交入口串行化，确保并发扫描或手动提交
// 最多创建一份 submission。每次最多检查 100 个候选，剩余记录由下一轮轮询处理。
func (s *Service) ScanExpiredSessions(ctx context.Context) (int, error) {
	if s == nil || s.pool == nil || s.alloc == nil {
		return 0, ErrNotConfigured
	}
	grace := s.GraceDurationSec
	if grace < 0 {
		grace = 0
	}
	rows, err := s.pool.Query(ctx, `
SELECT s.id
FROM exam_sessions s
JOIN exam_runs r ON r.id = s.run_id
WHERE s.status = 'active'
  AND s.deadline_at <= now() - ($1::double precision * interval '1 second')
  AND r.status IN ('open', 'closing')
ORDER BY s.deadline_at, s.id
LIMIT 100`, grace)
	if err != nil {
		return 0, fmt.Errorf("finalize.ScanExpiredSessions candidates: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("finalize.ScanExpiredSessions candidate: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("finalize.ScanExpiredSessions rows: %w", err)
	}

	count := 0
	var firstErr error
	for _, id := range ids {
		done, err := s.autoSubmitExpiredSession(ctx, id, grace)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if done {
			count++
		}
	}
	return count, firstErr
}

func (s *Service) autoSubmitExpiredSession(ctx context.Context, sessionID string, grace int) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("finalize.autoSubmitExpiredSession begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sess activeRow
	err = tx.QueryRow(ctx, `
SELECT s.id, s.run_id, s.employee_id, s.name, s.department, s.session_token_hash,
       s.started_at, s.deadline_at, s.status, s.draft_json, s.draft_revision, s.draft_saved_at,
       r.paper_id, r.snapshot_path, r.snapshot_hash
FROM exam_sessions s
JOIN exam_runs r ON r.id = s.run_id
WHERE s.id = $1 AND s.status = 'active'
  AND s.deadline_at <= now() - ($2::double precision * interval '1 second')
  AND r.status IN ('open', 'closing')
FOR UPDATE OF s SKIP LOCKED`, sessionID, grace).Scan(
		&sess.ID, &sess.RunID, &sess.EmployeeID, &sess.Name, &sess.Department,
		&sess.SessionTokenHash, &sess.StartedAt, &sess.DeadlineAt, &sess.Status,
		&sess.DraftJSON, &sess.DraftRevision, &sess.DraftSavedAt,
		&sess.PaperID, &sess.SnapshotPath, &sess.SnapshotHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // 手动提交、整场收卷或另一扫描器已处理。
	}
	if err != nil {
		return false, fmt.Errorf("finalize.autoSubmitExpiredSession lock %s: %w", sessionID, err)
	}

	input := buildAutoSubmitted(sess)
	input.AutoSubmitReason = stringPointer(individualTimeoutReason)
	subID, needsGrading, err := s.alloc.CreateFromSessionTx(ctx, tx, input)
	if err != nil {
		return false, fmt.Errorf("finalize.autoSubmitExpiredSession submit %s: %w", sessionID, err)
	}
	if needsGrading {
		submissionID, err := strconv.ParseInt(subID, 10, 64)
		if err != nil {
			return false, fmt.Errorf("finalize.autoSubmitExpiredSession invalid submission id %q: %w", subID, err)
		}
		if err := jobs.NewRepository().EnqueueTx(ctx, tx, jobs.JobSpec{
			SubmissionID: submissionID,
			PaperID:      sess.PaperID,
			RunID:        sess.RunID,
			Generation:   1,
		}); err != nil {
			return false, fmt.Errorf("finalize.autoSubmitExpiredSession enqueue %s: %w", sessionID, err)
		}
	}
	if tag, err := tx.Exec(ctx, `
UPDATE exam_sessions SET status = 'submitted', updated_at = now()
WHERE id = $1 AND status = 'active'`, sessionID); err != nil {
		return false, fmt.Errorf("finalize.autoSubmitExpiredSession mark submitted %s: %w", sessionID, err)
	} else if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("finalize.autoSubmitExpiredSession mark submitted %s: session changed while locked", sessionID)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("finalize.autoSubmitExpiredSession commit %s: %w", sessionID, err)
	}
	return true, nil
}

func stringPointer(value string) *string { return &value }
