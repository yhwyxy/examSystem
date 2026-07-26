// Package grading 抽出客观题 / 主观题识别 helper.
//
// 一个 workaround (Task 10 决断 3=A/K3): 仅抽 4 个纯函数, gradeObjectiveInTx
// 留在 submissions 包私有. review 包调用时通过 submissions 包 Exported 包装入口接入.
package grading

import (
	"fmt"
	"strconv"
)

// QID 取 question["id"]; 兼容 int / string 形态, 统一回 string.
// 与 submissions/repository.go 之前的 objQID 等价, 提为公共后供 review 包复用.
func QID(q map[string]any) string {
	id, ok := q["id"]
	if !ok || id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", id)
	}
}

// IsObjectiveType 区分客观 / 主观题; 与 objective.Grade 支持类型对齐.
// 客观题类型: single_choice / multiple_choice / true_false.
func IsObjectiveType(q map[string]any) bool {
	t, _ := q["type"].(string)
	switch t {
	case "single_choice", "multiple_choice", "true_false":
		return true
	}
	return false
}

// MaxScoreOf 取 question["score"] 浮点值; 取不到回 0.
func MaxScoreOf(q map[string]any) float64 {
	switch v := q["score"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// Placeholder 是主观题或异常 fallback 的占位项 (score=0).
// 字段集与 submissions/repository.go objPlaceholder 完全一致, 供 review 包复用
// 构造 grading_detail 子项模板时使用.
func Placeholder(q map[string]any, ans any, maxScore float64, reason string) map[string]any {
	return map[string]any{
		"question_id":        q["id"],
		"type":               q["type"],
		"student_answer":     ans,
		"reference_answer":   q["answer"],
		"score":              0.0,
		"machine_score":      0.0,
		"final_score":        0.0,
		"max_score":          maxScore,
		"is_correct":         false,
		"grading_method":     "subjective_pending",
		"confidence":         0.0,
		"reason":             reason,
		"review_status":      "unanswered",
		"manually_reviewed":  false,
		"detail":             map[string]any{},
	}
}
