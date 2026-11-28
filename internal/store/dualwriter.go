// Package store 用确定性“双写者”模拟度量记录的持久化过程，并提供
// 内容寻址的增量检查点缓存。
//
// 双写者对应两条物理写路径：
//   - 库写者：把记录追加到各摘要库自己的日志（按物理写次序，而非逻辑序号）；
//   - 签封写者：在全部库日志写完后最后落盘总签封。
//
// 崩溃在三个确定位置注入：重置先于扩展落盘、库已写完而总签封未落盘、
// 恢复并重新签封后又有迟到事件。重放器只信任逻辑序号与规范编码，
// 因此物理交错不影响最终结论（合流性由 smallmodel 包穷举验证）。
package store

import (
	"sort"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
	"example.invalid/local-fixture/internal/seal"
)

// CrashScenario 是双写者的崩溃/异常位置。
type CrashScenario string

const (
	NoCrash         CrashScenario = "none"
	CrashResetFirst CrashScenario = "reset_first"                // 重置记录先于扩展落盘
	CrashUnsealed   CrashScenario = "banks_written_seal_missing" // 库写完，总签封未落盘
	CrashLateEvent  CrashScenario = "late_after_recovery"        // 恢复重新签封后出现迟到事件
)

// JournalEntry 是某条记录在某个库日志中的物理落盘项。
type JournalEntry struct {
	PhysicalSeq int         `json:"physical_seq"`
	Bank        int         `json:"bank"`
	Record      meas.Record `json:"record"`
}

// PersistedBoot 是双写者提交一次启动后的持久化形态。
type PersistedBoot struct {
	Boot     meas.Boot                     `json:"boot"`
	Scenario CrashScenario                 `json:"scenario"`
	Journals [meas.NumBanks][]JournalEntry `json:"journals"`
	// Arrivals 是恢复阶段产生的补记/迟到到达流。
	Arrivals []orchestrator.Arrival `json:"arrivals,omitempty"`
}

// DualWriter 是确定性双写者；physical 序号由提交顺序决定，无随机抖动。
type DualWriter struct {
	scenario CrashScenario
	physical int
}

// NewDualWriter 构造指定崩溃场景的双写者。
func NewDualWriter(scenario CrashScenario) *DualWriter {
	return &DualWriter{scenario: scenario}
}

// Scenario 返回当前场景。
func (d *DualWriter) Scenario() CrashScenario { return d.scenario }

// Commit 按双写者语义提交一次启动，返回持久化形态。
// 输入 boot 的 Records 已带正确逻辑序号；输出的库日志按物理次序排列。
func (d *DualWriter) Commit(b meas.Boot) PersistedBoot {
	d.physical = 0
	pb := PersistedBoot{Boot: b, Scenario: d.scenario}

	// 物理写次序：reset_first 场景把重置记录排到最前，其余保持逻辑次序。
	order := make([]int, len(b.Records))
	for i := range order {
		order[i] = i
	}
	if d.scenario == CrashResetFirst {
		sort.SliceStable(order, func(i, j int) bool {
			ri, rj := b.Records[order[i]], b.Records[order[j]]
			if (ri.Op == meas.OpReset) != (rj.Op == meas.OpReset) {
				return ri.Op == meas.OpReset
			}
			return false
		})
	}

	for _, idx := range order {
		r := b.Records[idx]
		d.physical++
		for reg := 0; reg < meas.NumRegs; reg++ {
			if uint8(r.Mask)&meas.RegMask(reg) == 0 {
				continue
			}
			bank := meas.RegBank(reg)
			pb.Journals[bank] = append(pb.Journals[bank], JournalEntry{
				PhysicalSeq: d.physical,
				Bank:        bank,
				Record:      r,
			})
		}
	}

	// 签封写者最后落盘；崩溃场景下总签封缺失。
	if d.scenario == CrashUnsealed {
		pb.Boot.Seal.Sealed = false
		pb.Boot.Seal.SealValue = nil
	}
	return pb
}

// Recover 从库日志合并出逻辑记录流（按逻辑序号稳定排序），
// 并把 lateRecords 包装成指向该启动的到达流。
func (d *DualWriter) Recover(pb PersistedBoot, lateRecords []meas.Record) (meas.Boot, []orchestrator.Arrival) {
	seen := map[int]meas.Record{}
	var seqs []int
	for bank := 0; bank < meas.NumBanks; bank++ {
		for _, e := range pb.Journals[bank] {
			if _, ok := seen[e.Record.Seq]; !ok {
				seen[e.Record.Seq] = e.Record
				seqs = append(seqs, e.Record.Seq)
			}
		}
	}
	sort.Ints(seqs)
	merged := make([]meas.Record, 0, len(seqs))
	for _, s := range seqs {
		merged = append(merged, seen[s])
	}
	b := pb.Boot
	b.Records = merged

	arrivals := make([]orchestrator.Arrival, 0, len(lateRecords))
	for i, r := range lateRecords {
		arrivals = append(arrivals, orchestrator.Arrival{
			Record:      r,
			RefBoot:     b.BootNumber,
			Generation:  b.Generation,
			PhysicalSeq: len(seqs) + i + 1,
		})
	}
	return b, arrivals
}

// MarkLateCrossGen 与 MarkLateSameGen 供生成器构造恢复后迟到事件，
// 区别仅在到达流携带的固件代（跨代引用必须被拒绝）。
func MarkLateCrossGen(b meas.Boot, r meas.Record, foreignGeneration int) orchestrator.Arrival {
	return orchestrator.Arrival{
		Record:     r,
		RefBoot:    b.BootNumber,
		Generation: foreignGeneration,
	}
}

// RecomputePersistedSeal 在补记合并后重新计算签封（恢复流程使用）。
func RecomputePersistedSeal(b meas.Boot, finals [meas.NumRegs][]byte) []byte {
	return seal.Compute(b.Algorithm, b, finals)
}
