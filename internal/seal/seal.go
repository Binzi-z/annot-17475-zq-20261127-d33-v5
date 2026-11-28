// Package seal 实现虚构 BootLedger 的换代/启动签封哈希链。
//
// 签封把启动头、全部规范记录以及重放后的最终寄存器值绑定成一枚摘要，
// 并通过 PrevSeal 与同设备的上一枚签封串联。引擎重放与签封核验共用
// meas 包的规范化编码，保证证据可以脱离完整数据库独立重算。
package seal

import (
	"example.invalid/local-fixture/internal/meas"
)

// BodyHash 按记录顺序吸收每条记录的规范摘要，得到记录体哈希。
func BodyHash(alg meas.AlgID, records []meas.Record) []byte {
	h := meas.NewHasher(alg)
	for _, r := range records {
		h.Write(meas.CanonicalRecordHash(alg, r))
	}
	return h.Sum(nil)
}

// Compute 根据启动头、记录与最终寄存器值（按寄存器序号 0..5 排列，
// 不参与本次启动的寄存器位置填 nil，会被跳过）计算签封值。
func Compute(alg meas.AlgID, b meas.Boot, finalRegs [meas.NumRegs][]byte) []byte {
	h := meas.NewHasher(alg)
	h.Write(meas.DigestSum(alg, meas.CanonicalHeader(b)))
	h.Write(BodyHash(alg, b.Records))
	for reg := 0; reg < meas.NumRegs; reg++ {
		if finalRegs[reg] != nil {
			h.Write(finalRegs[reg])
		}
	}
	return h.Sum(nil)
}

// Verify 重算签封并与记录方落盘值比较，一致返回 true。
func Verify(alg meas.AlgID, b meas.Boot, finalRegs [meas.NumRegs][]byte) bool {
	got := Compute(alg, b, finalRegs)
	return equalBytes(got, b.Seal.SealValue)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
