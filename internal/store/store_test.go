package store

import (
	"testing"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
	"example.invalid/local-fixture/internal/testhelp"
)

func threeBootLedger(t *testing.T) meas.Ledger {
	t.Helper()
	alg := meas.AlgSHA256
	b1 := testhelp.Boot("D", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "D", 1, 1),
	})
	b2 := testhelp.Boot("D", 2, 1, b1.Seal.SealValue, []meas.Record{
		testhelp.Extend(1, meas.MaskC0, alg, "D", 2, 1),
	})
	b3 := testhelp.Boot("D", 3, 1, b2.Seal.SealValue, []meas.Record{
		testhelp.Extend(1, meas.MaskA0, alg, "D", 3, 1),
	})
	return meas.Ledger{DeviceID: "D", Boots: []meas.Boot{b1, b2, b3}}
}

// 增量检查点与整次重放必须逐字段一致，且第二次处理命中缓存。
func TestIncrementalEquivalenceAndReuse(t *testing.T) {
	l := threeBootLedger(t)
	cache := NewCache()
	if err := cache.VerifyEquivalence(l, nil); err != nil {
		t.Fatalf("首次一致性失败: %v", err)
	}
	if cache.Misses != 1 || cache.Hits != 0 {
		t.Fatalf("首次应冷 miss: hits=%d misses=%d", cache.Hits, cache.Misses)
	}
	// 第二次：完全相同输入，必须命中并复用全部 3 个启动。
	warm := cache.IncrementalProcess(l, nil)
	if !warm.Reused || warm.ReusedPrefix != 3 {
		t.Fatalf("相同输入应命中并复用 3 个启动: reused=%v prefix=%d", warm.Reused, warm.ReusedPrefix)
	}
	if cache.Hits != 1 {
		t.Fatalf("warm hit 计数错误: %d", cache.Hits)
	}
}

// 前缀扩展：先缓存 2 个启动，再给 3 个启动的账，应只复用前 2 个。
func TestPrefixExtensionReusesHead(t *testing.T) {
	l := threeBootLedger(t)
	head := meas.Ledger{DeviceID: "D", Boots: l.Boots[:2]}
	cache := NewCache()
	cache.IncrementalProcess(head, nil)
	full := cache.IncrementalProcess(l, nil)
	if !full.Reused || full.ReusedPrefix != 2 {
		t.Fatalf("应复用前 2 个启动: %+v", full)
	}
	if err := cache.VerifyEquivalence(l, nil); err != nil {
		t.Fatalf("前缀扩展后一致性失败: %v", err)
	}
}

// 篡改任一记录后内容键变化，缓存不可复用。
func TestMutationInvalidatesCache(t *testing.T) {
	l := threeBootLedger(t)
	cache := NewCache()
	cache.IncrementalProcess(l, nil)
	mutated := meas.Ledger{DeviceID: "D", Boots: append([]meas.Boot(nil), l.Boots...)}
	records := append([]meas.Record(nil), mutated.Boots[0].Records...)
	records[0].Digest = append([]byte(nil), records[0].Digest...)
	records[0].Digest[0] ^= 0x01
	mutated.Boots[0].Records = records
	res := cache.IncrementalProcess(mutated, nil)
	if res.Reused {
		t.Fatal("篡改后不应命中任何检查点")
	}
}

// 双写者三场景：重置先落盘合流、未签封崩溃、恢复后迟到被拒。
func TestDualWriterScenarios(t *testing.T) {
	alg := meas.AlgSHA256
	b := testhelp.Boot("DW", 1, 1, nil, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "DW", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "DW", 1, 1),
		testhelp.Reset(3, meas.MaskA0),
		testhelp.Extend(4, meas.MaskA0, alg, "DW", 1, 1),
	})
	// 场景一：重置物理先落盘，恢复归并后状态与逻辑次序一致。
	dw := NewDualWriter(CrashResetFirst)
	pb := dw.Commit(b)
	recovered, _ := dw.Recover(pb, nil)
	if !sameRegisters(recovered, b) {
		t.Fatal("重置先落盘场景未合流到逻辑次序")
	}
	// 库日志确实把重置排到了靠前的物理位置。
	if pb.Journals[meas.BankAPP][0].Record.Op != meas.OpReset {
		t.Fatal("重置记录未被双写者提前落盘")
	}

	// 场景二：库写完签封未落盘。
	dw2 := NewDualWriter(CrashUnsealed)
	pb2 := dw2.Commit(b)
	if pb2.Boot.Seal.Sealed {
		t.Fatal("崩溃场景签封不应落盘")
	}

	// 场景三：恢复后迟到事件指向已签封启动，必须被拒。
	late := testhelp.Extend(5, meas.MaskA1, alg, "DW", 1, 1)
	_, arrivals := dw2.Recover(pb2, []meas.Record{late})
	dr := orchestrator.Process(meas.Ledger{DeviceID: "DW", Boots: []meas.Boot{b}}, arrivals)
	if len(dr.CrossBootConflicts) == 0 ||
		dr.CrossBootConflicts[0].Rule != meas.ConfLateAfterSealed {
		t.Fatalf("迟到事件未被拒绝: %+v", dr.CrossBootConflicts)
	}
}

func sameRegisters(a, b meas.Boot) bool {
	// 恢复后的记录按逻辑序号归并，与原记录序列应等价（忽略物理次序）。
	if len(a.Records) != len(b.Records) {
		return false
	}
	for i := range a.Records {
		if a.Records[i].Seq != b.Records[i].Seq || a.Records[i].Op != b.Records[i].Op {
			return false
		}
	}
	return true
}
