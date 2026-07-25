package papers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 实跑 Python canonical sha256 vs Go ComputeSHA256 一致 -- 用 //go:generate 等价,
// 但更简单的是在本测试里 board 一段已知 Python 真值 hash 作 baseline 校验.
// (Python 端实跑结果已记录, 不依赖运行时 python)
func TestComputeSHA256_MatchesPythonGoldenValue(t *testing.T) {
	// 关键: 用 UseNumber 解码, 否则 Go json.Unmarshal 把 60 与 60.0 都变 float64(60)
	// 破坏与 Python json.dumps(60) -> "60" 的等价.
	doc, err := LoadDocumentUseNumber(filepath.Join(fixturesRoot(t), "paper.json"))
	if err != nil {
		t.Fatalf("LoadDocumentUseNumber: %v", err)
	}
	got, err := ComputeSHA256(doc)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	const expectedPy = "82d5d739eff5c80f0e5b1f456c317a6ec5ba26b218435199cdecdf81f970d268"
	if got != expectedPy {
		t.Fatalf("sha256 ≠ Python canonical\n got=%s\n py =%s", got, expectedPy)
	}
}

// 当 fixture 文件 (paper.json) 更新后, 此 baseline 失效会暴露 -- 与 Task 0 冻结一致
var _ = strings.TrimSpace
var _ = os.ReadFile
