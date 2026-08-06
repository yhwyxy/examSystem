package main

import (
	"os"
	"testing"

	"github.com/yhwyxy/examSystem/internal/config"
)

// TestMigrateDatabaseURL 角色分离: migrate 子命令优先用 EXAM_MIGRATOR_DATABASE_URL
// (exam_migrator, DDL+DML), 未设置时回退 config.database.url. serve 恒用
// EXAM_DATABASE_URL, 不受此影响 (本函数仅 migrate 路径调用).
func TestMigrateDatabaseURL(t *testing.T) {
	base := &config.Config{}
	base.Database.URL = "postgres://exam_app:app@db/exam_system?sslmode=disable"
	os.Unsetenv("EXAM_MIGRATOR_DATABASE_URL")

	t.Run("env 未设置 -> 回退 config.database.url", func(t *testing.T) {
		os.Unsetenv("EXAM_MIGRATOR_DATABASE_URL")
		if got := migrateDatabaseURL(base); got != base.Database.URL {
			t.Fatalf("got %q, want %q", got, base.Database.URL)
		}
	})

	t.Run("env 设置 -> 优先迁移角色连接串", func(t *testing.T) {
		want := "postgres://exam_migrator:mig@db/exam_system?sslmode=disable"
		t.Setenv("EXAM_MIGRATOR_DATABASE_URL", want)
		if got := migrateDatabaseURL(base); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("env 纯空白 -> 回退 config.database.url", func(t *testing.T) {
		t.Setenv("EXAM_MIGRATOR_DATABASE_URL", "   ")
		if got := migrateDatabaseURL(base); got != base.Database.URL {
			t.Fatalf("got %q, want %q", got, base.Database.URL)
		}
	})
}
