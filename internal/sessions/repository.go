// Package sessions 实现 exam_sessions 的仓储层与会话草稿 CAS/恢复/状态查询,
// 对齐 Python backend/exam_run_service.start_exam/save_draft/get_session_status 行为.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 错误约定: 匹配 plan HTTP 错误 code (大写 snake).
var (
	ErrSessionNotFound     = errors.New("sessions: SESSION_NOT_FOUND")
	ErrInvalidSessionToken = errors.New("sessions: INVALID_SESSION_TOKEN")
	ErrSessionSubmitted    = errors.New("sessions: SESSION_SUBMITTED")
	ErrStaleDraftRevision  = errors.New("sessions: STALE_DRAFT_REVISION")
	ErrDraftSaveFailed     = errors.New("sessions: DRAFT_SAVE_FAILED")
	ErrRunNotFound         = errors.New("sessions: RUN_NOT_FOUND")
	ErrRunClosed           = errors.New("sessions: RUN_CLOSED")
	ErrRunClosing          = errors.New("sessions: RUN_CLOSING")
	ErrDuplicateSubmission = errors.New("sessions: DUPLICATE_SUBMISSION")
	ErrActiveExists        = errors.New("sessions: SESSION_ACTIVE_EXISTS") // 并发竞争 (run+employee 已有 active)
	ErrNoRow               = errors.New("sessions: no row")
)

// SessionStatus 枚举.
type SessionStatus string

const (
	StatusActive    SessionStatus = "active"
	StatusSubmitted SessionStatus = "submitted"
)

// RunStatus 仅用于在 Service 内做状态机向下传 (避免与 runs 包直接耦合).
type RunStatus string

const (
	RunOpen    RunStatus = "open"
	RunClosing RunStatus = "closing"
	RunClosed  RunStatus = "closed"
)

// Session 映射 exam_sessions 单行, 时间用 *time.Time 表示, nil 即 NULL.
type Session struct {
	ID               string
	RunID            string
	EmployeeID       string
	Name             string
	Department       *string
	SessionTokenHash string
	StartedAt        time.Time
	DeadlineAt       time.Time
	DraftJSON        []byte // raw jsonb bytes; 空对象为 []byte("{}")
	DraftRevision    int
	DraftSavedAt     *time.Time
	Status           SessionStatus
	ClientIP         *string
	UserAgent        *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SessionCreateInput 用于新建一行. tokenHash 为 sha256(token) hex, runID 必须存在.
type SessionCreateInput struct {
	SessionID        string
	RunID            string
	EmployeeID       string
	Name             string
	Department       *string
	SessionTokenHash string
	StartedAt        time.Time
	DeadlineAt       time.Time
	ClientIP         *string
	UserAgent        *string
}

// DraftUpdateResult CAS 更新后返回的新状态 (供 Service 区分 throttle / saved).
type DraftUpdateResult struct {
	RowsAffected int64
	Revision     int
	DraftSavedAt *time.Time
	Status       SessionStatus
}

// Repository 只读/写 exam_sessions; 不与 runs/papers/submissions 耦合.
type Repository struct{}

// NewRepository 返回零值仓储.
func NewRepository() *Repository { return &Repository{} }

// scanSession 统一扫描一行的 INT8/int8/text/timestamptz/jsonb/integer/text 集合.
func scanSession(r pgx.Row) (*Session, error) {
	s := &Session{}
	var statusStr string
	var draftSavedAt *time.Time
	var clientIP, userAgent *string
	if err := r.Scan(
		&s.ID, &s.RunID, &s.EmployeeID, &s.Name, &s.Department,
		&s.SessionTokenHash, &s.StartedAt, &s.DeadlineAt,
		&s.DraftJSON, &s.DraftRevision, &draftSavedAt, &statusStr,
		&clientIP, &userAgent, &s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	s.DraftSavedAt = draftSavedAt
	s.ClientIP = clientIP
	s.UserAgent = userAgent
	s.Status = SessionStatus(statusStr)
	return s, nil
}

// sessionCols 为统一的列顺序字符串, 与 scanSession 顺序严格一致.
const sessionCols = `id, run_id, employee_id, name, department, session_token_hash,
	started_at, deadline_at, draft_json, draft_revision, draft_saved_at, status,
	client_ip, user_agent, created_at, updated_at`

// pgxExec 接受 *pgxpool.Pool 或 pgx.Tx 的最小公共接口, 让 Repository 既能走
// pool-only 查询也能在 Service 事务里续作 (Exec + QueryRow 都需要).
type pgxExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// FindByID 按 id 查单行; 找不到返回 ErrSessionNotFound (errors.Is).
func (r *Repository) FindByID(ctx context.Context, q pgxExec, sessionID string) (*Session, error) {
	row := q.QueryRow(ctx,
		"SELECT "+sessionCols+" FROM exam_sessions WHERE id = $1", sessionID)
	s, err := scanSession(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	return s, nil
}

// FindByRunEmployee 在指定 run 下按 employee_id 查一行 (Python get_session_for_run_employee).
// 找不到返回 ErrSessionNotFound.
func (r *Repository) FindByRunEmployee(ctx context.Context, q pgxExec,
	runID, employeeID string) (*Session, error) {
	row := q.QueryRow(ctx,
		"SELECT "+sessionCols+
			" FROM exam_sessions WHERE run_id = $1 AND employee_id = $2 ORDER BY created_at DESC LIMIT 1",
		runID, employeeID)
	s, err := scanSession(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	return s, nil
}

// isNoRows true 表示 pgx.ErrNoRows.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// isUniqueViolation 走 errors.As 解包 pgconn.PgError 检 SQLSTATE 23505.
// 与 runs 包 isUniqueViolation 同义, 复制以防循环 import; 两包 DB 表不同, 各自维护.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// InsertIfAbsent 写入一行新 session; 若 (run_id, employee_id, status='active')
// 已存在 (唯一索引冲突) 返回 ErrActiveExists, 让 service 区分"已开始" vs "系统错误".
// 与 Python backend/exam_run_service.create_session 对应, 但不在 repo 内查 run —
// service 先 runs.FindByToken 获取 run, 再调此 Insert.
//
// tg: 接受 *pgxpool.Pool (auto-tx) 或 pgx.Tx (service 续作); 用 Exec + QueryRow 模式.
// 共用 pgxExec (Exec + QueryRow); pgxWriter 为同名别名保持函数签名可读.
type pgxWriter = pgxExec

func (r *Repository) InsertIfAbsent(ctx context.Context, q pgxWriter, in SessionCreateInput) error {
	_, err := q.Exec(ctx, `
INSERT INTO exam_sessions
(id, run_id, employee_id, name, department, session_token_hash,
 started_at, deadline_at, draft_json, draft_revision, draft_saved_at, status,
 client_ip, user_agent, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, 0, NULL, 'active', $10, $11, NOW(), NOW())`,
		in.SessionID, in.RunID, in.EmployeeID, in.Name, in.Department,
		in.SessionTokenHash, in.StartedAt, in.DeadlineAt, "{}",
		in.ClientIP, in.UserAgent)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrActiveExists
		}
		return fmt.Errorf("sessions.InsertIfAbsent: %w", err)
	}
	return nil
}

// UpdateDraftCAS 用乐观锁更新 draft_json: 仅在 status='active' 且 draft_revision=$old
// 时把 draft_revision+1, draft_saved_at=NOW, 并写新 draft_json; RETURNING 新行.
// 行为对齐 Python save_draft:
//   - 行不存在 (session_id 错) -> ErrSessionNotFound
//   - 行 status='submitted' -> ErrSessionSubmitted (不允许再存草稿)
//   - draft_revision != old + status='active' -> ErrStaleDraftRevision (CAS 失败)
//   - 否则成功, 返回 rows_affected=1, revision=new, draft_saved_at, status='active'
//
// 用一条 SQL 的 CASE 完成判定, 避免 SELECT-then-UPDATE 的竞态:
//
//	UPDATE exam_sessions SET
//	    draft_json = $1::jsonb,
//	    draft_revision = draft_revision + 1,
//	    draft_saved_at = NOW(),
//	    updated_at = NOW()
//	WHERE id = $2 AND status = 'active' AND draft_revision = $3
//	RETURNING draft_revision, draft_saved_at, status;
//
// RETURNING 受影响时 rows=1; 若 0 行, 再用一条 SELECT 判定 reason.
func (r *Repository) UpdateDraftCAS(ctx context.Context, q pgxWriter,
	sessionID string, oldRevision int, draftJSON []byte) (*DraftUpdateResult, error) {
	res := &DraftUpdateResult{}
	row := q.QueryRow(ctx, `
UPDATE exam_sessions
SET draft_json = $1::jsonb,
    draft_revision = draft_revision + 1,
    draft_saved_at = NOW(),
    updated_at = NOW()
WHERE id = $2 AND status = 'active' AND draft_revision = $3
RETURNING draft_revision, draft_saved_at, status`, draftJSON, sessionID, oldRevision)
	var statusStr string
	err := row.Scan(&res.Revision, &res.DraftSavedAt, &statusStr)
	if err == nil {
		res.RowsAffected = 1
		res.Status = SessionStatus(statusStr)
		return res, nil
	}
	if isNoRows(err) {
		// 0 行, 判 reason: 找这一行看 status / 是否存在
		row2 := q.QueryRow(ctx,
			"SELECT status FROM exam_sessions WHERE id = $1", sessionID)
		var st2 string
		errScan := row2.Scan(&st2)
		if isNoRows(errScan) {
			return nil, ErrSessionNotFound
		}
		if errScan != nil {
			return nil, fmt.Errorf("sessions.UpdateDraftCAS probe: %w", errScan)
		}
		if SessionStatus(st2) == StatusSubmitted {
			return nil, ErrSessionSubmitted
		}
		return nil, ErrStaleDraftRevision
	}
	return nil, fmt.Errorf("sessions.UpdateDraftCAS: %w", err)
}

// MarkSubmitted 把 session 由 active → submitted; 仅在 status='active' 时生效.
// 调用方应在事务内依次: EnqueueTx (submissions) + MarkSubmitted + (grades insert).
// 行不存在 ErrSessionNotFound; 已 submitted 当作幂等成功 (RowsAffected=0, Status=submitted),
// 让上游 finalize 流程对中断 resume 友好.
func (r *Repository) MarkSubmitted(ctx context.Context, q pgxWriter, sessionID string) (rowsAffected int64, err error) {
	ct, err := q.Exec(ctx, `
UPDATE exam_sessions SET status='submitted', updated_at=NOW()
WHERE id=$1 AND status='active'`, sessionID)
	if err != nil {
		return 0, fmt.Errorf("sessions.MarkSubmitted: %w", err)
	}
	rowsAffected = ct.RowsAffected()
	if rowsAffected == 0 {
		// 检查是否真的存在 (区分 not-found vs 已 submitted)
		var st string
		errScan := q.QueryRow(ctx, "SELECT status FROM exam_sessions WHERE id=$1", sessionID).Scan(&st)
		if isNoRows(errScan) {
			return 0, ErrSessionNotFound
		}
		if errScan != nil {
			return 0, fmt.Errorf("sessions.MarkSubmitted probe: %w", errScan)
		}
		// 已 submitted 幂等: 返回 0, nil (无错)
		if SessionStatus(st) == StatusSubmitted {
			return 0, nil
		}
		// 其它状态 (理论不应发生) 视为 internal err
		return 0, fmt.Errorf("sessions.MarkSubmitted: unexpected status %q", st)
	}
	return rowsAffected, nil
}
