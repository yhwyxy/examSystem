// Package papers 把 Python backend.question_loader + paper_store 在 Go 端复刻:
//
//   - types.go: 共享类型 (Document / Question / Answer / Option) + 允许集合常量
//   - sanitize.go: SanitizeForStudent 100% 对齐 Python sanitize_for_student
//   - validate.go: ValidateDocument 100% 对齐 Python validate_questions +
//                  slug 安全正则 + 路径防逃逸
//   - snapshot.go: LoadSnapshot(path, sha256) 必须校哈希
//   - store.go: 文件存储 + 原子写 + per-slug mutex
//
// 设计要点 (Task 4):
//   * Document 是 JSON 加载钝化的 map 形态, 字段松: 与 Python 端 dict 等价,
//     保留所有原 key (含 exam_info / questions) ; 不用结构体硬绑 schema,
//     便于向后兼容 Python paper.json 任意版本.
//   * 路径安全: 所有相对路径以可配 data root 解析, 再用 filepath.Rel 二次确认无逃逸.
//   * 写入: 同目录 tmp 文件 + fsync + rename; per-slug sync.Mutex 串行化写入,
//     避免 paper 同时被两线程改写不一致.
package papers

import "encoding/json"

// Document 是 paper JSON 加载后的钝化形态. 保留原始 JSON 键 (大小写敏感),
// 与 Python 端 dict[str, Any] 等价. 不做强制类型断言, 由 validator 做结构判断.
type Document = map[string]any

// Question 是 Document["questions"] 的元素. 与 Document 同样钝化.
type Question = map[string]any

// Subquestion 是 composite 题 subquestions 数组元素. 同钝化.
type Subquestion = map[string]any

// Option 是 single_choice / multiple_choice options 数组元素; 同钝化.
type Option = map[string]any

// 允许集合常量 -- 与 Python ALLOWED_TYPES / ALLOWED_SCORING_MODES /
// ALLOWED_CODE_LANGUAGES 完全对齐.
var (
	allowedTypes = map[string]struct{}{
		"single_choice":   {},
		"multiple_choice": {},
		"true_false":      {},
		"short_answer":    {},
		"composite":       {},
		"essay":           {},
	}
	objectiveTypes = map[string]struct{}{
		"single_choice":   {},
		"multiple_choice": {},
		"true_false":      {},
	}
	subjectiveTypes = map[string]struct{}{
		"short_answer": {},
		"essay":         {},
		"composite":     {},
	}

	allowedScoringModes = map[string]struct{}{
		"text":          {},
		"sql":           {},
		"code":          {},
		"calculation":   {},
		"enumeration":   {},
		"translation":   {},
		"table":         {},
		"ledger":        {},
		"case_analysis": {},
	}

	allowedCodeLanguages = map[string]struct{}{
		"python":     {},
		"java":       {},
		"javascript": {},
		"typescript": {},
		"go":         {},
		"c":          {},
		"cpp":        {},
		"csharp":     {},
		"sql":        {},
		"bash":       {},
		"shell":      {},
	}

	sensitiveFields = map[string]struct{}{
		"answer":                       {},
		"answers_by_language":          {},
		"calculation":                  {},
		"scoring_points":               {},
		"scoring_points_by_language":   {},
		"scoring_rubric":               {},
	}
)

// AsJSON 把 Document 序列化为规范化 JSON (缩进 2), 用于快照写盘前的一致性.
func AsJSON(doc Document) ([]byte, error) {
	return json.MarshalIndent(doc, "", "  ")
}
