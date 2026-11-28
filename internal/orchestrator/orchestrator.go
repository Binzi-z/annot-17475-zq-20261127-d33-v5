// Package orchestrator 在单启动重放引擎之上执行跨启动规则：
// 启动编号重复、固件代回归、签封哈希链、历史补记与恢复后迟到事件的路由。
// 引擎只认一次启动内部的事实，编排器负责“同名编号在不同启动代不能重新引用”
// 等历史语义，并把所有结论汇总成设备级报告。
package orchestrator

import (
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/replay"
	"example.invalid/local-fixture/internal/seal"
)

// Arrival 是一条“带到达语义”的记录：RefBoot=0 归属自身启动，
// RefBoot>0 表示对历史启动的补记或恢复后的迟到事件。
type Arrival struct {
	Record meas.Record `json:"record"`
	// RefBoot 指向被补记/迟到的目标启动号。
	RefBoot int `json:"ref_boot"`
	// Generation 是记录产生时所属的模拟固件代；与目标启动代不同即跨代引用。
	Generation int `json:"generation"`
	// PhysicalSeq 是双写者观察到的物理写次序（可能晚于逻辑 Seq）。
	PhysicalSeq int `json:"physical_seq"`
}

// DeviceReport 是一个合成设备的完整离线结论。
type DeviceReport struct {
	DeviceID  string            `json:"device_id"`
	BootCount int               `json:"boot_count"`
	Boots     []meas.BootReport `json:"boots"`
	// FirstConflict 是全设备最早的不一致点。
	FirstConflict *meas.Evidence `json:"first_conflict,omitempty"`
	// CrossBootConflicts 是在单启动重放之外发现的历史级冲突。
	CrossBootConflicts []meas.Evidence `json:"cross_boot_conflicts"`
	// BackfilledBoots / BackfilledRecords 量化补记影响的启动代与触达条数。
	BackfilledBoots   int `json:"backfilled_boots"`
	BackfilledRecords int `json:"backfilled_records"`
	LateRejected      int `json:"late_rejected"`
}

type bootSlot struct {
	boot       meas.Boot
	report     meas.BootReport
	backfilled bool
	duplicate  bool
}

// Process 重放一个设备的全部启动，并应用到达流里的补记/迟到记录。
// 启动按输入位置全部保留（即使启动号重复也不丢报告）；arrivals 处理顺序确定。
func Process(ledger meas.Ledger, arrivals []Arrival) DeviceReport {
	return processWith(ledger, arrivals, nil, 0)
}

// ProcessWithSeal 供增量检查点使用：tail 段重放时注入前缀最后一枚有效
// 落盘签封与最后一代，使签封链/换代核验与整次重放完全等价。
func ProcessWithSeal(ledger meas.Ledger, arrivals []Arrival, initialPrevSeal []byte, prevGeneration int) DeviceReport {
	return processWith(ledger, arrivals, initialPrevSeal, prevGeneration)
}

func processWith(ledger meas.Ledger, arrivals []Arrival, initialPrevSeal []byte, prevGeneration int) DeviceReport {
	rep := DeviceReport{DeviceID: ledger.DeviceID}
	slots := make([]*bootSlot, 0, len(ledger.Boots))
	// byNumber 只登记每个启动号的首次出现，供到达流路由。
	byNumber := map[int]*bootSlot{}

	// 第一遍：启动级历史规则 + 单启动重放 + 签封链。
	prevSeal := append([]byte(nil), initialPrevSeal...)
	prevGen := prevGeneration
	for i := range ledger.Boots {
		b := ledger.Boots[i]
		slot := &bootSlot{boot: b}
		br := replay.Boot(b)
		if _, dup := byNumber[b.BootNumber]; dup {
			// 历史启动编号重复：不能被正常事件吞掉，重复槽不参与后续路由。
			br = attachCross(br, headerEvidence(b, meas.ConfBootNumberRepeat,
				"同设备出现重复启动编号，历史编号不可复用"))
			slot.duplicate = true
		} else {
			byNumber[b.BootNumber] = slot
			// 固件代不得倒退（与输入位置上的前一枚启动比较，含检查点前缀代）。
			if i > 0 && b.Generation < ledger.Boots[i-1].Generation {
				br = attachCross(br, headerEvidence(b, meas.ConfGenRegression,
					"模拟固件代号倒退，换代只能前进"))
			} else if i == 0 && prevGen > 0 && b.Generation < prevGen {
				br = attachCross(br, headerEvidence(b, meas.ConfGenRegression,
					"模拟固件代号相对检查点前缀倒退，换代只能前进"))
			}
			// 签封哈希链：PrevSeal 必须指向上一枚有效落盘签封。
			if i > 0 && !bytesEqual(b.Seal.PrevSeal, prevSeal) {
				br = attachCross(br, headerEvidence(b, meas.ConfSealChainBroken,
					"换代签封的 prev_seal 与上一枚签封断链"))
			} else if i == 0 && len(initialPrevSeal) > 0 && !bytesEqual(b.Seal.PrevSeal, prevSeal) {
				br = attachCross(br, headerEvidence(b, meas.ConfSealChainBroken,
					"换代签封的 prev_seal 与检查点前缀签封断链"))
			}
		}
		slot.report = br
		slots = append(slots, slot)
		// 重复启动号槽不推进签封链与代；只有正常槽且签封有效时才更新链头。
		if !slot.duplicate && br.SealOK && b.Seal.Sealed {
			prevSeal = append([]byte(nil), b.Seal.SealValue...)
			prevGen = b.Generation
		}
	}

	// 第二遍：路由补记/迟到事件（RefBoot=0 的正常记录已在 Boot.Records 内）。
	for _, ar := range arrivals {
		if ar.RefBoot == 0 {
			continue
		}
		target, ok := byNumber[ar.RefBoot]
		if !ok {
			rep.CrossBootConflicts = append(rep.CrossBootConflicts,
				orphanEvidence(ledger.DeviceID, ar, meas.ConfRefUnknownBoot,
					"补记引用了不存在的历史启动号"))
			rep.LateRejected++
			continue
		}
		// 同名编号在不同启动代不能重新引用：代不一致直接拒绝。
		if ar.Generation != 0 && ar.Generation != target.boot.Generation {
			ev := recordEvidence(target.boot, ar.Record, meas.ConfCrossGenRef,
				"记录来自不同模拟固件代，同名编号不得跨代重新引用")
			target.report = attachCross(target.report, ev)
			rep.CrossBootConflicts = append(rep.CrossBootConflicts, ev)
			rep.LateRejected++
			continue
		}
		// 签封已落盘后的迟到事件：拒绝，不能并入历史。
		if target.boot.Seal.Sealed && target.report.SealOK {
			ev := recordEvidence(target.boot, ar.Record, meas.ConfLateAfterSealed,
				"总签封已落盘，恢复后到达的事件只能标记为迟到冲突")
			target.report = attachCross(target.report, ev)
			rep.CrossBootConflicts = append(rep.CrossBootConflicts, ev)
			rep.LateRejected++
			continue
		}
		// 仅允许填补同代、未落盘启动的序号缺口。
		if conflict := integrateBackfill(target, ar); conflict != nil {
			target.report = attachCross(target.report, *conflict)
			rep.CrossBootConflicts = append(rep.CrossBootConflicts, *conflict)
			rep.LateRejected++
			continue
		}
		target.backfilled = true
		rep.BackfilledRecords++
	}

	// 第三遍：整合补记后重放受影响启动并按输入位置汇总。
	for _, slot := range slots {
		if slot.backfilled {
			// 重新重放：补记已按逻辑序号合并进记录流。
			slot.report = replay.Boot(slot.boot)
			rep.BackfilledBoots++
		}
		rep.Boots = append(rep.Boots, slot.report)
	}
	rep.BootCount = len(rep.Boots)
	rep.FirstConflict = earliestCross(rep)
	return rep
}

// integrateBackfill 校验并合并一条补记；返回非 nil Evidence 表示拒绝。
func integrateBackfill(slot *bootSlot, ar Arrival) *meas.Evidence {
	r := ar.Record
	existing := map[int]bool{}
	var maxSeq int
	for _, ex := range slot.boot.Records {
		existing[ex.Seq] = true
		if ex.Seq > maxSeq {
			maxSeq = ex.Seq
		}
	}
	switch {
	case existing[r.Seq]:
		ev := recordEvidence(slot.boot, r, meas.ConfBackfillDuplicate,
			"补记序号与既有事件冲突，不能覆盖历史")
		return &ev
	case r.Seq > maxSeq+1 || r.Seq <= 0:
		ev := recordEvidence(slot.boot, r, meas.ConfRefUnknownBoot,
			"补记只能紧邻缺口，不能凭空延伸历史链")
		return &ev
	}
	// 摘要长度必须与目标启动换代算法一致。
	if len(r.Digest) != 0 && len(r.Digest) != slot.boot.Algorithm.Size() {
		ev := recordEvidence(slot.boot, r, meas.ConfBadDigestLen,
			"补记摘要长度与目标启动换代算法不符")
		return &ev
	}
	slot.boot.Records = insertOrdered(slot.boot.Records, r)
	return nil
}

func insertOrdered(records []meas.Record, r meas.Record) []meas.Record {
	out := make([]meas.Record, 0, len(records)+1)
	inserted := false
	for _, ex := range records {
		if !inserted && r.Seq < ex.Seq {
			out = append(out, r)
			inserted = true
		}
		out = append(out, ex)
	}
	if !inserted {
		out = append(out, r)
	}
	return out
}

func attachCross(br meas.BootReport, ev meas.Evidence) meas.BootReport {
	br.Conflicts = append(br.Conflicts, ev)
	if br.FirstConflict == nil || ev.Seq < br.FirstConflict.Seq {
		cp := ev
		br.FirstConflict = &cp
	}
	br.Status = meas.BootConflicted
	return br
}

func headerEvidence(b meas.Boot, code meas.ConflictCode, note string) meas.Evidence {
	return meas.Evidence{
		Rule:            code,
		Algorithm:       b.Algorithm,
		DeviceID:        b.DeviceID,
		BootNumber:      b.BootNumber,
		Generation:      b.Generation,
		HeaderCanonical: string(meas.CanonicalHeader(b)),
		PrevSeal:        append([]byte(nil), b.Seal.PrevSeal...),
		Note:            note,
	}
}

func recordEvidence(b meas.Boot, r meas.Record, code meas.ConflictCode, note string) meas.Evidence {
	return meas.Evidence{
		Rule:            code,
		Algorithm:       b.Algorithm,
		DeviceID:        b.DeviceID,
		BootNumber:      b.BootNumber,
		Generation:      b.Generation,
		Seq:             r.Seq,
		Regs:            regNames(r.Mask),
		RecordCanonical: string(meas.CanonicalRecord(r)),
		RecordHash:      meas.CanonicalRecordHash(b.Algorithm, r),
		InputDigest:     append([]byte(nil), r.Digest...),
		Note:            note,
	}
}

func orphanEvidence(device string, ar Arrival, code meas.ConflictCode, note string) meas.Evidence {
	return meas.Evidence{
		Rule:            code,
		DeviceID:        device,
		BootNumber:      ar.RefBoot,
		Seq:             ar.Record.Seq,
		RecordCanonical: string(meas.CanonicalRecord(ar.Record)),
		Note:            note,
	}
}

// earliestCross 取全设备最早的不一致点：先按启动号、再按事件序号。
func earliestCross(rep DeviceReport) *meas.Evidence {
	var best *meas.Evidence
	for i := range rep.Boots {
		if rep.Boots[i].FirstConflict == nil {
			continue
		}
		c := rep.Boots[i].FirstConflict
		if best == nil || c.BootNumber < best.BootNumber ||
			(c.BootNumber == best.BootNumber && c.Seq < best.Seq) {
			cp := *c
			best = &cp
		}
	}
	return best
}

func regNames(m meas.Mask) []string {
	var out []string
	for reg := 0; reg < meas.NumRegs; reg++ {
		if uint8(m)&meas.RegMask(reg) != 0 {
			out = append(out, meas.RegisterName(reg))
		}
	}
	return out
}

func bytesEqual(a, b []byte) bool {
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

// RecomputeSeal 暴露给 CLI 证据核验子命令使用。
func RecomputeSeal(b meas.Boot, finals [meas.NumRegs][]byte) []byte {
	return seal.Compute(b.Algorithm, b, finals)
}
