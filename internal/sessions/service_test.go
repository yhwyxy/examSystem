// Package sessions_test 验 sessions.Service 行为契约 (external test).
package sessions_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

// _ 兜底引用 errors 包 (上面 import 中已有, 此行留作 doc 锚点).
var _ = errors.Is

// strPtr 返回指向 s 的新 *string, 测试用便捷构造.
func strPtr(s string) *string { return &s }

// startOrCommit 是开 tx -> StartOrResume -> commit 的便捷封装, 用于 setup.
func startOrCommit(t *testing.T, ctx context.Context, env *sessionsTestEnv, runToken, employeeID string) *sessions.StartResult {
	t.Helper()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	res, err := env.svc.StartOrResume(ctx, tx, runToken, employeeID, "Test", strPtr("Dept"), nil, nil)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("StartOrResume: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	return res
}

// nowUTC 固定测试时钟 (2026-01-01 09:00:00 UTC).
func nowUTC() time.Time { return time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) }

// stableID 给测试用确定性 id. n=1 -> "sess-0001".
func stableID(n int32) string {
	if n <= 0 {
		n = 1
	}
	s := ""
	for n > 0 {
		s = "0123456789"[n%10:n%10+1] + s
		n /= 10
	}
	for len(s) < 4 {
		s = "0" + s
	}
	return "sess-" + s
}

// runsSvcLookup 是把 runs.Service + runs.Repository (真实 repo 查询) 适配成 sessions.RunsLookup.
// 通过显式注入 tx 完成所有读取; HTTP 层将来按既定方式与 sessions 协作.
type runsSvcLookup struct {
	runsSvc *runs.Service
	runsRepo *runs.Repository
	pool    *pgxpool.Pool
}

func (r *runsSvcLookup) FindByToken(ctx context.Context, token string) (*sessions.RunLite, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	run, err := r.runsSvc.FindByToken(ctx, tx, token)
	if err != nil {
		// 把 runs.ErrTokenNotFound 翻译为 sessions.ErrInvalidSessionToken;
		// 这也就是 HTTP 层将来的产线 adaptor 应做的同种错码归一.
		if errors.Is(err, runs.ErrTokenNotFound) {
			return nil, sessions.ErrInvalidSessionToken
		}
		return nil, err
	}
	return runToRunLite(run), nil
}

func (r *runsSvcLookup) FindByID(ctx context.Context, id string) (*sessions.RunLite, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	run, err := r.runsRepo.FindByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return runToRunLite(run), nil
}

// runToRunLite 把 runs.Run 裁剪成 sessions.RunLite.
func runToRunLite(r *runs.Run) *sessions.RunLite {
	rl := &sessions.RunLite{
		ID:       r.ID,
		PaperID:  r.PaperID,
		Status:   string(r.Status),
		RoundNo:  r.RoundNo,
	}
	if r.DurationMinutes > 0 {
		rl.Duration = time.Duration(r.DurationMinutes) * time.Minute
	} else {
		rl.Duration = 60 * time.Minute // 默认
	}
	if r.FinalizeAt != nil {
		rl.FinalizeAt = r.FinalizeAt
	}
	return rl
}

// newEnv 是测试用 sessions.Service 环境: 跑 migrations + seed fixture + 启用真 runs.
// 在生产里 HTTP 层用 lookup 间接接通; 这份测试只测 sessions 单元, 故把 runsSvc/runsRepo
// 暴露出来让测试可在 lookup 之外另起 run 流程.
type sessionsTestEnv struct {
	pool     *pgxpool.Pool
	runsSvc  *runs.Service
	runsRepo *runs.Repository
	lookup   *runsSvcLookup
	svc      *sessions.Service
}

func newEnv(t *testing.T, opts ...sessions.Option) (*sessionsTestEnv, string) {
	t.Helper()
	ctx := context.Background()
	pgPool, cleanup := testutil.NewSchema(t)
	t.Cleanup(cleanup)
	if _, err := db.Migrate(ctx, pgPool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	papersRoot := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "contract", "paper.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(papersRoot, "paper-active.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := papers.NewStore(papersRoot)
	if err != nil {
		t.Fatalf("papers.NewStore: %v", err)
	}
	pubRoot := t.TempDir()
	runsRepo := runs.NewRepository()
	runsSvc := runs.NewService(runsRepo, store, pubRoot)
	lookup := &runsSvcLookup{runsSvc: runsSvc, runsRepo: runsRepo, pool: pgPool}

	var idCounter int32
	defOpts := []sessions.Option{
		sessions.WithExamDuration(60 * time.Minute),
		sessions.WithClock(nowUTC),
		sessions.WithIDGen(func() string { return stableID(atomic.AddInt32(&idCounter, 1)) }),
		sessions.WithTokenGen(func() (string, error) { return "fixed-session-token-00112233445566778899", nil }),
	}
	defOpts = append(defOpts, opts...)
	svc := sessions.NewService(sessions.NewRepository(), lookup, defOpts...)
	return &sessionsTestEnv{
		pool:     pgPool,
		runsSvc:  runsSvc,
		runsRepo: runsRepo,
		lookup:   lookup,
		svc:      svc,
	}, "paper-active"
}

// startRun 在 commit tx 内开一条 run, 返回 run_id + run_token. 用于准备"已有一条 open run"
// 的前置条件 (供 sessions.StartOrResume 测试用).
func startRun(t *testing.T, ctx context.Context, env *sessionsTestEnv, slug string) (runID, runToken string) {
	t.Helper()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	res, err := env.runsSvc.Open(ctx, tx, slug, 60)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("runsSvc.Open: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	return res.Run.ID, res.PublicToken
}

// ----- SaveDraft 测试 -----

// TestSaveDraftSuccess 用第一次 start 的 token 保存草稿 -> Revision=1.
func TestSaveDraftSuccess(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)
	_, runToken := startRun(t, ctx, env, slug)
	res := startOrCommit(t, ctx, env, runToken, "emp-draft-1")
	token := res.SessionToken
	if token == "" {
		t.Fatalf("first start SessionToken empty (no fixed tok gen?)")
	}

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	draft := []byte(`{"q1":"answer-a"}`)
	r, err := env.svc.SaveDraft(ctx, tx, token, 0, draft)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if r.Throttled {
		t.Error("Throttled=true, want false")
	}
	if r.Revision != 1 {
		t.Errorf("Revision = %d, want 1", r.Revision)
	}
	if r.DraftSavedAt.IsZero() {
		t.Error("DraftSavedAt zero")
	}
}

// TestSaveDraftStaleRevision CAS 冲突 -> ErrStaleDraftRevision.
func TestSaveDraftStaleRevision(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)
	_, runToken := startRun(t, ctx, env, slug)
	res := startOrCommit(t, ctx, env, runToken, "emp-draft-2")

	// 第一次保存让 revision=1
	tx1, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	if _, err := env.svc.SaveDraft(ctx, tx1, res.SessionToken, 0, []byte(`{"a":1}`)); err != nil {
		tx1.Rollback(ctx)
		t.Fatalf("first SaveDraft: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// 第二次用旧 revision=0 -> 应 STALE
	tx2, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(ctx)
	_, err = env.svc.SaveDraft(ctx, tx2, res.SessionToken, 0, []byte(`{"a":2}`))
	if !errors.Is(err, sessions.ErrStaleDraftRevision) {
		t.Errorf("err = %v, want ErrStaleDraftRevision", err)
	}
}

// TestSaveDraftBadToken token 错 -> ErrInvalidSessionToken.
func TestSaveDraftBadToken(t *testing.T) {
	ctx := context.Background()
	env, _ := newEnv(t)
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = env.svc.SaveDraft(ctx, tx, "no-such-token-000", 0, []byte(`{}`))
	if !errors.Is(err, sessions.ErrInvalidSessionToken) {
		t.Errorf("err = %v, want ErrInvalidSessionToken", err)
	}
}

// ----- Status 测试 -----

// TestStatusSuccess Status 返回 session 实时状态 + draft 与 revision.
func TestStatusSuccess(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)
	_, runToken := startRun(t, ctx, env, slug)
	res := startOrCommit(t, ctx, env, runToken, "emp-status-1")

	// 先存一次草稿 (revision=1)
	tx1, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	if _, err := env.svc.SaveDraft(ctx, tx1, res.SessionToken, 0, []byte(`{"q":"A"}`)); err != nil {
		tx1.Rollback(ctx)
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// Status 查
	tx2, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(ctx)

	st, err := env.svc.Status(ctx, tx2, res.SessionToken)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SessionID != res.SessionID {
		t.Errorf("SessionID = %q, want %q", st.SessionID, res.SessionID)
	}
	if st.Status != sessions.StatusActive {
		t.Errorf("Status = %q, want active", st.Status)
	}
	if st.DraftRevision != 1 {
		t.Errorf("DraftRevision = %d, want 1", st.DraftRevision)
	}
	if string(st.Draft) == "" || string(st.Draft) == "{}" {
		t.Errorf("Draft = %q, want {\"q\":\"A\"}", string(st.Draft))
	}
	if !st.DeadlineAt.Equal(nowUTC().Add(60 * time.Minute)) {
		t.Errorf("DeadlineAt = %v, want %v", st.DeadlineAt, nowUTC().Add(60*time.Minute))
	}
}

// TestStatusBadToken Status 不存在 token -> ErrSessionNotFound.
func TestStatusBadToken(t *testing.T) {
	ctx := context.Background()
	env, _ := newEnv(t)
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	_, err = env.svc.Status(ctx, tx, "no-such-status-token-zzz")
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

// ----- SubmitManual-disabled 测试 -----

// TestSubmitManualDisabled 在 submissions/jobs 依赖未注入时 SubmitManual 报 ErrSubmitDisabled.
// 这是 sessions.Service 在 Task 5 阶段的预期 gating (Task 6 接通后 disabled 测试应替换为真实链路).
func TestSubmitManualDisabled(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)
	_, runToken := startRun(t, ctx, env, slug)
	res := startOrCommit(t, ctx, env, runToken, "emp-submit-disabled")

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = env.svc.SubmitManual(ctx, tx, sessions.SubmitManualRequest{
		SessionToken: res.SessionToken,
		Answers:      []byte(`{"q":"A"}`),
	})
	if !errors.Is(err, sessions.ErrSubmitDisabled) {
		t.Errorf("err = %v, want ErrSubmitDisabled", err)
	}
}
// ----- StartOrResume 测试 -----

// TestStartOrResumeNew 通过真实 runs svc 开 run -> StartOrResume 新建一条 session.
// 期望 NewSession=true, SessionID 以 "sess-" 前缀, SessionToken=固定 token 字面量, RunID=run.id.
func TestStartOrResumeNew(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)

	runID, runToken := startRun(t, ctx, env, slug)
	if runID == "" || runToken == "" {
		t.Fatalf("startRun got empty runID=%q runToken=%q", runID, runToken)
	}

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	res, err := env.svc.StartOrResume(ctx, tx, runToken, "emp-001", "Alice", strPtr("Engineering"), nil, nil)
	if err != nil {
		t.Fatalf("StartOrResume: %v", err)
	}
	if !res.NewSession {
		t.Error("NewSession = false, want true (first start)")
	}
	if res.SessionID == "" {
		t.Error("SessionID empty")
	}
	if res.SessionToken != "fixed-session-token-00112233445566778899" {
		t.Errorf("SessionToken = %q, want fixed test token", res.SessionToken)
	}
	if res.RunID != runID {
		t.Errorf("RunID = %q, want %q", res.RunID, runID)
	}
	if res.DraftRevision != 0 {
		t.Errorf("DraftRevision = %d, want 0 (new)", res.DraftRevision)
	}
	wantDeadline := nowUTC().Add(60 * time.Minute)
	if !res.Deadline.Equal(wantDeadline) {
		t.Errorf("Deadline = %v, want %v", res.Deadline, wantDeadline)
	}
}

// TestStartOrResumeResume 通过 svc.StartOrResume 第二次同 (runToken, employee_id):
// 期望 NewSession=false, SessionToken="" (不发新), 拿回原有 session_id.
func TestStartOrResumeResume(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)
	runID, runToken := startRun(t, ctx, env, slug)
	_ = runID

	// 第一次 start
	res1 := startOrCommit(t, ctx, env, runToken, "emp-002")
	if !res1.NewSession {
		t.Fatalf("first start NewSession=false: %+v", res1)
	}

	// 第二次同 employee resume
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx.Rollback(ctx)
	res2, err := env.svc.StartOrResume(ctx, tx, runToken, "emp-002", "Bob", strPtr("Sales"), nil, nil)
	if err != nil {
		t.Fatalf("resume StartOrResume: %v", err)
	}
	if res2.NewSession {
		t.Error("resume NewSession = true, want false")
	}
	if res2.SessionToken != "" {
		t.Errorf("resume SessionToken = %q, want empty (no re-issue)", res2.SessionToken)
	}
	if res2.SessionID != res1.SessionID {
		t.Errorf("resume SessionID = %q, want %q (same session)", res2.SessionID, res1.SessionID)
	}
}

// TestStartOrResumeBadToken run token 不存在 -> ErrInvalidSessionToken.
func TestStartOrResumeBadToken(t *testing.T) {
	ctx := context.Background()
	env, _ := newEnv(t)
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = env.svc.StartOrResume(ctx, tx, "does-not-exist-token-aaaa", "emp-x", "X", nil, nil, nil)
	if !errors.Is(err, sessions.ErrInvalidSessionToken) {
		t.Errorf("err = %v, want ErrInvalidSessionToken", err)
	}
}

// TestStartOrResumeRunClosed run 已 closed -> ErrRunClosed.
func TestStartOrResumeRunClosed(t *testing.T) {
	ctx := context.Background()
	env, slug := newEnv(t)
	runID, runToken := startRun(t, ctx, env, slug)
	// 把 run 标 closed
	tx1, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	if _, err := tx1.Exec(ctx,
		"UPDATE exam_runs SET status='closed', closed_at=$2 WHERE id=$1",
		runID, nowUTC()); err != nil {
		tx1.Rollback(ctx)
		t.Fatalf("mark closed: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	_, err = env.svc.StartOrResume(ctx, tx, runToken, "emp-z", "Z", nil, nil, nil)
	if !errors.Is(err, sessions.ErrRunClosed) {
		t.Errorf("err = %v, want ErrRunClosed", err)
	}
}

