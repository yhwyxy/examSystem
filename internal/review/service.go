// Package review 提供线上复核 (Apply) 与重评 (Regrade) 服务.
//
// 设计原则 (按并发优先原则, 偏离 Python 蓝本):
//   - Regrade 走统一代次切换 (generation+1 + supersede 旧 job), 异步交回 scoring_worker
//     立即返 {success:true, status:'grading'}, 不复刻 Python in-place 同步重算
//   - UPDATE 强加 WHERE grading_generation = G+1 守护, 防 regrade 未决时学生又答卷再 regrade
//   - review 包自带 SQL 直查 submissions (decision 2=B), 不依赖 submissions 包, 隔离热区改动
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/yhwyxy/examSystem/internal/jobs"
)

// Pool 是 *pgxpool.Pool 的最小事务接口 (review.Service 自管事务).
type Pool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Service 封装 review.Apply + review.Regrade 跨表事务.
type Service struct {
	pool     Pool
	jobsRepo *jobs.Repository
}

// NewService 构造 review.Service. jobsRepo 可为 nil (单测可省).
func NewService(pool Pool, jr *jobs.Repository) *Service {
	return &Service{pool: pool, jobsRepo: jr}
}

// SubmissionSnap 是 review 视角的 submission 快照 (不引 submissions 包, 见决策 2=B).
type SubmissionSnap struct {
	ID              int64
	Generation      int64
	GradingDetail   []byte // jsonb: submissions.grading_detail_json
	ReviewStatus    string
	SubjectiveFinal float64
	TotalScore      float64
	GradingStatus   string // pending|grading|done|failed
}

// 业务级 sentinel 错误 (与 admin handler 之间约定的错误码前缀).
var (
	ErrSubmissionNotFound = errors.New("SUBMISSION_NOT_FOUND")
	ErrGradingInProgress  = errors.New("GRADING_IN_PROGRESS")
	ErrQuestionNotFound   = errors.New("QUESTION_NOT_FOUND")
	ErrScoreOutOfRange    = errors.New("SCORE_OUT_OF_RANGE")
)

// ApplyInput 是 Apply 入参; SubQID 空表示整题人工确认.
type ApplyInput struct {
	SubmissionID int64
	QuestionID   string
	SubQID       string
	NewScore     float64
	Note         string
}

// ApplyResult 是 Apply 成功返回.
type ApplyResult struct {
	NewTotalScore float64
	NewStatus     string
}

// getSubmissionTx 在事务内读 review 视角快照.
func getSubmissionTx(ctx context.Context, q pgx.Tx, id int64) (*SubmissionSnap, error) {
	const sel = `SELECT id, grading_generation, grading_detail_json,
		coalesce(review_status,''), subjective_score_final, total_score, grading_status
		FROM submissions WHERE id = $1`
	row := q.QueryRow(ctx, sel, id)
	var s SubmissionSnap
	if err := row.Scan(&s.ID, &s.Generation, &s.GradingDetail,
		&s.ReviewStatus, &s.SubjectiveFinal, &s.TotalScore, &s.GradingStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan submission: %w", err)
	}
	return &s, nil
}

// Apply 对单道题审核改分, 同事务更新 submissions + review_logs.
// 前置拦截 GRADING_IN_PROGRESS (review_status='grading' 表示代次切换未决).
//
// SQL 字段名按 migrations/0001_initial.sql 真实名: submissions.subjective_score_final,
// submissions.total_score, submissions.review_status, submissions.reviewed_at,
// submissions.reviewer_note; review_logs(submission_id, question_id, old_score, new_score, note)
// 无 sub_question_id 列; review_logs.created_at 无默认值, 必须显式 NOW().
func (s *Service) Apply(ctx context.Context, in ApplyInput) (*ApplyResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("review.Apply: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) 读快照. SELECT FOR UPDATE 锁行防 regrade 同时改.
	const sel = `SELECT id, grading_generation, grading_detail_json,
		coalesce(review_status,''), subjective_score_final, total_score, grading_status
		FROM submissions WHERE id = $1 FOR UPDATE`
	row := tx.QueryRow(ctx, sel, in.SubmissionID)
	var snap SubmissionSnap
	err = row.Scan(&snap.ID, &snap.Generation, &snap.GradingDetail,
		&snap.ReviewStatus, &snap.SubjectiveFinal, &snap.TotalScore, &snap.GradingStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubmissionNotFound
	} else if err != nil {
		return nil, fmt.Errorf("scan submission: %w", err)
	}
	if snap.ReviewStatus == "grading" {
		return nil, ErrGradingInProgress
	}

	// 2) 解析 grading_detail 找题, 取 max_score.
	detail, oldScore, maxScore, qtype, found := findScoreInDetail(snap.GradingDetail,
		in.QuestionID, in.SubQID)
	if !found {
		return nil, ErrQuestionNotFound
	}
	if in.NewScore < 0 || in.NewScore > maxScore {
		return nil, fmt.Errorf("%w: %g 超出 [0, %g]", ErrScoreOutOfRange, in.NewScore, maxScore)
	}

	// 3) INSERT review_logs (无 sub_question_id 列, created_at 单元默认值, 强 NOW()).
	if _, err := tx.Exec(ctx, `
		INSERT INTO review_logs (submission_id, question_id, old_score, new_score, note, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::text, NOW())`,
		in.SubmissionID, in.QuestionID, oldScore, in.NewScore, in.Note); err != nil {
		return nil, fmt.Errorf("insert review_log: %w", err)
	}

	// 4) 重算 total_score; 主观题同步更新 subjective_score_final (客观题不动 subjective).
	newTotal := snap.TotalScore - oldScore + in.NewScore
	if newTotal < 0 {
		newTotal = 0
	}
	GUARDSQL := `
		UPDATE submissions
		SET total_score = $1, review_status = 'reviewed',
		    reviewed_at = NOW(), reviewer_note = NULLIF($2, '')::text`
	args := []interface{}{newTotal, in.Note, in.SubmissionID, snap.Generation}
	if !isObjectiveQuestion(qtype) {
		GUARDSQL += `, subjective_score_final = subjective_score_final - $5 + $6`
		args = append(args, oldScore, in.NewScore)
	}
	GUARDSQL += ` WHERE id = $3 AND grading_generation = $4`
	tag, err := tx.Exec(ctx, GUARDSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("update submission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrGradingInProgress
	}
	_ = detail

	// 5) 同步回写 grading_detail_json 中该题 score (与 Python 蓝本 detail[qid].score = newScore 对齐).
	newDetail, err := setDetailScoreInPlace(snap.GradingDetail, in.QuestionID, in.SubQID, in.NewScore)
	if err != nil {
		return nil, fmt.Errorf("set detail score: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE submissions SET grading_detail_json = $1 WHERE id = $2 AND grading_generation = $3`,
		newDetail, in.SubmissionID, snap.Generation); err != nil {
		return nil, fmt.Errorf("update grading_detail: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &ApplyResult{NewTotalScore: newTotal, NewStatus: "reviewed"}, nil
}

// entryQIDMatches 匹配条目题号: 规范键 question_id, 兼容仅写 id 的旧条目
// (worker 2026-07 前的 objective/subjective 条目只有 id, 曾致所有复核 404).
func entryQIDMatches(it map[string]any, qid string) bool {
	if qid == "" {
		return false
	}
	if v, _ := it["question_id"].(string); v == qid {
		return true
	}
	v, _ := it["id"].(string)
	return v == qid
}

// entrySubResults 取复合题子结果数组: 规范键 sub_results (worker 写入, 前端
// detail.js 同款契约), 兼容早期设想的 sub_questions 键 (存量测试数据).
func entrySubResults(it map[string]any) []any {
	if subs, ok := it["sub_results"].([]any); ok {
		return subs
	}
	subs, _ := it["sub_questions"].([]any)
	return subs
}

// findScoreInDetail 从 grading_detail jsonb 解析目标题的旧分 / 满分 / 题型;
// SubQID 空时取整题主分, 非空时取子题分 (含 sub_results 数组). found=false 表示未命中.
//
// grading_detail item 字段集 (worker grading.py 规范化输出):
//
//	question_id, id, type, score, machine_score, final_score, max_score, is_correct,
//	review_status, manually_reviewed, sub_results (optional, composite 题)
func findScoreInDetail(detail []byte, qid, subQID string) (_ []byte, old, mx float64, qtype string, found bool) {
	var items []map[string]any
	if err := json.Unmarshal(detail, &items); err != nil {
		return detail, 0, 0, "", false
	}
	for _, it := range items {
		if !entryQIDMatches(it, qid) {
			continue
		}
		t, _ := it["type"].(string)
		qtype = t
		if subQID == "" {
			old = numFloat(it["final_score"])
			mx = numFloat(it["max_score"])
			return detail, old, mx, qtype, true
		}
		// composite 子题: sub_results 数组里找 sub_question_id == subQID.
		for _, sraw := range entrySubResults(it) {
			s, ok := sraw.(map[string]any)
			if !ok {
				continue
			}
			if sid, _ := s["sub_question_id"].(string); sid == subQID {
				old = numFloat(s["final_score"])
				mx = numFloat(s["max_score"])
				return detail, old, mx, qtype, true
			}
		}
	}
	return detail, 0, 0, "", false
}

// setDetailScoreInPlace 在 grading_detail 中把题分数设置为新值, 返回新 JSON.
// SubQID 空时改 question 整体 final_score, 非空时改子题 final_score +
// 重算题级 final_score = sum(sub.final_score) (与 Python aggregate 一致).
func setDetailScoreInPlace(detail []byte, qid, subQID string, newScore float64) ([]byte, error) {
	var items []map[string]any
	if err := json.Unmarshal(detail, &items); err != nil {
		return detail, err
	}
	for _, it := range items {
		if !entryQIDMatches(it, qid) {
			continue
		}
		if subQID == "" {
			it["final_score"] = newScore
			it["score"] = newScore
			it["manually_reviewed"] = true
			it["review_status"] = "reviewed"
		} else {
			var subTotal float64
			for _, sraw := range entrySubResults(it) {
				s, ok := sraw.(map[string]any)
				if !ok {
					continue
				}
				if sid, _ := s["sub_question_id"].(string); sid == subQID {
					s["final_score"] = newScore
					s["manually_reviewed"] = true
					s["review_status"] = "reviewed"
				}
				subTotal += numFloat(s["final_score"])
			}
			it["final_score"] = subTotal
			it["score"] = subTotal
			it["manually_reviewed"] = true
		}
	}
	return json.Marshal(items)
}

// isObjectiveQuestion 与 grading.IsObjectiveType 等价的本地轻量判断 (review 包避免引 grading 子依赖).
func isObjectiveQuestion(qtype string) bool {
	switch qtype {
	case "single_choice", "multiple_choice", "true_false":
		return true
	}
	return false
}

// numFloat 从 any 取 float64; 兼容 float64/int/nil.
func numFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

// RegradeInput 是 Regrade 入参.
type RegradeInput struct {
	SubmissionID int64
}

// RegradeResult 返回 {success:true, status:'grading' or 'graded' or 'failed'}.
type RegradeResult struct {
	Success bool
	Status  string
}

// Regrade 统一代次切换路径 (A/A/A/A 决断 2026-07-26): 不分纯客观/含主观,
// 一律 generation+1 + supersede 旧 generation 仍在跑的 job, 立即返 status='grading'.
// 主观题交回 scoring_worker 接班回写. UPDATE 全部带 WHERE generation 守护防并发撕裂数据.
//
// Plan 偏差说明: plan 原 Step 3 "纯客观 Go 重算 + 含主观 generation+1" 的二分支设计自废
// fencing 契约 (graded_generation 守护失效), 改统一走代次切换; 这是按 "并发优先" 核心目的
// 主动偏离 Python in-place 蓝本的决定 (memory: project-regrade-generation-design).
func (s *Service) Regrade(ctx context.Context, in RegradeInput) (*RegradeResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("review.Regrade: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) SELECT FOR UPDATE 锁行拿当前 generation + paper_id + run_id (EnqueueTx 入参用).
	var (
		curGen  int64
		paperID string
		runID   string
	)
	err = tx.QueryRow(ctx, `
		SELECT grading_generation, paper_id, run_id
		FROM submissions WHERE id = $1 FOR UPDATE`,
		in.SubmissionID).Scan(&curGen, &paperID, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubmissionNotFound
	} else if err != nil {
		return nil, fmt.Errorf("scan gen: %w", err)
	}

	// 2) supersede 旧 generation 仍在跑的 job; worker 下次续租观察到 status='superseded' 自降.
	tag, err := tx.Exec(ctx, `
		UPDATE grading_jobs SET status = 'superseded'
		WHERE submission_id = $1 AND generation = $2
		  AND status IN ('queued','leased')`,
		in.SubmissionID, curGen)
	if err != nil {
		return nil, fmt.Errorf("supersede jobs: %w", err)
	}
	_ = tag.RowsAffected() // 仅记 causal, worker 续租时校验自己的 generation 是否被 superseded

	// 3) generation+1 + grading_status 回 pending + review_status 改回 'grading', 启动 regrade reflow.
	//    守门: WHERE grading_generation = $curGen 防 regrade 未决时学生又答卷撕裂数据.
	//    grading_error 同步清空 (plan Task 10: 重判时 grading_status=pending、grading_error=null).
	tag, err = tx.Exec(ctx, `
		UPDATE submissions
		SET grading_generation = grading_generation + 1,
		    grading_status = 'pending',
		    grading_error = NULL,
		    review_status = 'grading',
		    reviewed_at = NULL,
		    reviewer_note = NULL
		WHERE id = $1 AND grading_generation = $2`,
		in.SubmissionID, curGen)
	if err != nil {
		return nil, fmt.Errorf("update generation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// generation 已变 — 并发 regrade 抢先; 视为已 regrade, 返 success=true.
		return &RegradeResult{Success: true, Status: "grading"}, nil
	}

	// 4) INSERT 新 generation grading_job (queued), 复用 jobs.EnqueueTx 保证字段集一致.
	//    主观题由 worker 调 grader 完成回写, HTTP 不阻塞.
	if s.jobsRepo == nil {
		return &RegradeResult{Success: true, Status: "grading"}, nil
	}
	if err := s.jobsRepo.EnqueueTx(ctx, tx, jobs.JobSpec{
		SubmissionID: in.SubmissionID,
		PaperID:      paperID,
		RunID:        runID,
		Generation:   curGen + 1,
	}); err != nil {
		return nil, fmt.Errorf("enqueue new-gen job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &RegradeResult{Success: true, Status: "grading"}, nil
}
