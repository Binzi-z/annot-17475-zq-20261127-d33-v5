// Package gen 生成完全虚构、确定性的多设备启动度量数据集，并为每个启动
// 附带独立真值标签（期望状态、冲突规则或无法证实原因）。
//
// 确定性来自固定种子的 math/rand 与按 (设备,启动,序号) 派生的内容摘要：
// 同一份配置在任意机器上生成字节级一致的输入，测试可重复执行。
package gen

import (
	"fmt"
	"math/rand"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
	"example.invalid/local-fixture/internal/replay"
)

// FaultKind 是注入的故障/异常类型，也是真值标签的一部分。
type FaultKind string

const (
	FaultNone           FaultKind = "none"
	FaultBankMissing    FaultKind = "bank_missing"      // 整库缺记录 → 无法证实
	FaultSeqGap         FaultKind = "seq_gap"           // 序号缺口 → 无法证实
	FaultSeqDuplicate   FaultKind = "seq_duplicate"     // 同启动序号重复 → 冲突
	FaultTamperEvidence FaultKind = "tamper_evidence"   // 声称值前后冲突
	FaultFWReset        FaultKind = "fw_reset"          // 固件库重置被拒
	FaultSealTamper     FaultKind = "seal_tamper"       // 总签封篡改
	FaultBootRepeat     FaultKind = "boot_repeat"       // 历史启动编号重复
	FaultGenRegression  FaultKind = "gen_regression"    // 固件代倒退
	FaultUnsealedCrash  FaultKind = "unsealed_crash"    // 签封未落盘
	FaultBackfill       FaultKind = "backfill"          // 缺口 + 同代补记恢复
	FaultLateSealed     FaultKind = "late_after_sealed" // 签封后迟到事件
	FaultCrossGenRef    FaultKind = "cross_gen_ref"     // 跨代引用
	FaultEmptyBank      FaultKind = "empty_bank"        // 合法空库声明
)

// BootTruth 是单个启动的独立真值。
type BootTruth struct {
	BootNumber int                   `json:"boot_number"`
	Generation int                   `json:"generation"`
	Fault      FaultKind             `json:"fault"`
	WantStatus meas.BootStatus       `json:"want_status"`
	WantRule   meas.ConflictCode     `json:"want_rule,omitempty"`
	WantReason meas.UnprovableReason `json:"want_reason,omitempty"`
}

// DeviceDataset 是单个合成设备的输入、到达流与真值。
type DeviceDataset struct {
	Ledger   meas.Ledger
	Arrivals []orchestrator.Arrival
	Truth    []BootTruth
}

// Dataset 是整套多设备隔离数据。
type Dataset struct {
	Input   meas.InputFile
	Devices []DeviceDataset
}

// Config 控制生成规模与故障密度。
type Config struct {
	Devices        int
	BootsPerDevice int
	BaseSeed       int64
	FaultRate      float64 // 非计划内启动被注入故障的概率
	GenSpan        int     // 每多少个启动固件换代一次（<1 时取 7）
}

// Generate 按配置确定性生成数据集。
func Generate(cfg Config) *Dataset {
	if cfg.GenSpan < 1 {
		cfg.GenSpan = 7
	}
	ds := &Dataset{Input: meas.InputFile{Synthetic: true}}
	for d := 0; d < cfg.Devices; d++ {
		seed := cfg.BaseSeed + int64(d)*1_000_003
		dd := generateDevice(d, cfg, rand.New(rand.NewSource(seed)))
		ds.Devices = append(ds.Devices, dd)
		ds.Input.Ledgers = append(ds.Input.Ledgers, dd.Ledger)
	}
	return ds
}

func generateDevice(idx int, cfg Config, rng *rand.Rand) DeviceDataset {
	deviceID := fmt.Sprintf("DEV-%04d", idx+1)
	dd := DeviceDataset{}
	dd.Ledger.DeviceID = deviceID

	var prevSeal []byte
	usedNumbers := map[int]bool{}

	for bIdx := 0; bIdx < cfg.BootsPerDevice; bIdx++ {
		bootNumber := bIdx + 1
		generation := 1 + bIdx/cfg.GenSpan
		fault := pickFault(bIdx, cfg, rng)

		// 故障特化：重复编号 / 代倒退需要改写头部字段。
		if fault == FaultBootRepeat && bIdx > 0 {
			bootNumber = dd.Ledger.Boots[bIdx-1].BootNumber
		}
		if fault == FaultGenRegression {
			// 只有前一枚启动的代高于 1 才能真实倒退；首代无法倒退时，
			// 退化为签封篡改，保证该槽仍是一个可验证的确认冲突。
			if bIdx == 0 || dd.Ledger.Boots[bIdx-1].Generation <= 1 {
				fault = FaultSealTamper
			} else {
				generation = dd.Ledger.Boots[bIdx-1].Generation - 1
			}
		}
		alg := meas.GenAlgorithm(generation)
		active := meas.Mask(meas.MaskAll)

		emptyBank := -1
		if fault == FaultEmptyBank {
			emptyBank = meas.BankCFG
		}
		records := buildRecords(deviceID, bootNumber, generation, alg, active, emptyBank, rng)

		// 构造启动骨架并先做一次干净重放以取得最终寄存器值。
		b := meas.Boot{
			DeviceID:   deviceID,
			BootNumber: bootNumber,
			Generation: generation,
			Algorithm:  alg,
			ActiveMask: active,
		}
		if emptyBank >= 0 {
			b.EmptyBanks = []int{emptyBank}
		}
		b.Records = records
		truth := BootTruth{BootNumber: bootNumber, Fault: fault}

		// 应用记录级故障并同步真值。
		var backfillRec *meas.Record
		records, backfillRec = applyRecordFault(fault, records, alg, rng, &truth)
		b.Records = records

		// 先放好签封骨架：规范头吸收 PrevSeal/EmptyBanks，必须在重算签封前就位，
		// 否则 CanonicalHeader 与最终落盘头不一致会造成全链 SEAL_MISMATCH。
		b.Seal = meas.GenerationSeal{
			DeviceID:   deviceID,
			BootNumber: bootNumber,
			Generation: generation,
			Algorithm:  alg,
			ActiveMask: active,
			EmptyBanks: append([]int(nil), b.EmptyBanks...),
			PrevSeal:   append([]byte(nil), prevSeal...),
			Sealed:     true,
		}
		b.Seal.SealValue = recomputeSeal(b)
		sealOK := fault != FaultSealTamper

		// 签封级与崩溃级故障。
		applySealFault(fault, &b, &truth)

		// 到达流：补记 / 迟到 / 跨代引用。
		applyArrivalFault(fault, b, backfillRec, dd.Ledger.Boots, &dd, &truth)

		// 真值状态收口。
		truth.Generation = generation
		finalizeTruth(&truth, fault)
		dd.Truth = append(dd.Truth, truth)
		dd.Ledger.Boots = append(dd.Ledger.Boots, b)
		// 仅正常槽且签封有效的启动推进链头，与编排器规则保持一致。
		if sealOK && b.Seal.Sealed && !usedNumbers[bootNumber] {
			prevSeal = append([]byte(nil), b.Seal.SealValue...)
		}
		usedNumbers[bootNumber] = true
	}
	return dd
}

// buildRecords 按“固件→配置→应用”的逻辑次序构造一次启动的有序记录。
func buildRecords(deviceID string, boot, gen int, alg meas.AlgID, active meas.Mask, emptyBank int, rng *rand.Rand) []meas.Record {
	var out []meas.Record
	seq := 0
	add := func(op meas.Op, mask uint8) {
		seq++
		r := meas.Record{Seq: seq, Op: op, Mask: meas.Mask(mask)}
		if op == meas.OpExtend {
			r.Digest = contentDigest(alg, deviceID, boot, gen, seq)
		}
		out = append(out, r)
	}
	// 固件库：只扩展，永不可重置。
	if uint8(active)&meas.MaskFW != 0 {
		add(meas.OpExtend, meas.MaskF0)
		if rng.Intn(2) == 0 {
			add(meas.OpExtend, meas.MaskF0|meas.MaskF1)
		}
	}
	// 配置库：扩展 + 偶发重置。
	if uint8(active)&meas.MaskCFG != 0 && emptyBank != meas.BankCFG {
		add(meas.OpExtend, meas.MaskC0)
		if rng.Intn(3) == 0 {
			add(meas.OpReset, meas.MaskC0|meas.MaskC1)
		}
		add(meas.OpExtend, meas.MaskC1)
	}
	// 应用库：扩展 + 偶发重置。
	if uint8(active)&meas.MaskAPP != 0 && emptyBank != meas.BankAPP {
		add(meas.OpExtend, meas.MaskA0|meas.MaskA1)
		if rng.Intn(3) == 0 {
			add(meas.OpReset, meas.MaskA0)
		}
		add(meas.OpExtend, meas.MaskA0)
	}
	return out
}

// contentDigest 用 (设备,启动,代,序号) 派生确定性摘要，长度匹配换代算法。
func contentDigest(alg meas.AlgID, deviceID string, boot, gen, seq int) []byte {
	h := meas.NewHasher(alg)
	fmt.Fprintf(h, "%s|b%d|g%d|s%d", deviceID, boot, gen, seq)
	return h.Sum(nil)
}

// pickFault 保证每类故障在长序列中周期性出现，其余按故障率随机。
func pickFault(bIdx int, cfg Config, rng *rand.Rand) FaultKind {
	rotating := []FaultKind{
		FaultBankMissing, FaultSeqGap, FaultSeqDuplicate, FaultTamperEvidence,
		FaultFWReset, FaultSealTamper, FaultBootRepeat, FaultGenRegression,
		FaultUnsealedCrash, FaultBackfill, FaultLateSealed, FaultCrossGenRef,
		FaultEmptyBank,
	}
	// 每 17 个启动轮转一类计划故障，确保十万条规模下各类型都有充分样本。
	if bIdx > 0 && bIdx%17 == 0 {
		return rotating[(bIdx/17-1)%len(rotating)]
	}
	if rng.Float64() >= cfg.FaultRate {
		return FaultNone
	}
	return rotating[rng.Intn(len(rotating))]
}

func applyRecordFault(fault FaultKind, records []meas.Record, alg meas.AlgID, rng *rand.Rand, truth *BootTruth) ([]meas.Record, *meas.Record) {
	var backfill *meas.Record
	if len(records) == 0 {
		return records, nil
	}
	switch fault {
	case FaultBankMissing:
		// 丢掉整个应用库的记录，但掩码仍包含它 → 整库无法证实。
		// 必须分配新切片，不能用 records[:0] 原地过滤（底层数组别名会让删除失效）。
		filtered := make([]meas.Record, 0, len(records))
		for _, r := range records {
			if uint8(r.Mask)&meas.MaskAPP == 0 {
				filtered = append(filtered, r)
			}
		}
		records = filtered
		truth.WantReason = meas.UnprovBankMissing
	case FaultSeqGap:
		if len(records) >= 3 {
			drop := rng.Intn(len(records)-2) + 1
			records = append(append([]meas.Record(nil), records[:drop]...), records[drop+1:]...)
			truth.WantReason = meas.UnprovSeqGap
		}
	case FaultSeqDuplicate:
		// 复制一条记录并沿用其序号 → 同启动序号重复。
		dup := records[len(records)/2]
		records = append(records, dup)
		truth.WantRule = meas.ConfSeqDuplicate
	case FaultTamperEvidence:
		// 预重放拿到真实 next 值，再取逐位补数作为声称值，保证必然不同。
		if i := pickExtendIndex(records); i >= 0 {
			next := previewExtend(alg, records, i)
			wrong := append([]byte(nil), next...)
			for k := range wrong {
				wrong[k] ^= 0xff
			}
			records[i].Expected = wrong
		}
		truth.WantRule = meas.ConfEvidenceMismatch
	case FaultFWReset:
		records = append(records, meas.Record{
			Seq: len(records) + 1, Op: meas.OpReset, Mask: meas.Mask(meas.MaskFW),
		})
		truth.WantRule = meas.ConfFWReset
	case FaultBackfill:
		if len(records) >= 3 {
			drop := rng.Intn(len(records)-2) + 1
			backfill = &records[drop]
			records = append(append([]meas.Record(nil), records[:drop]...), records[drop+1:]...)
		}
	}
	return records, backfill
}

// pickExtendIndex 返回第一条扩展记录的下标，没有则返回 -1。
func pickExtendIndex(records []meas.Record) int {
	for i := range records {
		if records[i].Op == meas.OpExtend {
			return i
		}
	}
	return -1
}

// previewExtend 在完整记录流上预重放到目标记录，返回该记录对其首个命中
// 寄存器执行扩展后的真实值，供构造“必然不符”的声称值。
func previewExtend(alg meas.AlgID, records []meas.Record, target int) []byte {
	values := make([][]byte, meas.NumRegs)
	zero := meas.ZeroDigest(alg)
	for reg := range values {
		values[reg] = append([]byte(nil), zero...)
	}
	for _, r := range records {
		for reg := 0; reg < meas.NumRegs; reg++ {
			if uint8(r.Mask)&meas.RegMask(reg) == 0 {
				continue
			}
			if r.Op == meas.OpExtend {
				values[reg] = meas.Extend(alg, values[reg], r.Digest)
			} else if r.Op == meas.OpReset && meas.Resettable(reg) {
				values[reg] = append([]byte(nil), zero...)
			}
		}
	}
	// 目标记录命中的首个寄存器。
	for reg := 0; reg < meas.NumRegs; reg++ {
		if uint8(records[target].Mask)&meas.RegMask(reg) != 0 {
			return values[reg]
		}
	}
	return nil
}

func applySealFault(fault FaultKind, b *meas.Boot, truth *BootTruth) {
	switch fault {
	case FaultSealTamper:
		if len(b.Seal.SealValue) > 0 {
			b.Seal.SealValue[0] ^= 0x5a
		}
		truth.WantRule = meas.ConfSealMismatch
	case FaultUnsealedCrash, FaultBackfill:
		// 库已写完但总签封未落盘。
		b.Seal.Sealed = false
		b.Seal.SealValue = nil
	}
}

func applyArrivalFault(fault FaultKind, b meas.Boot, backfill *meas.Record,
	prior []meas.Boot, dd *DeviceDataset, truth *BootTruth) {
	switch fault {
	case FaultBackfill:
		if backfill != nil {
			r := *backfill
			r.RefBoot = b.BootNumber
			dd.Arrivals = append(dd.Arrivals, orchestrator.Arrival{
				Record: r, RefBoot: b.BootNumber, Generation: b.Generation,
			})
			truth.WantStatus = meas.BootUnsealed // 补记后链完整，但崩溃启动未重新签封
		}
	case FaultLateSealed:
		// 恢复后迟到：引用一个已正确落盘的启动，必然被拒绝。
		r := meas.Record{
			Seq: len(b.Records) + 1, Op: meas.OpExtend, Mask: meas.Mask(meas.MaskA1),
			Digest:  contentDigest(b.Algorithm, b.DeviceID, b.BootNumber, b.Generation, 999),
			RefBoot: b.BootNumber,
		}
		dd.Arrivals = append(dd.Arrivals, orchestrator.Arrival{
			Record: r, RefBoot: b.BootNumber, Generation: b.Generation,
		})
		truth.WantRule = meas.ConfLateAfterSealed
	case FaultCrossGenRef:
		foreign := b.Generation + 1
		r := meas.Record{
			Seq: 1, Op: meas.OpExtend, Mask: meas.Mask(meas.MaskC0),
			Digest:  contentDigest(meas.GenAlgorithm(foreign), b.DeviceID, b.BootNumber, foreign, 1),
			RefBoot: b.BootNumber,
		}
		dd.Arrivals = append(dd.Arrivals, orchestrator.Arrival{
			Record: r, RefBoot: b.BootNumber, Generation: foreign,
		})
		truth.WantRule = meas.ConfCrossGenRef
	}
}

func finalizeTruth(truth *BootTruth, fault FaultKind) {
	if truth.WantStatus != "" {
		return
	}
	switch fault {
	case FaultNone, FaultEmptyBank:
		truth.WantStatus = meas.BootOK
	case FaultUnsealedCrash:
		truth.WantStatus = meas.BootUnsealed
	case FaultBankMissing, FaultSeqGap:
		truth.WantStatus = meas.BootUnprovable
	default:
		// 记录/签封/历史级矛盾一律是确认冲突。
		truth.WantStatus = meas.BootConflicted
	}
}

// recomputeSeal 复用引擎报告里的重算签封，避免与 seal 包形成重复实现。
func recomputeSeal(b meas.Boot) []byte {
	return replay.Boot(b).RecomputedSeal
}

// CountRecords 统计数据集中全部启动的记录条数。
func CountRecords(ds *Dataset) int {
	total := 0
	for _, dd := range ds.Devices {
		for _, b := range dd.Ledger.Boots {
			total += len(b.Records)
		}
	}
	return total
}

// roundRobinMasks 是补量启动使用的确定性掩码序列（跨三个库轮替）。
var roundRobinMasks = []uint8{
	meas.MaskF0, meas.MaskC0, meas.MaskA0,
	meas.MaskF1, meas.MaskC1, meas.MaskA1,
	meas.MaskC0 | meas.MaskC1, meas.MaskA0 | meas.MaskA1,
}

// BuildCleanBoot 构造一条无故障、恰好含 nRecords 条记录的完整签封启动，
// 记录在三个库间确定性轮替，应用库每 5 条出现一次合法重置。
func BuildCleanBoot(deviceID string, bootNumber, generation int, prevSeal []byte, nRecords int) meas.Boot {
	alg := meas.GenAlgorithm(generation)
	records := make([]meas.Record, 0, nRecords)
	for seq := 1; seq <= nRecords; seq++ {
		mask := roundRobinMasks[(seq-1)%len(roundRobinMasks)]
		op := meas.OpExtend
		// 第 5、10… 条对应用库做合法重置（绝不触碰固件库）。
		if seq%5 == 0 {
			op = meas.OpReset
			mask = meas.MaskA0
		}
		r := meas.Record{Seq: seq, Op: op, Mask: meas.Mask(mask)}
		if op == meas.OpExtend {
			r.Digest = contentDigest(alg, deviceID, bootNumber, generation, seq)
		}
		records = append(records, r)
	}
	b := meas.Boot{
		DeviceID:   deviceID,
		BootNumber: bootNumber,
		Generation: generation,
		Algorithm:  alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		Records:    records,
	}
	// 记录太少时部分库不会被触达；把这些库显式声明为空库（合法），
	// 而不是留下“整库缺记录”的无法证实状态。空库寄存器标 EMPTY_BANK，整体仍 OK。
	touched := map[int]bool{}
	for _, r := range records {
		for bank := 0; bank < meas.NumBanks; bank++ {
			if uint8(r.Mask)&meas.BankMask(bank) != 0 {
				touched[bank] = true
			}
		}
	}
	var empty []int
	for bank := 0; bank < meas.NumBanks; bank++ {
		if !touched[bank] {
			empty = append(empty, bank)
		}
	}
	b.EmptyBanks = empty
	// 先挂含 PrevSeal/EmptyBanks 的签封骨架，再让引擎重放取签封值，保证规范头一致。
	b.Seal = meas.GenerationSeal{
		DeviceID:   deviceID,
		BootNumber: bootNumber,
		Generation: generation,
		Algorithm:  alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		EmptyBanks: append([]int(nil), empty...),
		PrevSeal:   append([]byte(nil), prevSeal...),
		Sealed:     true,
	}
	b.Seal.SealValue = replay.Boot(b).RecomputedSeal
	return b
}

// GenerateByRecordCount 生成“总记录条数恰为 totalRecords”的多设备数据集。
// 先用常规故障生成器超额生成，再按设备裁剪整启动，最后用无故障补量启动
// 精确补齐差额，因此故障分布与真值标签仍然保留。
func GenerateByRecordCount(devices, totalRecords int, baseSeed int64) *Dataset {
	if devices < 1 {
		devices = 1
	}
	perDevice := totalRecords / devices
	bootsEst := perDevice/4 + 2 // 每次启动保守按 4 条估算，保证超额
	ds := Generate(Config{
		Devices: devices, BootsPerDevice: bootsEst,
		BaseSeed: baseSeed, FaultRate: 0.25,
	})
	for d := range ds.Devices {
		dd := &ds.Devices[d]
		target := perDevice
		if d == 0 {
			target += totalRecords - perDevice*devices // 余数交给首个设备
		}
		trimDevice(dd, target)
	}
	ds.Input = meas.InputFile{Synthetic: true}
	for _, dd := range ds.Devices {
		ds.Input.Ledgers = append(ds.Input.Ledgers, dd.Ledger)
	}
	return ds
}

// trimDevice 保留累计记录不超过 target 的完整启动，再用补量启动精确补齐。
func trimDevice(dd *DeviceDataset, target int) {
	kept := 0
	var boots []meas.Boot
	var truth []BootTruth
	for i, b := range dd.Ledger.Boots {
		if kept+len(b.Records) > target {
			break
		}
		kept += len(b.Records)
		boots = append(boots, b)
		truth = append(truth, dd.Truth[i])
	}
	// 到达流只保留落在保留启动号范围内的项。
	maxBoot := 0
	if len(boots) > 0 {
		maxBoot = boots[len(boots)-1].BootNumber
	}
	var arrivals []orchestrator.Arrival
	for _, ar := range dd.Arrivals {
		if ar.RefBoot <= maxBoot {
			arrivals = append(arrivals, ar)
		}
	}
	// 精确补量：一个无故障启动承载剩余条数。
	if kept < target {
		need := target - kept
		nextBoot, gen := 1, 1
		var prevSeal []byte
		if len(boots) > 0 {
			last := boots[len(boots)-1]
			nextBoot = last.BootNumber + 1
			gen = last.Generation
			if last.Seal.Sealed {
				prevSeal = append([]byte(nil), last.Seal.SealValue...)
			} else {
				prevSeal = append([]byte(nil), last.Seal.PrevSeal...)
			}
		}
		top := BuildCleanBoot(dd.Ledger.DeviceID, nextBoot, gen, prevSeal, need)
		boots = append(boots, top)
		truth = append(truth, BootTruth{
			BootNumber: nextBoot, Generation: gen,
			Fault: FaultNone, WantStatus: meas.BootOK,
		})
	}
	dd.Ledger.Boots = boots
	dd.Truth = truth
	dd.Arrivals = arrivals
}
