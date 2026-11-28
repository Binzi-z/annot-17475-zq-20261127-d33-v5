package orchestrator

import (
	"testing"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/testhelp"
)

// 历史启动编号重复：重复槽仍保留在报告里且判 BOOT_NUMBER_REPEATED。
func TestBootNumberRepeat(t *testing.T) {
	alg := meas.AlgSHA256
	b1 := testhelp.Boot("D", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 1),
	})
	b2 := testhelp.Boot("D", 1, 2, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 2),
	})
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b1, b2}}, nil)
	if rep.BootCount != 2 {
		t.Fatalf("重复启动也必须保留, got %d", rep.BootCount)
	}
	if !has(rep, meas.ConfBootNumberRepeat) {
		t.Fatalf("未检出重复启动号: %+v", rep.Boots[1].Conflicts)
	}
}

// 固件代倒退与签封断链都必须报冲突，不能被正常事件吞掉。
func TestGenerationRegressionAndChain(t *testing.T) {
	// 代倒退：b1 在第 2 代，b2 退回第 1 代（PrevSeal 保持正确以隔离规则）。
	b1 := testhelp.Boot("D", 1, 2, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, meas.AlgSHA384, "D", 1, 2),
	})
	b2 := testhelp.Boot("D", 2, 1, b1.Seal.SealValue, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, meas.AlgSHA256, "D", 2, 1),
	})
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b1, b2}}, nil)
	if !has(rep, meas.ConfGenRegression) {
		t.Fatal("未检出固件代倒退")
	}

	// 代正常前进但 PrevSeal 错误 → 断链。
	c1 := testhelp.Boot("D", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, meas.AlgSHA256, "D", 1, 1),
	})
	c2 := testhelp.Boot("D", 2, 2, []byte{1, 2, 3}, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, meas.AlgSHA384, "D", 2, 2),
	})
	rep2 := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{c1, c2}}, nil)
	if !has(rep2, meas.ConfSealChainBroken) {
		t.Fatal("未检出签封断链")
	}
}

// 同代补记：填补未落盘启动的序号缺口，重放后无冲突，计数正确。
func TestBackfillFillsGap(t *testing.T) {
	alg := meas.AlgSHA256
	full := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "D", 1, 1),
		testhelp.Extend(3, meas.MaskA0, alg, "D", 1, 1),
	}
	b := testhelp.Boot("D", 1, 1, nil, []meas.Record{full[0], full[2]})
	b = testhelp.Unsealed(b)
	arrivals := []Arrival{{Record: full[1], RefBoot: 1, Generation: 1}}
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b}}, arrivals)
	if rep.BackfilledRecords != 1 || rep.BackfilledBoots != 1 {
		t.Fatalf("补记计数错误: %+v", rep)
	}
	if rep.Boots[0].Status == meas.BootConflicted {
		t.Fatalf("同代补记填补缺口不应留冲突: %+v", rep.Boots[0].Conflicts)
	}
}

// 已签封启动的迟到事件必须拒绝。
func TestLateAfterSealedRejected(t *testing.T) {
	alg := meas.AlgSHA256
	b := testhelp.Boot("D", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 1),
	})
	late := testhelp.Extend(2, meas.MaskA1, alg, "D", 1, 1)
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b}},
		[]Arrival{{Record: late, RefBoot: 1, Generation: 1}})
	if !has(rep, meas.ConfLateAfterSealed) || rep.LateRejected != 1 {
		t.Fatalf("迟到事件未被拒绝: %+v", rep.CrossBootConflicts)
	}
}

// 跨代引用同名编号必须拒绝。
func TestCrossGenerationReference(t *testing.T) {
	alg := meas.AlgSHA384
	b := testhelp.Boot("D", 1, 2, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 2),
	})
	foreign := testhelp.Extend(2, meas.MaskC0, meas.AlgSHA256, "D", 1, 1)
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b}},
		[]Arrival{{Record: foreign, RefBoot: 1, Generation: 1}})
	if !has(rep, meas.ConfCrossGenRef) {
		t.Fatalf("跨代引用未被拒绝: %+v", rep.CrossBootConflicts)
	}
}

// 引用不存在的历史启动号。
func TestReferenceUnknownBoot(t *testing.T) {
	alg := meas.AlgSHA256
	b := testhelp.Boot("D", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 1),
	})
	r := testhelp.Extend(1, meas.MaskA0, alg, "D", 9, 1)
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b}},
		[]Arrival{{Record: r, RefBoot: 9, Generation: 1}})
	if !has(rep, meas.ConfRefUnknownBoot) {
		t.Fatalf("未知历史启动未检出: %+v", rep.CrossBootConflicts)
	}
}

// 补记序号与既有事件冲突时拒绝，不覆盖历史。
func TestBackfillDuplicateRejected(t *testing.T) {
	alg := meas.AlgSHA256
	b := testhelp.Unsealed(testhelp.Boot("D", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "D", 1, 1),
	}))
	dup := testhelp.Extend(2, meas.MaskA0, alg, "D", 1, 1)
	rep := Process(meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b}},
		[]Arrival{{Record: dup, RefBoot: 1, Generation: 1}})
	if !has(rep, meas.ConfBackfillDuplicate) {
		t.Fatalf("补记重复序号未被拒绝: %+v", rep.CrossBootConflicts)
	}
}

func has(rep DeviceReport, rule meas.ConflictCode) bool {
	for _, br := range rep.Boots {
		for _, c := range br.Conflicts {
			if c.Rule == rule {
				return true
			}
		}
	}
	for _, c := range rep.CrossBootConflicts {
		if c.Rule == rule {
			return true
		}
	}
	return false
}
