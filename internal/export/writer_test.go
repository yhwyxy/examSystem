package export

import (
	"bytes"
	"strings"
	"testing"
)

// TestXLSXWriter_Basic: 写 header + 2 row, Flush 后产物是 xlsx (ZIP magic "PK\\x03\\x04").
func TestXLSXWriter_Basic(t *testing.T) {
	w := NewXLSXWriter()
	if err := w.WriteHeader([]string{"id", "name", "score"}); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := w.WriteRow(Row{1, "alice", 9.5}); err != nil {
		t.Fatalf("row1: %v", err)
	}
	if err := w.WriteRow(Row{2, "bob", 7.0}); err != nil {
		t.Fatalf("row2: %v", err)
	}
	var buf bytes.Buffer
	n, err := w.Flush(&buf)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("flush n=%d buf=%d", n, buf.Len())
	}
	if buf.Len() < 100 {
		t.Fatalf("xlsx too small: %d bytes", buf.Len())
	}
	// ZIP magic: 50 4B 03 04
	if !strings.HasPrefix(buf.String(), "PK\x03\x04") {
		t.Fatalf("not xlsx (ZIP magic missing): first 4 bytes=%q", buf.Bytes()[:4])
	}
}

// TestXLSXWriter_Overflow: 超 DefaultRowsLimit 直接 ErrTooManyRows.
func TestXLSXWriter_Overflow(t *testing.T) {
	w := NewXLSXWriter()
	w.limit = 2 // 注入小上限
	_ = w.WriteHeader([]string{"id"})
	for i := 0; i < 2; i++ {
		if err := w.WriteRow(Row{i}); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	err := w.WriteRow(Row{999})
	if err != ErrTooManyRows {
		t.Fatalf("overflow err=%v want ErrTooManyRows", err)
	}
}
