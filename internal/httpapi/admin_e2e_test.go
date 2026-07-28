package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func TestAdminE2E(t *testing.T) {
	t.Run("save paper", func(t *testing.T) {
		pool, cleanup := testutil.NewSchema(t)
		defer cleanup()

		// papers store in tmp
		papersDir := t.TempDir()
		papersStore, err := papers.NewStore(papersDir)
		if err != nil {
			t.Fatal(err)
		}

		deps := Dependencies{
			Config:             &config.Config{Exam: config.ExamConfig{DurationMinutes: 90}},
			Pool:               pool,
			Papers:             papersStore,
			RunService:         runs.NewService(nil, papersStore, ""), // stub
			Sessions:           sessions.NewService(nil, nil),
			SubmissionsService: nil,
		}

		r := httptest.NewRequest("PUT", "/api/admin/papers/testpaper", strings.NewReader(`{"name":"test paper","exam_info":{"title":"t","description":"d","total_score":100,"passing_score":60},"questions":[]}`))
		r.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		MountAdmin(nil, deps).ServeHTTP(rr, r) // router not built, but handler called directly? Wait, no - use NewRouter but stub.

		// Better: use NewRouter with minimal deps
	}

	t.Run("publish open round", func(t *testing.T) {
		// similar
	})

	t.Run("load paper via public link", func(t *testing.T) {
		// similar
	})

	t.Run("start exam", func(t *testing.T) {
		// similar
	})
}