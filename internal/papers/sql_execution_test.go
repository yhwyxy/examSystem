package papers

import "testing"

func mutationQuestion() Question {
	fixtures := []any{}
	for _, name := range []string{"public", "hidden"} {
		fixtures = append(fixtures, map[string]any{"name": name, "visibility": name, "tables": []any{
			map[string]any{"name": "items", "columns": []any{map[string]any{"name": "value", "type": "INTEGER"}}, "rows": []any{[]any{1}}},
		}})
	}
	return Question{"id": "mutation", "type": "short_answer", "question": "删除空值", "score": 10.0,
		"answer": "DELETE FROM items WHERE value IS NULL", "scoring_mode": "sql", "sql_dialect": "sqlite",
		"sql_execution": map[string]any{"mode": "mutation", "mutation_tables": []any{"items"}}, "sql_test_datasets": fixtures}
}

func TestMutationSQLPolicyValidationAndStudentRedaction(t *testing.T) {
	question := mutationQuestion()
	if err := ValidateStandaloneQuestion(question); err != nil {
		t.Fatalf("valid configured mutation rejected: %v", err)
	}
	public := SanitizeForStudent([]Question{question})[0]
	if _, exists := public["sql_execution"]; exists {
		t.Fatal("teacher execution policy exposed to student")
	}
	if _, exists := question["sql_execution"]; !exists {
		t.Fatal("redaction modified authoritative question")
	}
	for name, policy := range map[string]any{
		"not an object":     true,
		"unknown mode":      map[string]any{"mode": "arbitrary"},
		"missing allowlist": map[string]any{"mode": "mutation"},
		"duplicate table":   map[string]any{"mode": "mutation", "mutation_tables": []any{"items", "ITEMS"}},
		"unknown table":     map[string]any{"mode": "mutation", "mutation_tables": []any{"other"}},
		"qualified table":   map[string]any{"mode": "mutation", "mutation_tables": []any{"main.items"}},
		"query writes":      map[string]any{"mode": "query", "mutation_tables": []any{"items"}},
		"runtime option":    map[string]any{"mode": "mutation", "mutation_tables": []any{"items"}, "image": "untrusted"},
	} {
		t.Run(name, func(t *testing.T) {
			q := mutationQuestion()
			q["sql_execution"] = policy
			if err := ValidateStandaloneQuestion(q); err == nil {
				t.Fatal("invalid mutation policy accepted")
			}
		})
	}
	question["sql_test_datasets"] = question["sql_test_datasets"].([]any)[:1]
	if err := ValidateStandaloneQuestion(question); err == nil {
		t.Fatal("mutation with one dataset accepted")
	}
}

func TestCompositeWithoutExplicitSubquestionTypeValidatesExecutionPolicy(t *testing.T) {
	sub := mutationQuestion()
	delete(sub, "type")
	parent := Question{"id": "composite", "type": "composite", "question": "执行任务", "score": 10.0, "subquestions": []any{sub}}
	if err := ValidateStandaloneQuestion(parent); err != nil {
		t.Fatalf("valid implicit subjective subquestion rejected: %v", err)
	}
	sub["sql_execution"] = map[string]any{"mode": "mutation"}
	if err := ValidateStandaloneQuestion(parent); err == nil {
		t.Fatal("invalid implicit subquestion mutation policy bypassed validation")
	}
}
