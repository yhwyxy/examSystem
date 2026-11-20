package answersimilarity

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const RuleVersion = "lexical-overlap-v1"

var ErrCandidateLimit = errors.New("answer similarity candidate limit exceeded")
var ErrAnswerTooLong = errors.New("answer similarity input exceeds size limit")

const maxCandidatePairs = 20_000
const maxAnswerRunes = 8_000
const maxTotalAnswerRunes = 250_000

type Question struct {
	ID              string
	Type            string
	Mode            string
	Language        string
	ReferenceAnswer string
}

type Response struct {
	SubmissionID  string
	ParticipantID string
	Name          string
	Answer        string
}

type Finding struct {
	QuestionID          string         `json:"question_id"`
	QuestionLabel       string         `json:"question_label,omitempty"`
	LeftSubmissionID    string         `json:"left_submission_id"`
	RightSubmissionID   string         `json:"right_submission_id"`
	LeftParticipantID   string         `json:"left_participant_id"`
	RightParticipantID  string         `json:"right_participant_id"`
	LeftName            string         `json:"left_name"`
	RightName           string         `json:"right_name"`
	LeftAnswer          string         `json:"left_answer"`
	RightAnswer         string         `json:"right_answer"`
	RiskLevel           string         `json:"risk_level"`
	RiskScore           float64        `json:"risk_score"`
	TextOverlap         float64        `json:"text_overlap"`
	SemanticSimilarity  float64        `json:"semantic_similarity,omitempty"`
	ReferenceSimilarity float64        `json:"reference_similarity"`
	SharedExcerpt       string         `json:"shared_excerpt"`
	LeftExcerpt         string         `json:"left_excerpt"`
	RightExcerpt        string         `json:"right_excerpt"`
	Reasons             []string       `json:"reasons"`
	CommonErrorEvidence []string       `json:"common_error_evidence"`
	RelatedQuestionIDs  []string       `json:"related_question_ids"`
	Features            map[string]any `json:"features"`
}

type Analysis struct {
	RuleVersion string    `json:"rule_version"`
	Findings    []Finding `json:"findings"`
}

type answerDoc struct {
	response Response
	original string
	tokens   []token
	terms    map[string]struct{}
	shingles map[string]struct{}
	chars    map[string]struct{}
	normal   string
	vector   []float64
}

type token struct {
	text       string
	start, end int
}

type pair struct{ left, right int }

// Analyze compares answer pairs within one frozen question version. It only emits
// review leads; it never changes grading or labels similarity as misconduct.
func Analyze(question Question, responses []Response) (Analysis, error) {
	return analyze(question, responses, nil, nil)
}

// AnalyzeWithEmbeddings adds semantic vectors to the lexical evidence. The
// reference vector is used to suppress similarities shared by both answers
// with the expected solution.
func AnalyzeWithEmbeddings(question Question, responses []Response, vectors map[string][]float64, referenceVector []float64) (Analysis, error) {
	return analyze(question, responses, vectors, referenceVector)
}

func analyze(question Question, responses []Response, vectors map[string][]float64, referenceVector []float64) (Analysis, error) {
	ruleVersion := RuleVersion
	if len(vectors) > 0 {
		ruleVersion = "hybrid-semantic-v1"
	}
	result := Analysis{RuleVersion: ruleVersion, Findings: []Finding{}}
	if question.ID == "" || len(responses) < 2 {
		return result, nil
	}
	if _, objective := map[string]bool{"single_choice": true, "multiple_choice": true, "true_false": true}[question.Type]; objective {
		return result, nil
	}
	docs := make([]answerDoc, 0, len(responses))
	seenSubmissions := map[string]bool{}
	totalRunes := 0
	vectorDimensions := len(referenceVector)
	for _, response := range responses {
		if response.SubmissionID == "" || seenSubmissions[response.SubmissionID] {
			continue
		}
		seenSubmissions[response.SubmissionID] = true
		answerRunes := utf8.RuneCountInString(response.Answer)
		if answerRunes > maxAnswerRunes {
			return Analysis{}, ErrAnswerTooLong
		}
		totalRunes += answerRunes
		if totalRunes > maxTotalAnswerRunes {
			return Analysis{}, ErrAnswerTooLong
		}
		cleaned := normalizeAnswer(response.Answer, question.Mode, question.Language)
		tokens := tokenize(cleaned)
		if len([]rune(cleaned)) < 24 || len(tokens) < 4 {
			continue // short answers are too likely to share required terms
		}
		vector := vectors[response.SubmissionID]
		if len(vector) > 0 {
			if vectorDimensions == 0 {
				vectorDimensions = len(vector)
			}
			if len(vector) != vectorDimensions || !validVector(vector) {
				return Analysis{}, ErrEmbeddingUnavailable
			}
		}
		docs = append(docs, answerDoc{
			response: response,
			tokens:   tokens,
			original: response.Answer,
			terms:    tokenSet(tokens),
			shingles: tokenShingles(tokens, 3),
			chars:    charShingles(cleaned, 4),
			normal:   cleaned,
			vector:   vector,
		})
	}
	if len(referenceVector) > 0 && (!validVector(referenceVector) || (vectorDimensions > 0 && len(referenceVector) != vectorDimensions)) {
		return Analysis{}, ErrEmbeddingUnavailable
	}
	if len(docs) < 2 {
		return result, nil
	}
	termFrequency := make(map[string]int)
	for _, doc := range docs {
		for term := range doc.terms {
			termFrequency[term]++
		}
	}
	candidates, err := candidatePairs(docs)
	if err != nil {
		return Analysis{}, err
	}
	referenceTokens := tokenize(normalizeAnswer(question.ReferenceAnswer, question.Mode, question.Language))
	referenceSet := tokenSet(referenceTokens)
	referenceShingles := tokenShingles(referenceTokens, 3)
	for _, candidate := range candidates {
		left, right := docs[candidate.left], docs[candidate.right]
		finding, ok := compare(question.ID, left, right, referenceSet, referenceShingles, termFrequency, len(docs), referenceVector)
		if ok {
			result.Findings = append(result.Findings, finding)
		}
	}
	sort.Slice(result.Findings, func(i, j int) bool {
		if result.Findings[i].RiskScore == result.Findings[j].RiskScore {
			if result.Findings[i].LeftSubmissionID == result.Findings[j].LeftSubmissionID {
				return result.Findings[i].RightSubmissionID < result.Findings[j].RightSubmissionID
			}
			return result.Findings[i].LeftSubmissionID < result.Findings[j].LeftSubmissionID
		}
		return result.Findings[i].RiskScore > result.Findings[j].RiskScore
	})
	return result, nil
}

// RaiseCrossQuestionRisk links the same participant pair across question findings.
func RaiseCrossQuestionRisk(findings []Finding) {
	questionsByPair := make(map[string]map[string]bool)
	for _, finding := range findings {
		key := participantPair(finding.LeftSubmissionID, finding.RightSubmissionID)
		if questionsByPair[key] == nil {
			questionsByPair[key] = make(map[string]bool)
		}
		questionsByPair[key][finding.QuestionID] = true
	}
	for i := range findings {
		key := participantPair(findings[i].LeftSubmissionID, findings[i].RightSubmissionID)
		questionIDs := make([]string, 0, len(questionsByPair[key]))
		for id := range questionsByPair[key] {
			questionIDs = append(questionIDs, id)
		}
		sort.Strings(questionIDs)
		findings[i].RelatedQuestionIDs = questionIDs
		if len(questionIDs) >= 2 {
			findings[i].Reasons = append(findings[i].Reasons,
				fmt.Sprintf("同一考生对在 %d 道题上出现相似答案线索。", len(questionIDs)))
			if findings[i].RiskLevel == "medium" {
				findings[i].RiskLevel = "high"
			}
		}
	}
}

func compare(questionID string, left, right answerDoc, reference, referenceShingles map[string]struct{}, frequencies map[string]int, documents int, referenceVector []float64) (Finding, bool) {
	termJaccard := jaccard(left.terms, right.terms)
	minSize := len(left.terms)
	if len(right.terms) < minSize {
		minSize = len(right.terms)
	}
	overlap := 0.0
	if minSize > 0 {
		intersection := intersectionSize(left.terms, right.terms)
		overlap = float64(intersection) / float64(minSize)
	}
	charJaccard := jaccard(left.chars, right.chars)
	baseSimilarity := 0.45*charJaccard + 0.35*termJaccard + 0.20*overlap
	leftReference := jaccard(left.terms, reference)
	rightReference := jaccard(right.terms, reference)
	sharedReference := math.Min(leftReference, rightReference)
	sharedPhrase, leftExcerpt, rightExcerpt, rareShared := sharedNonReferenceExcerpt(left, right, referenceShingles, frequencies, documents)
	semanticSimilarity := cosineSimilarity(left.vector, right.vector)
	leftReferenceSemantic := cosineSimilarity(left.vector, referenceVector)
	rightReferenceSemantic := cosineSimilarity(right.vector, referenceVector)
	semanticReference := math.Min(leftReferenceSemantic, rightReferenceSemantic)
	sharedReference = math.Max(sharedReference, semanticReference)
	adjusted := baseSimilarity * (1 - 0.60*sharedReference)
	adjustedSemantic := semanticSimilarity * (1 - 0.60*sharedReference)
	longSharedPhrase := len(strings.Fields(sharedPhrase)) >= 8
	exactNormalizedAnswer := left.normal == right.normal
	lexicalCandidate := sharedPhrase != "" && adjusted >= 0.45 && (rareShared >= 1 || longSharedPhrase || exactNormalizedAnswer)
	semanticCandidate := semanticSimilarity >= 0.82 && adjustedSemantic >= 0.45
	if !lexicalCandidate && !semanticCandidate {
		return Finding{}, false
	}
	level := "medium"
	riskScore := 0.0
	if lexicalCandidate {
		riskScore = adjusted
	}
	if semanticCandidate && adjustedSemantic > riskScore {
		riskScore = adjustedSemantic
	}
	if riskScore >= 0.85 && (rareShared >= 2 || exactNormalizedAnswer || semanticSimilarity >= 0.92) {
		level = "high"
	}
	reasons := make([]string, 0, 4)
	if lexicalCandidate {
		reasons = append(reasons, "答案存在较高的非参考答案重合片段，建议教师并排核查。")
	}
	if semanticCandidate {
		reasons = append(reasons, "语义向量显示答案含义接近；请结合题意和原文核查。")
	}
	if sharedPhrase == "" {
		reasons = append(reasons, "两份答案缺少可标注的共同原文片段，当前线索来自语义向量。")
	}
	commonErrors := commonUnexpectedNumbers(left.tokens, right.tokens, reference)
	if len(commonErrors) > 0 {
		reasons = append(reasons, "共同错误候选：两份答案包含参考答案未出现的相同数值，请结合题目条件核验。")
	}
	if sharedReference >= 0.65 {
		reasons = append(reasons, "两份答案都与参考答案接近，已降低风险分以控制标准解法误报。")
	}
	return Finding{
		QuestionID:          questionID,
		LeftSubmissionID:    left.response.SubmissionID,
		RightSubmissionID:   right.response.SubmissionID,
		LeftParticipantID:   left.response.ParticipantID,
		RightParticipantID:  right.response.ParticipantID,
		LeftName:            left.response.Name,
		RightName:           right.response.Name,
		LeftAnswer:          left.original,
		RightAnswer:         right.original,
		RiskLevel:           level,
		RiskScore:           round4(riskScore),
		TextOverlap:         round4(baseSimilarity),
		SemanticSimilarity:  round4(semanticSimilarity),
		ReferenceSimilarity: round4(sharedReference),
		SharedExcerpt:       leftExcerpt,
		LeftExcerpt:         leftExcerpt,
		RightExcerpt:        rightExcerpt,
		Reasons:             reasons,
		CommonErrorEvidence: commonErrors,
		Features: map[string]any{
			"char_4gram_jaccard":         round4(charJaccard),
			"token_jaccard":              round4(termJaccard),
			"containment_overlap":        round4(overlap),
			"rare_shared_terms":          rareShared,
			"shared_token_phrase":        sharedPhrase,
			"common_error_candidate":     len(commonErrors) > 0,
			"shared_unexpected_numbers":  commonErrors,
			"reference_similarity_left":  round4(leftReference),
			"reference_similarity_right": round4(rightReference),
			"semantic_cosine":            round4(semanticSimilarity),
			"reference_semantic_left":    round4(leftReferenceSemantic),
			"reference_semantic_right":   round4(rightReferenceSemantic),
			"semantic_candidate":         semanticCandidate,
		},
	}, true
}

func candidatePairs(docs []answerDoc) ([]pair, error) {
	pairs := make(map[pair]struct{})
	add := func(left, right int) error {
		if left > right {
			left, right = right, left
		}
		pairs[pair{left, right}] = struct{}{}
		if len(pairs) > maxCandidatePairs {
			return ErrCandidateLimit
		}
		return nil
	}
	if len(docs) <= 50 {
		for left := 0; left < len(docs); left++ {
			for right := left + 1; right < len(docs); right++ {
				if err := add(left, right); err != nil {
					return nil, err
				}
			}
		}
	} else {
		postings := make(map[string][]int)
		for index, doc := range docs {
			for shingle := range doc.shingles {
				postings[shingle] = append(postings[shingle], index)
			}
		}
		maxFrequency := len(docs) / 4
		if maxFrequency < 3 {
			maxFrequency = 3
		}
		for _, indexes := range postings {
			if len(indexes) > maxFrequency {
				continue
			}
			for a := 0; a < len(indexes); a++ {
				for b := a + 1; b < len(indexes); b++ {
					if err := add(indexes[a], indexes[b]); err != nil {
						return nil, err
					}
				}
			}
		}
		// Keep exact long duplicates as candidates even when their phrase is common.
		byAnswer := make(map[string][]int)
		for index, doc := range docs {
			byAnswer[doc.normal] = append(byAnswer[doc.normal], index)
		}
		for _, indexes := range byAnswer {
			for a := 0; a < len(indexes); a++ {
				for b := a + 1; b < len(indexes); b++ {
					if err := add(indexes[a], indexes[b]); err != nil {
						return nil, err
					}
				}
			}
		}
		semanticPairs, err := vectorCandidatePairs(docs)
		if err != nil {
			return nil, err
		}
		for _, item := range semanticPairs {
			if err := add(item.left, item.right); err != nil {
				return nil, err
			}
		}
	}
	result := make([]pair, 0, len(pairs))
	for item := range pairs {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].left == result[j].left {
			return result[i].right < result[j].right
		}
		return result[i].left < result[j].left
	})
	return result, nil
}

func vectorCandidatePairs(docs []answerDoc) ([]pair, error) {
	dimensions := 0
	for _, doc := range docs {
		if len(doc.vector) == 0 {
			continue
		}
		if dimensions == 0 {
			dimensions = len(doc.vector)
		} else if len(doc.vector) != dimensions {
			return nil, ErrEmbeddingUnavailable
		}
	}
	if dimensions == 0 {
		return nil, nil
	}
	const projectionCount = 64
	const bandWidth = 8
	const bandCount = projectionCount / bandWidth
	random := rand.New(rand.NewSource(1))
	projections := make([][]float64, projectionCount)
	for projection := range projections {
		projections[projection] = make([]float64, dimensions)
		for dimension := range projections[projection] {
			if random.Intn(2) == 0 {
				projections[projection][dimension] = -1
			} else {
				projections[projection][dimension] = 1
			}
		}
	}
	postings := make(map[uint16][]int)
	for index, doc := range docs {
		if len(doc.vector) == 0 {
			continue
		}
		var signature uint64
		for projection, hyperplane := range projections {
			dot := 0.0
			for dimension, weight := range hyperplane {
				dot += doc.vector[dimension] * weight
			}
			if dot >= 0 {
				signature |= uint64(1) << projection
			}
		}
		for band := 0; band < bandCount; band++ {
			key := uint16(band<<bandWidth) | uint16((signature>>uint(band*bandWidth))&0xff)
			postings[key] = append(postings[key], index)
		}
	}
	pairs := make(map[pair]struct{})
	for _, indexes := range postings {
		for left := 0; left < len(indexes); left++ {
			for right := left + 1; right < len(indexes); right++ {
				a, b := indexes[left], indexes[right]
				if a > b {
					a, b = b, a
				}
				pairs[pair{a, b}] = struct{}{}
				if len(pairs) > maxCandidatePairs {
					return nil, ErrCandidateLimit
				}
			}
		}
	}
	result := make([]pair, 0, len(pairs))
	for item := range pairs {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].left == result[j].left {
			return result[i].right < result[j].right
		}
		return result[i].left < result[j].left
	})
	return result, nil
}

func validVector(vector []float64) bool {
	if len(vector) == 0 || len(vector) > embeddingMaxDimensions {
		return false
	}
	norm := 0.0
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
		norm += value * value
	}
	return norm >= 1e-24
}

func cosineSimilarity(left, right []float64) float64 {
	if len(left) == 0 || len(right) == 0 || len(left) != len(right) {
		return 0
	}
	dot, leftNorm, rightNorm := 0.0, 0.0, 0.0
	for index := range left {
		dot += left[index] * right[index]
		leftNorm += left[index] * left[index]
		rightNorm += right[index] * right[index]
	}
	if leftNorm < 1e-24 || rightNorm < 1e-24 {
		return 0
	}
	value := dot / math.Sqrt(leftNorm*rightNorm)
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func normalizeByMode(value, mode string) string {
	return normalizeAnswer(value, mode, "")
}

func normalizeAnswer(value, mode, language string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	value = strings.TrimSpace(value)
	if mode == "sql" {
		value = lowercaseSQLOutsideQuotes(value)
	} else if mode != "code" {
		value = strings.ToLower(value)
	}
	switch mode {
	case "code":
		return strings.Join(strings.Fields(stripCodeComments(value, language)), "")
	case "sql":
		return normalizeSQL(value)
	case "calculation":
		return normalizeCalculation(value)
	default:
		var out strings.Builder
		for _, r := range value {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				out.WriteRune(r)
			} else if !unicode.IsSpace(r) {
				out.WriteRune(' ')
			}
		}
		return strings.Join(strings.Fields(out.String()), " ")
	}
}

func lowercaseSQLOutsideQuotes(value string) string {
	var out strings.Builder
	var quote byte
	for index := 0; index < len(value); {
		current := value[index]
		if quote != 0 {
			out.WriteByte(current)
			if current == quote {
				if index+1 < len(value) && value[index+1] == quote {
					out.WriteByte(value[index+1])
					index += 2
					continue
				}
				quote = 0
			}
			index++
			continue
		}
		if current == '\'' || current == '"' || current == '`' {
			quote = current
			out.WriteByte(current)
			index++
			continue
		}
		r, size := rune(current), 1
		if r >= 0x80 {
			r, size = utf8.DecodeRuneInString(value[index:])
		}
		out.WriteString(strings.ToLower(string(r)))
		index += size
	}
	return out.String()
}

func normalizeCalculation(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '+':
			out.WriteString(" calc_add ")
		case '-', '−', '–':
			out.WriteString(" calc_subtract ")
		case '*', '×', '·':
			out.WriteString(" calc_multiply ")
		case '/', '÷':
			out.WriteString(" calc_divide ")
		case '=':
			out.WriteString(" calc_equal ")
		case '<':
			out.WriteString(" calc_less ")
		case '>':
			out.WriteString(" calc_greater ")
		case '^':
			out.WriteString(" calc_power ")
		case '(', '（':
			out.WriteString(" calc_open ")
		case ')', '）':
			out.WriteString(" calc_close ")
		case '.':
			out.WriteString(" calc_decimal ")
		default:
			out.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func normalizeSQL(value string) string {
	value = stripSQLComments(value)
	var out strings.Builder
	var quote byte
	for index := 0; index < len(value); {
		if quote != 0 {
			out.WriteByte(value[index])
			if value[index] == quote {
				if index+1 < len(value) && value[index+1] == quote {
					out.WriteByte(value[index+1])
					index += 2
					continue
				}
				quote = 0
			}
			index++
			continue
		}
		if value[index] == '\'' || value[index] == '"' || value[index] == '`' {
			quote = value[index]
			out.WriteByte(value[index])
			index++
			continue
		}
		switch {
		case strings.HasPrefix(value[index:], "<="):
			out.WriteString(" sql_lte ")
			index += 2
		case strings.HasPrefix(value[index:], ">="):
			out.WriteString(" sql_gte ")
			index += 2
		case strings.HasPrefix(value[index:], "<>") || strings.HasPrefix(value[index:], "!="):
			out.WriteString(" sql_ne ")
			index += 2
		case value[index] == '=':
			out.WriteString(" sql_eq ")
			index++
		case value[index] == '<':
			out.WriteString(" sql_lt ")
			index++
		case value[index] == '>':
			out.WriteString(" sql_gt ")
			index++
		case value[index] == '*':
			out.WriteString(" sql_star ")
			index++
		case value[index] == '.':
			out.WriteString(" sql_dot ")
			index++
		case value[index] == '(':
			out.WriteString(" sql_open ")
			index++
		case value[index] == ')':
			out.WriteString(" sql_close ")
			index++
		default:
			r, size := rune(value[index]), 1
			if r >= 0x80 {
				r, size = utf8.DecodeRuneInString(value[index:])
			}
			out.WriteRune(r)
			index += size
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func stripSQLComments(value string) string {
	var out strings.Builder
	var quote byte
	for index := 0; index < len(value); {
		if quote != 0 {
			out.WriteByte(value[index])
			if value[index] == quote {
				if index+1 < len(value) && value[index+1] == quote {
					out.WriteByte(value[index+1])
					index += 2
					continue
				}
				quote = 0
			}
			index++
			continue
		}
		if value[index] == '\'' || value[index] == '"' || value[index] == '`' {
			quote = value[index]
			out.WriteByte(value[index])
			index++
			continue
		}
		if strings.HasPrefix(value[index:], "--") {
			for index < len(value) && value[index] != '\n' {
				index++
			}
			out.WriteByte(' ')
			continue
		}
		if strings.HasPrefix(value[index:], "/*") {
			index += 2
			for index < len(value) && !strings.HasPrefix(value[index:], "*/") {
				index++
			}
			if index < len(value) {
				index += 2
			}
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(value[index])
		index++
	}
	return out.String()
}

func stripCodeComments(value, language string) string {
	var out strings.Builder
	var quote byte
	triple := false
	escaped := false
	for index := 0; index < len(value); {
		if quote != 0 {
			if triple && index+2 < len(value) && value[index] == quote && value[index+1] == quote && value[index+2] == quote {
				out.WriteString(value[index : index+3])
				index += 3
				quote, triple = 0, false
				continue
			}
			if !triple && value[index] == quote && !escaped {
				out.WriteByte(value[index])
				index++
				quote = 0
				escaped = false
				continue
			}
			out.WriteByte(value[index])
			if value[index] == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
			index++
			continue
		}
		if value[index] == '\'' || value[index] == '"' || value[index] == '`' {
			quote = value[index]
			if index+2 < len(value) && value[index+1] == quote && value[index+2] == quote {
				triple = true
				out.WriteString(value[index : index+3])
				index += 3
			} else {
				out.WriteByte(value[index])
				index++
			}
			continue
		}
		if strings.HasPrefix(value[index:], "//") && usesSlashComments(language) {
			for index < len(value) && value[index] != '\n' {
				index++
			}
			out.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(value[index:], "/*") && usesBlockComments(language) {
			index += 2
			for index < len(value) && !strings.HasPrefix(value[index:], "*/") {
				index++
			}
			if index < len(value) {
				index += 2
			}
			out.WriteByte(' ')
			continue
		}
		if value[index] == '#' && usesHashComments(language) {
			for index < len(value) && value[index] != '\n' {
				index++
			}
			out.WriteByte('\n')
			continue
		}
		r, size := rune(value[index]), 1
		if r >= 0x80 {
			r, size = utf8.DecodeRuneInString(value[index:])
		}
		out.WriteRune(r)
		index += size
	}
	return out.String()
}

func usesHashComments(language string) bool {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "python", "bash", "shell", "ruby", "r":
		return true
	default:
		return false
	}
}

func usesSlashComments(language string) bool {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "javascript", "typescript", "java", "go", "c", "cpp", "csharp":
		return true
	default:
		return false
	}
}

func usesBlockComments(language string) bool {
	return usesSlashComments(language)
}

func tokenize(value string) []token {
	result := make([]token, 0)
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			result = append(result, token{text: value[start:end], start: start, end: end})
		}
		start = -1
	}
	for index, r := range value {
		if unicode.Is(unicode.Han, r) {
			flush(index)
			result = append(result, token{text: string(r), start: index, end: index + len(string(r))})
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' {
			if start < 0 {
				start = index
			}
			continue
		}
		flush(index)
	}
	flush(len(value))
	return result
}

func tokenSet(tokens []token) map[string]struct{} {
	result := make(map[string]struct{}, len(tokens))
	for _, item := range tokens {
		result[item.text] = struct{}{}
	}
	return result
}

func tokenShingles(tokens []token, size int) map[string]struct{} {
	result := make(map[string]struct{})
	for i := 0; i+size <= len(tokens); i++ {
		var phrase strings.Builder
		for j := 0; j < size; j++ {
			if j > 0 {
				phrase.WriteByte(' ')
			}
			phrase.WriteString(tokens[i+j].text)
		}
		result[phrase.String()] = struct{}{}
	}
	return result
}

func charShingles(value string, size int) map[string]struct{} {
	runes := []rune(value)
	result := make(map[string]struct{})
	if len(runes) < size {
		return result
	}
	for i := 0; i+size <= len(runes); i++ {
		result[string(runes[i:i+size])] = struct{}{}
	}
	return result
}

func sharedNonReferenceExcerpt(left, right answerDoc, reference map[string]struct{}, frequencies map[string]int, documents int) (string, string, string, int) {
	best, leftExcerpt, rightExcerpt, rare, bestSize := "", "", "", 0, 0
	for i := 0; i+3 <= len(left.tokens); i++ {
		maxSize := min(12, len(left.tokens)-i)
		for size := 3; size <= maxSize; size++ {
			phraseTokens := left.tokens[i : i+size]
			if !containsTokenSequence(right.tokens, phraseTokens) {
				break
			}
			if phraseCoveredByReference(phraseTokens, reference) {
				continue
			}
			rareCount := 0
			for _, item := range phraseTokens {
				if frequencies[item.text] <= rareFrequencyLimit(documents) {
					rareCount++
				}
			}
			key := tokenShingleKey(phraseTokens)
			if rareCount > rare || (rareCount == rare && (size > bestSize || (size == bestSize && (best == "" || key < best)))) {
				best = key
				leftExcerpt = findOriginalTokenExcerpt(left.original, phraseTokens)
				rightExcerpt = findOriginalTokenExcerpt(right.original, phraseTokens)
				rare = rareCount
				bestSize = size
			}
		}
	}
	return best, leftExcerpt, rightExcerpt, rare
}

func containsTokenSequence(tokens, phrase []token) bool {
	if len(phrase) == 0 || len(tokens) < len(phrase) {
		return false
	}
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		match := true
		for j := range phrase {
			if tokens[i+j].text != phrase[j].text {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func phraseCoveredByReference(phrase []token, reference map[string]struct{}) bool {
	if len(phrase) < 3 {
		return false
	}
	for i := 0; i+3 <= len(phrase); i++ {
		if _, found := reference[tokenShingleKey(phrase[i:i+3])]; !found {
			return false
		}
	}
	return true
}

func tokenShingleKey(tokens []token) string {
	parts := make([]string, len(tokens))
	for i, item := range tokens {
		parts[i] = item.text
	}
	return strings.Join(parts, " ")
}

func findOriginalTokenExcerpt(original string, target []token) string {
	tokens := tokenize(original)
	if len(target) == 0 || len(tokens) < len(target) {
		return tokenShingleKey(target)
	}
	for i := 0; i+len(target) <= len(tokens); i++ {
		match := true
		for j := range target {
			if strings.ToLower(tokens[i+j].text) != target[j].text {
				match = false
				break
			}
		}
		if match {
			return original[tokens[i].start:tokens[i+len(target)-1].end]
		}
	}
	return tokenShingleKey(target)
}

func rareFrequencyLimit(documents int) int {
	if documents < 10 {
		return 1
	}
	limit := documents / 10
	if limit < 2 {
		return 2
	}
	return limit
}

// commonUnexpectedNumbers reports numerical agreement that is absent from a
// numerical reference answer as a review clue. It deliberately has no effect on
// the risk score: alternate units and valid methods still require teacher review.
func commonUnexpectedNumbers(left, right []token, reference map[string]struct{}) []string {
	leftNumbers := make(map[string]bool)
	rightNumbers := make(map[string]bool)
	referenceHasNumber := false
	for term := range reference {
		if hasNumber(term) {
			referenceHasNumber = true
			break
		}
	}
	if !referenceHasNumber {
		return []string{}
	}
	for _, item := range left {
		if hasNumber(item.text) {
			leftNumbers[item.text] = true
		}
	}
	for _, item := range right {
		if hasNumber(item.text) {
			rightNumbers[item.text] = true
		}
	}
	common := []string{}
	for term := range leftNumbers {
		if rightNumbers[term] {
			if _, inReference := reference[term]; !inReference {
				common = append(common, term)
			}
		}
	}
	sort.Strings(common)
	if len(common) > 5 {
		common = common[:5]
	}
	return common
}

func hasNumber(value string) bool {
	for _, r := range value {
		if unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

func jaccard(left, right map[string]struct{}) float64 {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	intersection := intersectionSize(left, right)
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func intersectionSize(left, right map[string]struct{}) int {
	if len(left) > len(right) {
		left, right = right, left
	}
	count := 0
	for value := range left {
		if _, ok := right[value]; ok {
			count++
		}
	}
	return count
}

func round4(value float64) float64 { return math.Round(value*10_000) / 10_000 }

func participantPair(left, right string) string {
	if left > right {
		left, right = right, left
	}
	return left + "\x00" + right
}
