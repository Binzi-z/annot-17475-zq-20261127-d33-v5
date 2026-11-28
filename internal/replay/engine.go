// Package replay 在 meas 的类型与哈希原语之上实现单启动重放引擎。
// 引擎是纯函数：不查历史、不处理跨启动引用，跨启动规则由 orchestrator 负责。
package replay

import (
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/seal"
)

// regState 是引擎内部的寄存器工作状态。
type regState struct {
	value          []byte
	lastValid      []byte
	conflicted     bool
	unprovable     bool
	unprovableFrom int
	lastEventSeq   int
	reset          bool
	touched        bool
}

type engine struct {
	b            meas.Boot
	alg          meas.AlgID
	regs         [meas.NumRegs]*regState
	conflicts    []meas.Evidence
	unprov       []meas.RegisterRange
	emptyBankSet map[int]bool
	seenSeq      map[int]bool
	applied      int
	gap          bool
	gapFrom      int
	prevSeq      int
}

// Boot 对单次启动做纯函数重放。
func Boot(b meas.Boot) meas.BootReport {
	e := &engine{b: b, emptyBankSet: map[int]bool{}, seenSeq: map[int]bool{}}
	for _, bank := range b.EmptyBanks {
		e.emptyBankSet[bank] = true
	}
	e.alg = b.Algorithm
	e.initRegisters()
	e.validateHeader()
	e.replayRecords()
	e.coverage()
	return e.buildReport()
}

func (e *engine) initRegisters() {
	zero := meas.ZeroDigest(e.alg)
	for reg := 0; reg < meas.NumRegs; reg++ {
		st := &regState{}
		if uint8(e.b.ActiveMask)&meas.RegMask(reg) != 0 {
			st.value = append([]byte(nil), zero...)
			st.lastValid = append([]byte(nil), zero...)
		}
		e.regs[reg] = st
	}
}

func (e *engine) active(reg int) bool {
	return uint8(e.b.ActiveMask)&meas.RegMask(reg) != 0
}

func (e *engine) addConflict(ev meas.Evidence) {
	if ev.Algorithm == "" {
		ev.Algorithm = e.alg
	}
	if ev.DeviceID == "" {
		ev.DeviceID = e.b.DeviceID
	}
	ev.BootNumber = e.b.BootNumber
	ev.Generation = e.b.Generation
	e.conflicts = append(e.conflicts, ev)
}

func (e *engine) validateHeader() {
	if uint8(e.b.ActiveMask)&^meas.MaskAll != 0 {
		e.addConflict(meas.Evidence{
			Rule: meas.ConfHeaderMaskInvalid,
			Note: "启动头 active_mask 超出 6 个虚构寄存器范围",
		})
	}
	want := meas.GenAlgorithm(e.b.Generation)
	if e.b.Generation < 1 || e.b.Algorithm != want ||
		(e.b.Seal.Algorithm != "" && e.b.Seal.Algorithm != e.b.Algorithm) {
		e.addConflict(meas.Evidence{
			Rule:       meas.ConfAlgGeneration,
			Claimed:    meas.HexBytes(append([]byte(nil), e.b.Algorithm...)),
			Recomputed: meas.HexBytes(append([]byte(nil), want...)),
			Note:       "摘要算法未随模拟固件换代一致切换，或签封算法字段不一致",
		})
	}
	for bank := range e.emptyBankSet {
		bm := meas.BankMask(bank)
		if bm == 0 || uint8(e.b.ActiveMask)&bm != bm {
			e.addConflict(meas.Evidence{
				Rule: meas.ConfEmptyBankBadMask,
				Regs: bankRegNames(bank),
				Note: "空库声明指向未参与启动或不存在的库",
			})
		}
	}
}

func bankRegNames(bank int) []string {
	out := make([]string, 0, meas.RegPerBank)
	for reg := bank * meas.RegPerBank; reg < bank*meas.RegPerBank+meas.RegPerBank; reg++ {
		out = append(out, meas.RegisterName(reg))
	}
	return out
}

func (e *engine) replayRecords() {
	for _, r := range e.b.Records {
		if r.RefBoot != 0 && r.RefBoot != e.b.BootNumber {
			e.addConflict(e.baseEvidence(r, meas.ConfMisroutedHistory,
				"历史引用记录必须先经编排器路由"))
			continue
		}
		// 重复/倒退序号只记冲突，不应用到寄存器，避免污染度量链。
		if !e.checkSequence(r) {
			continue
		}
		switch r.Op {
		case meas.OpEmptyBank:
			e.handleEmptyDecl(r)
		case meas.OpExtend:
			e.handleExtend(r)
		case meas.OpReset:
			e.handleReset(r)
		default:
			e.addConflict(e.baseEvidence(r, meas.ConfUnknownOp, "未知操作类型: "+string(r.Op)))
		}
	}
}

// checkSequence 返回该记录是否应继续应用；重复/倒退序号返回 false。
func (e *engine) checkSequence(r meas.Record) bool {
	// 任意位置重复都算冲突，而不只是相邻重复。
	if e.seenSeq[r.Seq] {
		e.addConflict(e.baseEvidence(r, meas.ConfSeqDuplicate,
			"同一次启动内事件序号重复，不能被正常事件吞掉"))
		e.prevSeq = maxInt(e.prevSeq, r.Seq)
		return false
	}
	e.seenSeq[r.Seq] = true
	if r.Seq < e.prevSeq {
		e.addConflict(e.baseEvidence(r, meas.ConfSeqOutOfOrder, "事件序号倒退"))
		return false
	}
	if e.prevSeq != 0 && r.Seq != e.prevSeq+1 {
		// 缺口：记录仍应用，但后续链路无法证实；缺口本身不是“前后冲突”。
		e.gap = true
		e.gapFrom = e.prevSeq + 1
		for reg := 0; reg < meas.NumRegs; reg++ {
			st := e.regs[reg]
			if e.active(reg) && !st.conflicted && !e.emptyBankSet[meas.RegBank(reg)] {
				st.unprovable = true
				if st.unprovableFrom == 0 {
					st.unprovableFrom = e.gapFrom
				}
			}
		}
	}
	if r.Seq >= e.prevSeq {
		e.prevSeq = r.Seq
	}
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (e *engine) handleEmptyDecl(r meas.Record) {
	bank := -1
	for b := 0; b < meas.NumBanks; b++ {
		if uint8(r.Mask) == meas.BankMask(b) {
			bank = b
		}
	}
	if bank < 0 || !e.emptyBankSet[bank] {
		e.addConflict(e.baseEvidence(r, meas.ConfEmptyBankBadMask,
			"empty_bank 必须恰好指向启动头声明的空库"))
	}
}

func (e *engine) handleExtend(r meas.Record) {
	ev := e.baseEvidence(r, "", "")
	if uint8(r.Mask)&^uint8(e.b.ActiveMask) != 0 || uint8(r.Mask)&^meas.MaskAll != 0 {
		ev.Rule = meas.ConfMaskOutOfActive
		ev.Note = "记录命中了未参与本次启动的寄存器"
		e.addConflict(ev)
		return
	}
	for b := range e.emptyBankSet {
		if uint8(r.Mask)&meas.BankMask(b) != 0 {
			ev.Rule = meas.ConfEmptyBankViolated
			ev.Regs = bankRegNames(b)
			ev.Note = "该库已在启动头显式声明为空，却出现扩展记录"
			e.addConflict(ev)
			return
		}
	}
	if len(r.Digest) != e.alg.Size() {
		ev.Rule = meas.ConfBadDigestLen
		ev.InputDigest = append([]byte(nil), r.Digest...)
		ev.Note = "摘要长度与换代算法不符"
		e.addConflict(ev)
		return
	}
	applied := false
	for reg := 0; reg < meas.NumRegs; reg++ {
		if uint8(r.Mask)&meas.RegMask(reg) == 0 {
			continue
		}
		st := e.regs[reg]
		ev.Regs = append(ev.Regs, meas.RegisterName(reg))
		before := append([]byte(nil), st.value...)
		next := meas.Extend(e.alg, st.value, r.Digest)
		// 可选声称值：重算不符即“确有前后冲突”。
		if len(r.Expected) != 0 {
			regEv := e.baseEvidence(r, meas.ConfEvidenceMismatch, "")
			regEv.Regs = []string{meas.RegisterName(reg)}
			regEv.Before = before
			regEv.InputDigest = append([]byte(nil), r.Digest...)
			regEv.Recomputed = next
			regEv.Claimed = append([]byte(nil), r.Expected...)
			if !bytesEqual(next, r.Expected) {
				regEv.Note = "记录声称的扩展结果与离线重算值冲突"
				e.addConflict(regEv)
				st.conflicted = true // 冻结 LastValid
			}
		}
		st.value = next
		if !st.conflicted {
			st.lastValid = append([]byte(nil), next...)
			st.reset = false
		}
		st.touched = true
		st.lastEventSeq = r.Seq
		applied = true
	}
	if applied {
		e.applied++
	}
}

func (e *engine) handleReset(r meas.Record) {
	ev := e.baseEvidence(r, meas.ConfFWReset, "固件库寄存器不可重置，该指令的固件位被拒绝")
	if uint8(r.Mask)&^uint8(e.b.ActiveMask) != 0 || uint8(r.Mask)&^meas.MaskAll != 0 {
		ev.Rule = meas.ConfMaskOutOfActive
		ev.Note = "重置命中了未参与本次启动的寄存器"
		e.addConflict(ev)
		return
	}
	if uint8(r.Mask)&meas.MaskFW != 0 {
		ev.Regs = maskRegNames(uint8(r.Mask) & meas.MaskFW)
		e.addConflict(ev)
	}
	zero := meas.ZeroDigest(e.alg)
	for reg := 0; reg < meas.NumRegs; reg++ {
		if uint8(r.Mask)&meas.RegMask(reg) == 0 {
			continue
		}
		st := e.regs[reg]
		if !meas.Resettable(reg) {
			continue // 固件位拒绝执行，寄存器保持原值
		}
		st.value = append([]byte(nil), zero...)
		if !st.conflicted {
			st.lastValid = append([]byte(nil), zero...)
		}
		st.reset = true
		st.touched = true
		st.lastEventSeq = r.Seq
		e.applied++
	}
}

// coverage 在记录重放结束后补齐“整库缺记录”的无法证实范围。
func (e *engine) coverage() {
	for bank := 0; bank < meas.NumBanks; bank++ {
		bm := meas.BankMask(bank)
		if uint8(e.b.ActiveMask)&bm == 0 || e.emptyBankSet[bank] {
			continue
		}
		anyTouched := false
		for reg := bank * meas.RegPerBank; reg < bank*meas.RegPerBank+meas.RegPerBank; reg++ {
			if e.regs[reg].touched {
				anyTouched = true
			}
		}
		if !anyTouched {
			e.unprov = append(e.unprov, meas.RegisterRange{
				Bank:     bank,
				BankName: meas.BankName(bank),
				Regs:     bankRegNames(bank),
				Reason:   meas.UnprovBankMissing,
			})
			for reg := bank * meas.RegPerBank; reg < bank*meas.RegPerBank+meas.RegPerBank; reg++ {
				if !e.regs[reg].conflicted {
					e.regs[reg].unprovable = true
				}
			}
		}
	}
	if e.gap {
		for bank := 0; bank < meas.NumBanks; bank++ {
			bm := meas.BankMask(bank)
			if uint8(e.b.ActiveMask)&bm == 0 || e.emptyBankSet[bank] {
				continue
			}
			var regs []string
			for reg := bank * meas.RegPerBank; reg < bank*meas.RegPerBank+meas.RegPerBank; reg++ {
				if e.regs[reg].unprovable && e.regs[reg].unprovableFrom != 0 {
					regs = append(regs, meas.RegisterName(reg))
				}
			}
			if len(regs) != 0 {
				e.unprov = append(e.unprov, meas.RegisterRange{
					Bank: bank, BankName: meas.BankName(bank), Regs: regs,
					Reason: meas.UnprovSeqGap, FromSeq: e.gapFrom,
				})
			}
		}
	}
}

func (e *engine) buildReport() meas.BootReport {
	rep := meas.BootReport{
		DeviceID:     e.b.DeviceID,
		BootNumber:   e.b.BootNumber,
		Generation:   e.b.Generation,
		Algorithm:    e.alg,
		ActiveMask:   e.b.ActiveMask,
		Sealed:       e.b.Seal.Sealed,
		EmptyBanks:   append([]int(nil), e.b.EmptyBanks...),
		Conflicts:    e.conflicts,
		Unprovable:   e.unprov,
		RecordCount:  len(e.b.Records),
		AppliedCount: e.applied,
		ClaimedSeal:  append([]byte(nil), e.b.Seal.SealValue...),
	}
	var finalRegs [meas.NumRegs][]byte
	for reg := 0; reg < meas.NumRegs; reg++ {
		st := e.regs[reg]
		rr := meas.RegisterReport{Name: meas.RegisterName(reg), Bank: meas.RegBank(reg)}
		switch {
		case !e.active(reg):
			rr.Status = meas.RegExcluded
		case e.emptyBankSet[meas.RegBank(reg)]:
			rr.Status = meas.RegEmpty
		case st.conflicted:
			rr.Status = meas.RegConflicted
		case st.unprovable:
			rr.Status = meas.RegUnprovable
		case st.reset:
			rr.Status = meas.RegReset
		default:
			rr.Status = meas.RegOK
		}
		rr.LastValid = append([]byte(nil), st.lastValid...)
		rr.Final = append([]byte(nil), st.value...)
		rr.LastEventSeq = st.lastEventSeq
		rr.UnprovableFromSeq = st.unprovableFrom
		rep.Registers[reg] = rr
		if e.active(reg) {
			finalRegs[reg] = st.value
		}
	}
	rep.RecomputedSeal = seal.Compute(e.alg, e.b, finalRegs)
	rep.SealOK = bytesEqual(rep.RecomputedSeal, e.b.Seal.SealValue)
	if e.b.Seal.Sealed && !rep.SealOK {
		e.addConflict(meas.Evidence{
			Rule:            meas.ConfSealMismatch,
			Algorithm:       e.alg,
			HeaderCanonical: string(meas.CanonicalHeader(e.b)),
			PrevSeal:        append([]byte(nil), e.b.Seal.PrevSeal...),
			Recomputed:      append([]byte(nil), rep.RecomputedSeal...),
			Claimed:         append([]byte(nil), e.b.Seal.SealValue...),
			Note:            "总签封重算值与落盘签封不一致",
		})
		rep.Conflicts = e.conflicts
	}
	if len(e.conflicts) > 0 {
		earliest := e.conflicts[0]
		for _, c := range e.conflicts[1:] {
			if c.Seq < earliest.Seq {
				earliest = c
			}
		}
		rep.FirstConflict = &earliest
		rep.Status = meas.BootConflicted
	} else if !e.b.Seal.Sealed {
		rep.Status = meas.BootUnsealed
	} else if len(e.unprov) > 0 {
		rep.Status = meas.BootUnprovable
	} else {
		rep.Status = meas.BootOK
	}
	return rep
}

// baseEvidence 从记录填充证据的自包含字段。
func (e *engine) baseEvidence(r meas.Record, rule meas.ConflictCode, note string) meas.Evidence {
	return meas.Evidence{
		Rule:            rule,
		Algorithm:       e.alg,
		DeviceID:        e.b.DeviceID,
		BootNumber:      e.b.BootNumber,
		Generation:      e.b.Generation,
		Seq:             r.Seq,
		Regs:            maskRegNames(uint8(r.Mask)),
		RecordCanonical: string(meas.CanonicalRecord(r)),
		RecordHash:      meas.CanonicalRecordHash(e.alg, r),
		InputDigest:     append([]byte(nil), r.Digest...),
		Note:            note,
	}
}

func maskRegNames(mask uint8) []string {
	var out []string
	for reg := 0; reg < meas.NumRegs; reg++ {
		if mask&meas.RegMask(reg) != 0 {
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
