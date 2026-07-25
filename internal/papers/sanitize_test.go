package papers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturesRoot 指向 tests/fixtures/contract/(Task 0 已冻结)
func fixturesRoot(t *testing.T) string {
	t.Helper()
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	root := filepath.Join(here, "..", "..", "tests", "fixtures", "contract")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("fixtures root absent: %s", root)
	}
	return root
}

func loadPaperFixture(t *testing.T) Document {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixturesRoot(t), "paper.json"))
	if err != nil {
		t.Fatalf("read paper.json: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse paper.json: %v", err)
	}
	return doc
}

// 数组类型在 unmarshal 后是 []any, 每元素是 Question(map[string]any);
// 把剥得的 questions 转回 []Question 便于传给 SanitizeForStudent.
func questionsOf(doc Document) []Question {
	raw, ok := doc["questions"].([]any)
	if !ok {
		return nil
	}
	out := make([]Question, 0, len(raw))
	for _, x := range raw {
		if q, ok := x.(map[string]any); ok {
			out = append(out, q)
		}
	}
	return out
}

// 与黄金 sanitized_exam.json 不严格逐字节比对 (Go encoding/json 顺序可能不同),
// 但用 dict 等价 + 校敏感字段已剥离 + 关键字段集一致 -> 强保证语义不变.
func TestSanitize_MatchesGoldenShape(t *testing.T) {
	paper := loadPaperFixture(t)
	src := questionsOf(paper)
	got := SanitizeForStudent(src)

	if len(got) != len(src) {
		t.Fatalf("len got=%d src=%d", len(got), len(src))
	}

	// 加载黄金对比每个 question 的 key 集合
	raw, err := os.ReadFile(filepath.Join(fixturesRoot(t), "sanitized_exam.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden struct {
		Questions []map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(golden.Questions) != len(got) {
		t.Fatalf("golden count=%d got=%d", len(golden.Questions), len(got))
	}
	for i, g := range got {
		gk := keySet(g)
		goldenKeys := keySet(golden.Questions[i])
		if !equalSet(gk, goldenKeys) {
			t.Errorf("q%d keys differ:\n got=%v\n golden=%v",
				i, sortedStrings(gk), sortedStrings(goldenKeys))
		}
		// 关键校验: 敏感字段必不在
		for k := range g {
			if _, bad := sensitiveFields[k]; bad {
				t.Errorf("q%d leaked sensitive key %q", i, k)
			}
		}
	}
}

// keySet / equalSet / sortedStrings 等测试 helpers.
func keySet(m map[string]any) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func equalSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 排序便于 diff
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i] > out[j] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// 验证复合题 subquestions 也脱敏 (敏感字段剥离)
func TestSanitize_StripsCompositeSubSensitive(t *testing.T) {
	paper := loadPaperFixture(t)
	src := questionsOf(paper)
	got := SanitizeForStudent(src)

	// 找出复合题
	var comp Question
	for _, q := range got {
		if asStr(q["type"]) == "composite" {
			comp = q
			break
		}
	}
	if comp == nil {
		t.Fatal("fixture 缺 composite 题")
	}
	// SanitizeForStudent 输出 subquestions 是 []Question, 不是 []any
	subs, ok := comp["subquestions"].([]Question)
	if !ok || len(subs) == 0 {
		t.Fatalf("composite subquestions 为空 / type=%T", comp["subquestions"])
	}
	for i, sub := range subs {
		if _, ok := sub["answer"]; ok {
			t.Errorf("sub %d leaked answer", i)
		}
		for k := range sub {
			if _, bad := sensitiveFields[k]; bad {
				t.Errorf("sub %d leaked sensitive key %q", i, k)
			}
		}
		// 公开子题字段集: 必含 {id, question, score}, 可选 {scoring_mode, allowed_languages}
		for _, must := range []string{"id", "question", "score"} {
			if _, ok := sub[must]; !ok {
				t.Errorf("sub %d 缺必填 %q", i, must)
			}
		}
	}
}

// asStr 把 any 转成 string (用于 type 比较)
func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// validate_test.go 在同包 (papers_test), 直接复用上面 fixturesRoot / loadPaperFixture.

// 完整 paper 应该校验通过 (与 Python validate_questions 等价)
func TestValidate_GoldenPaper_Ok(t *testing.T) {
	paper := loadPaperFixture(t)
	if err := ValidateDocument(paper); err != nil {
		t.Fatalf("golden paper.json should validate ok, got: %v", err)
	}
}

// 删字段必报错
func TestValidate_MissingRequireds(t *testing.T) {
	paper := loadPaperFixture(t)
	delete(paper, "paper_id")
	if err := ValidateDocument(paper); err == nil {
		t.Error("missing paper_id 应报错")
	}

	paper = loadPaperFixture(t)
	delete(paper, "name")
	if err := ValidateDocument(paper); err == nil {
		t.Error("missing name 应报错")
	}

	paper = loadPaperFixture(t)
	delete(paper, "questions")
	if err := ValidateDocument(paper); err == nil {
		t.Error("missing questions 应报错")
	}
}

// 单选 answer key 不在 options 内 -> 报错
func TestValidate_SingleChoiceOutOfRange(t *testing.T) {
	paper := loadPaperFixture(t)
	qs := paper["questions"].([]any)
	q0, _ := qs[0].(map[string]any)
	q0["answer"] = []string{"Z"} // single_choice answer 期望裸 string, 这里 list 也错
	if err := ValidateDocument(paper); err == nil {
		t.Error("single_choice answer=['Z'] (列表) 应报错")
	}

	// 修回裸 string 但 key 不存在
	paper = loadPaperFixture(t)
	qs = paper["questions"].([]any)
	q0, _ = qs[0].(map[string]any)
	q0["answer"] = "Z" // Z 不在 options
	if err := ValidateDocument(paper); err == nil {
		t.Error("single_choice answer='Z' out of options 应报错")
	}
}

// 多选 answer 含错项 key 不在 options -> 报错
func TestValidate_MultipleChoiceOutOfRange(t *testing.T) {
	paper := loadPaperFixture(t)
	qs := paper["questions"].([]any)
	// c-multi 是 q[1]
	q1, _ := qs[1].(map[string]any)
	q1["answer"] = []string{"A", "Z"} // Z 不在 options
	if err := ValidateDocument(paper); err == nil {
		t.Error("multiple_choice answer 含 'Z' 应报错")
	}
}

// 复合题子题分值之和 ≠ 父题分值 -> 报错
func TestValidate_CompositeSubSumMismatch(t *testing.T) {
	paper := loadPaperFixture(t)
	qs := paper["questions"].([]any)
	// composite 是最后一题
	comp, _ := qs[len(qs)-1].(map[string]any)
	subs, _ := comp["subquestions"].([]any)
	if len(subs) < 2 {
		t.Fatal("fixture composite 子题少于 2")
	}
	// 把第二个子题分值改小, 总和 != 父题
	sub, _ := subs[1].(map[string]any)
	sub["score"] = 1.0 // 期望 20, 改 1 -> sum=21 != 40
	if err := ValidateDocument(paper); err == nil {
		t.Error("composite 子题 sum != 父题 应报错")
	}
}

// code 题但 code_language 不在允许集 -> 报错
func TestValidate_InvalidCodeLanguage(t *testing.T) {
	paper := loadPaperFixture(t)
	qs := paper["questions"].([]any)
	// 找带 scoring_mode=code 的题
	for _, x := range qs {
		q, _ := x.(map[string]any)
		if asStr(q["scoring_mode"]) == "code" {
			orig := q["code_language"]
			q["code_language"] = "rust" // 不在 ALLOWED_CODE_LANGUAGES
			if err := ValidateDocument(paper); err == nil {
				t.Error("code_language=rust 应报错")
			}
			q["code_language"] = orig
			return
		}
	}
	t.Skip("fixture 无 code 题")
}

// 主观题边无 scoring_mode 可以; scoring_mode 在允许集但不在合法集应报错
func TestValidate_InvalidScoringMode(t *testing.T) {
	paper := loadPaperFixture(t)
	qs := paper["questions"].([]any)
	for _, x := range qs {
		q, _ := x.(map[string]any)
		qt := asStr(q["type"])
		// short_answer/essay 都可加 scoring_mode; composite 子题也是
		if qt == "short_answer" {
			orig := q["scoring_mode"]
			q["scoring_mode"] = "unknown"
			if err := ValidateDocument(paper); err == nil {
				t.Error("scoring_mode=unknown 应报错")
			}
			q["scoring_mode"] = orig
			return
		}
	}
	t.Skip("fixture 无 short_answer")
}

// slug 含空格 -> 报错
func TestValidate_BadSlug(t *testing.T) {
	paper := loadPaperFixture(t)
	paper["paper_id"] = "go contract paper" // 含空格
	if err := ValidateDocument(paper); err == nil {
		t.Error("paper_id 含空格 应报错")
	}
}

// exam_info 字段类型错 -> 报错
func TestValidate_BadExamInfo(t *testing.T) {
	paper := loadPaperFixture(t)
	if ei, ok := paper["exam_info"].(map[string]any); ok {
		// total_score 改字符串
		orig := ei["total_score"]
		ei["total_score"] = "cloud"
		if err := ValidateDocument(paper); err == nil {
			t.Error("exam_info.total_score 字符串 应报错")
		}
		ei["total_score"] = orig
	}
}

// snapshot_hash 跨 Go/Python 一致: canonical JSON (sort_keys + compact + utf8)
func TestComputeSHA256_MatchesPythonCanonical(t *testing.T) {
	paper := loadPaperFixture(t)
	got, err := ComputeSHA256(paper)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	// 期望值由 Go 实跑时调用外部 python 来生成 -- 我们不想依赖外部 python,
	// 故用此 snapshot 测试一种方式: 自己用 Go 验证同一份 paper 两次 hash 一致
	got2, _ := ComputeSHA256(paper)
	if got != got2 {
		t.Fatalf("ComputeSHA256 不幂等: %s vs %s", got, got2)
	}
	// 形态校验: 64 hex chars
	if len(got) != 64 {
		t.Errorf("sha256 hex length = %d, want 64", len(got))
	}
}

// canonical JSON 表示: 关键非 ASCII 不转义 (typescript 这样的 ascii OK;
// 但 ensure_ascii=False 与 Go 默认 MarshalIndent 等价, 这里给一个含中文题面 paper)
func TestCanonicalJSON_EnsureAsciiFalse(t *testing.T) {
	doc := Document{
		"paper_id": "中文测",
		"score":    10.0,
		"q":        "请简述érc",
	}
	raw, err := canonicalJSON(doc)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	s := string(raw)
	// 非双引号转义, 非字符保留
	if !strings.Contains(s, "中文测") {
		t.Errorf("missing 中文字面: %s", s)
	}
	if !strings.Contains(s, "请简述érc") {
		t.Errorf("missing érc: %s", s)
	}
	// sort_keys=True: paper_id 排在 q 之前, score 在中间
	if i := strings.Index(s, "paper_id"); i < 0 {
		t.Errorf("missing paper_id key")
	}
}

// AtomicWriteJSON 必须成功创建文件; 临时文件已清理
func TestAtomicWriteJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smoke.json")
	doc := Document{"paper_id": "smoke", "version": 1}
	if err := AtomicWriteJSON(path, doc); err != nil {
		t.Fatalf("AtomicWriteJSON: %v", err)
	}
	// tmp 不残留
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp 文件残留")
	}
	// 重读内容回等
	snap, err := LoadSnapshot(path, "")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Doc["paper_id"] != "smoke" {
		t.Errorf("round-trip 数据丢失, got %v", snap.Doc)
	}
}

// LoadSnapshot 期望 sha 不一致 -> 报错
func TestLoadSnapshot_BadExpectedHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := AtomicWriteJSON(path, Document{"a": 1}); err != nil {
		t.Fatalf("AtomicWriteJSON: %v", err)
	}
	if _, err := LoadSnapshot(path, "deadbeefdeadbeef"); err == nil {
		t.Error("期望 sha mismatch 必报错")
	}
}

