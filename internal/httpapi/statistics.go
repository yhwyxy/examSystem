package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type scoreSummary struct {
	Count        int64            `json:"count"`
	Average      *float64         `json:"avg_total_score"`
	Minimum      *float64         `json:"min_total_score"`
	Maximum      *float64         `json:"max_total_score"`
	PassingCount int64            `json:"passing_count"`
	PassRate     *float64         `json:"pass_rate"`
	Distribution map[string]int64 `json:"distribution_percent"`
}

type reviewProgress struct {
	RequiredCount  int64    `json:"required_count"`
	CompletedCount int64    `json:"completed_count"`
	ProgressRate   *float64 `json:"progress_rate"`
}

type statsBreakdown struct {
	Key            string         `json:"key"`
	PaperID        string         `json:"paper_id,omitempty"`
	RunID          string         `json:"run_id,omitempty"`
	RoundNo        *int           `json:"round_no,omitempty"`
	SubmittedCount int64          `json:"submitted_count"`
	ScoredCount    int64          `json:"scored_count"`
	Machine        scoreSummary   `json:"machine"`
	ReviewedFinal  scoreSummary   `json:"reviewed_final"`
	Review         reviewProgress `json:"review"`
	Published      scoreSummary   `json:"published"`
	PublishedReady bool           `json:"published_available"`
}

type questionScoreRate struct {
	PaperID           string   `json:"paper_id"`
	RunID             string   `json:"run_id"`
	RoundNo           int      `json:"round_no"`
	QuestionID        string   `json:"question_id"`
	AttemptCount      int64    `json:"attempt_count"`
	ReviewedCount     int64    `json:"reviewed_count"`
	MachineScoreRate  *float64 `json:"machine_score_rate"`
	ReviewedScoreRate *float64 `json:"reviewed_score_rate"`
}

type statisticsBreakdownResponse struct {
	GeneratedAt  time.Time           `json:"generated_at"`
	Filters      map[string]string   `json:"filters"`
	ByProfession []statsBreakdown    `json:"by_profession"`
	ByDepartment []statsBreakdown    `json:"by_department"`
	ByRound      []statsBreakdown    `json:"by_round"`
	ByQuestion   []questionScoreRate `json:"by_question"`
}

const filteredSubmissionsCTE = `
WITH filtered AS (
	SELECT s.paper_id,
	       COALESCE(NULLIF(s.paper_name, ''), s.paper_id) AS paper_name,
	       NULLIF(s.department, '') AS department,
	       s.run_id, r.round_no, r.max_score, r.passing_score,
	       s.objective_score, s.subjective_score_machine,
	       s.subjective_score_final, s.total_score,
	       s.review_status, s.grading_status, s.reviewed_at,
	       COALESCE(s.grading_detail_json, '[]'::jsonb) AS grading_detail_json
	FROM submissions s
	JOIN exam_runs r ON r.id = s.run_id
	WHERE ($1 = '' OR s.paper_id = $1)
	  AND ($2 = '' OR COALESCE(s.department, '') = $2)
	  AND ($3 = '' OR s.run_id = $3)
)
`

const statsBreakdownSQL = filteredSubmissionsCTE + `
SELECT
	CASE WHEN GROUPING(f.run_id) = 0 THEN 'round'
	     WHEN GROUPING(f.paper_id) = 0 THEN 'profession'
	     ELSE 'department' END AS scope,
	CASE WHEN GROUPING(f.run_id) = 0 THEN f.run_id
	     WHEN GROUPING(f.paper_id) = 0 THEN f.paper_name
	     ELSE COALESCE(f.department, '(未填写)') END AS group_key,
	COALESCE(f.paper_id, ''), COALESCE(f.run_id, ''), f.round_no,
	COUNT(*) AS submitted_count,
	COUNT(*) FILTER (WHERE f.grading_status = 'done') AS scored_count,
	COUNT(*) FILTER (WHERE f.grading_status = 'done') AS machine_count,
	ROUND(AVG(f.objective_score + f.subjective_score_machine)
		FILTER (WHERE f.grading_status = 'done')::numeric, 2)::float8 AS machine_avg,
	MIN(f.objective_score + f.subjective_score_machine)
		FILTER (WHERE f.grading_status = 'done') AS machine_min,
	MAX(f.objective_score + f.subjective_score_machine)
		FILTER (WHERE f.grading_status = 'done') AS machine_max,
	COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.passing_score IS NOT NULL
		AND f.objective_score + f.subjective_score_machine >= f.passing_score) AS machine_pass_count,
	ROUND((COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.passing_score IS NOT NULL
		AND f.objective_score + f.subjective_score_machine >= f.passing_score)::numeric /
		NULLIF(COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.passing_score IS NOT NULL), 0)), 4)::float8 AS machine_pass_rate,
	jsonb_build_object(
		'0-49%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score < 50),
		'50-59%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score >= 50 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score < 60),
		'60-69%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score >= 60 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score < 70),
		'70-79%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score >= 70 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score < 80),
		'80-89%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score >= 80 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score < 90),
		'90-100%+', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_machine) * 100 / f.max_score >= 90),
		'unknown_max_score', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND COALESCE(f.max_score, 0) <= 0)
	) AS machine_distribution,
	COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed') AS reviewed_count,
	ROUND(AVG(f.objective_score + f.subjective_score_final)
		FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed')::numeric, 2)::float8 AS reviewed_avg,
	MIN(f.objective_score + f.subjective_score_final)
		FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed') AS reviewed_min,
	MAX(f.objective_score + f.subjective_score_final)
		FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed') AS reviewed_max,
	COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed'
		AND f.passing_score IS NOT NULL AND f.objective_score + f.subjective_score_final >= f.passing_score) AS reviewed_pass_count,
	ROUND((COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed'
		AND f.passing_score IS NOT NULL AND f.objective_score + f.subjective_score_final >= f.passing_score)::numeric /
		NULLIF(COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL
		AND f.review_status = 'reviewed' AND f.passing_score IS NOT NULL), 0)), 4)::float8 AS reviewed_pass_rate,
	jsonb_build_object(
		'0-49%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score < 50),
		'50-59%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score >= 50 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score < 60),
		'60-69%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score >= 60 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score < 70),
		'70-79%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score >= 70 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score < 80),
		'80-89%', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score >= 80 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score < 90),
		'90-100%+', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND f.max_score > 0 AND (f.objective_score + f.subjective_score_final) * 100 / f.max_score >= 90),
		'unknown_max_score', COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed' AND COALESCE(f.max_score, 0) <= 0)
	) AS reviewed_distribution,
	COUNT(*) FILTER (WHERE f.review_status IN ('pending', 'need_review', 'low_confidence', 'partial_manual')
		OR f.grading_status = 'failed'
		OR (f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed')) AS review_required,
	COUNT(*) FILTER (WHERE f.grading_status = 'done' AND f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed') AS review_completed
FROM filtered f
GROUP BY GROUPING SETS (
	(f.paper_id, f.paper_name),
	(f.department),
	(f.paper_id, f.paper_name, f.run_id, f.round_no)
)
	ORDER BY scope, group_key, round_no NULLS FIRST
`

const questionScoreRatesSQL = filteredSubmissionsCTE + `
, question_items AS (
	SELECT f.paper_id, f.run_id, f.round_no,
	       CASE WHEN jsonb_array_length(children.entries) > 0
	            THEN CASE WHEN strpos(ids.item_id, '/') > 0 THEN ids.item_id
	                      ELSE ids.parent_id || '/' || ids.item_id END
	            ELSE ids.item_id END AS question_id,
	       CASE WHEN jsonb_typeof(item.value -> 'max_score') = 'number'
	            THEN (item.value ->> 'max_score')::float8 END AS max_score,
	       COALESCE(
	         CASE WHEN jsonb_typeof(item.value -> 'machine_score') = 'number'
	              THEN (item.value ->> 'machine_score')::float8 END,
	         CASE WHEN jsonb_typeof(item.value -> 'score') = 'number'
	              THEN (item.value ->> 'score')::float8 END
	       ) AS machine_score,
	       COALESCE(
	         CASE WHEN jsonb_typeof(item.value -> 'final_score') = 'number'
	              THEN (item.value ->> 'final_score')::float8 END,
	         CASE WHEN jsonb_typeof(item.value -> 'score') = 'number'
	              THEN (item.value ->> 'score')::float8 END
	       ) AS final_score,
	       (f.reviewed_at IS NOT NULL AND f.review_status = 'reviewed') AS fully_reviewed
	FROM filtered f
	CROSS JOIN LATERAL jsonb_array_elements(
		CASE WHEN jsonb_typeof(f.grading_detail_json) = 'array'
		     THEN f.grading_detail_json ELSE '[]'::jsonb END
	) parent(value)
	CROSS JOIN LATERAL (
		SELECT CASE WHEN jsonb_typeof(parent.value -> 'sub_results') = 'array'
		            THEN parent.value -> 'sub_results' ELSE '[]'::jsonb END AS entries
	) children
	CROSS JOIN LATERAL jsonb_array_elements(
		CASE WHEN jsonb_array_length(children.entries) > 0
		     THEN children.entries
		     ELSE jsonb_build_array(parent.value) END
	) item(value)
	CROSS JOIN LATERAL (
		SELECT COALESCE(NULLIF(parent.value ->> 'question_id', ''), NULLIF(parent.value ->> 'id', '')) AS parent_id,
		       COALESCE(NULLIF(item.value ->> 'question_id', ''), NULLIF(item.value ->> 'id', '')) AS item_id
	) ids
	WHERE f.grading_status = 'done'
)
SELECT paper_id, run_id, round_no, question_id,
	COUNT(*) FILTER (WHERE max_score > 0 AND machine_score IS NOT NULL) AS attempt_count,
	COUNT(*) FILTER (WHERE fully_reviewed AND max_score > 0 AND final_score IS NOT NULL) AS reviewed_count,
	ROUND(AVG(machine_score / NULLIF(max_score, 0)) FILTER (WHERE max_score > 0 AND machine_score IS NOT NULL)::numeric, 4)::float8 AS machine_score_rate,
	ROUND(AVG(final_score / NULLIF(max_score, 0)) FILTER (WHERE fully_reviewed AND max_score > 0 AND final_score IS NOT NULL)::numeric, 4)::float8 AS reviewed_score_rate
FROM question_items
WHERE COALESCE(question_id, '') <> ''
GROUP BY paper_id, run_id, round_no, question_id
ORDER BY paper_id, round_no, question_id
`

const publishedScoreBreakdownSQL = `
WITH latest AS (
	SELECT DISTINCT ON (run_id) id,run_id,paper_id,paper_name
	FROM score_publication_batches ORDER BY run_id,version DESC
), published AS (
	SELECT b.run_id,b.paper_id,b.paper_name,r.round_no,r.passing_score,g.department,g.total_score,g.max_score
	FROM latest b JOIN exam_runs r ON r.id=b.run_id
	JOIN score_publication_grades g ON g.batch_id=b.id
	WHERE ($1='' OR b.paper_id=$1) AND ($2='' OR COALESCE(g.department,'')=$2) AND ($3='' OR b.run_id=$3)
)
SELECT CASE WHEN GROUPING(run_id)=0 THEN 'round'
		WHEN GROUPING(paper_name)=0 THEN 'profession' ELSE 'department' END AS scope,
	CASE WHEN GROUPING(run_id)=0 THEN run_id
		WHEN GROUPING(paper_name)=0 THEN paper_name ELSE COALESCE(department,'(未填写)') END AS group_key,
	COALESCE(paper_id,''),COALESCE(run_id,''),round_no,
	count(*),round(avg(total_score)::numeric,2)::float8,min(total_score),max(total_score),
	count(*) FILTER (WHERE passing_score IS NOT NULL AND total_score>=passing_score),
	round((count(*) FILTER (WHERE passing_score IS NOT NULL AND total_score>=passing_score)::numeric /
	NULLIF(count(*) FILTER (WHERE passing_score IS NOT NULL),0)),4)::float8,
	jsonb_build_object(
		'0-49%',count(*) FILTER (WHERE total_score*100/max_score<50),
		'50-59%',count(*) FILTER (WHERE total_score*100/max_score>=50 AND total_score*100/max_score<60),
		'60-69%',count(*) FILTER (WHERE total_score*100/max_score>=60 AND total_score*100/max_score<70),
		'70-79%',count(*) FILTER (WHERE total_score*100/max_score>=70 AND total_score*100/max_score<80),
		'80-89%',count(*) FILTER (WHERE total_score*100/max_score>=80 AND total_score*100/max_score<90),
		'90-100%+',count(*) FILTER (WHERE total_score*100/max_score>=90)
	)
FROM published
GROUP BY GROUPING SETS ((paper_id,paper_name),(department),(paper_id,paper_name,run_id,round_no))
ORDER BY scope,group_key,round_no NULLS FIRST
`

func adminStatisticsBreakdownHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SUBMISSIONS_POOL_NOT_CONFIGURED", "database pool missing")
			return
		}
		// Counts, per-question rates and immutable publication versions must
		// describe the same point in time even when a correction commits mid-read.
		tx, err := pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "STATS_QUERY_FAILED", "statistics snapshot unavailable")
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		q := r.URL.Query()
		filters := map[string]string{
			"paper_id":   strings.TrimSpace(q.Get("paper_id")),
			"department": strings.TrimSpace(q.Get("department")),
			"run_id":     strings.TrimSpace(q.Get("run_id")),
		}
		resp := statisticsBreakdownResponse{
			GeneratedAt: time.Now().UTC(), Filters: filters,
			ByProfession: []statsBreakdown{}, ByDepartment: []statsBreakdown{},
			ByRound: []statsBreakdown{}, ByQuestion: []questionScoreRate{},
		}
		rows, err := tx.Query(r.Context(), statsBreakdownSQL, filters["paper_id"], filters["department"], filters["run_id"])
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "STATS_QUERY_FAILED", "breakdown: "+err.Error())
			return
		}
		for rows.Next() {
			var (
				scope, key, paperID, runID                                               string
				roundNo                                                                  pgtype.Int4
				submitted, scored, machineCount, machinePasses, reviewed, reviewedPasses int64
				reviewRequired, reviewCompleted                                          int64
				machineAvg, machineMin, machineMax, machinePassRate                      pgtype.Float8
				reviewedAvg, reviewedMin, reviewedMax, reviewedPassRate                  pgtype.Float8
				machineDistributionRaw, reviewedDistributionRaw                          []byte
			)
			if err := rows.Scan(&scope, &key, &paperID, &runID, &roundNo,
				&submitted, &scored, &machineCount, &machineAvg, &machineMin, &machineMax,
				&machinePasses, &machinePassRate, &machineDistributionRaw,
				&reviewed, &reviewedAvg, &reviewedMin, &reviewedMax, &reviewedPasses,
				&reviewedPassRate, &reviewedDistributionRaw, &reviewRequired, &reviewCompleted); err != nil {
				rows.Close()
				writeAdminError(w, http.StatusInternalServerError, "STATS_SCAN_FAILED", err.Error())
				return
			}
			var machineDistribution, reviewedDistribution map[string]int64
			if err := json.Unmarshal(machineDistributionRaw, &machineDistribution); err != nil {
				rows.Close()
				writeAdminError(w, http.StatusInternalServerError, "STATS_DECODE_FAILED", "machine distribution: "+err.Error())
				return
			}
			if err := json.Unmarshal(reviewedDistributionRaw, &reviewedDistribution); err != nil {
				rows.Close()
				writeAdminError(w, http.StatusInternalServerError, "STATS_DECODE_FAILED", "review distribution: "+err.Error())
				return
			}
			var round *int
			if roundNo.Valid {
				v := int(roundNo.Int32)
				round = &v
			}
			reviewRate := ratio(reviewCompleted, reviewRequired)
			item := statsBreakdown{
				Key: key, PaperID: paperID, RunID: runID, RoundNo: round,
				SubmittedCount: submitted, ScoredCount: scored,
				Machine: scoreSummary{
					Count: machineCount, Average: nullableFloat(machineAvg), Minimum: nullableFloat(machineMin), Maximum: nullableFloat(machineMax),
					PassingCount: machinePasses, PassRate: nullableFloat(machinePassRate), Distribution: machineDistribution,
				},
				ReviewedFinal: scoreSummary{
					Count: reviewed, Average: nullableFloat(reviewedAvg), Minimum: nullableFloat(reviewedMin), Maximum: nullableFloat(reviewedMax),
					PassingCount: reviewedPasses, PassRate: nullableFloat(reviewedPassRate), Distribution: reviewedDistribution,
				},
				Review:    reviewProgress{RequiredCount: reviewRequired, CompletedCount: reviewCompleted, ProgressRate: reviewRate},
				Published: scoreSummary{Count: 0, Distribution: map[string]int64{}}, PublishedReady: false,
			}
			switch scope {
			case "profession":
				resp.ByProfession = append(resp.ByProfession, item)
			case "department":
				resp.ByDepartment = append(resp.ByDepartment, item)
			case "round":
				resp.ByRound = append(resp.ByRound, item)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeAdminError(w, http.StatusInternalServerError, "STATS_QUERY_FAILED", "iterate breakdown: "+err.Error())
			return
		}
		rows.Close()

		questionRows, err := tx.Query(r.Context(), questionScoreRatesSQL, filters["paper_id"], filters["department"], filters["run_id"])
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "STATS_QUERY_FAILED", "question rates: "+err.Error())
			return
		}
		defer questionRows.Close()
		for questionRows.Next() {
			var item questionScoreRate
			var machineRate, reviewedRate pgtype.Float8
			if err := questionRows.Scan(&item.PaperID, &item.RunID, &item.RoundNo, &item.QuestionID,
				&item.AttemptCount, &item.ReviewedCount, &machineRate, &reviewedRate); err != nil {
				writeAdminError(w, http.StatusInternalServerError, "STATS_SCAN_FAILED", err.Error())
				return
			}
			item.MachineScoreRate = nullableFloat(machineRate)
			item.ReviewedScoreRate = nullableFloat(reviewedRate)
			resp.ByQuestion = append(resp.ByQuestion, item)
		}
		if err := questionRows.Err(); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "STATS_QUERY_FAILED", "iterate question rates: "+err.Error())
			return
		}
		questionRows.Close()
		if err := applyPublishedBreakdown(r.Context(), tx, filters, &resp); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PUBLISHED_STATS_FAILED", "published score breakdown is unavailable")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "STATS_QUERY_FAILED", "statistics snapshot read failed")
			return
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

func applyPublishedBreakdown(ctx context.Context, tx pgx.Tx, filters map[string]string, response *statisticsBreakdownResponse) error {
	rows, err := tx.Query(ctx, publishedScoreBreakdownSQL, filters["paper_id"], filters["department"], filters["run_id"])
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var scope, key, paperID, runID string
		var roundNo pgtype.Int4
		var count, passes int64
		var avg, min, max, passRate pgtype.Float8
		var distributionRaw []byte
		if err := rows.Scan(&scope, &key, &paperID, &runID, &roundNo, &count, &avg, &min, &max, &passes, &passRate, &distributionRaw); err != nil {
			return err
		}
		var distribution map[string]int64
		if err := json.Unmarshal(distributionRaw, &distribution); err != nil {
			return err
		}
		var items []statsBreakdown
		switch scope {
		case "profession":
			items = response.ByProfession
		case "department":
			items = response.ByDepartment
		case "round":
			items = response.ByRound
		}
		for index := range items {
			if items[index].Key != key {
				continue
			}
			if scope == "round" && (items[index].RunID != runID || items[index].PaperID != paperID) {
				continue
			}
			items[index].Published = scoreSummary{Count: count, Average: nullableFloat(avg), Minimum: nullableFloat(min), Maximum: nullableFloat(max),
				PassingCount: passes, PassRate: nullableFloat(passRate), Distribution: distribution}
			items[index].PublishedReady = count > 0
			break
		}
	}
	return rows.Err()
}

func nullableFloat(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	value := v.Float64
	return &value
}

func ratio(numerator, denominator int64) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}
