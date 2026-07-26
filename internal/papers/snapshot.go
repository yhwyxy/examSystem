package papers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Snapshot 是 LoadSnapshot 的产物: 解析过的 Document + 来源路径 + 校验过的 SHA256.
type Snapshot struct {
	Doc       Document
	Path      string
	SHA256Hex string
}

// ComputeSHA256 计算 Document 已序列化 JSON 的 SHA256 (与 Python write_run_snapshot 一致).
// canonical: sort_keys=True + separators=(",", ":") + ensure_ascii=False (无 BOM, UTF-8).
//
// 必须用 json.UseNumber 解码保持整/浮点类型区分 (Python json.dumps(60) -> "60",
// 而 json.Unmarshal 直进 float64 会被写成 "60.0" -> 与 Python hash 不一致).
// 化为 ComputeSHA256FromBytes 把已 UseNumber 解码的 Document 接进来.
func ComputeSHA256(doc Document) (string, error) {
	raw, err := canonicalJSON(doc)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// LoadDocumentUseNumber 读 JSON 文件并以 useNumber=true 解码.
// 用于 snapshot sha256 计算前确保 int/float 区分.
func LoadDocumentUseNumber(path string) (Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc Document
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}


// canonicalJSON 用 Python json.dumps(payload, ensure_ascii=False,
// sort_keys=True, separators=(",", ":")) 等价形态写字节: 不留空格, key 升序,
// UTF-8 非 ASCII 不转义. slice 顺序保持.
func canonicalJSON(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// writeCanonical 用手写紧凑 JSON writer 实现 sort_keys + 不转义非 ASCII.
func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		fmt.Fprintf(b, "%d", x)
	case int64:
		fmt.Fprintf(b, "%d", x)
	case float64:
		// 与 Python default json.dumps float 行为恢复: 整数 -> "1.0";
		// 这里用 strconv.FormatFloat 'f' -1 -> 整数会丢 .0 -> 与 Python 略不同,
		// 但 snapshot 字段中分数有 .0 形式; depot 使用 json/encoding 的 MarshalNumberDefault 写.
		writeFloatCanonical(b, x)
	case json.Number:
		// 用 UseNumber 解码后, 数字直接以原字面量形式; 写原样即与 Python json.dumps
		// 输出等价 (Python "60" -> "60", "100.0" -> "100.0").
		b.WriteString(string(x))
	case string:
		writeJSONString(b, x)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		// sort_keys=True
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		// 回退到 json.Marshal (可能转非 ASCII)
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(raw)
	}
	return nil
}

// writeFloatCanonical: 与 json.Marshal 等价表达 (默认科学记数法避免, 整数带 .0).
// Python json.dumps(3.0) -> "3.0"; Go json.Marshal(3.0) -> "3".
// 这是 hash 一致性关键 -- 把整数 float 显式写 ".0" 后缀.
func writeFloatCanonical(b *strings.Builder, v float64) {
	if v == float64(int64(v)) {
		fmt.Fprintf(b, "%d.0", int64(v))
		return
	}
	// 非整数 -- 用最短表达 ('g' 风格), 与 json.Marshal 对齐
	raw, _ := json.Marshal(v)
	b.Write(raw)
}

// writeJSONString 写 JSON 字符串字面量, 但非 ASCII 字符原样不转义
// (Python ensure_ascii=False 行为). ASCII 控制符与 " / \ 仍按 JSON 标准转义.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// LoadSnapshot 读取 paper snapshot JSON 文件, 校验 sha256 (强制可选), 返回 Snapshot.
//
// 与 Python open_run_snapshot 等价: 文件 path 必须在合法 snapshot 根目录下;
// 期望 sha256Hex != "" 则比对计算 sha256 == 期望, 不一致报 ErrChecksumMismatch.
//
// 解码使用 UseNumber (与 Service.Open 计算 snapshot_hash 的 LoadDocumentUseNumber
// 同源), 以保证 int 与 float 在 canonical JSON 中分别以 "60" / "60.0" 写出,
// 使重算 hash 与已入库的 snapshot_hash 一致.
//
// 此处只做读 + 校验; 路径防逃逸交给调用方 (store.go safeResolveWithinRoot),
// 因为本函数不应自行决定 root.
func LoadSnapshot(path, expectedSHA256Hex string) (*Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("papers.LoadSnapshot: %w", err)
	}
	var doc Document
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("papers.LoadSnapshot parse: %w", err)
	}
	sum, err := ComputeSHA256(doc)
	if err != nil {
		return nil, fmt.Errorf("papers.LoadSnapshot sha: %w", err)
	}
	if expectedSHA256Hex != "" && sum != expectedSHA256Hex {
		return nil, fmt.Errorf("papers.LoadSnapshot: snapshot checksum mismatch")
	}
	return &Snapshot{
		Doc:       doc,
		Path:      path,
		SHA256Hex: sum,
	}, nil
}

// AtomicWriteJSON 把 doc 以 JSON 缩进 2 写到 path, 临时文件 + fsync + rename.
// 与 Python _atomic_write_json 等价.
func AtomicWriteJSON(path string, doc Document) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("papers.AtomicWriteJSON mkdir: %w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("papers.AtomicWriteJSON marshal: %w", err)
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("papers.AtomicWriteJSON tmp open: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("papers.AtomicWriteJSON write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("papers.AtomicWriteJSON fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("papers.AtomicWriteJSON close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("papers.AtomicWriteJSON rename: %w", err)
	}
	return nil
}
