package review

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB 单测连接池 (TEST_DATABASE_URL env 注入). 连不上 skip 即不挂 CI.
var (
	testDBOnce sync.Once
	testDBPool *pgxpool.Pool
	testDBErr  error
)

// testPool 返回单测用 pgxpool; 缺 env / 连不上 -> t.Skip (而非 fail).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testDBOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			testDBErr = fmt.Errorf("TEST_DATABASE_URL unset")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			testDBErr = err
			return
		}
		if err := p.Ping(ctx); err != nil {
			testDBErr = err
			p.Close()
			return
		}
		testDBPool = p
	})
	if testDBErr != nil {
		t.Skipf("review integration test skipped: %v", testDBErr)
	}
	return testDBPool
}

// truncate 测试前清表 (CASCADE 链路: review_logs, submissions, grading_jobs).
// 在每个测试函数开头 defer 调用清理.
func truncate(ctx context.Context, pool *pgxpool.Pool) {
	for _, t := range []string{
		"review_logs",        // 依赖 submissions.id
		"grading_jobs",       // 依赖 submissions.id
		"submissions",        // 依赖 exam_sessions.id
		"exam_sessions",      // 依赖 exam_runs.id
		"exam_runs",          // 顶层 paper_slug
	} {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+t+" RESTART IDENTITY CASCADE"); err != nil {
			// 不 fail, 仅记 log; 顺序错误容忍
			_ = err
		}
	}
}

// seedSubmission 构造一条 closed run + submitted session + 已评 submissions 行
// 备 Apply / Regrade 单测使用. 返回 submissions.id.
//
// 极简快照: paper_slug = "smoke-1", questions_json 一道主观题一题分外一道客观题.
// grading_status='done' / review_status='reviewed' (整卷已粗评完, 等待人工 review).
// grading_detail_json 直填已知字段集 (review 包内 findScoreInDetail 期望的格式).
//
// grading_generation 默认 1 (regrade 后 +1 至 2, 形成可观察的代次切换).
func seedSubmission(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) exam_runs: paper_id='smoke-1' round_no=1 status='closed' id='run-1'.
	const runSQL = `INSERT INTO exam_runs
		(id, paper_id, round_no, status, duration_minutes, public_token_hash,
		 snapshot_path, snapshot_hash, opened_at, closed_at, created_at)
		VALUES ('run-1', 'smoke-1', 1, 'closed', 60,
		 'pub-tok-hash', '/tmp/snapshot.json', 'snap-hash-xyz',
		 NOW(), NOW(), NOW())`
	if _, err := tx.Exec(ctx, runSQL); err != nil {
		return 0, fmt.Errorf("seed exam_runs: %w", err)
	}

	// 2) exam_sessions: id='sess-1' run_id='run-1' employee_id='emp-1' status='submitted'.
	const sessSQL = `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash,
		 started_at, deadline_at, status, created_at, updated_at)
		VALUES ('sess-1', 'run-1', 'emp-1', 'tester', 'sess-tok-hash',
		 NOW(), NOW() + interval '1 hour', 'submitted', NOW(), NOW())`
	if _, err := tx.Exec(ctx, sessSQL); err != nil {
		return 0, fmt.Errorf("seed exam_sessions: %w", err)
	}

	// 3) submissions: 已评完一行 主観3分 + 客观4分 = 7 总分.
	//    名字段 schema 真实名: name, employee_id, paper_id, run_id, answers_json,
	//    grading_detail_json, objective_score, subjective_score_machine,
	//    subjective_score_final, total_score, review_status='reviewed',
	//    grading_status='done', grading_generation=1, submitted_at 必填.
	detail := `[` +
		`{"question_id":"subj-A","type":"short_answer","score":3,"machine_score":2,` +
		`"final_score":3,"max_score":10,"is_correct":false,` +
		`"manually_reviewed":false,"reason":"low-confidence"}` +
		`,{"question_id":"obj-A","type":"single_choice","score":4,"machine_score":4,` +
		`"final_score":4,"max_score":4,"is_correct":true,` +
		`"manually_reviewed":false}` +
		`]`
	const subSQL = `INSERT INTO submissions
		(id, name, employee_id, paper_id, run_id, answers_json,
		 grading_detail_json, objective_score, subjective_score_machine,
		 subjective_score_final, total_score, review_status, grading_status,
		 grading_generation, submitted_at)
		VALUES (5001, 'tester', 'emp-1', 'smoke-1', 'run-1', '[]'::jsonb,
		 $1::jsonb, 4, 2, 3, 7, 'reviewed', 'done', 1, NOW())`
	if _, err := tx.Exec(ctx, subSQL, detail); err != nil {
		return 0, fmt.Errorf("seed submissions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("seed commit: %w", err)
	}
	return 5001, nil
}

