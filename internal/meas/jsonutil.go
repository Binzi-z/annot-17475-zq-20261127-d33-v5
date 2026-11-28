package meas

import (
	"encoding/hex"
	"strconv"
)

// parseMaskString 解析 "0x3f" / "3f" / "63" 形式的掩码字符串。
func parseMaskString(s string) (uint8, error) {
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 8)
		if err != nil {
			return 0, err
		}
		return uint8(v), nil
	}
	// 不带前缀时按十六进制处理（掩码习惯写法）。
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0, err
	}
	return uint8(v), nil
}

// parseInt 把 JSON 数字字节解码进 uint8（避免引入 encoding/json 环依赖）。
func parseInt(data []byte, out *uint8) (int, error) {
	n, err := strconv.ParseUint(string(data), 10, 8)
	if err != nil {
		return 0, err
	}
	*out = uint8(n)
	return int(n), nil
}

// decodeHex 是包内统一的十六进制解码入口。
func decodeHex(s string) ([]byte, error) { return hex.DecodeString(s) }
