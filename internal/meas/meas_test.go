package meas

import (
	"encoding/hex"
	"testing"
)

// NIST FIPS 180 已知答案向量（abc 与空串），确保标准库算法选择正确，
// 也固定了“换代算法切换”的字节级行为。
func TestNISTVectors(t *testing.T) {
	cases := []struct {
		alg  AlgID
		in   string
		want string
	}{
		{AlgSHA256, "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{AlgSHA256, "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{AlgSHA384, "abc", "cb00753f45a35e8bb5a03d699ac65007272c32ab0eded1631a8b605a43ff5bed8086072ba1e7cc2358baeca134c825a7"},
		{AlgSHA512, "abc", "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"},
	}
	for _, tc := range cases {
		got := hex.EncodeToString(DigestSum(tc.alg, []byte(tc.in)))
		if got != tc.want {
			t.Errorf("%s(%q) = %s, want %s", tc.alg, tc.in, got, tc.want)
		}
		if tc.alg.Size() != len(DigestSum(tc.alg, nil)) {
			t.Errorf("%s 长度不一致", tc.alg)
		}
	}
}

func TestGenerationAlgorithmCycle(t *testing.T) {
	want := map[int]AlgID{1: AlgSHA256, 2: AlgSHA384, 3: AlgSHA512, 4: AlgSHA256, 5: AlgSHA384, 6: AlgSHA512}
	for gen, alg := range want {
		if got := GenAlgorithm(gen); got != alg {
			t.Errorf("gen %d: got %s want %s", gen, got, alg)
		}
	}
	if GenAlgorithm(0) != "" {
		t.Fatal("非法固件代应返回空算法")
	}
}

func TestMaskLayout(t *testing.T) {
	if MaskAll != 0x3f || MaskFW != 0x03 || MaskCFG != 0x0c || MaskAPP != 0x30 {
		t.Fatalf("掩码布局错误: all=%02x fw=%02x cfg=%02x app=%02x", MaskAll, MaskFW, MaskCFG, MaskAPP)
	}
	for reg := 0; reg < NumRegs; reg++ {
		if RegBank(reg) != reg/2 || RegIndex(reg) != reg%2 {
			t.Fatalf("寄存器 %d 库映射错误", reg)
		}
		if Resettable(reg) == (RegBank(reg) == BankFW) {
			t.Fatalf("固件库不可重置、其余库可重置的不变量被破坏: %s", RegisterName(reg))
		}
	}
}

func TestCanonicalRecordRoundTrip(t *testing.T) {
	r := Record{Seq: 7, Op: OpExtend, Mask: Mask(MaskC0 | MaskA1), RefBoot: 3,
		Digest: DigestSum(AlgSHA256, []byte("d")), Expected: DigestSum(AlgSHA256, []byte("e"))}
	text := string(CanonicalRecord(r))
	back, err := ParseCanonicalRecord(text)
	if err != nil {
		t.Fatal(err)
	}
	if back.Seq != r.Seq || back.Op != r.Op || back.Mask != r.Mask || back.RefBoot != r.RefBoot ||
		!hexEqual(back.Digest, r.Digest) || !hexEqual(back.Expected, r.Expected) {
		t.Fatalf("规范编码往返不一致: %+v vs %+v", r, back)
	}
	if _, err := ParseCanonicalRecord("BLR1|x|extend|0c|0||"); err == nil {
		t.Fatal("非法序号应解析失败")
	}
}

func TestHexAndMaskJSON(t *testing.T) {
	var h HexBytes
	if err := h.UnmarshalJSON([]byte(`"abcd"`)); err != nil || len(h) != 2 || h[0] != 0xab {
		t.Fatalf("HexBytes 解码错误: %v", err)
	}
	var m Mask
	if err := m.UnmarshalJSON([]byte(`"0x3f"`)); err != nil || uint8(m) != 0x3f {
		t.Fatalf("Mask 十六进制解码错误: %v", err)
	}
	if err := m.UnmarshalJSON([]byte(`12`)); err != nil || uint8(m) != 12 {
		t.Fatalf("Mask 数字解码错误: %v", err)
	}
}

func hexEqual(a, b []byte) bool {
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
