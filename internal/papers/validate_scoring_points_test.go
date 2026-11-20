package papers

import (
	"strings"
	"testing"
)

func TestValidateStandaloneQuestionScoringPointDescriptors(t *testing.T) {
	question := Question{
		"id": "q1", "type": "single_choice", "question": "答案是什么？", "score": 2.0,
		"options": []any{map[string]any{"key": "A", "text": "甲"}, map[string]any{"key": "B", "text": "乙"}},
		"answer":  "A",
		"scoring_points": []any{map[string]any{
			"id": "p1", "text": "识别正确选项", "score": 2.0,
			"accepted_expressions": []any{"选项甲", "A"}, "key_conditions": []any{"指出甲"},
		}},
	}
	if err := ValidateStandaloneQuestion(question); err != nil {
		t.Fatalf("valid scoring point descriptors rejected: %v", err)
	}
}

func TestValidateStandaloneQuestionRejectsInvalidScoringPointDescriptors(t *testing.T) {
	base := func(values any) Question {
		return Question{
			"id": "q1", "type": "single_choice", "question": "答案是什么？", "score": 2.0,
			"options": []any{map[string]any{"key": "A", "text": "甲"}, map[string]any{"key": "B", "text": "乙"}},
			"answer":  "A",
			"scoring_points": []any{map[string]any{
				"id": "p1", "text": "识别正确选项", "score": 2.0,
				"accepted_expressions": values,
			}},
		}
	}
	tests := []struct {
		name   string
		values any
	}{
		{name: "not a list", values: "A"},
		{name: "blank", values: []any{" "}},
		{name: "duplicate after trim", values: []any{"A", " A "}},
		{name: "too many", values: make([]any, 31)},
		{name: "too long", values: []any{strings.Repeat("x", 501)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateStandaloneQuestion(base(test.values)); err == nil {
				t.Fatal("expected invalid scoring point descriptors to be rejected")
			}
		})
	}
}

func TestValidateStandaloneQuestionBoundsScoringPointTextAndEvidence(t *testing.T) {
	question := Question{
		"id": "q1", "type": "single_choice", "question": "答案是什么？", "score": 2.0,
		"options": []any{map[string]any{"key": "A", "text": "甲"}, map[string]any{"key": "B", "text": "乙"}},
		"answer":  "A",
	}
	for name, point := range map[string]map[string]any{
		"long text":     {"text": strings.Repeat("x", 501), "score": 2.0},
		"long evidence": {"text": "有效描述", "score": 2.0, "source_evidence": strings.Repeat("x", 2001)},
	} {
		t.Run(name, func(t *testing.T) {
			copyQuestion := Question{}
			for key, value := range question {
				copyQuestion[key] = value
			}
			copyQuestion["scoring_points"] = []any{point}
			if err := ValidateStandaloneQuestion(copyQuestion); err == nil {
				t.Fatal("expected overlong scoring point content to be rejected")
			}
		})
	}
}

func TestValidateStandaloneQuestionSQLDatasets(t *testing.T) {
	makeQuestion := func(datasets any, withDialect bool) Question {
		question := Question{
			"id": "sql1", "type": "short_answer", "question": "查询商品", "score": 10.0,
			"scoring_mode": "sql", "answer": "SELECT name FROM products",
			"sql_test_datasets": datasets,
		}
		if withDialect {
			question["sql_dialect"] = "sqlite"
		}
		return question
	}
	table := func(value any) map[string]any {
		return map[string]any{
			"name":    "products",
			"columns": []any{map[string]any{"name": "id", "type": "INTEGER"}, map[string]any{"name": "name", "type": "TEXT"}},
			"rows":    value,
		}
	}
	datasets := []any{
		map[string]any{"name": "public", "visibility": "public", "tables": []any{table([]any{[]any{1, "甲"}})}},
		map[string]any{"name": "hidden", "visibility": "hidden", "tables": []any{table([]any{[]any{2, "乙"}})}},
	}
	if err := ValidateStandaloneQuestion(makeQuestion(datasets, true)); err != nil {
		t.Fatalf("valid SQL execution fixtures rejected: %v", err)
	}
	if err := ValidateStandaloneQuestion(makeQuestion(datasets, false)); err == nil {
		t.Fatal("SQL datasets without a declared dialect were accepted")
	}
	badRows := []any{map[string]any{
		"name": "public", "tables": []any{table([]any{[]any{1}})},
	}}
	if err := ValidateStandaloneQuestion(makeQuestion(badRows, true)); err == nil {
		t.Fatal("fixture row with the wrong number of values was accepted")
	}
}

func TestValidateStandaloneQuestionCodeTestCases(t *testing.T) {
	makeQuestion := func(cases []any, mode string) Question {
		return Question{
			"id": "code1", "type": "short_answer", "question": "读入整数并输出", "score": 10.0,
			"scoring_mode": mode, "code_language": "python", "answer": "print(input())",
			"code_test_cases": cases,
		}
	}
	cases := []any{
		map[string]any{"name": "zero", "visibility": "public", "input": "0\n", "expected_output": "0\n"},
		map[string]any{"name": "hidden-positive", "visibility": "hidden", "input": "7\n", "expected_output": "7\n"},
	}
	if err := ValidateStandaloneQuestion(makeQuestion(cases, "code")); err != nil {
		t.Fatalf("valid code test cases rejected: %v", err)
	}
	if err := ValidateStandaloneQuestion(makeQuestion(cases[:1], "code")); err == nil {
		t.Fatal("code cases without both public and hidden coverage were accepted")
	}
	if err := ValidateStandaloneQuestion(makeQuestion(cases, "text")); err == nil {
		t.Fatal("code test cases on a non-code question were accepted")
	}
}
