// Package export 提供 admin 端 XLSX 导出能力.
//
// 设计意图 (决策 2=A 真用 excelize): 走 github.com/xuri/excelize/v2 真库,
// 输出真 .xlsx 二进制 (与 plan 字面一致). 网络层 GOPROXY=https://proxy.golang.org
// + HTTPS_PROXY=6152 已探通 (Task 10 探活记录).
//
// Writer 接口预留零迁移成本 (原 decision 2=C CSV+BOM 应急方案保留不可丢, 后续切回).
package export

import (
	"errors"
	"fmt"
	"io"

	"github.com/xuri/excelize/v2"
)

// Row 是导出的一行; 类型在导出时按列类型定 (int/float/string/time).
type Row []any

// Writer 是 XLSX/CSV 等导出器的最小接口. XLSXWriter 是当前实现;
// 任何新加 CSVWriter 只需实现此接口, 调用方零改.
type Writer interface {
	// WriteHeader 写表头; 重复调用应覆写最后一行表头.
	WriteHeader(cols []string) error
	// WriteRow 追加一行数据.
	WriteRow(row Row) error
	// Flush 把整表写入参数 io.Writer (XLSX 二进制), 返回最终 byte 数.
	Flush(w io.Writer) (int64, error)
}

// ErrTooManyRows 是导出超上限 sentinel; 上限由 Plan 规定 100000 行.
var ErrTooManyRows = errors.New("EXPORT_TOO_MANY_ROWS")

// DefaultRowsLimit 默认导出上限. Plan hardening: 100000 行.
const DefaultRowsLimit = 100000

// XLSXWriter 是 Writer 的真 xlsx 实现 (excelize v2).
type XLSXWriter struct {
	f         *excelize.File
	sheet     string
	rowOffset int
	limit     int   // 最大行数; 0 = DefaultRowsLimit
	written   int   // 已写数据行数 (不含 header)
	headered  bool
	overflowed bool
}

// NewXLSXWriter 构造 XLSXWriter; sheet 名默认 "Submissions".
func NewXLSXWriter() *XLSXWriter {
	f := excelize.NewFile()
	sheet := f.GetSheetName(0) // 默认 "Sheet1"
	f.SetSheetName(sheet, "Submissions")
	return &XLSXWriter{f: f, sheet: "Submissions", limit: DefaultRowsLimit}
}

// WriteHeader 写表头到第 1 行.
func (w *XLSXWriter) WriteHeader(cols []string) error {
	if w.headered {
		return nil
	}
	defer func() { w.headered = true }()
	for i, c := range cols {
		axis, err := excelize.CoordinatesToCellName(i+1, 1)
		if err != nil {
			return err
		}
		if err := w.f.SetCellValue(w.sheet, axis, c); err != nil {
			return err
		}
	}
	w.rowOffset = 1 // 下一行从第 2 行开始
	return nil
}

// WriteRow 追加一行; 超 DefaultRowsLimit 直接返 ErrTooManyRows (写入提前 abort).
func (w *XLSXWriter) WriteRow(row Row) error {
	if w.overflowed {
		return ErrTooManyRows
	}
	if w.written >= w.limit {
		w.overflowed = true
		return ErrTooManyRows
	}
	// 使用 SetSheetRow 一次写一行 (axis = "A{n}"); excelize 自动按列序写入.
	axis := fmt.Sprintf("A%d", w.rowOffset+1)
	if err := w.f.SetSheetRow(w.sheet, axis, &row); err != nil {
		return err
	}
	w.rowOffset++
	w.written++
	return nil
}

// Flush 把 xlsx 二进制写入 w (HTTP ResponseWriter); 返回字节数.
func (w *XLSXWriter) Flush(out io.Writer) (int64, error) {
	buf, err := w.f.WriteToBuffer()
	if err != nil {
		return 0, err
	}
	defer func() { _ = w.f.Close() }()
	n, err := out.Write(buf.Bytes())
	return int64(n), err
}
