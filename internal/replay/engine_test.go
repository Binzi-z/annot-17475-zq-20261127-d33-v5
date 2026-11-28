package replay

import (
	"testing"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/testhelp"
)

func sealedBoot(t *testing.T, records []meas.Record) meas.Boot {
	t.Helper()
	return testhelp.Boot("T-DEV", 1, 1, nil, records)
}

// 扩展链：重算值必须等于手工 H 链，重置与扩展语义分离。
func TestExtendChainAndReset(t *testing.T) {
	alg := meas.AlgSHA256
	records := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "T-DEV", 1, 1),
		testhelp.Extend(3, meas.MaskA0, alg, "T-DEV", 1, 1),
		testhelp.Reset(4, meas.MaskA0),
		testhelp.Extend(5, meas.MaskA0, alg, "T-DEV", 1, 1),
	}
	b := sealedBoot(t, records)
	rep := Boot(b)
	if rep.Status != meas.BootOK {
		t.Fatalf("干净启动应 OK, got %s; conflicts=%+v", rep.Status, rep.Conflicts)
	}
	// seq3 扩展、seq4 归零、seq5 在零值上再扩展；A0 是第 4 号寄存器。
	got := rep.Registers[4].Final
	expect := meas.Extend(alg, meas.ZeroDigest(alg), records[4].Digest)
	if !eq(got, expect) {
		t.Fatalf("重置后扩展结果错误\n got=%x\nwant=%x", got, expect)
	}
	if rep.Registers[4].LastEventSeq != 5 {
		t.Fatalf("最后事件序号错误: %d", rep.Registers[4].LastEventSeq)
	}

	// 末操作是重置时，寄存器状态应为 RESET。
	resetOnly := sealedBoot(t, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
		testhelp.Extend(2, meas.MaskC1, alg, "T-DEV", 1, 1),
		testhelp.Extend(3, meas.MaskA1, alg, "T-DEV", 1, 1),
		testhelp.Reset(4, meas.MaskA1),
	})
	if Boot(resetOnly).Registers[5].Status != meas.RegReset {
		t.Fatal("末操作为重置时状态应为 RESET")
	}
}

// 固件库重置被拒绝：产生冲突，但固件寄存器保持原值不被清零。
func TestFirmwareResetRejected(t *testing.T) {
	alg := meas.AlgSHA256
	records := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
		testhelp.Reset(2, meas.MaskFW),
	}
	b := testhelp.Boot("T-DEV", 1, 1, nil, records)
	rep := Boot(b)
	if rep.Status != meas.BootConflicted || rep.FirstConflict == nil ||
		rep.FirstConflict.Rule != meas.ConfFWReset {
		t.Fatalf("固件重置应产生 FIRMWARE_RESET_REJECTED, got %+v", rep.FirstConflict)
	}
	// F0 仍保留扩展后的值，未被清零。
	expectF0 := meas.Extend(alg, meas.ZeroDigest(alg), records[0].Digest)
	if !eq(rep.Registers[0].Final, expectF0) {
		t.Fatal("固件寄存器不应被非法重置清零")
	}
}

// 核心区分：某库缺记录 = 无法证实；另一库证据篡改 = 确认冲突。
func TestMissingBankVersusConflict(t *testing.T) {
	alg := meas.AlgSHA256
	// 场景 A：应用库整库缺记录 → UNPROVABLE，且无冲突。
	missing := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "T-DEV", 1, 1),
	}
	bA := testhelp.Boot("T-DEV", 1, 1, nil, missing)
	repA := Boot(bA)
	if repA.Status != meas.BootUnprovable || len(repA.Conflicts) != 0 {
		t.Fatalf("缺库应是无法证实而非冲突: status=%s conflicts=%d", repA.Status, len(repA.Conflicts))
	}
	found := false
	for _, rg := range repA.Unprovable {
		if rg.Reason == meas.UnprovBankMissing && rg.BankName == "APP" {
			found = true
		}
	}
	if !found {
		t.Fatal("未报告 APP 库 BANK_NO_RECORD 范围")
	}
	if repA.Registers[4].Status != meas.RegUnprovable {
		t.Fatalf("A0 应为 UNPROVABLE, got %s", repA.Registers[4].Status)
	}

	// 场景 B：配置库证据前后冲突 → CONFLICTED，且 LastValid 冻结在冲突前。
	tampered := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 2, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "T-DEV", 2, 1),
	}
	tampered[1].Expected = meas.ZeroDigest(alg) // 必然与重算值不同
	bB := testhelp.Boot("T-DEV", 2, 1, nil, tampered)
	repB := Boot(bB)
	if repB.Status != meas.BootConflicted || repB.FirstConflict.Rule != meas.ConfEvidenceMismatch {
		t.Fatalf("证据篡改应是确认冲突: %+v", repB.FirstConflict)
	}
	if repB.Registers[2].Status != meas.RegConflicted {
		t.Fatalf("C0 应为 CONFLICTED, got %s", repB.Registers[2].Status)
	}
}

// 序号缺口无法证实；任意位置序号重复都是冲突。
func TestSeqGapAndDuplicate(t *testing.T) {
	alg := meas.AlgSHA256
	base := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "T-DEV", 1, 1),
		testhelp.Extend(3, meas.MaskA0, alg, "T-DEV", 1, 1),
	}
	gap := []meas.Record{base[0], base[2]}
	repGap := Boot(testhelp.Boot("T-DEV", 1, 1, nil, gap))
	if repGap.Status != meas.BootUnprovable {
		t.Fatalf("序号缺口应无法证实, got %s", repGap.Status)
	}
	dup := append(append([]meas.Record{}, base...), base[1]) // seq=2 在尾部重复（非相邻）
	repDup := Boot(testhelp.Boot("T-DEV", 2, 1, nil, dup))
	if repDup.Status != meas.BootConflicted || !hasRule(repDup.Conflicts, meas.ConfSeqDuplicate) {
		t.Fatalf("非相邻重复序号也应判冲突: %+v", repDup.Conflicts)
	}
	// 重复记录不得污染度量链：F0/C0/A0 的最终值应与无重复的干净链一致。
	clean := Boot(testhelp.Boot("T-DEV", 3, 1, nil, base))
	for reg := 0; reg < meas.NumRegs; reg++ {
		if !eq(repDup.Registers[reg].Final, clean.Registers[reg].Final) {
			t.Fatalf("寄存器 %s 被重复序号污染", meas.RegisterName(reg))
		}
	}
}

// 空库是合法显式声明，不应被当作缺库；但向空库写记录即冲突。
func TestEmptyBank(t *testing.T) {
	alg := meas.AlgSHA256
	records := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
		testhelp.Extend(2, meas.MaskA0, alg, "T-DEV", 1, 1),
	}
	b := testhelp.BootWithEmpty("T-DEV", 1, 1, nil, []int{meas.BankCFG}, records)
	rep := Boot(b)
	if rep.Status != meas.BootOK {
		t.Fatalf("合法空库声明应 OK, got %s %+v", rep.Status, rep.Conflicts)
	}
	if rep.Registers[2].Status != meas.RegEmpty {
		t.Fatalf("C0 应为 EMPTY_BANK, got %s", rep.Registers[2].Status)
	}

	// 向空库扩展 → 冲突（重新计算签封以隔离该规则）。
	violated := append(append([]meas.Record{}, records...),
		testhelp.Extend(3, meas.MaskC1, alg, "T-DEV", 1, 1))
	b2 := testhelp.BootWithEmpty("T-DEV", 1, 1, nil, []int{meas.BankCFG}, violated)
	rep2 := Boot(b2)
	if rep2.Status != meas.BootConflicted || !hasRule(rep2.Conflicts, meas.ConfEmptyBankViolated) {
		t.Fatalf("空库被写应包含 EMPTY_BANK_VIOLATED: %+v", rep2.Conflicts)
	}
}

func hasRule(conflicts []meas.Evidence, rule meas.ConflictCode) bool {
	for _, c := range conflicts {
		if c.Rule == rule {
			return true
		}
	}
	return false
}

// 算法必须随固件代切换，否则判 ALG_GENERATION_MISMATCH。
func TestAlgorithmGenerationMismatch(t *testing.T) {
	alg := meas.AlgSHA256
	b := testhelp.Boot("T-DEV", 1, 2, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1), // 摘要长度按 SHA-256
	})
	// 第 2 代应为 SHA-384，这里把启动算法字段篡改成 SHA-256。
	b.Algorithm = meas.AlgSHA256
	b.Seal.Algorithm = meas.AlgSHA256
	rep := Boot(b)
	if rep.FirstConflict == nil || rep.FirstConflict.Rule != meas.ConfAlgGeneration {
		t.Fatalf("换代算法不一致应判冲突: %+v", rep.FirstConflict)
	}
}

// 短证据自包含：仅凭证据字段可独立重算并确认冲突。
func TestEvidenceSelfContained(t *testing.T) {
	alg := meas.AlgSHA256
	r := testhelp.Extend(1, meas.MaskC0, alg, "T-DEV", 1, 1)
	r.Expected = meas.ZeroDigest(alg)
	rep := Boot(testhelp.Boot("T-DEV", 1, 1, nil, []meas.Record{r}))
	ev := rep.FirstConflict
	if ev == nil || ev.RecordCanonical == "" || len(ev.RecordHash) == 0 {
		t.Fatal("证据缺少规范文本或记录哈希")
	}
	parsed, err := meas.ParseCanonicalRecord(ev.RecordCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(meas.CanonicalRecordHash(ev.Algorithm, parsed), ev.RecordHash) {
		t.Fatal("证据记录哈希无法独立复算")
	}
	recomputed := meas.Extend(ev.Algorithm, ev.Before, ev.InputDigest)
	if !eq(recomputed, ev.Recomputed) || eq(recomputed, ev.Claimed) {
		t.Fatal("证据扩展重算不自洽")
	}
}

// 崩溃未签封：重放本身干净，状态是 UNSEALED 而非冲突。
func TestUnsealedCrash(t *testing.T) {
	alg := meas.AlgSHA256
	b := testhelp.Unsealed(testhelp.Boot("T-DEV", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "T-DEV", 1, 1),
	}))
	rep := Boot(b)
	if rep.Status != meas.BootUnsealed || len(rep.Conflicts) != 0 {
		t.Fatalf("未签封崩溃应是 UNSEALED 且无冲突: %s %+v", rep.Status, rep.Conflicts)
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
