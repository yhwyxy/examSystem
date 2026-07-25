package runs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yhwyxy/examSystem/internal/papers"
)

// Service 是 exam_runs 业务层. 封装 token 生成 + papers.Store 调用 +
// 事务管理 (container 流).
type Service struct {
	repo   *Repository
	papers *papers.Store
	// PaperPubRoot 是 snapshot 文件落盘根目录 (绝对路径). 每个 run 一个文件, 名 run-<id>.json
	pubRoot string
}

// NewService 创建 Service.
func NewService(repo *Repository, papersStore *papers.Store, pubRoot string) *Service {
	return &Service{repo: repo, papers: papersStore, pubRoot: pubRoot}
}

// OpenResult 是 Open 返回. 带明文 public_token (一次性裸值, 不写库) + run.
type OpenResult struct {
	Run         *Run
	PublicToken string // 明文 token, 仅在新创建时返回给调用方
}

// Open 在 paper slug 上创建新 round.
//
// 流程 (与 Python open_run 等价):
//   1. 用 papers.Store.LoadEditable(slug) 读 paper.json (Filesys)
//   2. 用 papers.ComputeSHA256(doc) 算 snapshot hash
//   3. 生成 32-byte 裸 token + sha256 hash
//   4. 写 snapshot 文件: <pubRoot>/paper-<slug>-run-<id>.json
//   5. 在 PG 事务内 INSERT exam_runs
//   6. 返回 OpenResult{ Run, PublicToken }
//
// 事务失败 -> snapshot 文件残留但不影响下次 (id 唯一的 tmp 文件会随 cleanup);
// 生产可加 compensating 清理逻辑, 但本 Task 不做 (与 Python 行为等价前向兼容).
func (s *Service) Open(ctx context.Context, tx pgx.Tx, slug string,
	durationMinutes int) (*OpenResult, error) {
	if s.papers == nil || s.pubRoot == "" {
		return nil, fmt.Errorf("runs.Open: papers store or pubRoot is nil")
	}
	if !s.papers.Exists(slug) {
		return nil, fmt.Errorf("paper %q does not exist", slug)
	}
	doc, err := s.papers.LoadEditable(slug)
	if err != nil {
		return nil, fmt.Errorf("runs.Open: load paper %q: %w", slug, err)
	}
	docUseNumber, err := papers.LoadDocumentUseNumber(s.papers.Root() + "/" + slug + ".json")
	if err != nil {
		// 回退: 直接用 doc (非 useNumber); sha 与 Python 不一致但不影响业务
		docUseNumber = doc
	}
	snapshotHash, err := papers.ComputeSHA256(docUseNumber)
	if err != nil {
		return nil, fmt.Errorf("runs.Open: compute snapshot hash: %w", err)
	}

	// token: 32 bytes random -> base64url no padding
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("runs.Open: token rng: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenHash := hashToken(token)

	// run id: uuid v4 string
	runID := "run-" + uuid.NewString()

	// 写 snapshot 文件: 必须用 docUseNumber (json.Number) 而非 doc (float64),
	// 因为 json.MarshalIndent(float64(100),...) 在 Go 里输出 "100" (丢 ".0"),
	// 而 ComputeSHA256 走 UseNumber 时输出 "100.0"; 两套字节在二次 LoadSnapshot
	// 校验时 hash 不一致. 用 docUseNumber 写盘可让文件保留原始 100.0/60 字面量,
	// 与 snapshot_hash 自洽. sanitize 仍由后续 GetPublicExam 在 LoadSnapshot 之后再做.
	snapshotPath := filepath.Join(s.pubRoot,
		fmt.Sprintf("paper-%s-run-%s.json", slug, runID))
	if err := papers.AtomicWriteJSON(snapshotPath, docUseNumber); err != nil {
		return nil, fmt.Errorf("runs.Open: write snapshot: %w", err)
	}

	run, err := s.repo.Open(ctx, tx, runID, slug, snapshotPath, snapshotHash,
		tokenHash, durationMinutes)
	if err != nil {
		return nil, err
	}
	return &OpenResult{Run: run, PublicToken: token}, nil
}

// PublicExamResult 是 GetPublicExam 返回, 与 Python /api/exam 端点契约对齐:
//   - base 主表字段: paper_id, run_id, round_no, paper_name, run_status,
//     duration_minutes, finalize_at, server_time, auto_submit, closed
//   - exam_info: source paper 的 exam_info 字段
//   - questions: 已 sanitized
type PublicExamResult struct {
	PaperID         string
	RunID           string
	RoundNo         int
	PaperName       string
	RunStatus       string
	DurationMinutes int
	FinalizeAt      *time.Time
	ServerTime      time.Time
	AutoSubmit      bool
	Closed          bool
	ExamInfo        any
	Questions       []papers.Document
}

// GetPublicExam 在指定 run token 下读出脱敏卷 + run 状态信息.
// 服务层不验 token 存在 (FindByToken 先做); 调用方先 FindByToken 后 GetPublicExam.
func (s *Service) GetPublicExam(ctx context.Context, tx pgx.Tx, run *Run) (*PublicExamResult, error) {
	// snapshot 文件路径 = s.pubRoot/<basename>; 校验 root 前缀防逃逸
	if s.pubRoot == "" {
		return nil, fmt.Errorf("runs.GetPublicExam: pubRoot empty")
	}
	full := run.SnapshotPath
	rel, err := filepath.Rel(s.pubRoot, full)
	if err != nil || rel == "" || startsDotDot(rel) {
		return nil, fmt.Errorf("runs.GetPublicExam: snapshot path escape")
	}
	// 校验 snapshot hash
	snap, err := papers.LoadSnapshot(full, run.SnapshotHash)
	if err != nil {
		return nil, fmt.Errorf("runs.GetPublicExam: load snapshot: %w", err)
	}
	doc := snap.Doc
	rawQs, ok := doc["questions"].([]any)
	if !ok {
		return nil, fmt.Errorf("runs.GetPublicExam: paper snapshot has no questions")
	}
	questionsDocs := make([]papers.Document, 0, len(rawQs))
	for _, x := range rawQs {
		if m, ok := x.(map[string]any); ok {
			questionsDocs = append(questionsDocs, m)
		}
	}
	closed := run.Status == StatusClosing || run.Status == StatusClosed
	paperName, _ := doc["name"].(string)
	var examInfo any
	if ei, ok := doc["exam_info"].(papers.Document); ok {
		examInfo = ei
	}
	return &PublicExamResult{
		PaperID:         run.PaperID,
		RunID:           run.ID,
		RoundNo:         run.RoundNo,
		PaperName:       paperName,
		RunStatus:       string(run.Status),
		DurationMinutes: run.DurationMinutes,
		FinalizeAt:      run.FinalizeAt,
		ServerTime:      time.Now().UTC(),
		AutoSubmit:      true, // TODO: 取自 settings.exam.auto_submit
		Closed:          closed,
		ExamInfo:        examInfo,
		Questions:       papers.SanitizeForStudent(questionsDocs),
	}, nil
}

// startsDotDot 判 rel 是否 ".." 开头 (path 逃逸).
func startsDotDot(rel string) bool {
	if len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return true
	}
	return false
}

var _ = uuid.New

// hashToken 把明文 token sha256 -> hex string, 作 DB 索引.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	// hex.EncodeToString 不引入, 自己小写手写
	return bytesToHex(h[:])
}

// bytesToHex 把字节切片转小写 hex.
func bytesToHex(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = hexChars[x>>4]
		out[i*2+1] = hexChars[x&0x0F]
	}
	return string(out)
}

// ErrTokenNotFound 表示按明文 token 查无 run.
var ErrTokenNotFound = errors.New("TOKEN_NOT_FOUND")

// FindByToken 等价 Python get_active_run: 按明文查 run (内部 sha256 比对 hash).
func (s *Service) FindByToken(ctx context.Context, tx pgx.Tx, publicToken string) (*Run, error) {
	if publicToken == "" {
		return nil, ErrTokenNotFound
	}
	hash := hashToken(publicToken)
	run, err := s.repo.FindByPublicTokenHash(ctx, tx, hash)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return nil, ErrTokenNotFound
		}
		return nil, err
	}
	return run, nil
}

// BeginClose 启动收卷: open -> closing 状态机切换; finalize_at = now + grace.
func (s *Service) BeginClose(ctx context.Context, tx pgx.Tx, runID string,
	graceDuration time.Duration) (*Run, error) {
	return s.repo.BeginClose(ctx, tx, runID, graceDuration)
}

var _ = uuid.New
