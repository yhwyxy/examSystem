package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/questionbank"
	"github.com/yhwyxy/examSystem/internal/runs"
)

func TestQuestionBankHTTPVersionedWorkflow(t *testing.T) {
	pool := testSubmPool(t)
	dataRoot := t.TempDir()
	papersRoot := filepath.Join(dataRoot, "papers")
	if err := os.MkdirAll(papersRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	papersStore, err := papers.NewStore(papersRoot)
	if err != nil {
		t.Fatal(err)
	}
	runsRepo := runs.NewRepository()
	runService := runs.NewService(runsRepo, papersStore, filepath.Join(dataRoot, "exam_runs"))
	router := NewRouter(Dependencies{
		Config:       &config.Config{},
		Auth:         auth.NewStore(false, nil),
		Pool:         pool,
		Papers:       papersStore,
		Runs:         runsRepo,
		RunService:   runService,
		QuestionBank: questionbank.NewStore(pool),
	})
	workflowVersion := 1
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		if fields, ok := body.(map[string]any); ok && (method == http.MethodPost || method == http.MethodPut) {
			fields["reason"] = "workflow integration test"
		}
		var reader *bytes.Reader
		if body == nil && (method == http.MethodPost || method == http.MethodDelete) {
			body = map[string]any{"reason": "workflow integration test", "expected_version": workflowVersion}
		}
		if body == nil {
			reader = bytes.NewReader(nil)
		} else {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	question := func(id, stem string) map[string]any {
		return map[string]any{
			"id": id, "type": "single_choice", "question": stem, "score": 2.0,
			"options": []any{map[string]any{"key": "A", "text": "甲"}, map[string]any{"key": "B", "text": "乙"}},
			"answer":  "A",
		}
	}
	input := func(id, stem string) map[string]any {
		return map[string]any{
			"id": id, "discipline": "机械工程", "difficulty": "medium",
			"knowledge_points": []string{"传动", "安全规范"}, "question": question(id, stem),
		}
	}
	decodeItem := func(response *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var item map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
			t.Fatalf("decode response: %v; body=%s", err, response.Body.String())
		}
		return item
	}

	created := request(http.MethodPost, "/api/admin/question-bank", input("q-bank-1", "齿轮传动题干"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	if item := decodeItem(created); item["version"] != float64(1) || item["status"] != "draft" || item["id"] != "q-bank-1" {
		t.Fatalf("created item = %s", created.Body.String())
	}
	if response := request(http.MethodPost, "/api/admin/question-bank/q-bank-1/enable", nil); response.Code != http.StatusConflict {
		t.Fatalf("enable draft status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPost, "/api/admin/question-bank/q-bank-1/review", nil); response.Code != http.StatusOK {
		t.Fatalf("review status=%d body=%s", response.Code, response.Body.String())
	}
	enabled := request(http.MethodPost, "/api/admin/question-bank/q-bank-1/enable", nil)
	if enabled.Code != http.StatusOK || decodeItem(enabled)["status"] != "enabled" {
		t.Fatalf("enable reviewed item status=%d body=%s", enabled.Code, enabled.Body.String())
	}
	assembled := request(http.MethodPost, "/api/admin/question-bank/assembly-snapshot", map[string]any{
		"items": []any{map[string]any{"id": "q-bank-1", "version": 1}},
	})
	var assembly struct {
		Items []map[string]any `json:"items"`
	}
	if assembled.Code != http.StatusOK || json.Unmarshal(assembled.Body.Bytes(), &assembly) != nil || len(assembly.Items) != 1 {
		t.Fatalf("assembly snapshot status=%d body=%s", assembled.Code, assembled.Body.String())
	}
	assemblyQuestion := assembly.Items[0]["question"].(map[string]any)
	if assemblyQuestion["question_bank_ref"].(map[string]any)["version"] != float64(1) || assemblyQuestion["question"] != "齿轮传动题干" {
		t.Fatalf("assembly did not include a fixed source version: %s", assembled.Body.String())
	}
	paper := map[string]any{
		"paper_id": "assembled-paper", "name": "Assembly snapshot",
		"exam_info": map[string]any{"title": "Assembly snapshot", "description": "Snapshot test", "total_score": 2.0, "passing_score": 1.0},
		"questions": []any{assemblyQuestion},
	}
	if response := request(http.MethodPut, "/api/admin/papers/assembled-paper", paper); response.Code != http.StatusOK {
		t.Fatalf("save assembled paper status=%d body=%s", response.Code, response.Body.String())
	}
	unpublished := request(http.MethodGet, "/api/admin/papers/assembled-paper", nil)
	if unpublished.Code != http.StatusOK || decodeItem(unpublished)["status"] != "unpublished" || decodeItem(unpublished)["editable"] != true {
		t.Fatalf("unpublished paper status/editable: %d %s", unpublished.Code, unpublished.Body.String())
	}
	opened := request(http.MethodPost, "/api/admin/papers/assembled-paper/open", nil)
	if opened.Code != http.StatusOK {
		t.Fatalf("open assembled paper status=%d body=%s", opened.Code, opened.Body.String())
	}
	var openBody struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &openBody); err != nil || openBody.RunID == "" {
		t.Fatalf("decode opened run: %v; body=%s", err, opened.Body.String())
	}
	locked := request(http.MethodGet, "/api/admin/papers/assembled-paper", nil)
	lockedItem := decodeItem(locked)
	if locked.Code != http.StatusOK || lockedItem["status"] != "open" || lockedItem["editable"] != false {
		t.Fatalf("active paper status/editable: %d %s", locked.Code, locked.Body.String())
	}
	var snapshotPath, snapshotHash string
	if err := pool.QueryRow(t.Context(), `SELECT snapshot_path,snapshot_hash FROM exam_runs WHERE id=$1`, openBody.RunID).Scan(&snapshotPath, &snapshotHash); err != nil {
		t.Fatal(err)
	}
	snapshot, err := papers.LoadSnapshot(snapshotPath, snapshotHash)
	if err != nil {
		t.Fatalf("load published run snapshot: %v", err)
	}
	snapshotQuestions := snapshot.Doc["questions"].([]any)
	frozenQuestion := snapshotQuestions[0].(map[string]any)
	frozenRef := frozenQuestion["question_bank_ref"].(map[string]any)
	if frozenQuestion["question"] != "齿轮传动题干" || frozenRef["id"] != "q-bank-1" || frozenRef["version"] != json.Number("1") {
		t.Fatalf("run snapshot did not preserve assembled version: %#v", frozenQuestion)
	}
	filter := url.Values{"discipline": {"机械工程"}, "knowledge_point": {"传动"}, "status": {"enabled"}}
	listed := request(http.MethodGet, "/api/admin/question-bank?"+filter.Encode(), nil)
	var list struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &list) != nil || list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("filtered list status=%d body=%s", listed.Code, listed.Body.String())
	}

	update := input("q-bank-1", "更新后的齿轮传动题干")
	update["expected_version"] = 1
	workflowVersion = 2
	updated := request(http.MethodPut, "/api/admin/question-bank/q-bank-1", update)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	if item := decodeItem(updated); item["version"] != float64(2) || item["status"] != "draft" {
		t.Fatalf("updated head = %s", updated.Body.String())
	}
	afterBankEdit, err := papers.LoadSnapshot(snapshotPath, snapshotHash)
	if err != nil {
		t.Fatalf("reload published run snapshot after bank edit: %v", err)
	}
	afterEditQuestion := afterBankEdit.Doc["questions"].([]any)[0].(map[string]any)
	if afterEditQuestion["question"] != "齿轮传动题干" || afterEditQuestion["question_bank_ref"].(map[string]any)["version"] != json.Number("1") {
		t.Fatalf("bank edit changed an already-open exam snapshot: %#v", afterEditQuestion)
	}
	stale := input("q-bank-1", "stale update")
	stale["expected_version"] = 1
	if response := request(http.MethodPut, "/api/admin/question-bank/q-bank-1", stale); response.Code != http.StatusConflict {
		t.Fatalf("stale update status=%d body=%s", response.Code, response.Body.String())
	}
	old := request(http.MethodGet, "/api/admin/question-bank/q-bank-1?version=1", nil)
	if old.Code != http.StatusOK {
		t.Fatalf("get old version status=%d body=%s", old.Code, old.Body.String())
	}
	oldItem := decodeItem(old)
	if oldItem["current"] != false || oldItem["status"] != "superseded" || oldItem["question"].(map[string]any)["question"] != "齿轮传动题干" {
		t.Fatalf("historical version was not preserved: %s", old.Body.String())
	}
	if response := request(http.MethodPost, "/api/admin/question-bank/assembly-snapshot", map[string]any{
		"items": []any{map[string]any{"id": "q-bank-1", "version": 1}},
	}); response.Code != http.StatusConflict {
		t.Fatalf("superseded version was accepted for assembly: status=%d body=%s", response.Code, response.Body.String())
	}
	versions := request(http.MethodGet, "/api/admin/question-bank/q-bank-1/versions", nil)
	var history struct {
		Versions []map[string]any `json:"versions"`
	}
	if versions.Code != http.StatusOK || json.Unmarshal(versions.Body.Bytes(), &history) != nil || len(history.Versions) != 2 {
		t.Fatalf("versions status=%d body=%s", versions.Code, versions.Body.String())
	}
	for _, action := range []string{"review", "enable", "disable"} {
		response := request(http.MethodPost, "/api/admin/question-bank/q-bank-1/"+action, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", action, response.Code, response.Body.String())
		}
	}
	archived := request(http.MethodDelete, "/api/admin/question-bank/q-bank-1", nil)
	if archived.Code != http.StatusOK || decodeItem(archived)["status"] != "archived" {
		t.Fatalf("archive status=%d body=%s", archived.Code, archived.Body.String())
	}

	badBatch := map[string]any{"items": []any{input("q-bank-2", "第二题"), input("q-bank-1", "duplicate")}}
	if response := request(http.MethodPost, "/api/admin/question-bank/import", badBatch); response.Code != http.StatusConflict {
		t.Fatalf("duplicate import status=%d body=%s", response.Code, response.Body.String())
	}
	missing := request(http.MethodGet, "/api/admin/question-bank/q-bank-2", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("partial import created an item: status=%d body=%s", missing.Code, missing.Body.String())
	}
	goodBatch := map[string]any{"items": []any{input("q-bank-2", "第二题")}}
	if response := request(http.MethodPost, "/api/admin/question-bank/import", goodBatch); response.Code != http.StatusCreated {
		t.Fatalf("import status=%d body=%s", response.Code, response.Body.String())
	}
	exported := request(http.MethodGet, "/api/admin/question-bank/export", nil)
	var exportBody struct {
		Count         int              `json:"count"`
		FormatVersion int              `json:"format_version"`
		Items         []map[string]any `json:"items"`
	}
	if exported.Code != http.StatusOK || json.Unmarshal(exported.Body.Bytes(), &exportBody) != nil || exportBody.Count != 2 || exportBody.FormatVersion != 1 || len(exportBody.Items) != 2 {
		t.Fatalf("export status=%d body=%s", exported.Code, exported.Body.String())
	}
	if response := request(http.MethodPost, "/api/admin/question-bank/import", map[string]any{
		"format_version": exportBody.FormatVersion, "items": exportBody.Items,
	}); response.Code != http.StatusConflict {
		t.Fatalf("exported JSON was not accepted by import parser: status=%d body=%s", response.Code, response.Body.String())
	}

	closeOnce := request(http.MethodPost, "/api/admin/papers/assembled-paper/close", map[string]any{"grace_seconds": 60})
	closeTwice := request(http.MethodPost, "/api/admin/papers/assembled-paper/close", map[string]any{"grace_seconds": 3600})
	var firstClose, secondClose map[string]any
	if closeOnce.Code != http.StatusOK || json.Unmarshal(closeOnce.Body.Bytes(), &firstClose) != nil {
		t.Fatalf("first close status=%d body=%s", closeOnce.Code, closeOnce.Body.String())
	}
	if closeTwice.Code != http.StatusOK || json.Unmarshal(closeTwice.Body.Bytes(), &secondClose) != nil {
		t.Fatalf("repeated close status=%d body=%s", closeTwice.Code, closeTwice.Body.String())
	}
	if firstClose["run_id"] != secondClose["run_id"] || firstClose["round_no"] != secondClose["round_no"] ||
		firstClose["status"] != "closing" || secondClose["status"] != "closing" ||
		firstClose["finalize_at"] != secondClose["finalize_at"] {
		t.Fatalf("repeated close changed run/finalize result: first=%v second=%v", firstClose, secondClose)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE exam_runs SET status='closed',closed_at=now() WHERE id=$1`, openBody.RunID); err != nil {
		t.Fatalf("finish run for close retry test: %v", err)
	}
	closeAfterFinalize := request(http.MethodPost, "/api/admin/papers/assembled-paper/close", nil)
	var closedAgain map[string]any
	if closeAfterFinalize.Code != http.StatusOK || json.Unmarshal(closeAfterFinalize.Body.Bytes(), &closedAgain) != nil ||
		closedAgain["run_id"] != firstClose["run_id"] || closedAgain["round_no"] != firstClose["round_no"] || closedAgain["status"] != "closed" {
		t.Fatalf("close after finalization was not idempotent: status=%d body=%s", closeAfterFinalize.Code, closeAfterFinalize.Body.String())
	}
}
