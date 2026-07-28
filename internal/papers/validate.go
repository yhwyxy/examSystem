// Package papers -- validate.go 100% 复刻 Python question_loader.validate_questions.
//
// 校验逻辑 (对照 Python):
//  1. paper_id & name & questions 必填
//  2. paper_id slug 正则 ^[a-zA-Z0-9_\\-]+$ (与 Python slug 安全正则一致)
//  3. exam_info: total_score / passing_score / title / description 必填且类型正确
//  4. 每题: type 在 ALLOWED_QUESTION_TYPES 内
//     single_choice / multiple_choice / true_false: options 必裸 list, answer 形状合规
//  5. short_answer / composite: scoring_mode 在 ALLOWED_SCORING_MODES; code 子模
//     式有 code_language 与可选 allowed_languages 都落在 ALLOWED_CODE_LANGUAGES
//  6. composite 子题分值之和必等父题 (Python _validate_sub_questions)
//  7. scoring_points 表 (若存在) 必是 list[{text, score}]
//  8. score 为非负数, 不为 0/missing 时报错
//
// 校验失败返回 *ValidationError; 主要 cause 子表对齐 Python message.
package papers

import (
	"errors"
	"fmt"
	"strings"
)

// ValidationError 是单条校验失败. Code 用于 i18n / cause; Msg 人类可读.
type ValidationError struct {
	Code string
	Msg  string
}

func (e *ValidationError) Error() string {
	if e.Code == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// MultiError 是多个单条 ValidationError 的合包 (Python raise 在第一处不同, 我们一并返).
type MultiError struct{ Errors []*ValidationError }

func (m *MultiError) Error() string {
	if m == nil || len(m.Errors) == 0 {
		return "validation ok"
	}
	parts := make([]string, 0, len(m.Errors))
	for _, e := range m.Errors {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}

// IsHas 使 MultiError 兼容 errors.Is 检查 (仍然主要走 As).
//
//nolint:unused // 仅为 grep 可读
func (m *MultiError) Has() bool { return m != nil && len(m.Errors) > 0 }

// dedup 把同 Code/Msg 的重复项去重.
func (m *MultiError) dedup() {
	if m == nil || len(m.Errors) == 0 {
		return
	}
	seen := make(map[string]bool, len(m.Errors))
	out := m.Errors[:0]
	for _, e := range m.Errors {
		k := e.Code + "|" + e.Msg
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	m.Errors = out
}

// asCallerError 当 errors 数 0 返 nil, 否则返 m 本身 (实现 error 接).
func (m *MultiError) asCallerError() error {
	if m.Has() {
		return m
	}
	return nil
}

// add 追加一条 ValidationError.
func (m *MultiError) add(code, msg string) {
	m.Errors = append(m.Errors, &ValidationError{Code: code, Msg: msg})
}

var _ error = (*MultiError)(nil)
var _ = errors.Is

// ValidateDocument 把 paper JSON 加载后的 tree 全量校验, 一并返回 MultiError.
// 与 Python validate_questions(doc) 行为一致: 一遇到错即继续收集, 返回多重 error.
func ValidateDocument(doc Document) error {
	mu := &MultiError{}
	validateDocument(mu, doc)
	mu.dedup()
	return mu.asCallerError()
}

// validateDocument 是真正的实现, 收集 type/option/subjective_mode/composite sub
// 6 类错误, 与 Python validate_questions 1 对 1.
func validateDocument(mu *MultiError, doc Document) {
	if doc == nil {
		mu.add("missing", "document is nil")
		return
	}
	// 1. paper_id / name / questions 必填
	pid := stringOr(doc, "paper_id")
	if pid == "" {
		mu.add("missing", "paper_id is required")
	} else if !slugOK(pid) {
		mu.add("slug", "paper_id must match [A-Za-z0-9_-]+")
	}
	if name := stringOr(doc, "name"); name == "" {
		mu.add("missing", "name is required")
	}
	rawQs, ok := doc["questions"].([]any)
	if !ok {
		mu.add("missing", "questions must be a list")
		return
	}
	if len(rawQs) == 0 {
		mu.add("missing", "questions cannot be empty")
	}

	// 2. exam_info
	if ei, ok := doc["exam_info"].(map[string]any); ok {
		validateExamInfo(mu, ei)
	}

	// 3. 每题
	for i, x := range rawQs {
		q, ok := x.(map[string]any)
		if !ok {
			mu.add("type", fmt.Sprintf("q[%d]: must be object", i))
			continue
		}
		validateOne(mu, q, i)
	}
}

// slugOK 与 Python slug 安全正则一致 ^[A-Za-z0-9_-]+$.
func slugOK(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// validateExamInfo: total_score 必 num; passing_score 必 num; title/description 文本.
func validateExamInfo(mu *MultiError, ei map[string]any) {
	if ei == nil {
		mu.add("missing", "exam_info is nil")
		return
	}
	if tf := anyNumber(ei["total_score"]); tf == nil {
		mu.add("type", "exam_info.total_score must be number")
	}
	if ps := anyNumber(ei["passing_score"]); ps == nil {
		mu.add("type", "exam_info.passing_score must be number")
	}
	if stringOr(ei, "title") == "" {
		mu.add("missing", "exam_info.title required")
	}
	if stringOr(ei, "description") == "" {
		mu.add("missing", "exam_info.description required")
	}
}

// anyNumber: int / int64 / float64 -> *float64; 其它 -> nil.
func anyNumber(v any) *float64 {
	switch x := v.(type) {
	case int:
		f := float64(x)
		return &f
	case int64:
		f := float64(x)
		return &f
	case float64:
		return &x
	}
	return nil
}

// validateOne: 单题校验, 与 Python _validate_single_question 1 对 1.
func validateOne(mu *MultiError, q Question, i int) {
	prefix := fmt.Sprintf("q[%d]", i)
	id := stringOr(q, "id")
	if id == "" {
		mu.add("missing", prefix+": id required")
	}
	qtype := stringOr(q, "type")
	if _, ok := allowedTypes[qtype]; !ok {
		mu.add("type", prefix+": invalid type "+qtype)
		return // 后续校验依赖 type 合法
	}
	if score := anyNumber(q["score"]); score == nil {
		mu.add("missing", prefix+": score required")
	} else if *score <= 0 {
		mu.add("score", prefix+": score must be > 0")
	}
	if stringOr(q, "question") == "" {
		mu.add("missing", prefix+": question required")
	}
	// 按分支
	switch qtype {
	case "single_choice", "multiple_choice":
		validateOptionList(mu, q, prefix)
	case "true_false":
		validateTrueFalse(mu, q, prefix)
	case "short_answer", "essay":
		validateSubjectiveModeAndLanguage(mu, q, prefix)
		validateAnswerTextOrList(mu, q, prefix)
	case "composite":
		validateSubQuestions(mu, q, prefix)
	}
}

// validateOptionList: options 列表 + key 唯一 + answer 形状合规.
func validateOptionList(mu *MultiError, q Question, prefix string) {
	raw, ok := q["options"].([]any)
	if !ok || len(raw) == 0 {
		mu.add("missing", prefix+": options must be a non-empty list")
		return
	}
	keys := make(map[string]bool, len(raw))
	for j, o := range raw {
		om, ok := o.(map[string]any)
		if !ok {
			mu.add("type", fmt.Sprintf("%s.options[%d]: must be object", prefix, j))
			continue
		}
		k := stringOr(om, "key")
		if k == "" {
			mu.add("missing", fmt.Sprintf("%s.options[%d]: key required", prefix, j))
			continue
		}
		if keys[k] {
			mu.add("duplicate", fmt.Sprintf("%s.options[%d]: duplicate key %q", prefix, j, k))
		}
		keys[k] = true
		if stringOr(om, "text") == "" {
			mu.add("missing", fmt.Sprintf("%s.options[%d]: text required", prefix, j))
		}
	}
	// answer 校验
	ans, exists := q["answer"]
	if !exists {
		mu.add("missing", prefix+": answer required")
		return
	}
	switch stringOr(q, "type") {
	case "single_choice":
		if s, ok := ans.(string); ok {
			if !keys[s] {
				mu.add("answer", prefix+": single_choice answer key not in options")
			}
		} else {
			mu.add("type", prefix+": single_choice answer must be string")
		}
	case "true_false":
		if _, ok := ans.(bool); !ok {
			// 允许字符串 "true"/"false"/"正确"/"错误"
			if s, ok := ans.(string); !ok || (s != "true" && s != "false" && s != "正确" && s != "错误") {
				mu.add("type", prefix+": true_false answer must be bool or 正确/错误")
			}
		}
	case "multiple_choice":
		arr, ok := ans.([]any)
		if !ok {
			mu.add("type", prefix+": multiple_choice answer must be list")
			return
		}
		for j, x := range arr {
			s, ok := x.(string)
			if !ok || !keys[s] {
				mu.add("answer", fmt.Sprintf("%s.answer[%d]: %v not in options", prefix, j, x))
			}
		}
	}
}

func validateTrueFalse(mu *MultiError, q Question, prefix string) {
	ans, exists := q["answer"]
	if !exists {
		mu.add("missing", prefix+": answer required")
		return
	}
	if _, ok := ans.(bool); ok {
		return
	}
	if s, ok := ans.(string); !ok || (s != "true" && s != "false" && s != "正确" && s != "错误") {
		mu.add("type", prefix+": true_false answer must be bool or 正确/错误")
	}
}

// validateSubjectiveModeAndLanguage: scoring_mode 合法 + code 子模有 code_language,
// 可选 allowed_languages 都合法. 与 Python _validate_subjective_mode_and_language 1 对 1.
func validateSubjectiveModeAndLanguage(mu *MultiError, q Question, prefix string) {
	sm := stringOr(q, "scoring_mode")
	if sm == "" {
		return // option, 不强制 subjective 题 (与 Python 行为一致)
	}
	if _, ok := allowedScoringModes[sm]; !ok {
		mu.add("scoring_mode", prefix+": invalid scoring_mode "+sm)
		return
	}
	// sql / calculation 无 code_language / allowed_languages 限制
	if sm == "sql" || sm == "calculation" {
		return
	}
	// code 模式必填 code_language, allowed_languages 全在 ALLOWED_CODE_LANGUAGES
	if sm == "code" {
		lang := stringOr(q, "code_language")
		if lang == "" {
			mu.add("code_language", prefix+": scoring_mode=code requires code_language")
		} else if _, ok := allowedCodeLanguages[lang]; !ok {
			mu.add("code_language", prefix+": invalid code_language "+lang)
		}
		if al := anyLanguages(q, "allowed_languages"); len(al) > 0 {
			for _, l := range al {
				if _, ok := allowedCodeLanguages[l]; !ok {
					mu.add("code_language", prefix+": invalid allowed_language "+l)
				}
			}
		}
	}
	// text 模式无言/language 约束
}

// validateAnswerTextOrList: short_answer answer 必填非空 (string 或 list[string]).
func validateAnswerTextOrList(mu *MultiError, q Question, prefix string) {
	ans, ok := q["answer"]
	if !ok {
		mu.add("missing", prefix+": answer required")
		return
	}
	switch a := ans.(type) {
	case string:
		if a == "" {
			mu.add("missing", prefix+": answer must be non-empty")
		}
	case []any:
		if len(a) == 0 {
			mu.add("missing", prefix+": answer list cannot be empty")
		}
	default:
		mu.add("type", prefix+": answer must be string or list")
	}
}

// validateSubQuestions: 复合题子题分值之和等于父题分值, 子题字段也合规.
// 与 Python _validate_sub_questions 1 对 1.
func validateSubQuestions(mu *MultiError, q Question, prefix string) {
	parentScore := anyNumber(q["score"])
	subs := subquestions(q)
	if len(subs) == 0 {
		mu.add("missing", prefix+": composite requires subquestions")
		return
	}
	var sum float64
	hasSumError := false
	for j, s := range subs {
		sp := fmt.Sprintf("%s.sub[%d]", prefix, j)
		if s == nil {
			mu.add("type", sp+": nil subquestion")
			continue
		}
		if id := stringOr(s, "id"); id == "" {
			mu.add("missing", sp+": subquestion id required")
		}
		if q2 := stringOr(s, "question"); q2 == "" {
			mu.add("missing", sp+": subquestion question required")
		}
		if sc := anyNumber(s["score"]); sc == nil {
			mu.add("missing", sp+": subquestion score required")
			hasSumError = true
		} else if *sc <= 0 {
			mu.add("score", sp+": subquestion score must be > 0")
			hasSumError = true
		} else {
			sum += *sc
		}
		// 子题也校 objective 类型 / subjective mode&language
		if stype := stringOr(s, "type"); stype == "" {
			// 子题无 type -> 默认 short_answer (与 Python 一致)
			validateSubjectiveModeAndLanguage(mu, s, sp)
			validateAnswerTextOrList(mu, s, sp)
		} else if _, ok := allowedTypes[stype]; !ok {
			mu.add("type", sp+": invalid subquestion type "+stype)
		} else {
			validateOne(mu, s, j) // 复用 objective 分支
		}
	}
	// 分值之和 == 父题
	if parentScore != nil && !hasSumError && sum != *parentScore {
		mu.add("score",
			fmt.Sprintf("%s: sum of subquestion scores (%g) != parent score (%g)",
				prefix, sum, *parentScore))
	}
}
