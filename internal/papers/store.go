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

type Store struct {
	root string
	mu   sync.Map // by slug -> *sync.Mutex
}

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

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

func (s *Store) Root() string { return s.root }

func (s *Store) Exists(slug string) bool {
	abs, err := s.pathFor(slug)
	if err != nil {
		return false
	}
	fi, err := os.Stat(abs)
	return err == nil && !fi.IsDir()
}

func (s *Store) slugLock(slug string) *sync.Mutex {
	v, _ := s.mu.LoadOrStore(slug, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Store) pathFor(slug string) (string, error) {
	if !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("papers.Store: invalid slug %q", slug)
	}
	abs := filepath.Join(s.root, slug+".json")
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("papers.Store: path escapes root: %q", slug)
	}
	return abs, nil
}

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

// === 关键修复：原子写 + 临时文件 ===
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

func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		slug := strings.TrimSuffix(entry.Name(), ".json")
		if !slugPattern.MatchString(slug) {
			continue
		}
		doc, err := s.LoadEditable(slug)
		if err == nil && ValidateDocument(doc) == nil {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

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

	tmpPath := abs + ".tmp"
	if err := AtomicWriteJSON(tmpPath, doc); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, abs); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
