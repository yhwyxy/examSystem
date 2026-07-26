// Package migrations 把 migrations/*.sql 通过 go:embed 嵌进二进制.
//
// 嵌入策略: 嵌入 migrations 目录下所有 .sql 文件; 由 Go migrator
// 负责按文件名(版本)排序后逐个 apply.
//
// 想 verify checksum + 防止外部修改, 把每个文件的字节作 SHA256.
package migrations

import (
	"embed"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed *.sql
var fsys embed.FS

// File 是单个 migration 的元数据 + 原文 + SHA256 checksum.
type File struct {
	Version  string // 文件名去 .sql
	Path     string // migrations/xxxx.sql 完整路径
	Source   string // SQL 原文
	Checksum string // SHA256 hex
}

// All 按文件名升序返回所有 migration 文件.
//
// 嵌入是 build-time 资源, 所以外界无法对 SQL 内容做修改;
// 但 checksum mismatch 测试需通过修改 schema_migrations 表的 checksum 来触发.
// 所以 checksum 必须 Go 端实时计算 + 写库, 不是 SQL 内嵌固定值.
func All() ([]File, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		raw, err := fsys.ReadFile(name)
		if err != nil {
			return nil, err
		}
		sum, err := sha256Hex(raw)
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Version:  strings.TrimSuffix(name, ".sql"),
			Path:     filepath.Join("migrations", name), // 显示用, 实际加载已嵌入
			Source:   string(raw),
			Checksum: sum,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Version < files[j].Version
	})
	return files, nil
}
