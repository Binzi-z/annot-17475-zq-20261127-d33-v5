package gen

import (
	"reflect"
	"testing"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
)

// 同种子必须生成字节级一致的数据集。
func TestDeterministic(t *testing.T) {
	a := Generate(Config{Devices: 3, BootsPerDevice: 20, BaseSeed: 42, FaultRate: 0.3})
	b := Generate(Config{Devices: 3, BootsPerDevice: 20, BaseSeed: 42, FaultRate: 0.3})
	if !reflect.DeepEqual(a.Input, b.Input) {
		t.Fatal("同种子生成结果不一致，确定性被破坏")
	}
	c := Generate(Config{Devices: 3, BootsPerDevice: 20, BaseSeed: 43, FaultRate: 0.3})
	if reflect.DeepEqual(a.Input, c.Input) {
		t.Fatal("不同种子不应生成相同数据集")
	}
}

// 按总记录数生成必须精确，且设备间严格隔离。
func TestGenerateByRecordCountExact(t *testing.T) {
	for _, total := range []int{100, 999, 1000} {
		ds := GenerateByRecordCount(4, total, 7)
		if got := CountRecords(ds); got != total {
			t.Fatalf("记录数不精确: want=%d got=%d", total, got)
		}
		if len(ds.Devices) != 4 {
			t.Fatalf("设备隔离数错误: %d", len(ds.Devices))
		}
		ids := map[string]bool{}
		for _, dd := range ds.Devices {
			if ids[dd.Ledger.DeviceID] {
				t.Fatal("设备 ID 重复，隔离被破坏")
			}
			ids[dd.Ledger.DeviceID] = true
		}
	}
}

// 长序列必须覆盖全部故障类型，且引擎结论与独立真值一致。
func TestTruthCoverageAndMatch(t *testing.T) {
	ds := Generate(Config{Devices: 2, BootsPerDevice: 200, BaseSeed: 99, FaultRate: 0.4})
	seenFaults := map[FaultKind]bool{}
	mismatches := 0
	for _, dd := range ds.Devices {
		rep := orchestrator.Process(dd.Ledger, dd.Arrivals)
		if len(rep.Boots) != len(dd.Truth) {
			t.Fatalf("真值条数不符: %d vs %d", len(rep.Boots), len(dd.Truth))
		}
		for i, truth := range dd.Truth {
			seenFaults[truth.Fault] = true
			if rep.Boots[i].Status != truth.WantStatus {
				mismatches++
				t.Errorf("设备 %s boot %d fault=%s: got=%s want=%s",
					dd.Ledger.DeviceID, truth.BootNumber, truth.Fault,
					rep.Boots[i].Status, truth.WantStatus)
			}
		}
	}
	for _, f := range []FaultKind{
		FaultBankMissing, FaultSeqGap, FaultSeqDuplicate, FaultTamperEvidence,
		FaultFWReset, FaultSealTamper, FaultBootRepeat, FaultGenRegression,
		FaultUnsealedCrash, FaultBackfill, FaultLateSealed, FaultCrossGenRef, FaultEmptyBank,
	} {
		if !seenFaults[f] {
			t.Errorf("故障类型未被覆盖: %s", f)
		}
	}
	if mismatches > 0 {
		t.Fatalf("存在 %d 处真值不符", mismatches)
	}
}

// 换代算法映射在生成器里必须与引擎一致。
func TestGeneratedAlgorithmConsistency(t *testing.T) {
	ds := Generate(Config{Devices: 1, BootsPerDevice: 30, BaseSeed: 5, FaultRate: 0, GenSpan: 7})
	for _, b := range ds.Input.Ledgers[0].Boots {
		if b.Algorithm != meas.GenAlgorithm(b.Generation) {
			t.Fatalf("boot %d 算法与换代规则不一致", b.BootNumber)
		}
		if b.Seal.Algorithm != b.Algorithm {
			t.Fatalf("boot %d 签封算法字段不一致", b.BootNumber)
		}
	}
}
