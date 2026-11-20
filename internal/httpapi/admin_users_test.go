package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/adminusers"
	"github.com/yhwyxy/examSystem/internal/answersimilarity"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/review"
	"github.com/yhwyxy/examSystem/internal/scoreanomaly"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func TestNamedAdminLoginAndRoleBoundaries(t *testing.T) {
	pool, _ := testutil.NewSchema(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users := adminusers.NewStore(pool)
	if err := users.EnsureBootstrapAdmin(ctx, "admin-password", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
(id,paper_id,round_no,public_token_hash,status,duration_minutes,snapshot_path,snapshot_hash,is_legacy,opened_at,closed_at,created_at,max_score,passing_score)
VALUES ('review-queue-run','review-paper',1,'review-queue-token','closed',60,'snapshot.json',repeat('a',64),false,now(),now(),now(),10,5)`); err != nil {
		t.Fatalf("seed review queue run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO submissions
(name,employee_id,paper_id,paper_name,run_id,answers_json,grading_status,grading_generation,review_status,submitted_at)
VALUES ('Review Candidate','employee-r1','review-paper','Review Paper','review-queue-run','{}'::jsonb,'done',1,'need_review',now())`); err != nil {
		t.Fatalf("seed review queue submission: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO submissions
(name,employee_id,paper_id,paper_name,run_id,answers_json,grading_status,grading_generation,review_status,submitted_at)
VALUES ('Second Candidate','employee-r2','review-paper','Review Paper','review-queue-run','{"q1":"different answer"}'::jsonb,'done',1,'high_confidence',now())`); err != nil {
		t.Fatalf("seed second review queue submission: %v", err)
	}
	answerHash := reviewQueueAnswerHash(t, pool)
	var leftID, rightID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM submissions WHERE run_id='review-queue-run' ORDER BY id LIMIT 1`).Scan(&leftID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM submissions WHERE run_id='review-queue-run' ORDER BY id DESC LIMIT 1`).Scan(&rightID); err != nil {
		t.Fatal(err)
	}
	var similarityBatchID int64
	if err := pool.QueryRow(ctx, `INSERT INTO answer_similarity_batches
(run_id,snapshot_hash,answer_set_hash,rule_version,status,submission_count,question_count,completed_at)
VALUES ('review-queue-run',repeat('a',64),$1,'lexical-overlap-v1','completed',2,1,now()) RETURNING id`, answerHash).Scan(&similarityBatchID); err != nil {
		t.Fatalf("seed current similarity batch: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO answer_similarity_findings
(batch_id,question_id,left_submission_id,right_submission_id,left_participant_id,right_participant_id,left_name,right_name,
left_answer,right_answer,risk_level,risk_score,text_overlap,reference_similarity,shared_excerpt,left_excerpt,right_excerpt)
VALUES ($1,'q1',$2,$3,'employee-r1','employee-r2','Review Candidate','Second Candidate','same phrase A','same phrase B',
'high',0.9,0.9,0,'same phrase','same phrase A','same phrase B')`, similarityBatchID, leftID, rightID); err != nil {
		t.Fatalf("seed similarity review finding: %v", err)
	}
	for _, input := range []adminusers.CreateInput{
		{Username: "reviewer01", Password: "review-password", Role: adminusers.RoleReviewer, Actor: "admin"},
		{Username: "editor01", Password: "editor-password", Role: adminusers.RoleQuestionBankEditor, Actor: "admin"},
		{Username: "publisher01", Password: "publisher-password", Role: adminusers.RolePublisher, Actor: "admin"},
	} {
		if _, err := users.Create(ctx, input); err != nil {
			t.Fatalf("create %s: %v", input.Username, err)
		}
	}
	authStore := auth.NewStore(true, nil)
	authStore.SetPasswordProvider(auth.PasswordProvider{Authenticate: users.Authenticate})
	router := chi.NewRouter()
	router.Route("/api", func(api chi.Router) {
		MountAdmin(api, Dependencies{Auth: authStore, AdminUsers: users, Pool: pool,
			AnswerSimilarity: answersimilarity.NewService(pool, nil), ScoreAnomaly: scoreanomaly.NewService(pool),
			Review: review.NewService(pool, jobs.NewRepository())})
	})

	loginBody, _ := json.Marshal(map[string]string{"username": "reviewer01", "password": "review-password"})
	login := httptest.NewRecorder()
	router.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(loginBody)))
	if login.Code != http.StatusOK {
		t.Fatalf("named login status=%d body=%s", login.Code, login.Body.String())
	}
	var loginResponse struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginResponse); err != nil {
		t.Fatal(err)
	}
	if loginResponse.Token == "" || loginResponse.Username != "reviewer01" || loginResponse.Role != auth.RoleReviewer {
		t.Fatalf("unexpected login response: %+v", loginResponse)
	}

	adminToken, role, err := authStore.LoginUser(ctx, "admin", "admin-password")
	if err != nil || role != auth.RoleAdmin {
		t.Fatalf("admin login role=%q err=%v", role, err)
	}
	publisherToken, _, err := authStore.LoginUser(ctx, "publisher01", "publisher-password")
	if err != nil {
		t.Fatalf("publisher login: %v", err)
	}
	editorToken, _, err := authStore.LoginUser(ctx, "editor01", "editor-password")
	if err != nil {
		t.Fatalf("editor login: %v", err)
	}

	request := func(method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	if got := request(http.MethodGet, "/api/admin/users", loginResponse.Token).Code; got != http.StatusForbidden {
		t.Fatalf("reviewer user-list status=%d, want 403", got)
	}
	adminUsersResponse := request(http.MethodGet, "/api/admin/users", adminToken)
	if adminUsersResponse.Code != http.StatusOK || strings.Contains(adminUsersResponse.Body.String(), "password_hash") {
		t.Fatalf("admin user-list status=%d body=%s", adminUsersResponse.Code, adminUsersResponse.Body.String())
	}
	if got := request(http.MethodPost, "/api/admin/publications/runs/run-1/batches", loginResponse.Token).Code; got != http.StatusForbidden {
		t.Fatalf("reviewer publication status=%d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/admin/publications/runs/run-1/batches", publisherToken).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("publisher route should pass role guard then report missing service, status=%d", got)
	}
	if got := request(http.MethodGet, "/api/admin/submissions", editorToken).Code; got != http.StatusForbidden {
		t.Fatalf("editor submissions status=%d, want 403", got)
	}
	queueResponse := request(http.MethodGet, "/api/admin/review-queue", loginResponse.Token)
	if queueResponse.Code != http.StatusOK || !strings.Contains(queueResponse.Body.String(), `"kind":"submission"`) ||
		!strings.Contains(queueResponse.Body.String(), `"kind":"answer_similarity"`) ||
		!strings.Contains(queueResponse.Body.String(), `"status":"need_review"`) {
		t.Fatalf("reviewer queue status=%d body=%s", queueResponse.Code, queueResponse.Body.String())
	}
	if got := request(http.MethodGet, "/api/admin/review-queue", editorToken).Code; got != http.StatusForbidden {
		t.Fatalf("editor review queue status=%d, want 403", got)
	}
	if got := request(http.MethodGet, "/api/admin/review-queue", publisherToken).Code; got != http.StatusForbidden {
		t.Fatalf("publisher review queue status=%d, want 403", got)
	}
	var findingID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM answer_similarity_findings WHERE batch_id=$1", similarityBatchID).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	// The body cannot impersonate an actor; authentication supplies audit identity.
	reviewRequest := httptest.NewRequest(http.MethodPatch,
		"/api/admin/answer-similarity/findings/"+strconv.FormatInt(findingID, 10)+"/review",
		strings.NewReader(`{"expected_revision":0,"state":"normal_consistent","note":"核对后确认正常一致","actor":"publisher01"}`))
	reviewRequest.Header.Set("Authorization", "Bearer "+loginResponse.Token)
	reviewed := httptest.NewRecorder()
	router.ServeHTTP(reviewed, reviewRequest)
	if reviewed.Code != http.StatusOK {
		t.Fatalf("named similarity review status=%d body=%s", reviewed.Code, reviewed.Body.String())
	}
	var reviewer, actor string
	var revision int
	if err := pool.QueryRow(ctx, `SELECT f.reviewed_by,a.actor,a.revision
FROM answer_similarity_findings f JOIN answer_similarity_review_audit a ON a.finding_id=f.id
WHERE f.id=$1`, findingID).Scan(&reviewer, &actor, &revision); err != nil {
		t.Fatal(err)
	}
	if reviewer != "reviewer01" || actor != "reviewer01" || revision != 1 {
		t.Fatalf("review identity reviewer=%q actor=%q revision=%d", reviewer, actor, revision)
	}
	if _, err := pool.Exec(ctx, `UPDATE submissions SET objective_score=0,subjective_score_machine=1,
subjective_score_final=1,total_score=1,grading_detail_json='[{"question_id":"q1","type":"essay","max_score":10,"score":1,"machine_score":1,"final_score":1}]'
WHERE id=$1`, leftID); err != nil {
		t.Fatal(err)
	}
	mutate := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+loginResponse.Token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	manualPath := "/api/admin/submissions/" + strconv.FormatInt(leftID, 10) + "/review"
	for _, body := range []string{`{"question_id":"q1","note":"确认评分"}`, `{"question_id":"q1","new_score":2,"note":" "}`} {
		if response := mutate(manualPath, body); response.Code != http.StatusBadRequest {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	var unchanged float64
	var manualAudits int
	if err := pool.QueryRow(ctx, "SELECT total_score,(SELECT count(*) FROM review_logs WHERE submission_id=$1) FROM submissions WHERE id=$1", leftID).Scan(&unchanged, &manualAudits); err != nil || unchanged != 1 || manualAudits != 0 {
		t.Fatalf("invalid review mutated score=%g audits=%d err=%v", unchanged, manualAudits, err)
	}
	manual := mutate(manualPath,
		`{"question_id":"q1","new_score":2,"note":"核对评分依据后调整","actor":"publisher01"}`)
	if manual.Code != http.StatusOK {
		t.Fatal(manual.Code, manual.Body.String())
	}
	var auditGeneration int64
	if err := pool.QueryRow(ctx, "SELECT actor,grading_generation FROM review_logs WHERE submission_id=$1", leftID).Scan(&actor, &auditGeneration); err != nil || actor != "reviewer01" || auditGeneration != 1 {
		t.Fatalf("manual actor=%q generation=%d err=%v", actor, auditGeneration, err)
	}
	path := "/api/admin/regrade/" + strconv.FormatInt(leftID, 10)
	if response := mutate(path, `{}`); response.Code != http.StatusBadRequest {
		t.Fatal(response.Code, response.Body.String())
	}
	var generation int64
	if err := pool.QueryRow(ctx, "SELECT grading_generation FROM submissions WHERE id=$1", leftID).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("invalid regrade changed generation=%d err=%v", generation, err)
	}
	if response := mutate(path, `{"reason":"评分标准复核后重新计算","actor":"publisher01"}`); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var from, to int64
	if err := pool.QueryRow(ctx, "SELECT actor,from_generation,to_generation FROM regrade_audit WHERE submission_id=$1", leftID).Scan(&actor, &from, &to); err != nil || actor != "reviewer01" || from != 1 || to != 2 {
		t.Fatalf("regrade actor=%q transition=%d/%d err=%v", actor, from, to, err)
	}
	detail := request(http.MethodGet, "/api/admin/submissions/"+strconv.FormatInt(leftID, 10), loginResponse.Token)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"regrade_events":[{`) || !strings.Contains(detail.Body.String(), `"actor":"reviewer01"`) {
		t.Fatal(detail.Code, detail.Body.String())
	}
	for _, path := range []string{
		"/api/admin/submissions",
		"/api/admin/answer-similarity/runs",
		"/api/admin/score-anomalies/runs",
	} {
		if got := request(http.MethodGet, path, publisherToken).Code; got != http.StatusForbidden {
			t.Errorf("publisher %s status=%d, want 403", path, got)
		}
	}
	if got := request(http.MethodGet, "/api/admin/publications/runs", publisherToken).Code; got != http.StatusOK {
		t.Fatalf("publisher publication read should pass role guard, status=%d", got)
	}
	if got := request(http.MethodGet, "/api/admin/papers", editorToken).Code; got == http.StatusForbidden {
		t.Fatalf("editor papers unexpectedly forbidden: %d", got)
	}
	fullRouter := NewRouter(Dependencies{
		Auth:         authStore,
		ReloadConfig: func() (bool, string) { return true, "reloaded" },
	})
	for _, item := range []struct {
		name, token string
		want        int
	}{
		{name: "editor", token: editorToken, want: http.StatusForbidden},
		{name: "reviewer", token: loginResponse.Token, want: http.StatusForbidden},
		{name: "publisher", token: publisherToken, want: http.StatusForbidden},
		{name: "admin", token: adminToken, want: http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/reload-config", nil)
		req.Header.Set("Authorization", "Bearer "+item.token)
		resp := httptest.NewRecorder()
		fullRouter.ServeHTTP(resp, req)
		if resp.Code != item.want {
			t.Errorf("%s reload-config status=%d want=%d body=%s", item.name, resp.Code, item.want, resp.Body.String())
		}
	}

	createUserBody := bytes.NewBufferString(`{"username":"neweditor","password":"new-password","role":"question_bank_editor"}`)
	createRequest := httptest.NewRequest(http.MethodPost, "/api/admin/users", createUserBody)
	createRequest.Header.Set("Authorization", "Bearer "+adminToken)
	createRequest.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	router.ServeHTTP(created, createRequest)
	if created.Code != http.StatusCreated || strings.Contains(created.Body.String(), "password_hash") {
		t.Fatalf("admin create status=%d body=%s", created.Code, created.Body.String())
	}
	updateRequest := httptest.NewRequest(http.MethodPatch, "/api/admin/users/publisher01", strings.NewReader(`{"active":false,"role":"publisher"}`))
	updateRequest.Header.Set("Authorization", "Bearer "+adminToken)
	updateRequest.Header.Set("Content-Type", "application/json")
	updated := httptest.NewRecorder()
	router.ServeHTTP(updated, updateRequest)
	if updated.Code != http.StatusOK {
		t.Fatalf("admin disable user status=%d body=%s", updated.Code, updated.Body.String())
	}
	if err := authStore.VerifyToken(publisherToken); err != auth.ErrInvalidToken {
		t.Fatalf("disabled account token err=%v, want ErrInvalidToken", err)
	}
}

func reviewQueueAnswerHash(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id,employee_id,name,answers_json::text
FROM submissions WHERE run_id='review-queue-run' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hasher := sha256.New()
	for rows.Next() {
		var id int64
		var employeeID, name, answer string
		if err := rows.Scan(&id, &employeeID, &name, &answer); err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{strconv.FormatInt(id, 10), employeeID, name, answer} {
			_, _ = hasher.Write([]byte(strconv.Itoa(len(value))))
			_, _ = hasher.Write([]byte{':'})
			_, _ = hasher.Write([]byte(value))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
