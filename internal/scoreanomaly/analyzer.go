package scoreanomaly

import (
	"errors"
	"math"
	"sort"
	"strings"
	"unicode"
)

const RuleVersion = "score-consistency-v2"

const maxFindings = 50_000

var ErrFindingLimit = errors.New("score anomaly finding limit exceeded")

type Point struct {
	ID                string
	Score             float64
	MaxScore          float64
	Relation          string
	Evidence          string
	EvidenceLocated   bool
	ExecutionClaimed  bool
	ExecutionVerified bool
}

type QuestionResult struct {
	QuestionID string
	Question   string
	Answer     string
	Machine    float64
	Final      float64
	MaxScore   float64
	Method     string
	Backend    string
	Model      string
	Review     string
	Points     []Point
}

type History struct {
	Generation int64
	Questions  []QuestionResult
}

type Submission struct {
	ID         string
	EmployeeID string
	Name       string
	Generation int64
	Questions  []QuestionResult
	History    []History
}

type SimilarPair struct {
	QuestionID          string
	LeftSubmissionID    string
	RightSubmissionID   string
	RiskLevel           string
	RiskScore           float64
	LeftAnswer          string
	RightAnswer         string
	CommonErrorEvidence []string
}

type Finding struct {
	RuleCode         string         `json:"rule_code"`
	Severity         string         `json:"severity"`
	QuestionID       string         `json:"question_id,omitempty"`
	SubmissionIDs    []string       `json:"submission_ids"`
	ParticipantNames []string       `json:"participant_names"`
	Message          string         `json:"message"`
	ScoreGap         float64        `json:"score_gap,omitempty"`
	Evidence         map[string]any `json:"evidence"`
}

// Analyze applies deterministic consistency and evidence checks to one frozen
// scoring generation. Findings are review signals and never edit scores.
func Analyze(submissions []Submission, similarPairs []SimilarPair) ([]Finding, error) {
	findings := make([]Finding, 0)
	byID := make(map[string]Submission, len(submissions))
	questionResults := make(map[string]map[string]QuestionResult, len(submissions))
	for _, submission := range submissions {
		byID[submission.ID] = submission
		questionResults[submission.ID] = make(map[string]QuestionResult, len(submission.Questions))
		for _, result := range submission.Questions {
			if result.QuestionID == "" {
				continue
			}
			questionResults[submission.ID][result.QuestionID] = result
			findings = append(findings, validateResult(submission, result)...)
			if len(findings) > maxFindings {
				return nil, ErrFindingLimit
			}
		}
		findings = append(findings, compareRegrades(submission)...)
		if len(findings) > maxFindings {
			return nil, ErrFindingLimit
		}
	}

	groups := make(map[string][]candidateAnswer)
	for _, submission := range submissions {
		for _, result := range submission.Questions {
			if result.QuestionID == "" || normalizeAnswer(result.Answer) == "" || result.MaxScore <= 0 {
				continue
			}
			groups[result.QuestionID] = append(groups[result.QuestionID], candidateAnswer{submission: submission, result: result})
		}
	}
	for questionID, group := range groups {
		for left := 0; left < len(group); left++ {
			for right := left + 1; right < len(group); right++ {
				if normalizeAnswer(group[left].result.Answer) != normalizeAnswer(group[right].result.Answer) {
					continue
				}
				gap := normalizedGap(group[left].result.Machine, group[left].result.MaxScore,
					group[right].result.Machine, group[right].result.MaxScore)
				if gap < 0.20 {
					continue
				}
				severity := "attention"
				if gap >= 0.40 {
					severity = "blocking"
				}
				findings = append(findings, pairFinding("identical_answer_score_gap", severity, questionID,
					group[left].submission, group[right].submission, group[left].result, group[right].result,
					gap, "相同答案的机器评分差异超过初始一致性阈值。"))
				if len(findings) > maxFindings {
					return nil, ErrFindingLimit
				}
			}
		}
	}

	for _, pair := range similarPairs {
		left, leftOK := byID[pair.LeftSubmissionID]
		right, rightOK := byID[pair.RightSubmissionID]
		if !leftOK || !rightOK {
			continue
		}
		leftResult, leftOK := questionResults[left.ID][pair.QuestionID]
		rightResult, rightOK := questionResults[right.ID][pair.QuestionID]
		if !leftOK || !rightOK || leftResult.MaxScore <= 0 || rightResult.MaxScore <= 0 {
			continue
		}
		gap := normalizedGap(leftResult.Machine, leftResult.MaxScore, rightResult.Machine, rightResult.MaxScore)
		if gap < 0.20 || hasPolarityConflict(pair.LeftAnswer, pair.RightAnswer) {
			continue
		}
		rule, message := "similar_answer_score_gap", "高相似答案的机器评分差异较大，请核对评分点和答案含义。"
		if len(pair.CommonErrorEvidence) > 0 {
			rule, message = "shared_error_score_disagreement", "具有共同错误候选的相似答案得分不同，请核对相同错误是否得到一致处理。"
		}
		severity := "attention"
		if gap >= 0.40 && pair.RiskLevel == "high" && len(pair.CommonErrorEvidence) > 0 {
			severity = "blocking"
		}
		finding := pairFinding(rule, severity, pair.QuestionID, left, right, leftResult, rightResult, gap, message)
		finding.Evidence["similarity_risk_level"] = pair.RiskLevel
		finding.Evidence["similarity_risk_score"] = pair.RiskScore
		finding.Evidence["shared_error_candidates"] = pair.CommonErrorEvidence
		findings = append(findings, finding)
		if len(findings) > maxFindings {
			return nil, ErrFindingLimit
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity == "blocking"
		}
		if findings[i].RuleCode != findings[j].RuleCode {
			return findings[i].RuleCode < findings[j].RuleCode
		}
		if findings[i].QuestionID != findings[j].QuestionID {
			return findings[i].QuestionID < findings[j].QuestionID
		}
		return strings.Join(findings[i].SubmissionIDs, ":") < strings.Join(findings[j].SubmissionIDs, ":")
	})
	return findings, nil
}

type candidateAnswer struct {
	submission Submission
	result     QuestionResult
}

func validateResult(submission Submission, result QuestionResult) []Finding {
	findings := make([]Finding, 0)
	add := func(code, severity, message string, evidence map[string]any) {
		findings = append(findings, Finding{RuleCode: code, Severity: severity, QuestionID: result.QuestionID,
			SubmissionIDs: []string{submission.ID}, ParticipantNames: []string{submission.Name}, Message: message, Evidence: evidence})
	}
	base := map[string]any{"generation": submission.Generation, "machine_score": result.Machine,
		"final_score": result.Final, "max_score": result.MaxScore, "scoring_method": result.Method,
		"scoring_backend": result.Backend, "scoring_model": result.Model, "review_status": result.Review}
	if result.MaxScore < 0 || !finite(result.MaxScore) || !finite(result.Machine) || !finite(result.Final) ||
		result.Machine < -0.01 || result.Machine > result.MaxScore+0.01 || result.Final < -0.01 || result.Final > result.MaxScore+0.01 {
		add("score_out_of_range", "blocking", "评分超出题目分值范围或包含非有限数值。", cloneEvidence(base))
	}
	seen := make(map[string]bool)
	pointTotal := 0.0
	pointScoreCount := 0
	for _, point := range result.Points {
		if point.ID != "" && seen[point.ID] {
			add("duplicate_scoring_point", "blocking", "同一评分点在机器结果中重复出现。", map[string]any{
				"point_id": point.ID, "generation": submission.Generation, "scoring_method": result.Method})
		}
		seen[point.ID] = true
		pointTotal += point.Score
		pointScoreCount++
		if !finite(point.Score) || !finite(point.MaxScore) || point.Score < -0.01 || point.Score > point.MaxScore+0.01 {
			add("point_score_out_of_range", "blocking", "评分点得分超出该点分值范围。", map[string]any{
				"point_id": point.ID, "score": point.Score, "max_score": point.MaxScore, "generation": submission.Generation})
		}
		if point.Score > 0.01 && point.ExecutionClaimed && !point.ExecutionVerified {
			add("invalid_execution_evidence", "blocking", "执行验证记录缺失或与当前评分点不一致。", map[string]any{
				"point_id": point.ID, "generation": submission.Generation})
		}
		if point.Score > 0.01 && strings.TrimSpace(point.Evidence) == "" && !point.ExecutionVerified {
			add("credited_without_evidence", "blocking", "有得分的评分点缺少考生答案证据。", map[string]any{
				"point_id": point.ID, "score": point.Score, "max_score": point.MaxScore, "generation": submission.Generation})
		}
		if point.Score > 0.01 && (point.Relation == "contradicted" || point.Relation == "unknown") {
			add("unsupported_point_score", "blocking", "矛盾或无法判断的评分点仍获得正分。", map[string]any{
				"point_id": point.ID, "score": point.Score, "relation": point.Relation, "evidence": point.Evidence,
				"generation": submission.Generation})
		}
	}
	if pointScoreCount > 0 && math.Abs(pointTotal-result.Machine) > 0.05 {
		add("point_total_mismatch", "blocking", "评分点分数合计与题目机器分不一致。", map[string]any{
			"point_total": round(pointTotal), "machine_score": result.Machine, "generation": submission.Generation,
			"scoring_method": result.Method})
	}
	return findings
}

func compareRegrades(submission Submission) []Finding {
	if submission.Generation <= 1 || len(submission.History) == 0 {
		return nil
	}
	history := append([]History(nil), submission.History...)
	sort.Slice(history, func(i, j int) bool { return history[i].Generation > history[j].Generation })
	previous := History{}
	for _, item := range history {
		if item.Generation < submission.Generation && item.Generation > previous.Generation {
			previous = item
		}
	}
	if previous.Generation == 0 {
		return nil
	}
	previousQuestions := make(map[string]QuestionResult, len(previous.Questions))
	for _, result := range previous.Questions {
		previousQuestions[result.QuestionID] = result
	}
	findings := make([]Finding, 0)
	for _, current := range submission.Questions {
		old, ok := previousQuestions[current.QuestionID]
		if !ok || old.MaxScore <= 0 || current.MaxScore <= 0 {
			continue
		}
		gap := math.Abs(current.Machine/current.MaxScore - old.Machine/old.MaxScore)
		if gap < 0.30 {
			continue
		}
		severity := "attention"
		if gap >= 0.50 {
			severity = "blocking"
		}
		findings = append(findings, Finding{RuleCode: "regrade_score_drift", Severity: severity,
			QuestionID: current.QuestionID, SubmissionIDs: []string{submission.ID}, ParticipantNames: []string{submission.Name},
			Message: "重新评分前后该题归一化机器分差异较大，需确认评分依据或版本变化。", ScoreGap: round(gap),
			Evidence: map[string]any{"previous_generation": previous.Generation, "current_generation": submission.Generation,
				"previous_machine_score": old.Machine, "current_machine_score": current.Machine,
				"max_score": current.MaxScore, "previous_scoring_method": old.Method, "current_scoring_method": current.Method,
				"previous_scoring_model": old.Model, "current_scoring_model": current.Model,
				"previous_backend": old.Backend, "current_backend": current.Backend}})
	}
	return findings
}

func pairFinding(rule, severity, questionID string, left, right Submission, leftResult, rightResult QuestionResult, gap float64, message string) Finding {
	return Finding{RuleCode: rule, Severity: severity, QuestionID: questionID,
		SubmissionIDs: []string{left.ID, right.ID}, ParticipantNames: []string{left.Name, right.Name},
		Message: message, ScoreGap: round(gap), Evidence: map[string]any{
			"left_machine_score": leftResult.Machine, "right_machine_score": rightResult.Machine,
			"left_max_score": leftResult.MaxScore, "right_max_score": rightResult.MaxScore,
			"left_generation": left.Generation, "right_generation": right.Generation,
			"left_scoring_method": leftResult.Method, "right_scoring_method": rightResult.Method,
			"left_scoring_model": leftResult.Model, "right_scoring_model": rightResult.Model,
			"left_answer": leftResult.Answer, "right_answer": rightResult.Answer,
			"left_points": leftResult.Points, "right_points": rightResult.Points,
		}}
}

func normalizedGap(left, leftMax, right, rightMax float64) float64 {
	if leftMax <= 0 || rightMax <= 0 || !finite(left) || !finite(right) {
		return 0
	}
	return math.Abs(left/leftMax - right/rightMax)
}

func normalizeAnswer(answer string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(answer) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func hasPolarityConflict(left, right string) bool {
	left, right = strings.ToLower(left), strings.ToLower(right)
	for _, pair := range [][2]string{{"不", ""}, {"未", ""}, {"无", ""}, {"不能", "能"}, {"无效", "有效"}, {"下降", "上升"}, {"降低", "提高"}, {"失败", "成功"}, {"false", "true"}, {"not", ""}} {
		if pair[0] != "" && strings.Contains(left, pair[0]) && !strings.Contains(right, pair[0]) &&
			(pair[1] == "" || strings.Contains(right, pair[1])) {
			return true
		}
		if pair[0] != "" && strings.Contains(right, pair[0]) && !strings.Contains(left, pair[0]) &&
			(pair[1] == "" || strings.Contains(left, pair[1])) {
			return true
		}
	}
	return false
}

func cloneEvidence(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func round(value float64) float64 { return math.Round(value*10000) / 10000 }
