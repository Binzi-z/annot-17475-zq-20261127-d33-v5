package meas

import (
	"crypto/sha256"
	"crypto/sha512"
	"hash"
)

// NewHasher 按算法 ID 构造标准库哈希器；未知算法返回 nil。
func NewHasher(alg AlgID) hash.Hash {
	switch alg {
	case AlgSHA256:
		return sha256.New()
	case AlgSHA384:
		return sha512.New384()
	case AlgSHA512:
		return sha512.New()
	default:
		return nil
	}
}

// DigestSum 一次性计算 data 的摘要；算法非法时返回 nil。
func DigestSum(alg AlgID, data []byte) []byte {
	h := NewHasher(alg)
	if h == nil {
		return nil
	}
	h.Write(data)
	return h.Sum(nil)
}

// Extend 实现虚构扩展语义：newValue = H(oldValue || eventDigest)。
// 输出长度由算法决定；算法非法返回 nil。
func Extend(alg AlgID, oldValue, eventDigest []byte) []byte {
	h := NewHasher(alg)
	if h == nil {
		return nil
	}
	h.Write(oldValue)
	h.Write(eventDigest)
	return h.Sum(nil)
}

// ZeroDigest 返回算法长度的全零初始寄存器值。
func ZeroDigest(alg AlgID) []byte {
	n := alg.Size()
	if n <= 0 {
		return nil
	}
	return make([]byte, n)
}
