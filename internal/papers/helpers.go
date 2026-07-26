package papers

import (
	"strconv"
	"strings"
)

// normChoice 是 Python _norm_choice: str(value).strip()
func normChoice(v any) string {
	return strings.TrimSpace(anyToString(v))
}

// normBool 是 Python _norm_bool: bool/int/float/str("true","1","t","yes","y","对","正确","是") -> true.
// nil / 空 -> false. 与 Python 行为一致 (Python bool(None) == False).
func normBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		switch s {
		case "true", "1", "t", "yes", "y", "对", "正确", "是":
			return true
		}
		return false
	case nil:
		return false
	default:
		return false
	}
}

// anyToString 把任意值转成字符串. 数字/bool 用 fmt; string 直返; 切片/数组空串.
// Python `str(value)` 行为对数字/字符串/列表完全 1:1 不可强求, 但 choice 校验仅
// 用于字符串小写化比较, 我们保守地转字符串即可.
func anyToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return intToString(int64(x))
	case int64:
		return intToString(x)
	case float64:
		return floatToString(x)
	default:
		// fallback: 不展开 (复合值转字典/列表不会出现在选项键)
		return ""
	}
}

func intToString(v int64) string {
	// 优化避免 strconv 引入多包; 手动转 string 简单可靠
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func floatToString(v float64) string {
	// 选项键极少小数, 实际 case 走 strconv -1 表示最简短表达, 备整数
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// stringOr 取字符串字段; 缺或非字符串返 "".
func stringOr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// stringOrDefault 取字符串字段; 空 -> dv.
func stringOrDefault(m map[string]any, key, dv string) string {
	s := stringOr(m, key)
	if s == "" {
		return dv
	}
	return s
}

// anyLanguages 把 []any 形式字符串数组转 []string (trim). 非 list 或非字符串 -> nil.
// 与 Python list[...] 1:1, 不做大小写化.
func anyLanguages(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isComposite 等价 Python is_composite_question.
// type==composite OR (subquestions 数组非空): 视为复合题.
func isComposite(q Question) bool {
	return stringOr(q, "type") == "composite" || len(subquestions(q)) > 0
}

// subquestions 等价 Python get_subquestions: 优先 subquestions, 回退 sub_questions.
func subquestions(q Question) []Question {
	if q == nil {
		return nil
	}
	if v, ok := q["subquestions"]; ok {
		if arr, ok := v.([]any); ok {
			out := make([]Question, 0, len(arr))
			for _, x := range arr {
				if s, ok := x.(map[string]any); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	if v, ok := q["sub_questions"]; ok {
		if arr, ok := v.([]any); ok {
			out := make([]Question, 0, len(arr))
			for _, x := range arr {
				if s, ok := x.(map[string]any); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

// normalizeCompositeQuestionInPlace 等价 Python normalize_composite_question.
// 将 sub_questions -> subquestions, 不返回.
func normalizeCompositeQuestionInPlace(q Question) {
	if q == nil {
		return
	}
	if _, ok := q["subquestions"]; ok {
		return
	}
	if _, ok := q["sub_questions"]; !ok {
		return
	}
	q["subquestions"] = q["sub_questions"]
	delete(q, "sub_questions")
}
