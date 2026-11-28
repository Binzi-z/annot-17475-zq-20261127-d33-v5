package meas

import (
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// 规范化编码是“短证据可独立验证”的基础：任何实现只要按本文件的规则
// 重新拼接字节并调用同一标准库哈希，就必须得到完全一致的结果。
// 编码只使用可打印 ASCII 与 '|' 分隔，字段顺序固定，不依赖 Go map 遍历。

const (
	canonicalRecordVersion = "BLR1" // BootLedger Record v1
	canonicalHeadVersion   = "BLH1" // BootLedger Header v1
)

// CanonicalRecord 返回单条记录的确定性字节编码。
// 格式：BLR1|seq|op|mask(两位hex)|refboot|digestHex|expectedHex
func CanonicalRecord(r Record) []byte {
	parts := []string{
		canonicalRecordVersion,
		strconv.Itoa(r.Seq),
		string(r.Op),
		hex.EncodeToString([]byte{byte(r.Mask)}),
		strconv.Itoa(r.RefBoot),
		hex.EncodeToString(r.Digest),
		hex.EncodeToString(r.Expected),
	}
	return []byte(strings.Join(parts, "|"))
}

// CanonicalRecordHash 返回规范记录的摘要，作为证据中的独立锚点。
func CanonicalRecordHash(alg AlgID, r Record) []byte {
	return DigestSum(alg, CanonicalRecord(r))
}

// CanonicalHeader 返回启动头（不含记录体）的确定性编码。
// emptyBanks 会被排序，避免输入顺序影响签封。
func CanonicalHeader(b Boot) []byte {
	empty := append([]int(nil), b.EmptyBanks...)
	sort.Ints(empty)
	emptyStrs := make([]string, len(empty))
	for i, v := range empty {
		emptyStrs[i] = strconv.Itoa(v)
	}
	parts := []string{
		canonicalHeadVersion,
		b.DeviceID,
		strconv.Itoa(b.BootNumber),
		strconv.Itoa(b.Generation),
		string(b.Algorithm),
		hex.EncodeToString([]byte{byte(b.ActiveMask)}),
		strings.Join(emptyStrs, ","),
		hex.EncodeToString(b.Seal.PrevSeal),
	}
	return []byte(strings.Join(parts, "|"))
}

// ParseCanonicalRecord 是 CanonicalRecord 的逆过程，使短证据可以脱离
// 原始数据库、仅凭证据内的规范文本重建记录并重算记录哈希。
func ParseCanonicalRecord(text string) (Record, error) {
	var r Record
	parts := strings.Split(text, "|")
	if len(parts) != 7 || parts[0] != canonicalRecordVersion {
		return r, &CanonicalError{Text: text}
	}
	seq, err := strconv.Atoi(parts[1])
	if err != nil {
		return r, &CanonicalError{Text: text, Err: err}
	}
	// 字段顺序与 CanonicalRecord 严格对应：
	// 0=版本 1=seq 2=op 3=mask 4=refboot 5=digest 6=expected
	op := parts[2]
	maskRaw, err := hex.DecodeString(parts[3])
	if err != nil || len(maskRaw) != 1 {
		return r, &CanonicalError{Text: text, Err: err}
	}
	refBoot, err := strconv.Atoi(parts[4])
	if err != nil {
		return r, &CanonicalError{Text: text, Err: err}
	}
	digest, err := hex.DecodeString(parts[5])
	if err != nil {
		return r, &CanonicalError{Text: text, Err: err}
	}
	expected, err := hex.DecodeString(parts[6])
	if err != nil {
		return r, &CanonicalError{Text: text, Err: err}
	}
	r.Seq = seq
	r.Op = Op(op)
	r.Mask = Mask(maskRaw[0])
	r.RefBoot = refBoot
	r.Digest = digest
	r.Expected = expected
	return r, nil
}

// CanonicalError 表示规范记录文本无法解析。
type CanonicalError struct {
	Text string
	Err  error
}

func (e *CanonicalError) Error() string {
	if e.Err != nil {
		return "meas: 非法规范记录: " + e.Text + " (" + e.Err.Error() + ")"
	}
	return "meas: 非法规范记录: " + e.Text
}
