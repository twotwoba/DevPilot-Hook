// Package id 提供无外部依赖的 UUIDv7 生成器。
//
// UUIDv7 按时间单调递增，既能做消息幂等键，也能在日志里粗略排序。
package id

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// New 返回一个 UUIDv7 字符串（8-4-4-4-12）。
func New() string {
	return NewAt(time.Now())
}

// NewAt 用指定时间生成 UUIDv7，便于测试。
func NewAt(t time.Time) string {
	var b [16]byte
	// 前 48 位：Unix 毫秒时间戳。
	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand 在正常系统上不会失败；若失败，时间戳部分仍可保证唯一性不足，
		// 因此这里用 panic 暴露环境问题，而不是静默生成可预测 ID。
		panic("id: failed to read random bytes: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// RandomHex 生成 n 字节的十六进制随机串。
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("id: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}
