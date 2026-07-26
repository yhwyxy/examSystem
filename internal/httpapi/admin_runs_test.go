package httpapi

// Task 14: admin tail 真补后的轻量回归测.
// 不建真 PG fixture; 测路由真注册 + handler 错误分支 (deps nil -> 503/400).
// 真 PG 路径在 docs/cutover-go-pg.md 末尾"已知遗留5"中说明: 主控机单机部署靠 curl 实测
// 验证, 测试覆盖可在真上生产 + CI 流水线建立时再补真 fixture.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
)

// newNoDepsRouter 仅装路由 + auth (enable=false 放行), 不填 Pool/RunService/Papers.
// handler 应返 503/400.
// 注: MountAdmin 要求 deps.Auth 非 nil (Task 9 设计约束). enabled=false 时 RequireAdmin 放行.
func newNoDepsRouter(t *testing.T) http.Handler {
	t.Helper()
	deps := Dependencies{
		Config: &config.Config{Grading: config.GradingConfig{SyncGrading: false}},
		Auth:   auth.NewStore(false, func() (string, error) { return "test-secret", nil }),
	}
	deps.Config.Server.Host = "127.0.0.1"
	deps.Config.Server.Port = 8000
	return NewRouter(deps)
}

// TestRoutes_NewBatchAndExamLinkRegistered: Task 14 新加路由真挂载.
// 任一路由 404 -> Task 14 admin.go route block 写错.
func TestRoutes_NewBatchAndExamLinkRegistered(t *testing.T) {
	r := newNoDepsRouter(t)
	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/admin/papers/batch/open"},
		{http.MethodPost, "/api/admin/papers/batch/close"},
		{http.MethodGet, "/api/admin/exam-link"},
		{http.MethodPost, "/api/admin/exam-link"},
		{http.MethodPost, "/api/admin/exams/reset-rounds"},
		{http.MethodGet, "/api/admin/exams"},
		// 2026-07-26 修正: UI exam.js:746 真 调 /api/submit (前仅 /api/session/submit -> 404)
		{http.MethodPost, "/api/submit"},
		{http.MethodPost, "/api/session/submit"}, // 向后兼容保留
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		if tc.method == http.MethodGet {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		// 路由真挂 -> 非 404 (503/400 都是 handler 真响应)
		if rr.Code == http.StatusNotFound {
			t.Errorf("route %s %s returned 404 (Task 14 route missing)", tc.method, tc.path)
		}
	}
}

// TestBatchOpen_NoDeps_503: 无 RunService -> 503 RUN_SERVICE_NOT_CONFIGURED, 不 panic.
func TestBatchOpen_NoDeps_503(t *testing.T) {
	r := newNoDepsRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/papers/batch/open",
		strings.NewReader(`{"slugs":["x"],"duration_minutes":30}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("batch/open no deps: status=%d want 503; body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestBatchOpen_InvalidJSON_503: deps nil 时优先返 503 (设计 fail-fast,
// 不解 malformed body). 等 deps 真注入时才能测 400 路径.
func TestBatchOpen_InvalidJSON_503(t *testing.T) {
	r := newNoDepsRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/papers/batch/open",
		strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("batch/open bad json no deps: status=%d want 503 (deps nil 优先)", rr.Code)
	}
}

// TestExamLinkGET_NoDeps_503: 无 deps -> 503 (TestRoutes 已测路由真挂, 这里测错误分支).
func TestExamLinkGET_NoDeps_503(t *testing.T) {
	r := newNoDepsRouter(t)
	req := httptest.NewRequest(http.MethodGet,
		"/api/admin/exam-link?paper_slug=x", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	// 路由真挂 + 没 deps -> 503 (非 404)
	if rr.Code == http.StatusNotFound {
		t.Fatalf("exam-link GET: 404 (route missing); body=%s", rr.Body.String())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("exam-link GET no deps: status=%d want 503; body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestResetRounds_NoDeps_503: 无 deps -> 503.
func TestResetRounds_NoDeps_503(t *testing.T) {
	r := newNoDepsRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/exams/reset-rounds",
		strings.NewReader(`{"slugs":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code == http.StatusNotFound {
		t.Fatalf("reset-rounds: 404 (route missing); body=%s", rr.Body.String())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("reset-rounds no deps: status=%d want 503; body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestListExams_NoDeps_503: 无 deps -> 503 (handler 真响应非空数组 stub).
func TestListExams_NoDeps_503(t *testing.T) {
	r := newNoDepsRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/exams", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		// handler 真存在 + 没 Papers -> 应该返 503. 不能返 {exams: []} 空数组,
		// 否则 admin UI 会以为"没任何考试" (误空状态而不是失败状态)
		var arr []interface{}
		if json.Unmarshal(rr.Body.Bytes(), &arr) == nil {
			t.Fatalf("listExams no deps: returned array (got %s) but want 503 (deps missing, "+
				"empty array = 误导 UI 误以为 '无任何考试')", rr.Body.String())
		}
		t.Fatalf("listExams no deps: status=%d want 503; body=%s",
			rr.Code, rr.Body.String())
	}
}
