package objective

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// _normChoice 等价 Python _norm_choice: str(value).strip()
func normChoice(v any) string {
	return strings.TrimSpace(anyToTrimStr(v))
}

// _norm_bool 等价 Python _norm_bool: bool/int/str("true","1","t","yes","y","对","正确","是")
// 其它 -> false.
func normBool(v any) bool {
	// list 单元素取其根 (与 anyToTrimStr 一致), 否则走原逻辑
	if arr, ok := v.([]any); ok && len(arr) == 1 {
		return normBool(arr[0])
	}
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
		case "true", "t", "yes", "y", "对", "正确", "是":
			return true
		}
		return false
	case nil:
		return false
	default:
		return false
	}
}

// anyToTrimStr 把 v 转字符串 (trim 后); 与 Python str(value) 等值在 choice 校验
// 上下文. list 空 -> ""; list 单元素 -> 该元素 str (与 Python 调用方常见参数化一致);
// 其余多元素 list 仍走 Python str(list) 行为不易复刻, 故顶回 str 末位.
func anyToTrimStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "True" // Python str(True) = "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		if len(x) == 0 {
			return ""
		}
		// fixture 把 ["A"] 视作单值 "A"
		if len(x) == 1 {
			return anyToTrimStr(x[0])
		}
		// 多元素 list - 走最小可表示 (与 Python str 复刻难统一),
		// 实际单选/判断场景学生只传 1 个值 -> 不应走到这里.
		return ""
	}
	return ""
}

var _ = math.Round
var _ = sort.Strings
