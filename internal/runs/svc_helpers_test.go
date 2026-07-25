// Package runs_test 验 runs.Service 的行为契约 (external test).
package runs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

// newSvcCtx 建一个临时 schema + 跑 migrations + 临时 papers root + pubRoot;
// 返回 svc 与 pool 以让 test 启 pgx.Tx 并跑各种 case.
func newSvcCtx(t *testing.T) (ctx context.Context, pool *pgxpool.Pool,
	svc *runs.Service, slug string) {
	t.Helper()
	ctx = context.Background()

	// 用 testutil.NewSchema 建 schema + pool (内部 t.Cleanup 已注册)
	pgPool, cleanup := testutil.NewSchema(t)
	t.Cleanup(cleanup)

	// 跑 migrations(它内部 ensureSchemaMigrationsTable + 按 schema_migrations 应用)
	if _, err := db.Migrate(ctx, pgPool); err != nil {
		t.Fatalf("migrate random schema: %v", err)
	}

	// 临时 papers root + 塞 fixture 副本
	papersRoot := t.TempDir()
	fixturePath := filepath.Join("..", "..", "tests", "fixtures", "contract", "paper.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(papersRoot, "go-contract-paper.json"), data, 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	store, err := papers.NewStore(papersRoot)
	if err != nil {
		t.Fatalf("papers.NewStore: %v", err)
	}
	pubRoot := t.TempDir()
	svc = runs.NewService(runs.NewRepository(), store, pubRoot)
	pool = pgPool
	slug = "go-contract-paper"
	return
}

// openRun 是测试捷径: 启 tx -> svc.Open -> commit. 失败 t.Fatalf.
func openRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	svc *runs.Service, slug string) (*runs.Run, string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit tx: %v", err)
		}
	}()
	res, err := svc.Open(ctx, tx, slug, 60)
	if err != nil {
		t.Fatalf("svc.Open: %v", err)
	}
	return res.Run, res.PublicToken
}

// openRunNoCommit 启 tx 但不 commit, 供需观察失败后状态用. 调方 defer Rollback.
func openRunNoCommit(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	svc *runs.Service, slug string) (*runs.Run, string, pgx.Tx) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	res, err := svc.Open(ctx, tx, slug, 60)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("svc.Open: %v", err)
	}
	return res.Run, res.PublicToken, tx
}

// ---- Open 测试 ----

func TestOpenSuccess(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)
	run, token := openRun(t, ctx, pool, svc, slug)
	if run == nil {
		t.Fatal("run is nil")
	}
	if run.ID == "" {
		t.Error("run.ID is empty")
	}
	if run.PaperID != slug {
		t.Errorf("PaperID = %q, want %q", run.PaperID, slug)
	}
	if run.RoundNo != 1 {
		t.Errorf("RoundNo = %d, want 1", run.RoundNo)
	}
	if run.Status != runs.StatusOpen {
		t.Errorf("Status = %q, want %q", run.Status, runs.StatusOpen)
	}
	if token == "" {
		t.Error("PublicToken is empty")
	}
	if len(token) != 43 { // 32 bytes base64url no-padding -> 43 chars
		t.Errorf("PublicToken len = %d, want 43", len(token))
	}
	if run.PublicTokenHash == "" {
		t.Error("PublicTokenHash is empty")
	}
}

func TestOpenDuplicateActiveRejects(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)
	openRun(t, ctx, pool, svc, slug)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = svc.Open(ctx, tx, slug, 60)
	if !errors.Is(err, runs.ErrActiveRunExists) {
		t.Errorf("second Open err = %v, want ErrActiveRunExists", err)
	}
}

// ---- FindByToken 测试 ----

func TestFindByTokenSuccess(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)
	run, token := openRun(t, ctx, pool, svc, slug)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	got, err := svc.FindByToken(ctx, tx, token)
	if err != nil {
		t.Fatalf("FindByToken: %v", err)
	}
	if got.ID != run.ID {
		t.Errorf("FindByToken.ID = %q, want %q", got.ID, run.ID)
	}
}

func TestFindByTokenNotFound(t *testing.T) {
	ctx, pool, svc, _ := newSvcCtx(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = svc.FindByToken(ctx, tx, "no-such-token-xxxxxxxxxxxxxxxxxxxxxxxx")
	if !errors.Is(err, runs.ErrTokenNotFound) {
		t.Errorf("FindByToken err = %v, want ErrTokenNotFound", err)
	}
}

// ---- BeginClose 测试 ----

func TestBeginCloseOpenToClosing(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)
	run, _ := openRun(t, ctx, pool, svc, slug)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	updated, err := svc.BeginClose(ctx, tx, run.ID, 5*60) // 5 min grace
	if err != nil {
		t.Fatalf("BeginClose: %v", err)
	}
	if updated.Status != runs.StatusClosing {
		t.Errorf("Status = %q, want %q", updated.Status, runs.StatusClosing)
	}
	if updated.FinalizeAt == nil {
		t.Error("FinalizeAt is nil after BeginClose")
	}
	if updated.ClosingStartedAt == nil {
		t.Error("ClosingStartedAt is nil after BeginClose")
	}
}

func TestBeginCloseNotFound(t *testing.T) {
	ctx, pool, svc, _ := newSvcCtx(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = svc.BeginClose(ctx, tx, "run-no-such-id", 60*time.Second)
	if !errors.Is(err, runs.ErrRunNotFound) {
		t.Errorf("BeginClose err = %v, want ErrRunNotFound", err)
	}
}

func TestBeginCloseIdempotent(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)
	run, _ := openRun(t, ctx, pool, svc, slug)

	// 第一次 close
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	_, err = svc.BeginClose(ctx, tx1, run.ID, 5*60)
	if err != nil {
		tx1.Rollback(ctx)
		t.Fatalf("first BeginClose: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// 第二次 close (已 closing) -> 应仍返回 closing 而非 error
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(ctx)
	updated, err := svc.BeginClose(ctx, tx2, run.ID, 5*60)
	if err != nil {
		t.Errorf("second BeginClose: %v", err)
	}
	if updated.Status != runs.StatusClosing {
		t.Errorf("Status = %q, want %q", updated.Status, runs.StatusClosing)
	}
}

// ---- round_no 递增测试 ----

func TestOpenRoundNumberIncrements(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)

	// 第一次: round_no=1
	r1, _ := openRun(t, ctx, pool, svc, slug)
	if r1.RoundNo != 1 {
		t.Fatalf("r1.RoundNo = %d, want 1", r1.RoundNo)
	}

	// 关掉第一个 -> 才能再开
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx close: %v", err)
	}
	if _, err := svc.BeginClose(ctx, tx, r1.ID, 1*time.Second); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("BeginClose r1: %v", err)
	}
	// 立即标 closed 让唯一索引放行 (用 UPDATE 直接置 closed)
	if _, err := tx.Exec(ctx,
		"UPDATE exam_runs SET status='closed', closed_at=NOW() WHERE id=$1", r1.ID); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("mark closed r1: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit close: %v", err)
	}

	// 第二次开 -> round_no=2
	r2, _ := openRun(t, ctx, pool, svc, slug)
	if r2.RoundNo != 2 {
		t.Errorf("r2.RoundNo = %d, want 2", r2.RoundNo)
	}
}

// ---- GetPublicExam 测试 ----

func TestGetPublicExamSuccess(t *testing.T) {
	ctx, pool, svc, slug := newSvcCtx(t)
	run, _ := openRun(t, ctx, pool, svc, slug)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	res, err := svc.GetPublicExam(ctx, tx, run)
	if err != nil {
		t.Fatalf("GetPublicExam: %v", err)
	}
	if res.RunID != run.ID {
		t.Errorf("RunID = %q, want %q", res.RunID, run.ID)
	}
	if res.PaperID != slug {
		t.Errorf("PaperID = %q, want %q", res.PaperID, slug)
	}
	if res.RoundNo != 1 {
		t.Errorf("RoundNo = %d, want 1", res.RoundNo)
	}
	if res.RunStatus != string(runs.StatusOpen) {
		t.Errorf("RunStatus = %q, want %q", res.RunStatus, string(runs.StatusOpen))
	}
	if res.Closed {
		t.Error("Closed = true, want false (status=open)")
	}
	if len(res.Questions) == 0 {
		t.Error("Questions is empty")
	}
	// 检查脱敏: 任一 question 不应有 "answer" 字段
	for i, q := range res.Questions {
		if _, has := q["answer"]; has {
			t.Errorf("Question[%d] has answer (not sanitized)", i)
		}
	}
}
