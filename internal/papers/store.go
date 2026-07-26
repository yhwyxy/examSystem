package papers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Store 文件存储: 在 data root 下管理 paper JSON. 路径公式:
//   <root>/<slug>.json
// 安全: 所有 slug 必须匹配 safeSlugPattern, 再用 filepath.Rel 二次防逃逸 (../).
type Store struct {
	root string
	mu   sync.Map // by slug -> *sync.Mutex
}

// slugPattern 与 Python slug 安全正则 ^[A-Za-z0-9_-]+$ 一致.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// NewStore 创建新 paper store. root 必须存在且是目录, 否则 _ 警告但不 panic.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("papers.NewStore: root is empty")
	}
	if fi, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("papers.NewStore root: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("papers.NewStore root not dir: %s", root)
	}
	return &Store{root: root}, nil
}

// Root 返回数据目录 (供 caller 知道).
func (s *Store) Root() string { return s.root }

// slugLock 返回 per-slug 的 *sync.Mutex (lazy init).
func (s *Store) slugLock(slug string) *sync.Mutex {
	v, _ := s.mu.LoadOrStore(slug, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// pathFor 返回 slug 对应的 paper.json 绝对路径, 并用 Rel 二次防逃逸.
func (s *Store) pathFor(slug string) (string, error) {
	if !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("papers.Store: invalid slug %q", slug)
	}
	abs := filepath.Join(s.root, slug+".json")
	// 防逃逸: <param>/../../etc/passwd 等
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("papers.Store: path escapes root: %q", slug)
	}
	return abs, nil
}

// LoadEditable 读 slug 对应的 paper.json (不校 sha).
// 文件不存在 -> os.ErrNotExist (与 Python Q/_load_paper 1 对 1).
func (s *Store) LoadEditable(slug string) (Document, error) {
	abs, err := s.pathFor(slug)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("papers.Store.LoadEditable: parse %s: %w", slug, err)
	}
	return doc, nil
}

// SaveEditable 写 paper JSON. per-slug 串行化 + 原子写 (fsync + rename).
// doc 写前会做 ValidateDocument; 失败不写文件.
func (s *Store) SaveEditable(slug string, doc Document) error {
	if err := ValidateDocument(doc); err != nil {
		return fmt.Errorf("papers.Store.SaveEditable: %w", err)
	}
	abs, err := s.pathFor(slug)
	if err != nil {
		return err
	}
	mu := s.slugLock(slug)
	mu.Lock()
	defer mu.Unlock()
	return AtomicWriteJSON(abs, doc)
}

// LoadSnapshot 读 paper 但同时校验期望 sha256 (与 exam_runs.snapshot_hash 比对).
// 与 papers.LoadSnapshot 区别: 从 store 解析路径, 调用者只传 slug + 切勿传 path.
func (s *Store) LoadSnapshot(slug, expectedSHA256Hex string) (*Snapshot, error) {
	abs, err := s.pathFor(slug)
	if err != nil {
		return nil, err
	}
	mu := s.slugLock(slug)
	mu.Lock()
	defer mu.Unlock()
	return LoadSnapshot(abs, expectedSHA256Hex)
}

// List 列出 store root 下所有 *.json slug. 不保证顺序.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		slug := strings.TrimSuffix(name, ".json")
		if !slugPattern.MatchString(slug) {
			continue
		}
		out = append(out, slug)
	}
	return out, nil
}

// Exists 仅检查 slug 对应文件是否存在.
func (s *Store) Exists(slug string) bool {
	abs, err := s.pathFor(slug)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}

// Delete 删除 slug 对应文件. per-slug 串行化. 文件不存在 -> os.ErrNotExist.
func (s *Store) Delete(slug string) error {
	abs, err := s.pathFor(slug)
	if err != nil {
		return err
	}
	mu := s.slugLock(slug)
	mu.Lock()
	defer mu.Unlock()
	return os.Remove(abs)
}
