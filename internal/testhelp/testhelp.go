// Package testhelp 提供各包测试共用的确定性构造器，只依赖 meas 与 seal，
// 避免与 replay/orchestrator 形成测试期导入环。
package testhelp

import (
	"fmt"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/seal"
)

// ContentDigest 按 (设备,启动,代,序号) 派生确定性摘要。
func ContentDigest(alg meas.AlgID, device string, boot, gen, seq int) []byte {
	h := meas.NewHasher(alg)
	fmt.Fprintf(h, "%s|b%d|g%d|s%d", device, boot, gen, seq)
	return h.Sum(nil)
}

// Extend 构造一条扩展记录。
func Extend(seq int, mask uint8, alg meas.AlgID, device string, boot, gen int) meas.Record {
	return meas.Record{
		Seq: seq, Op: meas.OpExtend, Mask: meas.Mask(mask),
		Digest: ContentDigest(alg, device, boot, gen, seq),
	}
}

// Reset 构造一条重置记录。
func Reset(seq int, mask uint8) meas.Record {
	return meas.Record{Seq: seq, Op: meas.OpReset, Mask: meas.Mask(mask)}
}

// Boot 组装一个带正确签封的完整启动。
func Boot(device string, bootNumber, generation int, prevSeal []byte, records []meas.Record) meas.Boot {
	return BootWithEmpty(device, bootNumber, generation, prevSeal, nil, records)
}

// BootWithEmpty 在签封计算前先设置空库声明，保证规范头与签封一致。
func BootWithEmpty(device string, bootNumber, generation int, prevSeal []byte, emptyBanks []int, records []meas.Record) meas.Boot {
	return bootWithBanks(device, bootNumber, generation, prevSeal, emptyBanks, records)
}

func bootWithBanks(device string, bootNumber, generation int, prevSeal []byte, emptyBanks []int, records []meas.Record) meas.Boot {
	alg := meas.GenAlgorithm(generation)
	b := meas.Boot{
		DeviceID:   device,
		BootNumber: bootNumber,
		Generation: generation,
		Algorithm:  alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		EmptyBanks: emptyBanks,
		Records:    records,
	}
	var finals [meas.NumRegs][]byte
	value := make([][]byte, meas.NumRegs)
	zero := meas.ZeroDigest(alg)
	for reg := 0; reg < meas.NumRegs; reg++ {
		value[reg] = append([]byte(nil), zero...)
	}
	for _, r := range records {
		for reg := 0; reg < meas.NumRegs; reg++ {
			if uint8(r.Mask)&meas.RegMask(reg) == 0 {
				continue
			}
			switch r.Op {
			case meas.OpExtend:
				value[reg] = meas.Extend(alg, value[reg], r.Digest)
			case meas.OpReset:
				if meas.Resettable(reg) {
					value[reg] = append([]byte(nil), zero...)
				}
			}
		}
	}
	for reg := 0; reg < meas.NumRegs; reg++ {
		finals[reg] = value[reg]
	}
	// 先把含 PrevSeal 的签封骨架挂到 b 上，再计算签封值。
	// 顺序不能反：复合字面量内联求值会让 Compute 读到零值 PrevSeal，
	// 与引擎重放时的规范头不一致。
	b.Seal = meas.GenerationSeal{
		DeviceID:   device,
		BootNumber: bootNumber,
		Generation: generation,
		Algorithm:  alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		PrevSeal:   append([]byte(nil), prevSeal...),
		Sealed:     true,
	}
	b.Seal.SealValue = seal.Compute(alg, b, finals)
	return b
}

// Unsealed 把启动标记为“库已写完但总签封未落盘”。
func Unsealed(b meas.Boot) meas.Boot {
	b.Seal.Sealed = false
	b.Seal.SealValue = nil
	return b
}
