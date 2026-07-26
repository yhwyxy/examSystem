package migrations

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex 计算字节切片的 SHA256 十六进制摘要, 用于 checksum 校验.
func sha256Hex(b []byte) (string, error) {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
