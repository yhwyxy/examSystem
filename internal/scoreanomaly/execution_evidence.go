package scoreanomaly

import (
	"encoding/json"
	"strconv"
	"strings"
)

func executionCount(raw any, maximum int) (int, bool) {
	var value float64
	switch count := raw.(type) {
	case float64:
		value = count
	case int:
		value = float64(count)
	case int64:
		value = float64(count)
	case json.Number:
		parsed, err := count.Int64()
		if err != nil {
			return 0, false
		}
		value = float64(parsed)
	default:
		return 0, false
	}
	if !finite(value) || value < 0 || value > float64(maximum) || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

func verifiedPointExecution(item, point map[string]any) bool {
	if stringValue(point["evidence_kind"]) != "execution_record" {
		return false
	}
	mode := stringValue(item["scoring_mode"])
	key, prefix, maximum := "", "", 0
	switch mode {
	case "sql":
		key, prefix, maximum = "datasets", "sql.dataset.", 20
	case "code":
		key, prefix, maximum = "cases", "code.test.", 12
	default:
		return false
	}
	metadata, _ := item["scoring_metadata"].(map[string]any)
	report, _ := metadata["execution_validation"].(map[string]any)
	if stringValue(report["status"]) != "verified" {
		return false
	}
	total, totalOK := executionCount(report["total"], maximum)
	passed, passedOK := executionCount(report["passed"], maximum)
	records, recordsOK := report[key].([]any)
	if !totalOK || !passedOK || total < 2 || passed > total || !recordsOK || len(records) != total {
		return false
	}
	observed := 0
	for _, raw := range records {
		record, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		matched, ok := record["passed"].(bool)
		if !ok {
			return false
		}
		if matched {
			observed++
		}
	}
	if observed != passed {
		return false
	}
	id := stringValue(point["point_id"])
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if err != nil || index < 1 || index > total || id != prefix+strconv.Itoa(index) {
		return false
	}
	record, _ := records[index-1].(map[string]any)
	if record["passed"] != true {
		return false
	}
	reason := stringValue(record["reason"])
	if mode == "code" && reason != "output_match" || mode == "sql" && reason != "result_match" && reason != "state_match" {
		return false
	}
	if mode == "sql" {
		if stringValue(report["dialect"]) != "sqlite" {
			return false
		}
		expected, expectedOK := executionCount(record["expected_rows"], 5000)
		actual, actualOK := executionCount(record["actual_rows"], 5000)
		if !expectedOK || !actualOK || expected != actual {
			return false
		}
	}
	if mode == "code" {
		language := stringValue(report["language"])
		if language != "python" && language != "javascript" && language != "java" {
			return false
		}
	}
	validation, _ := point["validation"].(map[string]any)
	if validation["passed"] != true {
		return false
	}
	for _, field := range []string{"name", "visibility", "reason"} {
		expected, expectedOK := record[field].(string)
		actual, actualOK := validation[field].(string)
		if !expectedOK || !actualOK || expected == "" || actual != expected {
			return false
		}
	}
	if mode == "sql" {
		for _, field := range []string{"expected_rows", "actual_rows"} {
			expected, expectedOK := executionCount(record[field], 5000)
			actual, actualOK := executionCount(validation[field], 5000)
			if !expectedOK || !actualOK || expected != actual {
				return false
			}
		}
	}
	visibility := stringValue(record["visibility"])
	return visibility == "public" || visibility == "hidden"
}
