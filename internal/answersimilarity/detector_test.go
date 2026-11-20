package answersimilarity

import (
	"errors"
	"strings"
	"testing"
)

func TestAnalyzeFindsSharedNonReferenceExcerpts(t *testing.T) {
	analysis, err := Analyze(Question{
		ID: "q1", Type: "essay", Mode: "text",
		ReferenceAnswer: "另一项分析指出不同因素影响实验过程",
	}, []Response{
		{SubmissionID: "11", ParticipantID: "A", Name: "甲", Answer: "测量结果显示温度升高导致反应速度增加，同时样本边缘出现蓝色条纹，说明设备校准存在明显偏差。"},
		{SubmissionID: "12", ParticipantID: "B", Name: "乙", Answer: "研究发现温度升高导致反应速度增加，同时样本边缘出现蓝色条纹，导致最终结果不可信。"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.RuleVersion != RuleVersion || len(analysis.Findings) != 1 {
		t.Fatalf("analysis=%+v", analysis)
	}
	finding := analysis.Findings[0]
	if finding.RiskScore < 0.45 || finding.SharedExcerpt == "" || finding.LeftExcerpt == "" || finding.RightExcerpt == "" {
		t.Fatalf("finding lacks a reviewable signal: %+v", finding)
	}
	if !strings.Contains(finding.LeftExcerpt, finding.SharedExcerpt) && !strings.Contains(finding.SharedExcerpt, finding.LeftExcerpt) {
		t.Fatalf("left excerpt was not preserved from source: %+v", finding)
	}
}

func TestAnalyzeWithEmbeddingsFindsSemanticOnlyLeadAndSuppressesReferenceConsensus(t *testing.T) {
	responses := []Response{
		{SubmissionID: "21", Answer: "温度改变会使反应速率发生明显变化，造成测量数据出现系统偏差。"},
		{SubmissionID: "22", Answer: "随着温度不同，化学反应的速度也会显著变化，因此观测结果产生系统误差。"},
	}
	analysis, err := AnalyzeWithEmbeddings(Question{ID: "q-semantic", Type: "essay", Mode: "text"}, responses,
		map[string][]float64{"21": {1, 0}, "22": {0.99, 0.01}}, []float64{0, 1})
	if err != nil || len(analysis.Findings) != 1 {
		t.Fatalf("semantic-only analysis=%+v err=%v", analysis, err)
	}
	finding := analysis.Findings[0]
	if finding.SemanticSimilarity < 0.99 || finding.SharedExcerpt != "" || !strings.Contains(strings.Join(finding.Reasons, " "), "语义向量") {
		t.Fatalf("semantic finding lacks distinct semantic evidence: %+v", finding)
	}

	standard, err := AnalyzeWithEmbeddings(Question{ID: "q-standard-semantic", Type: "essay", Mode: "text",
		ReferenceAnswer: "提高设备校准准确度可以减少系统误差并改善测量结果。"}, []Response{
		{SubmissionID: "31", Answer: "设备校准准确可减少系统误差，测量结果会更加可靠。"},
		{SubmissionID: "32", Answer: "准确校准设备有助于减少系统误差，使测量结果更可靠。"},
	}, map[string][]float64{"31": {0.99, 0.1}, "32": {0.99, -0.1}}, []float64{1, 0})
	if err != nil || len(standard.Findings) != 0 {
		t.Fatalf("reference-consensus paraphrases should be suppressed: analysis=%+v err=%v", standard, err)
	}
}

func TestVectorCandidatePairsFindsLargeCohortNearDuplicates(t *testing.T) {
	docs := make([]answerDoc, 64)
	for index := range docs {
		docs[index] = answerDoc{vector: []float64{1, float64(index%3) * 0.0001}}
	}
	pairs, err := vectorCandidatePairs(docs)
	if err != nil || len(pairs) != len(docs)*(len(docs)-1)/2 {
		t.Fatalf("vector candidate pairs=%d err=%v", len(pairs), err)
	}
}

func TestAnalyzeReducesStandardAnswerAndShortAnswerFalsePositives(t *testing.T) {
	standard, err := Analyze(Question{
		ID: "q-standard", Type: "short_answer", Mode: "text",
		ReferenceAnswer: "索引可以提高数据库查询效率并减少全表扫描",
	}, []Response{
		{SubmissionID: "1", Answer: "索引可以提高数据库查询效率并减少全表扫描"},
		{SubmissionID: "2", Answer: "索引可以提高数据库查询效率并减少全表扫描"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(standard.Findings) != 0 {
		t.Fatalf("standard-reference overlap was flagged: %+v", standard.Findings)
	}

	short, err := Analyze(Question{ID: "q-short", Type: "short_answer", Mode: "text"}, []Response{
		{SubmissionID: "1", Answer: "顯著提升"},
		{SubmissionID: "2", Answer: "顯著提升"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(short.Findings) != 0 {
		t.Fatalf("short required answer was flagged: %+v", short.Findings)
	}
}

func TestRaiseCrossQuestionRiskLinksTheSameSubmissionPair(t *testing.T) {
	findings := []Finding{
		{QuestionID: "q1", LeftSubmissionID: "4", RightSubmissionID: "9", RiskLevel: "medium"},
		{QuestionID: "q2", LeftSubmissionID: "9", RightSubmissionID: "4", RiskLevel: "medium"},
		{QuestionID: "q3", LeftSubmissionID: "4", RightSubmissionID: "10", RiskLevel: "medium"},
	}
	RaiseCrossQuestionRisk(findings)
	for i := range findings[:2] {
		if findings[i].RiskLevel != "high" || strings.Join(findings[i].RelatedQuestionIDs, ",") != "q1,q2" {
			t.Fatalf("cross-question link was not retained: %+v", findings[i])
		}
	}
	if findings[2].RiskLevel != "medium" || len(findings[2].RelatedQuestionIDs) != 1 {
		t.Fatalf("unrelated pair risk changed: %+v", findings[2])
	}
}

func TestAnalyzeNormalizesCodeWhitespaceAndEnforcesCapacity(t *testing.T) {
	code, err := Analyze(Question{ID: "q-code", Type: "short_answer", Mode: "code"}, []Response{
		{SubmissionID: "1", Answer: "def calculate(value):\n    # same algorithm\n    return value * 37 + 12"},
		{SubmissionID: "2", Answer: "def calculate(value):\n  return value*37+12"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(code.Findings) != 1 {
		t.Fatalf("equivalent source formatting was not matched: %+v", code.Findings)
	}

	duplicate := "分析过程指出温度变化引起材料颜色明显改变并影响测量结果"
	responses := make([]Response, 500)
	for i := range responses {
		responses[i] = Response{SubmissionID: string(rune(i + 1)), Answer: duplicate}
	}
	if _, err := Analyze(Question{ID: "q-limit", Type: "essay", Mode: "text"}, responses); !errors.Is(err, ErrCandidateLimit) {
		t.Fatalf("candidate cap err=%v, want ErrCandidateLimit", err)
	}
}

func TestModeSpecificNormalizationPreservesMeaningfulOperatorsAndLiterals(t *testing.T) {
	if normalizeByMode("2 × 3 = 6", "calculation") != normalizeByMode("2*3=6", "calculation") {
		t.Fatal("calculation formatting and multiplication glyphs should normalize consistently")
	}
	if normalizeByMode("2 + 3 = 5", "calculation") == normalizeByMode("2 - 3 = 5", "calculation") {
		t.Fatal("calculation normalization must preserve operator differences")
	}
	leftSQL := normalizeByMode("SELECT id FROM users WHERE label = 'A=B' /* note */", "sql")
	rightSQL := normalizeByMode("select id from users where label='A=B'", "sql")
	if leftSQL != rightSQL {
		t.Fatalf("SQL whitespace/comments should normalize: %q != %q", leftSQL, rightSQL)
	}
	if normalizeByMode("SELECT * FROM users WHERE id = 1", "sql") == normalizeByMode("SELECT * FROM users WHERE id != 1", "sql") {
		t.Fatal("SQL normalization must preserve comparison operators")
	}
	if normalizeAnswer("print('# in a string')", "code", "python") != normalizeAnswer("print('# in a string') # trailing comment", "code", "python") {
		t.Fatal("code comments should be removed without deleting comment-like text in string literals")
	}
	if normalizeAnswer("result = total // 3", "code", "python") != normalizeAnswer("result=total//3", "code", "python") {
		t.Fatal("Python floor-division operator should not be treated as a comment")
	}
	if !strings.Contains(normalizeAnswer("#define VALUE 3", "code", "cpp"), "#") {
		t.Fatal("C preprocessor lines should not be stripped as comments")
	}
}

func TestAnalyzeReportsSharedUnexpectedNumericEvidenceWithoutScoringIt(t *testing.T) {
	analysis, err := Analyze(Question{ID: "q-number", Type: "short_answer", Mode: "calculation",
		ReferenceAnswer: "计算结果为 42，单位为米。"}, []Response{
		{SubmissionID: "1", Answer: "计算步骤显示初始测量结果是 17 米，随后设备校准误差让最终结果明显偏离题目要求。"},
		{SubmissionID: "2", Answer: "计算过程采用初始测量结果 17 米，随后设备校准误差让最终结果明显偏离题目要求。"},
	})
	if err != nil || len(analysis.Findings) != 1 {
		t.Fatalf("analysis=%+v err=%v", analysis, err)
	}
	finding := analysis.Findings[0]
	if len(finding.CommonErrorEvidence) == 0 || finding.CommonErrorEvidence[0] != "17" {
		t.Fatalf("shared unexpected number was not preserved as a clue: %+v", finding)
	}
}

func TestAnalyzeRejectsOversizedAnswersAndSkipsObjectiveQuestions(t *testing.T) {
	if _, err := Analyze(Question{ID: "q-long", Type: "essay", Mode: "text"}, []Response{
		{SubmissionID: "1", Answer: strings.Repeat("x", maxAnswerRunes+1)},
		{SubmissionID: "2", Answer: strings.Repeat("y", 32)},
	}); !errors.Is(err, ErrAnswerTooLong) {
		t.Fatalf("oversize answer err=%v, want ErrAnswerTooLong", err)
	}
	objective, err := Analyze(Question{ID: "q-choice", Type: "single_choice", Mode: "text"}, []Response{
		{SubmissionID: "1", Answer: strings.Repeat("A", 40)},
		{SubmissionID: "2", Answer: strings.Repeat("A", 40)},
	})
	if err != nil || len(objective.Findings) != 0 {
		t.Fatalf("objective answers should not be compared: analysis=%+v err=%v", objective, err)
	}
	largeCohort := make([]Response, 40)
	for i := range largeCohort {
		largeCohort[i] = Response{SubmissionID: string(rune(i + 1)), Answer: strings.Repeat("x", maxAnswerRunes)}
	}
	if _, err := Analyze(Question{ID: "q-total-limit", Type: "essay", Mode: "text"}, largeCohort); !errors.Is(err, ErrAnswerTooLong) {
		t.Fatalf("aggregate answer corpus err=%v, want ErrAnswerTooLong", err)
	}
}
