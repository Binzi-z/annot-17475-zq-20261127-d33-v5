package seal

import (
	"testing"

	"example.invalid/local-fixture/internal/meas"
)

func ext(seq int, mask uint8) meas.Record {
	return meas.Record{
		Seq: seq, Op: meas.OpExtend, Mask: meas.Mask(mask),
		Digest: meas.DigestSum(meas.AlgSHA256, []byte{byte(seq)}),
	}
}

func bootWith(prev []byte, records []meas.Record) meas.Boot {
	alg := meas.AlgSHA256
	b := meas.Boot{
		DeviceID: "D", BootNumber: 1, Generation: 1, Algorithm: alg,
		ActiveMask: meas.Mask(meas.MaskAll), Records: records,
	}
	var finals [meas.NumRegs][]byte
	value := make([][]byte, meas.NumRegs)
	zero := meas.ZeroDigest(alg)
	for i := range value {
		value[i] = append([]byte(nil), zero...)
	}
	for _, r := range records {
		for reg := 0; reg < meas.NumRegs; reg++ {
			if uint8(r.Mask)&meas.RegMask(reg) != 0 && r.Op == meas.OpExtend {
				value[reg] = meas.Extend(alg, value[reg], r.Digest)
			}
		}
	}
	for i := range value {
		finals[i] = value[i]
	}
	b.Seal = meas.GenerationSeal{
		DeviceID: "D", BootNumber: 1, Generation: 1, Algorithm: alg,
		ActiveMask: meas.Mask(meas.MaskAll), PrevSeal: prev,
		SealValue: Compute(alg, b, finals), Sealed: true,
	}
	return b
}

func TestSealChainDeterministic(t *testing.T) {
	b1 := bootWith(nil, []meas.Record{ext(1, meas.MaskF0)})
	b2 := bootWith(b1.Seal.SealValue, []meas.Record{ext(1, meas.MaskC0)})
	var finals [meas.NumRegs][]byte
	if s1, s2 := Compute(meas.AlgSHA256, b2, finals), Compute(meas.AlgSHA256, b2, finals); !eq(s1, s2) {
		t.Fatal("签封计算不确定")
	}
	if Verify(meas.AlgSHA256, b2, finals) {
		t.Fatal("空 finals 不应通过签封核验")
	}
	tampered := b2
	tampered.Records = append([]meas.Record{}, b2.Records...)
	tampered.Records[0].Digest = append([]byte(nil), b2.Records[0].Digest...)
	tampered.Records[0].Digest[0] ^= 0xff
	if eq(Compute(meas.AlgSHA256, b2, finals), Compute(meas.AlgSHA256, tampered, finals)) {
		t.Fatal("记录篡改未改变签封")
	}
}

func TestBodyHashOrderSensitive(t *testing.T) {
	alg := meas.AlgSHA256
	r1, r2 := ext(1, meas.MaskF0), ext(2, meas.MaskC0)
	if eq(BodyHash(alg, []meas.Record{r1, r2}), BodyHash(alg, []meas.Record{r2, r1})) {
		t.Fatal("记录顺序变化必须改变记录体哈希")
	}
}

func eq(a, b []byte) bool {
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
