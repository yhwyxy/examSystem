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
	"math"
	"strings"
	"unicode/utf8"
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

// ValidateStandaloneQuestion applies the same question contract used by paper files,
// without requiring callers to manufacture a paper document.
func ValidateStandaloneQuestion(question Question) error {
	if question == nil {
		return fmt.Errorf("question is nil")
	}
	score := anyNumber(question["score"])
	if score == nil {
		return fmt.Errorf("question score is required")
	}
	return ValidateDocument(Document{
		"paper_id": "question-bank-validation",
		"name":     "Question bank validation",
		"exam_info": map[string]any{
			"title":         "Question bank validation",
			"description":   "Question bank validation",
			"total_score":   *score,
			"passing_score": 0,
		},
		"questions": []any{question},
	})
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
	} else if *score <= 0 || math.IsNaN(*score) || math.IsInf(*score, 0) {
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
	validateScoringPoints(mu, q, prefix)
	validateSQLTestDatasets(mu, q, prefix)
	validateCodeTestCases(mu, q, prefix)
}

func validateCodeTestCases(mu *MultiError, q Question, prefix string) {
	rawCases, exists := q["code_test_cases"]
	if !exists {
		return
	}
	cases, ok := rawCases.([]any)
	if !ok {
		mu.add("code_execution", prefix+": code_test_cases must be a list")
		return
	}
	if len(cases) == 0 {
		return
	}
	if len(cases) < 2 || len(cases) > 12 {
		mu.add("code_execution", prefix+": code_test_cases must contain 2 to 12 cases")
	}
	if stringOr(q, "scoring_mode") != "code" {
		mu.add("code_execution", prefix+": code_test_cases require scoring_mode=code")
	}
	if stringOr(q, "code_language") == "" {
		mu.add("code_execution", prefix+": code_test_cases require code_language")
	}
	names := make(map[string]bool, len(cases))
	visibilities := make(map[string]bool, 2)
	for i, rawCase := range cases {
		path := fmt.Sprintf("%s.code_test_cases[%d]", prefix, i)
		item, ok := rawCase.(map[string]any)
		if !ok {
			mu.add("code_execution", path+": test case must be an object")
			continue
		}
		name := stringOr(item, "name")
		if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 80 {
			mu.add("code_execution", path+": name must be a non-empty string up to 80 characters")
		}
		nameKey := strings.ToLower(strings.TrimSpace(name))
		if names[nameKey] {
			mu.add("code_execution", path+": test case names must be unique")
		}
		names[nameKey] = true
		visibility := stringOr(item, "visibility")
		if visibility != "public" && visibility != "hidden" {
			mu.add("code_execution", path+": visibility must be public or hidden")
		} else {
			visibilities[visibility] = true
		}
		for _, key := range []string{"input", "expected_output"} {
			value, ok := item[key].(string)
			if !ok || utf8.RuneCountInString(value) > 16384 || strings.ContainsRune(value, 0) {
				mu.add("code_execution", path+": input and expected_output must be strings up to 16384 characters")
			}
		}
	}
	if !visibilities["public"] || !visibilities["hidden"] {
		mu.add("code_execution", prefix+": code_test_cases must include public and hidden cases")
	}
}

func validateSQLTestDatasets(mu *MultiError, q Question, prefix string) {
	mutationTables := validateSQLExecutionPolicy(mu, q, prefix)
	dialect, hasDialect := q["sql_dialect"]
	if hasDialect {
		name, ok := dialect.(string)
		if !ok || strings.TrimSpace(name) == "" || len(name) > 32 {
			mu.add("sql_execution", prefix+": sql_dialect must be a non-empty string")
		}
	}
	rawDatasets, exists := q["sql_test_datasets"]
	if !exists {
		return
	}
	datasets, ok := rawDatasets.([]any)
	if !ok {
		mu.add("sql_execution", prefix+": sql_test_datasets must be a list")
		return
	}
	if len(datasets) > 20 {
		mu.add("sql_execution", prefix+": sql_test_datasets must contain at most 20 datasets")
	}
	if len(datasets) > 0 && !hasDialect {
		mu.add("sql_execution", prefix+": sql_dialect is required when sql_test_datasets are configured")
	}
	datasetNames := make(map[string]bool, len(datasets))
	for i, rawDataset := range datasets {
		path := fmt.Sprintf("%s.sql_test_datasets[%d]", prefix, i)
		dataset, ok := rawDataset.(map[string]any)
		if !ok {
			mu.add("sql_execution", path+": dataset must be an object")
			continue
		}
		name := stringOr(dataset, "name")
		if strings.TrimSpace(name) == "" || len(name) > 80 {
			mu.add("sql_execution", path+": name must be a non-empty string up to 80 characters")
		}
		if datasetNames[strings.ToLower(name)] {
			mu.add("sql_execution", path+": dataset names must be unique")
		}
		datasetNames[strings.ToLower(name)] = true
		visibility := stringOr(dataset, "visibility")
		if visibility != "" && visibility != "public" && visibility != "hidden" {
			mu.add("sql_execution", path+": visibility must be public or hidden")
		}
		rawTables, ok := dataset["tables"]
		if !ok {
			rawTables = []any{}
		}
		tables, ok := rawTables.([]any)
		if !ok || len(tables) > 50 {
			mu.add("sql_execution", path+": tables must be a list with at most 50 entries")
			continue
		}
		tableNames := make(map[string]bool, len(tables))
		totalRows, totalCells := 0, 0
		for tableIndex, rawTable := range tables {
			tablePath := fmt.Sprintf("%s.tables[%d]", path, tableIndex)
			table, ok := rawTable.(map[string]any)
			if !ok {
				mu.add("sql_execution", tablePath+": table must be an object")
				continue
			}
			tableName := stringOr(table, "name")
			if !validSQLIdentifier(tableName) || tableNames[strings.ToLower(tableName)] {
				mu.add("sql_execution", tablePath+": table name must be a unique SQL identifier")
			}
			tableNames[strings.ToLower(tableName)] = true
			rawColumns, ok := table["columns"].([]any)
			if !ok || len(rawColumns) == 0 || len(rawColumns) > 50 {
				mu.add("sql_execution", tablePath+": columns must be a non-empty list with at most 50 entries")
				continue
			}
			columnNames := make(map[string]bool, len(rawColumns))
			for columnIndex, rawColumn := range rawColumns {
				columnPath := fmt.Sprintf("%s.columns[%d]", tablePath, columnIndex)
				column, ok := rawColumn.(map[string]any)
				if !ok {
					mu.add("sql_execution", columnPath+": column must be an object")
					continue
				}
				columnName := stringOr(column, "name")
				if !validSQLIdentifier(columnName) || columnNames[strings.ToLower(columnName)] {
					mu.add("sql_execution", columnPath+": column name must be a unique SQL identifier")
				}
				columnNames[strings.ToLower(columnName)] = true
				columnType := strings.ToUpper(stringOr(column, "type"))
				if columnType == "" {
					columnType = "TEXT"
				}
				if columnType != "INTEGER" && columnType != "REAL" && columnType != "TEXT" && columnType != "BLOB" && columnType != "NUMERIC" {
					mu.add("sql_execution", columnPath+": unsupported SQLite column type")
				}
			}
			rawRows, ok := table["rows"]
			if !ok {
				rawRows = []any{}
			}
			rows, ok := rawRows.([]any)
			if !ok || len(rows) > 1000 {
				mu.add("sql_execution", tablePath+": rows must be a list with at most 1000 entries")
				continue
			}
			totalRows += len(rows)
			totalCells += len(rows) * len(rawColumns)
			for rowIndex, rawRow := range rows {
				row, ok := rawRow.([]any)
				if !ok || len(row) != len(rawColumns) {
					mu.add("sql_execution", fmt.Sprintf("%s.rows[%d]: row width must match columns", tablePath, rowIndex))
					continue
				}
				for _, cell := range row {
					if !validSQLTestCell(cell) {
						mu.add("sql_execution", tablePath+": cells must be finite JSON scalars up to 4096 characters")
					}
				}
			}
		}
		for _, table := range mutationTables {
			if !tableNames[table] {
				mu.add("sql_execution", path+": allowed mutation table must exist in every fixture: "+table)
			}
		}
		if totalRows > 2000 || totalCells > 20000 {
			mu.add("sql_execution", path+": dataset exceeds row or cell limits")
		}
	}
}

func validSQLIdentifier(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for i, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (i > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func validSQLTestCell(value any) bool {
	switch cell := value.(type) {
	case nil, bool, int, int64:
		return true
	case float64:
		return !math.IsNaN(cell) && !math.IsInf(cell, 0)
	case string:
		return utf8.RuneCountInString(cell) <= 4096
	default:
		return false
	}
}

func validateScoringPoints(mu *MultiError, q Question, prefix string) {
	validatePointList := func(raw any, path string) {
		points, ok := raw.([]any)
		if !ok {
			mu.add("scoring_points", path+": must be a list")
			return
		}
		ids := map[string]bool{}
		for i, rawPoint := range points {
			p, ok := rawPoint.(map[string]any)
			if !ok {
				mu.add("scoring_points", path+": point must be an object")
				continue
			}
			id := stringOr(p, "id")
			if id == "" {
				id = fmt.Sprintf("p%d", i+1) // Legacy points without an ID.
			}
			if ids[id] {
				mu.add("scoring_points", path+": duplicate point ID "+id)
			}
			ids[id] = true
			pointText := stringOr(p, "text")
			if strings.TrimSpace(pointText) == "" {
				mu.add("scoring_points", path+": point text required")
			} else if utf8.RuneCountInString(pointText) > 500 {
				mu.add("scoring_points", path+": point text must contain at most 500 characters")
			}
			if score := anyNumber(p["score"]); score == nil || *score < 0 || math.IsNaN(*score) || math.IsInf(*score, 0) {
				mu.add("scoring_points", path+": point score must be finite and nonnegative")
			}
			for _, key := range []string{"required", "critical"} {
				if value, exists := p[key]; exists {
					if _, ok := value.(bool); !ok {
						mu.add("scoring_points", path+": "+key+" must be boolean")
					}
				}
			}
			if policy, exists := p["conflict_policy"]; exists && policy != "point_zero" && policy != "cap_total" && policy != "zero_total" {
				mu.add("scoring_points", path+": invalid conflict_policy")
			}
			if ratio, exists := p["conflict_score_cap_ratio"]; exists {
				if cap := anyNumber(ratio); cap == nil || *cap < 0 || *cap > 1 || math.IsNaN(*cap) || math.IsInf(*cap, 0) {
					mu.add("scoring_points", path+": conflict_score_cap_ratio must be in [0, 1]")
				}
			}
			for _, key := range []string{"accepted_expressions", "key_conditions"} {
				rawValues, exists := p[key]
				if !exists {
					continue
				}
				values, ok := rawValues.([]any)
				if !ok {
					mu.add("scoring_points", path+": "+key+" must be a list")
					continue
				}
				if len(values) > 30 {
					mu.add("scoring_points", path+": "+key+" must contain at most 30 entries")
				}
				seen := make(map[string]bool, len(values))
				for _, rawValue := range values {
					value, ok := rawValue.(string)
					if !ok || strings.TrimSpace(value) == "" {
						mu.add("scoring_points", path+": "+key+" entries must be non-empty strings")
						continue
					}
					value = strings.TrimSpace(value)
					if utf8.RuneCountInString(value) > 500 {
						mu.add("scoring_points", path+": "+key+" entries must contain at most 500 characters")
					}
					if seen[value] {
						mu.add("scoring_points", path+": "+key+" entries must be unique")
					}
					seen[value] = true
				}
			}
			if rawEvidence, exists := p["source_evidence"]; exists {
				evidence, ok := rawEvidence.(string)
				if !ok || strings.TrimSpace(evidence) == "" {
					mu.add("scoring_points", path+": source_evidence must be a non-empty string")
				} else if utf8.RuneCountInString(evidence) > 2000 {
					mu.add("scoring_points", path+": source_evidence must contain at most 2000 characters")
				}
			}
		}
	}
	if points, exists := q["scoring_points"]; exists {
		validatePointList(points, prefix+".scoring_points")
	}
	if raw, exists := q["scoring_points_by_language"]; exists {
		values, ok := raw.(map[string]any)
		if !ok {
			mu.add("scoring_points", prefix+": scoring_points_by_language must be an object")
			return
		}
		for language, points := range values {
			if _, ok := allowedCodeLanguages[language]; !ok {
				mu.add("code_language", prefix+": invalid scoring point language "+language)
			}
			validatePointList(points, prefix+".scoring_points_by_language."+language)
		}
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
// 开放题: answer="" 且 scoring_points 为空列表时跳过校验 (直接走人工复核).
func validateAnswerTextOrList(mu *MultiError, q Question, prefix string) {
	ans, ok := q["answer"]
	if !ok {
		mu.add("missing", prefix+": answer required")
		return
	}
	switch a := ans.(type) {
	case string:
		if a == "" {
			// 开放题: 无参考答案, 无评分点, 全凭人工复核
			if isOpenEnded(q) {
				return
			}
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

// isOpenEnded 判断是否为开放题 (无评分点的纯人工复核题).
func isOpenEnded(q Question) bool {
	raw, ok := q["scoring_points"]
	if !ok {
		return false
	}
	pts, ok := raw.([]any)
	if !ok {
		return false
	}
	return len(pts) == 0
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
			validateScoringPoints(mu, s, sp)
			validateSQLTestDatasets(mu, s, sp)
			validateCodeTestCases(mu, s, sp)
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
