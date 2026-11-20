package scoreanomaly

import (
	"encoding/json"
	"fmt"
	"testing"
)

func workerExecutionDetail(mode string) map[string]any {
	prefix, key, reason := "code.test.", "cases", "output_match"
	if mode == "sql" {
		prefix, key, reason = "sql.dataset.", "datasets", "state_match"
	}
	points, records := []any{}, []any{}
	for i := 1; i <= 2; i++ {
		record := map[string]any{"name": fmt.Sprintf("hidden_case_%d", i), "visibility": "hidden", "passed": true, "reason": reason}
		if mode == "sql" {
			record["expected_rows"], record["actual_rows"] = 2, 2
		}
		points = append(points, map[string]any{"point_id": fmt.Sprintf("%s%d", prefix, i), "score": 5, "max_score": 5,
			"evidence": nil, "evidence_kind": "execution_record", "validation": record})
		records = append(records, record)
	}
	report := map[string]any{"status": "verified", "total": 2, "passed": 2, key: records, "language": "python", "dialect": "sqlite"}
	return map[string]any{"question_id": "q", "machine_score": 10, "final_score": 8, "max_score": 10, "student_answer": "valid answer",
		"detail": map[string]any{"scoring_mode": mode, "final_score": 99, "machine_score": 99,
			"matched_points": points, "scoring_metadata": map[string]any{"execution_validation": report}}}
}

func analyzeWorkerDetail(t *testing.T, item map[string]any) ([]Finding, QuestionResult) {
	t.Helper()
	raw, err := json.Marshal([]any{item})
	if err != nil {
		t.Fatal(err)
	}
	questions, err := parseQuestions(raw)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := Analyze([]Submission{{ID: "1", Name: "test", Generation: 1, Questions: questions}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return findings, questions[0]
}

func TestNestedWorkerExecutionEvidenceIsReadAndOuterScoresRemainAuthoritative(t *testing.T) {
	for _, mode := range []string{"sql", "code"} {
		t.Run(mode, func(t *testing.T) {
			findings, question := analyzeWorkerDetail(t, workerExecutionDetail(mode))
			if question.Machine != 10 || question.Final != 8 || len(question.Points) != 2 {
				t.Fatalf("nested evidence or outer correction lost: %+v", question)
			}
			if len(findings) != 0 || !question.Points[1].ExecutionVerified {
				t.Fatalf("valid hidden execution incorrectly flagged: %+v", findings)
			}
		})
	}
}

func TestForgedOrInconsistentExecutionRecordsRemainBlocking(t *testing.T) {
	for _, failure := range []string{"status", "count", "ordinal", "reason", "point_copy", "nested_object", "rows"} {
		t.Run(failure, func(t *testing.T) {
			item := workerExecutionDetail("sql")
			detail := item["detail"].(map[string]any)
			report := detail["scoring_metadata"].(map[string]any)["execution_validation"].(map[string]any)
			point := detail["matched_points"].([]any)[0].(map[string]any)
			switch failure {
			case "status":
				report["status"] = "failed"
			case "count":
				report["passed"] = 1
			case "ordinal":
				point["point_id"] = "sql.dataset.3"
			case "reason":
				report["datasets"].([]any)[0].(map[string]any)["reason"] = "student_query_error"
			case "point_copy":
				point["validation"] = map[string]any{"passed": true, "name": "fake", "reason": "state_match", "visibility": "hidden"}
			case "nested_object":
				point["validation"].(map[string]any)["name"] = map[string]any{"untrusted": true}
			case "rows":
				report["datasets"].([]any)[0].(map[string]any)["actual_rows"] = 3
			}
			findings, _ := analyzeWorkerDetail(t, item)
			found := false
			for _, finding := range findings {
				found = found || finding.RuleCode == "invalid_execution_evidence" && finding.Severity == "blocking"
			}
			if !found {
				t.Fatalf("invalid execution did not block: %+v", findings)
			}
		})
	}
}

func TestNestedWorkerPointBoundsCannotBeSkipped(t *testing.T) {
	item := workerExecutionDetail("code")
	item["detail"].(map[string]any)["matched_points"].([]any)[0].(map[string]any)["max_score"] = 4
	findings, _ := analyzeWorkerDetail(t, item)
	for _, finding := range findings {
		if finding.RuleCode == "point_score_out_of_range" {
			return
		}
	}
	t.Fatalf("nested out-of-range point was not detected: %+v", findings)
}
