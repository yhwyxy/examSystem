package scoreanomaly

import "testing"

func TestAnalyzeFlagsPerQuestionBoundsEvidenceAndPointConsistency(t *testing.T) {
	findings, err := Analyze([]Submission{{
		ID: "1", Name: "甲", Generation: 3,
		Questions: []QuestionResult{{QuestionID: "q1", Machine: 5, Final: 5, MaxScore: 4, Method: "llm", Model: "m1",
			Points: []Point{
				{ID: "p1", Score: 3, MaxScore: 2, Relation: "supported", Evidence: ""},
				{ID: "p2", Score: 2, MaxScore: 2, Relation: "contradicted", Evidence: "相反结论"},
			}}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.RuleCode] = true
		if finding.Severity != "blocking" || finding.QuestionID != "q1" || finding.SubmissionIDs[0] != "1" {
			t.Fatalf("finding missing review context: %+v", finding)
		}
	}
	for _, code := range []string{"score_out_of_range", "point_score_out_of_range", "credited_without_evidence", "unsupported_point_score"} {
		if !codes[code] {
			t.Errorf("missing rule %q in findings: %+v", code, findings)
		}
	}
}

func TestAnalyzeFlagsIdenticalAndHighSimilarityScoreGaps(t *testing.T) {
	submissions := []Submission{
		{ID: "11", Name: "甲", Generation: 1, Questions: []QuestionResult{{QuestionID: "q1", Answer: "同一个答案。", Machine: 2, Final: 2, MaxScore: 10, Method: "text", Model: "m1"}}},
		{ID: "12", Name: "乙", Generation: 1, Questions: []QuestionResult{{QuestionID: "q1", Answer: "同一个答案", Machine: 8, Final: 8, MaxScore: 10, Method: "text", Model: "m1"}}},
		{ID: "13", Name: "丙", Generation: 1, Questions: []QuestionResult{{QuestionID: "q2", Answer: "设备校准误差造成观测偏差", Machine: 1, Final: 1, MaxScore: 10, Method: "llm", Model: "m2"}}},
		{ID: "14", Name: "丁", Generation: 1, Questions: []QuestionResult{{QuestionID: "q2", Answer: "设备的校准误差导致测量偏差", Machine: 7, Final: 7, MaxScore: 10, Method: "llm", Model: "m2"}}},
	}
	findings, err := Analyze(submissions, []SimilarPair{{QuestionID: "q2", LeftSubmissionID: "13", RightSubmissionID: "14",
		RiskLevel: "high", RiskScore: 0.91, LeftAnswer: "设备校准误差造成观测偏差", RightAnswer: "设备的校准误差导致测量偏差"}})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]Finding{}
	for _, finding := range findings {
		codes[finding.RuleCode] = finding
	}
	if finding, ok := codes["identical_answer_score_gap"]; !ok || finding.Severity != "blocking" || finding.ScoreGap < 0.59 {
		t.Fatalf("identical answer gap missing or understated: %+v", finding)
	}
	if finding, ok := codes["similar_answer_score_gap"]; !ok || finding.Severity != "attention" {
		t.Fatalf("similar answer gap missing or incorrectly blocking: %+v", finding)
	}
}

func TestAnalyzeSuppressesPolarityConflictAndFindsRegradeDrift(t *testing.T) {
	submissions := []Submission{
		{ID: "21", Name: "甲", Generation: 1, Questions: []QuestionResult{{QuestionID: "q1", Answer: "该结论不成立", Machine: 2, Final: 2, MaxScore: 10}}},
		{ID: "22", Name: "乙", Generation: 1, Questions: []QuestionResult{{QuestionID: "q1", Answer: "该结论成立", Machine: 8, Final: 8, MaxScore: 10}}},
		{ID: "23", Name: "丙", Generation: 2,
			Questions: []QuestionResult{{QuestionID: "q2", Answer: "分析过程", Machine: 7, Final: 7, MaxScore: 10, Model: "new"}},
			History:   []History{{Generation: 1, Questions: []QuestionResult{{QuestionID: "q2", Answer: "分析过程", Machine: 1, Final: 1, MaxScore: 10, Model: "old"}}}}},
	}
	findings, err := Analyze(submissions, []SimilarPair{{QuestionID: "q1", LeftSubmissionID: "21", RightSubmissionID: "22",
		RiskLevel: "high", RiskScore: 0.94, LeftAnswer: submissions[0].Questions[0].Answer, RightAnswer: submissions[1].Questions[0].Answer}})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.RuleCode == "similar_answer_score_gap" {
			t.Fatal("opposite conclusions should not be treated as an automatic scoring inconsistency")
		}
		if finding.RuleCode == "regrade_score_drift" {
			if finding.Severity != "blocking" || finding.ScoreGap < 0.59 || finding.Evidence["previous_scoring_model"] != "old" || finding.Evidence["current_scoring_model"] != "new" {
				t.Fatalf("regrade report lost generation/model evidence: %+v", finding)
			}
			return
		}
	}
	t.Fatal("regrade score drift was not reported")
}

func TestAnalyzeDoesNotFlagConsistentScores(t *testing.T) {
	findings, err := Analyze([]Submission{
		{ID: "31", Questions: []QuestionResult{{QuestionID: "q1", Answer: "相同标准答案", Machine: 5, Final: 5, MaxScore: 10}}},
		{ID: "32", Questions: []QuestionResult{{QuestionID: "q1", Answer: "相同标准答案。", Machine: 5.1, Final: 5.1, MaxScore: 10}}},
	}, nil)
	if err != nil || len(findings) != 0 {
		t.Fatalf("consistent machine results flagged: findings=%+v err=%v", findings, err)
	}
}
