// Package submissions - repository.go
// PG 层实现: 实现 sessions.SubmissionStore 接口 (Task 6 新签名) + SubmissionRead
// (事务外只读) + 内部 createTx (事务内写). 所有方法对 tx / pool 自适应: 读方法
// 用 *pgxpool.Pool 自动事务, 写 tx 方法接受 pgx.Tx.
package submissions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yhwyxy/examSystem/internal/objective"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/sessions"
)

// 哨兵 error 与 sessions 包保持同义; HTTP 层映射 errors.Is.
var (
	ErrNoRows = errors.New("NO_ROWS")
)

// Repository 实现 SubmissionRead 与 sessions.SubmissionStore. 纯 SQL, 唯一状态是
// MultipleChoicePartial (config scoring.multiple_choice_partial, main 装配注入),
// 供 deprecated 事务内判分路径 (CreateFromSessionTx / finalize 自动收卷) 使用.
type Repository struct {
	MultipleChoicePartial bool
}

// NewRepository 返回零状态实例.
func NewRepository() *Repository { return &Repository{} }

// hashToken 复刻 sessions.hashToken (sha256 hex).
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func wrapNoRows(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoRows
	}
	return err
}

func isPgNoRows(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return false
	}
	return errors.Is(err, pgx.ErrNoRows)
}

// FindSessionByToken 用原始 token (包内 sha256) 在 pool 上查询 exam_sessions.
// 仅返回 status / id / run_id / employee_id / name / department / started_at/
// (事务外用, 不加锁).
func (r *Repository) FindSessionByToken(ctx context.Context, pool PoolExec, token string) (*Session, error) {
	const sql = `SELECT id, run_id, employee_id, name, department,
		session_token_hash, started_at, deadline_at, status
		FROM exam_sessions
		WHERE session_token_hash = $1`
	row := pool.QueryRow(ctx, sql, hashToken(token))
	var s Session
	if err := row.Scan(&s.ID, &s.RunID, &s.EmployeeID, &s.Name, &s.Department,
		&s.SessionTokenHash, &s.StartedAt, &s.DeadlineAt, &s.Status); err != nil {
		if isPgNoRows(err) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidSessionToken, "session not found")
		}
		return nil, fmt.Errorf("submissions.FindSessionByToken: %w", err)
	}
	return &s, nil
}

// FindRunByID 在 pool 上查 exam_runs 行 (含 snapshot_path / hash).
func (r *Repository) FindRunByID(ctx context.Context, pool PoolExec, runID string) (*RunLite, error) {
	const sql = `SELECT id, paper_id, snapshot_path, snapshot_hash
		FROM exam_runs
		WHERE id = $1`
	row := pool.QueryRow(ctx, sql, runID)
	var r2 RunLite
	var snapPath, snapHash *string
	if err := row.Scan(&r2.ID, &r2.PaperName, &snapPath, &snapHash); err != nil {
		if isPgNoRows(err) {
			return nil, fmt.Errorf("%w: run %s not found", ErrNoRows, runID)
		}
		return nil, fmt.Errorf("submissions.FindRunByID: %w", err)
	}
	if snapPath != nil {
		r2.SnapshotPath = *snapPath
	}
	if snapHash != nil {
		r2.SnapshotHash = *snapHash
	}
	return &r2, nil
}

// PoolExec 是事务外只读 / exec 的最小公共接口 (*pgxpool.Pool 与 pgx.Tx 都满足).
// 与 sessions.pgxExec 等价但显式 (该接口在 submissions 包内私有/未导出).
type PoolExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SubmissionRead 是事务外只读 + 事务内 (FOR UPDATE) 通用最小接口, 接收 PoolExec
// 兼容 pool/txt 两种模式: Submit 编排事务外用 pool (自动提交), sessions.SubmitManual
// 事务内用 tx (与 INSERT 同事务).
type SubmissionRead interface {
	FindSessionByToken(ctx context.Context, q PoolExec, token string) (*Session, error)
	FindRunByID(ctx context.Context, q PoolExec, runID string) (*RunLite, error)
	DuplicateExists(ctx context.Context, q PoolExec, runID, employeeID string) (bool, error)
	FindIDForRunEmployee(ctx context.Context, q PoolExec, runID, employeeID string) (string, error)
	GetByID(ctx context.Context, q PoolExec, submissionID string) (*Submission, error)
}

// DuplicateExists 查 (run_id, employee_id) 是否已有 submissions 行.
func (r *Repository) DuplicateExists(ctx context.Context, q PoolExec,
	runID, employeeID string) (bool, error) {
	const sql = `SELECT 1 FROM submissions WHERE run_id = $1 AND employee_id = $2 LIMIT 1`
	var dummy int
	err := q.QueryRow(ctx, sql, runID, employeeID).Scan(&dummy)
	if err == nil {
		return true, nil
	}
	if isPgNoRows(err) {
		return false, nil
	}
	return false, fmt.Errorf("submissions.DuplicateExists: %w", err)
}

// FindIDForRunEmployee 幂等取回 (run_id, employee_id) 已存在的 submission id (字符串).
func (r *Repository) FindIDForRunEmployee(ctx context.Context, q PoolExec,
	runID, employeeID string) (string, error) {
	const sql = `SELECT id::text FROM submissions
		WHERE run_id = $1 AND employee_id = $2
		ORDER BY submitted_at DESC LIMIT 1`
	var id string
	if err := q.QueryRow(ctx, sql, runID, employeeID).Scan(&id); err != nil {
		if isPgNoRows(err) {
			return "", fmt.Errorf("%w: no submission for (%s,%s)", ErrNoRows, runID, employeeID)
		}
		return "", fmt.Errorf("submissions.FindIDForRunEmployee: %w", err)
	}
	return id, nil
}

// GetByID 取单行; 不存在返回 ErrNoRows.字段精简到 StatusResult 所需.
func (r *Repository) GetByID(ctx context.Context, q PoolExec, submissionID string) (*Submission, error) {
	const sql = `SELECT id::text, grading_status, review_status,
		COALESCE(objective_score, 0)::float8
		FROM submissions WHERE id = $1::bigint`
	var s Submission
	if err := q.QueryRow(ctx, sql, submissionID).Scan(
		&s.ID, &s.GradingStatus, &s.ReviewStatus, &s.ObjectiveScore); err != nil {
		if isPgNoRows(err) {
			return nil, fmt.Errorf("%w: submission %s not found", ErrNoRows, submissionID)
		}
		return nil, fmt.Errorf("submissions.GetByID: %w", err)
	}
	return &s, nil
}

// CreateFromSessionTx 实现 sessions.SubmissionStore.CreateFromSessionTx.
// 注意: 在调用方事务内执行, 不开 / 不提交 tx.
//
// 事务内的 IO (papers.LoadSnapshot 读磁盘) 在 deprecated sessions.SubmitManual
// 路径上发生, 违反 plan Step 2 "事务外只读 + CPU"; 但 Submit 编排主路径会
// 在事务外算好客观分 (见 Service.Submit). 本函数承担兼容旧接口的最小保证:
// SnapshotPath / SnapshotHash / AnswersMap 任一为空则跳过即时判分, 写零分.
//
// 入参 sessions.SubmissionInput 由 sessions 包定义 (submissions 依赖 sessions 单向).
// 返回的 submission id 是字符串 (submissions.id bigserial -> ::text).
func (r *Repository) CreateFromSessionTx(ctx context.Context, tx pgx.Tx,
	in sessions.SubmissionInput) (string, bool, error) {
	// 客观题即时判分 (在事务内; 主 HTTP 路径传 Prepared 已避免此处 IO).
	var (
		objScore      float64
		objDetailJSON []byte
		hasSubjective = true
	)
	if in.SnapshotPath != "" && in.AnswersMap != nil {
		objScore, _, objDetailJSON, _, hasSubjective =
			gradeObjectiveInTx(ctx, in, r.MultipleChoicePartial)
	}
	if len(objDetailJSON) == 0 {
		objDetailJSON = []byte("[]")
	}
	gradingStatus := "pending"
	reviewStatus := "grading"
	generation := 1
	if !hasSubjective {
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
	now := time.Now().UTC()
	row := tx.QueryRow(ctx, sql,
		in.Name, in.EmployeeID, in.PaperID, nullableStr(in.PaperName),
		in.RunID, nullablePtrStr(in.Department),
		in.Answers, objScore, objScore, objDetailJSON,
		gradingStatus, reviewStatus, generation,
		now, nullableTime(in.StartedAt),
		nullablePtrStr(in.ClientIP), nullablePtrStr(in.UserAgent),
		nullablePtrStr(in.AutoSubmitReason),
	)
	var id string
	if err := row.Scan(&id); err != nil {
		return "", false, fmt.Errorf("submissions.CreateFromSessionTx insert: %w", err)
	}
	return id, hasSubjective, nil
}

// gradeObjectiveInTx 在事务内加载快照 + 客观题评分 (兼容 deprecated sessions.SubmitManual 路径).
// 主 HTTP 路径 Service.Submit 在事务外算好 PreparedObjective 后写入, 不进此函数.
//
// 实现:
//   - 调 papers.LoadSnapshot(path, shaHex) 加载已固化快照 (磁盘 IO).
//   - 遍历 Doc["questions"], 对 single_choice/multiple_choice/true_false 调
//     objective.Grade; 主观题占位 score=0, 由 Worker(Task 7) 回填.
//   - 累计 objective_score / objective_max_score; 序列化单题 detail array.
//
// 任一步失败返回零分 (与 Python insert_submission_pending 兼容: 评分完后再异
// 步处理). 不返回 error 以保证 INSERT 主路径不被卡.
// partial: 多选部分给分开关 (config scoring.multiple_choice_partial).
func gradeObjectiveInTx(ctx context.Context, in sessions.SubmissionInput,
	partial bool) (float64, float64, []byte, int, bool) {
	if in.SnapshotPath == "" || in.AnswersMap == nil {
		return 0, 0, []byte("[]"), 0, true
	}
	snap, err := papers.LoadSnapshot(in.SnapshotPath, in.SnapshotHash)
	if err != nil {
		return 0, 0, []byte("[]"), 0, true
	}
	questions, _ := snap.Doc["questions"].([]any)
	details := make([]map[string]any, 0, len(questions))
	var objScore, objMax float64
	var hasSubjective bool
	for _, qraw := range questions {
		question, ok := qraw.(map[string]any)
		if !ok {
			continue
		}
		qid := objQID(question)
		ans, ansOK := in.AnswersMap[qid]
		if !ansOK {
			ans = nil
		}
		isObj := isObjectiveType(question)
		maxScore := objMaxScoreOf(question)
		switch {
		case isObj:
			d, gerr := objective.Grade(question, ans, partial)
			if gerr != nil {
				details = append(details, objPlaceholder(question, ans, maxScore, gerr.Error()))
				continue
			}
			details = append(details, d)
			if v, ok := d["final_score"].(float64); ok {
				objScore += v
			}
			if v, ok := d["max_score"].(float64); ok {
				objMax += v
			}
		default:
			// 主观题: 占位 score=0, 由 Worker (Task 7) 异步补完.
			hasSubjective = true
			details = append(details, objPlaceholder(question, ans, maxScore, "subjective_pending"))
		}
	}
	objJSON, merr := json.Marshal(details)
	if merr != nil {
		return 0, 0, []byte("[]"), 0, true
	}
	// float 原样返回, 不截断 (1.5 分制客观题).
	return objScore, objMax, objJSON, len(questions), hasSubjective
}

// objQID 取 question["id"]; 兼容 int / string 形态, 统一回 string.
func objQID(q map[string]any) string {
	id, ok := q["id"]
	if !ok || id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", id)
	}
}

// isObjectiveType 区分客观 / 主观题; 与 objective.Grade 支持类型对齐.
func isObjectiveType(q map[string]any) bool {
	t, _ := q["type"].(string)
	switch t {
	case "single_choice", "multiple_choice", "true_false":
		return true
	}
	return false
}

// objMaxScoreOf 取 question["score"] 浮点值.
func objMaxScoreOf(q map[string]any) float64 {
	switch v := q["score"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			return f
		}
	}
	return 0
}

// objPlaceholder 是主观题或异常 fallback 的占位项 (score=0).
func objPlaceholder(q map[string]any, ans any, maxScore float64, reason string) map[string]any {
	return map[string]any{
		"question_id":       q["id"],
		"type":              q["type"],
		"student_answer":    ans,
		"reference_answer":  q["answer"],
		"score":             0.0,
		"machine_score":     0.0,
		"final_score":       0.0,
		"max_score":         maxScore,
		"is_correct":        false,
		"grading_method":    "subjective_pending",
		"confidence":        0.0,
		"reason":            reason,
		"review_status":     "unanswered",
		"manually_reviewed": false,
		"detail":            map[string]any{},
	}
}

// nullableStr 把空字符串转为 nil -> 落库 NULL.
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullablePtrStr 把 *string nil 转 nil; 否则原值.
func nullablePtrStr(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// nullableTime 把 zero time 转 nil 落 NULL.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
