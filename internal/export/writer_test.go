package export

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
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

// TestXLSXWriter_MultiSheet: 重命名主表 + 新建分表, 交替写入时各 sheet 行号
// 独立, Flush 产物包含全部 sheet 且内容正确.
func TestXLSXWriter_MultiSheet(t *testing.T) {
	w := NewXLSXWriter()
	if err := w.RenameSheet("Submissions", "总表"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := w.WriteHeader([]string{"专业", "工号"}); err != nil {
		t.Fatalf("总表 header: %v", err)
	}
	if err := w.WriteRow(Row{"专业1", "emp-1"}); err != nil {
		t.Fatalf("总表 row1: %v", err)
	}
	if err := w.NewSheet("专业1"); err != nil {
		t.Fatalf("new sheet: %v", err)
	}
	if err := w.WriteHeader([]string{"专业", "工号"}); err != nil {
		t.Fatalf("专业1 header: %v", err)
	}
	if err := w.WriteRow(Row{"专业1", "emp-1"}); err != nil {
		t.Fatalf("专业1 row1: %v", err)
	}
	if err := w.SelectSheet("总表"); err != nil {
		t.Fatalf("select 总表: %v", err)
	}
	// 切回总表后行号应继续 (第 3 行), 而不是回到 1.
	if err := w.WriteRow(Row{"专业1", "emp-2"}); err != nil {
		t.Fatalf("总表 row2: %v", err)
	}
	var buf bytes.Buffer
	if _, err := w.Flush(&buf); err != nil {
		t.Fatalf("flush: %v", err)
	}
	f, err := excelize.OpenReader(&buf)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	if got := f.GetSheetList(); len(got) != 2 || got[0] != "总表" || got[1] != "专业1" {
		t.Fatalf("sheets=%q", got)
	}
	mainRows, err := f.GetRows("总表")
	if err != nil {
		t.Fatalf("总表 rows: %v", err)
	}
	if len(mainRows) != 3 || mainRows[1][1] != "emp-1" || mainRows[2][1] != "emp-2" {
		t.Fatalf("总表 rows=%q", mainRows)
	}
	subRows, err := f.GetRows("专业1")
	if err != nil {
		t.Fatalf("专业1 rows: %v", err)
	}
	if len(subRows) != 2 || subRows[1][1] != "emp-1" {
		t.Fatalf("专业1 rows=%q", subRows)
	}
}
