package httpapi

import (
	"testing"
	"time"
)

func TestBeijingTZOffset(t *testing.T) {
	utc := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	bj := utc.In(beijingTZ)
	if bj.Hour() != 10 {
		t.Fatalf("2:00 UTC -> %d:00 Beijing, want 10:00", bj.Hour())
	}
}
