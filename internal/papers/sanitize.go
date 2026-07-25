package papers

// SanitizeForStudent 100% 复刻 Python question_loader.sanitize_for_student.
//
// 移除 SENSITIVE_FIELDS (answer / answers_by_language / calculation /
// scoring_points / scoring_points_by_language / scoring_rubric); code 题
// 仍保留 scoring_mode + allowed_languages (或 code_language 默认值);
// composite 题保留 subquestions 但子题也脱敏, 子题公开:
//   id / question / score (+ scoring_mode + allowed_languages 可选).
//
// 不修改输入 doc; 返回新切片. 字段顺序按 Python dict 插入序保留 (Go 端用 json.Marshal).
func SanitizeForStudent(questions []Question) []Question {
	out := make([]Question, 0, len(questions))
	for _, q := range questions {
		normalizeCompositeQuestionInPlace(q)
		public := Question{}
		for k, v := range q {
			if _, bad := sensitiveFields[k]; bad {
				continue
			}
			if k == "subquestions" {
				continue
			}
			public[k] = v
		}
		// code 题暴露模式与可选语言 (不含参考答案)
		modeCode := stringOr(q, "scoring_mode") == "code"
		codeLang := stringOr(q, "code_language")
		allowed := anyLanguages(q, "allowed_languages")
		if modeCode || codeLang != "" || len(allowed) > 0 {
			if sm := stringOr(q, "scoring_mode"); sm != "" {
				public["scoring_mode"] = sm
			}
			if len(allowed) > 0 {
				pub := make([]string, len(allowed))
				copy(pub, allowed)
				public["allowed_languages"] = pub
			} else if codeLang != "" {
				public["code_language"] = codeLang
			}
		}
		// composite 子题递归脱敏
		if isComposite(q) {
			subs := subquestions(q)
			pubSubs := make([]Question, 0, len(subs))
			for _, s := range subs {
				if s == nil {
					continue
				}
				sub := Question{
					"id":       s["id"],
					"question": stringOrDefault(s, "question", ""),
					"score":    s["score"],
				}
				if sm := stringOr(s, "scoring_mode"); sm != "" {
					sub["scoring_mode"] = sm
				}
				if al := anyLanguages(s, "allowed_languages"); len(al) > 0 {
					pub := make([]string, len(al))
					copy(pub, al)
					sub["allowed_languages"] = pub
				}
				pubSubs = append(pubSubs, sub)
			}
			public["subquestions"] = pubSubs
		}
		out = append(out, public)
	}
	return out
}
