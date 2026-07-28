// Package objective 复刻 Python backend.objective_grader.grade_objective.
//
// 输入: paper 中的单个 question (Python Question dict) + 学生答案 (normalized).
// 输出: GradingDetail dict, 13+ 字段与 Python grade_objective 完全等价:
//
//	question_id / type / question / student_answer / reference_answer /
//	score / machine_score / final_score / max_score / is_correct /
//	grading_method / confidence / reason / review_status /
//	manually_reviewed / detail
//
// 不调主观模型. true_false/single_choice/multiple_choice 全部一比一打分;
// short_answer/essay/composite 由上层主观 service 评分, 此包不应处理.
package objective

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Detail 是 Python grade_objective 返回的 dict, 13+ 字段钝化形态.
type Detail = map[string]any

// rejectType 非客观题 -> Python ValueError 行为等价.
const ErrNonObjectiveType = "非客观题类型: "

// Grade 等价 Python grade_objective(question, student_answer, partial=True).
// question 必含 type / id / score; 严禁传 subjective 类型.
//
// partial 仅 multiple_choice 用. 单选/判断 无 partial 概念.
func Grade(question map[string]any, studentAnswer any, partial bool) (Detail, error) {
	qtype, _ := question["type"].(string)
	maxScore := anyFloat(question["score"])
	reference := question["answer"]

	var raw Detail
	switch qtype {
	case "single_choice":
		raw = gradeSingleChoice(studentAnswer, reference, maxScore)
	case "multiple_choice":
		raw = gradeMultipleChoice(studentAnswer, reference, maxScore, partial)
	case "true_false":
		raw = gradeTrueFalse(studentAnswer, reference, maxScore)
	default:
		return nil, fmt.Errorf("%s%s", ErrNonObjectiveType, qtype)
	}

	score := anyFloat(raw["score"])
	score = round6(score)

	// detail 去掉 score / is_correct, 其它原样塞
	detail := Detail{}
	for k, v := range raw {
		if k == "score" || k == "is_correct" {
			continue
		}
		detail[k] = v
	}

	return Detail{
		"question_id":       question["id"],
		"type":              qtype,
		"question":          question["question"],
		"student_answer":    studentAnswer,
		"reference_answer":  reference,
		"score":             score,
		"machine_score":     score,
		"final_score":       score,
		"max_score":         maxScore,
		"is_correct":        raw["is_correct"],
		"grading_method":    "objective_rule",
		"confidence":        1.0,
		"reason":            "客观题规则判分",
		"review_status":     "auto_scored",
		"manually_reviewed": false,
		"detail":            detail,
	}, nil
}

// gradeSingleChoice 等价 Python grade_single_choice: 比较两值 norm 后是否一致.
// 一致 -> max_score; 否则 -> 0.
func gradeSingleChoice(student, reference any, maxScore float64) Detail {
	correct := normChoice(student) == normChoice(reference)
	score := 0.0
	if correct {
		score = maxScore
	}
	return Detail{"is_correct": correct, "score": score}
}

// gradeMultipleChoice 等价 Python grade_multiple_choice.
//
// student 可能 None / 单值 / list: 全部转 list 然后归一化为集合.
// reference 为 [] 列表. 算法:
//
//	selected 集合 ∩ correct_set 集合; 若 selected 含错项(不在 correct_set) -> 0 分.
//	partial=True 且无错项: 得分 = max_score * hit / len(correct_set).
//	partial=False 且无错项: 仅当 selected == correct_set 才给满分.
//	correct_set 为空 -> 0 (Python 提防 0/0).
func gradeMultipleChoice(student any, reference any, maxScore float64, partial bool) Detail {
	var studentList []any
	switch s := student.(type) {
	case nil:
		studentList = []any{}
	case []any:
		studentList = s
	default:
		studentList = []any{s}
	}

	selected := map[string]bool{}
	for _, x := range studentList {
		if c := normChoice(x); c != "" {
			selected[c] = true
		}
	}

	correctSet := map[string]bool{}
	if arr, ok := reference.([]any); ok {
		for _, x := range arr {
			if c := normChoice(x); c != "" {
				correctSet[c] = true
			}
		}
	}

	// wrong = selected - correctSet
	wrong := []string{}
	for k := range selected {
		if !correctSet[k] {
			wrong = append(wrong, k)
		}
	}
	if len(wrong) > 0 {
		sort.Strings(wrong)
		hit := 0
		for k := range selected {
			if correctSet[k] {
				hit++
			}
		}
		return Detail{
			"is_correct":    false,
			"score":         0.0,
			"wrong_choices": wrong,
			"correct_count": hit,
			"total_count":   len(correctSet),
		}
	}

	if !partial {
		ok := sameSet(selected, correctSet)
		score := 0.0
		if ok {
			score = maxScore
		}
		return Detail{"is_correct": ok, "score": score}
	}

	hit := 0
	for k := range selected {
		if correctSet[k] {
			hit++
		}
	}
	var score float64
	if len(correctSet) > 0 {
		score = maxScore * float64(hit) / float64(len(correctSet))
	} else {
		score = 0
	}
	return Detail{
		"is_correct":    sameSet(selected, correctSet),
		"score":         score,
		"correct_count": hit,
		"total_count":   len(correctSet),
	}
}

// gradeTrueFalse 等价 Python grade_true_false.
// student nil / 空白 -> 直接 false + 0 分. 否则 norm_bool 比较.
func gradeTrueFalse(student, reference any, maxScore float64) Detail {
	if student == nil {
		return Detail{"is_correct": false, "score": 0.0}
	}
	if s, ok := student.(string); ok {
		if strings.TrimSpace(s) == "" {
			return Detail{"is_correct": false, "score": 0.0}
		}
	}
	correct := normBool(student) == normBool(reference)
	score := 0.0
	if correct {
		score = maxScore
	}
	return Detail{"is_correct": correct, "score": score}
}

// anyFloat 把 int / int64 / float64 -> float64; 缺 -> 0.
func anyFloat(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f
		}
	}
	return 0
}

// round6 对齐 Python round(x, 6); Python 用 banker's rounding, Go math.Round 是
// 就近 (半向上). 学生场景满分通常整数, 差异极小, 但兼容性保留:
//
// 6 位小数后, Go math.Round 与 Python round 在极少数情况下 (e.g. exactly 0.5E-6)
// 差 1 ulp. 为减少二次方差, 用 math.Round 进行四舍五入即可; 后续会 pass 测试集对齐.
func round6(x float64) float64 {
	return math.Round(x*1e6) / 1e6
}

// sameSet 集合相等 (避免多次迭代).
func sameSet(a, b map[string]bool) bool {
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

var _ = fmt.Sprintf
