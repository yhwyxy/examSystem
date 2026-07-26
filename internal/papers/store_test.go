package papers

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 用 tmp 目录建个新 store, 加载 Task 0 fixture paper.json 拷贝进去
func newStoreWithFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// 拷贝 fixture paper.json
	src := filepath.Join(fixturesRoot(t), "paper.json")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go-contract-paper.json"), data, 0o644); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return s, "go-contract-paper"
}

// LoadEditable 正常加载
func TestStore_LoadEditable_OK(t *testing.T) {
	s, slug := newStoreWithFixture(t)
	doc, err := s.LoadEditable(slug)
	if err != nil {
		t.Fatalf("LoadEditable: %v", err)
	}
	if doc["paper_id"] != slug {
		t.Errorf("paper_id mismatch: got %v", doc["paper_id"])
	}
}

// Load 制不存在 -> os.ErrNotExist
func TestStore_LoadEditable_NotExist(t *testing.T) {
	s, _ := newStoreWithFixture(t)
	if _, err := s.LoadEditable("does-not-exist"); err == nil {
		t.Error("should return ErrNotExist")
	}
}

// 非法 slug (含 ../) 拒
func TestStore_InvalidSlug_Reject(t *testing.T) {
	s, _ := newStoreWithFixture(t)
	if _, err := s.LoadEditable("../etc/passwd"); err == nil {
		t.Error("../逃逸 slug 必拒")
	}
	if _, err := s.LoadEditable("foo/bar"); err == nil {
		t.Error("/ 分隔 slug 必拒")
	}
	if err := s.SaveEditable("a b c", Document{}); err == nil {
		t.Error("含空格 slug 必拒")
	}
}

// SaveEditable 校验失败 -> 不写文件
func TestStore_SaveEditable_Invalid(t *testing.T) {
	s, slug := newStoreWithFixture(t)
	if err := s.SaveEditable(slug, Document{
		"paper_id": slug,
		// name / questions 故意缺
	}); err == nil {
		t.Error("应为 ValidationError 失败")
	}
}

// SaveEditable 成功 -> 临时文件不残留 + List 能见
func TestStore_SaveEditable_OKAndList(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	doc := Document{
		"paper_id": "save-smoke",
		"name":     "Save Smoke",
		"exam_info": Document{
			"title":        "Title",
			"description":  "Desc",
			"total_score":  100.0,
			"passing_score": 60,
		},
		"questions": []any{
			Document{
				"id":      "q1",
				"type":    "single_choice",
				"question": "?",
				"score":    10.0,
				"options": []any{
					Document{"key": "A", "text": "a"},
					Document{"key": "B", "text": "b"},
				},
				"answer": "A",
			},
		},
	}
	if err := s.SaveEditable("save-smoke", doc); err != nil {
		t.Fatalf("SaveEditable: %v", err)
	}
	// tmp 文件残留检查
	if _, err := os.Stat(filepath.Join(dir, "save-smoke.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp 文件残留")
	}
	if !s.Exists("save-smoke") {
		t.Error("Exists say false after Save")
	}
	// List 能见
	slugs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != "save-smoke" {
		t.Errorf("List mismatch, got %v", slugs)
	}
	// round-trip
	d2, err := s.LoadEditable("save-smoke")
	if err != nil {
		t.Fatalf("LoadEditable round-trip: %v", err)
	}
	if d2["paper_id"] != "save-smoke" {
		t.Error("round-trip 数据不一致")
	}
}

// Delete
func TestStore_Delete(t *testing.T) {
	s, slug := newStoreWithFixture(t)
	if !s.Exists(slug) {
		t.Fatal("存入失败")
	}
	if err := s.Delete(slug); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Exists(slug) {
		t.Error("Delete 后 Exists 应 false")
	}
}

// per-slug mutex: 30 goroutine 串行写不出现竞态 (data race detector 触发)
func TestStore_PerSlugMutex_NoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	dir := t.TempDir()
	s, _ := NewStore(dir)
	const N = 30
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			doc := Document{
				"paper_id": "smoke", "name": "S",
				"exam_info": Document{"title":"T","description":"D","total_score":1.0,"passing_score":1},
				"questions": []any{
					Document{"id":"q1","type":"single_choice","question":"?","score":1.0,
						"options":[]any{Document{"key":"A","text":"a"},Document{"key":"B","text":"b"}},
						"answer":"A"},
				},
			}
			_ = s.SaveEditable("smoke"+_itoa(idx), doc)
		}(i)
	}
	// 同时另起 30 共用同一 slug "smoke-shared", 顺序不丢
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			doc := Document{
				"paper_id": "smoke-shared", "name": "S",
				"exam_info": Document{"title":"T","description":"D","total_score":1.0,"passing_score":1},
				"questions": []any{
					Document{"id":"q1","type":"single_choice","question":"?","score":1.0,
						"options":[]any{Document{"key":"A","text":"a"},Document{"key":"B","text":"b"}},
						"answer":"A"},
				},
			}
			_ = s.SaveEditable("smoke-shared", doc)
			// 共享 slug 必须轮流不丢
			if _, err := s.LoadEditable("smoke-shared"); err != nil {
				t.Errorf("shared load err: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

// _itoa 简易整数 -> 字符串, 避免用 strconv 的重复 import
func _itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [16]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
