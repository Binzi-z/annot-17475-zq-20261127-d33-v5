// genbench 生成十万条级合成序列，验证增量检查点与整次重放一致，
// 并以实际计数说明内容寻址缓存何时可复用，同时量化补记影响。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"example.invalid/local-fixture/internal/envelope"
	"example.invalid/local-fixture/internal/gen"
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
	"example.invalid/local-fixture/internal/replay"
	"example.invalid/local-fixture/internal/smallmodel"
	"example.invalid/local-fixture/internal/store"
)

// Stats 是基准运行的量化结论。
type Stats struct {
	TargetRecords       int               `json:"target_records"`
	ActualRecords       int               `json:"actual_records"`
	Devices             int               `json:"devices"`
	TotalBoots          int               `json:"total_boots"`
	EquivalenceChecks   int               `json:"equivalence_checks"`
	EquivalenceFailures int               `json:"equivalence_failures"`
	Cache               CacheStats        `json:"cache"`
	Backfill            BackfillStats     `json:"backfill"`
	StatusCounts        map[string]int    `json:"boot_status_counts"`
	RuleCounts          map[string]int    `json:"conflict_rule_counts"`
	TruthMismatches     int               `json:"truth_mismatches"`
	SmallModel          SmallModelStat    `json:"small_model"`
	DualWriter          map[string]string `json:"dual_writer"`
	ElapsedMS           int64             `json:"elapsed_ms"`
}

type CacheStats struct {
	ColdMisses        int `json:"cold_misses"`
	WarmHits          int `json:"warm_hits"`
	MutatedMisses     int `json:"mutated_misses"`
	ReusedBootsOnWarm int `json:"reused_boots_on_warm_hit"`
	CheckpointsStored int `json:"checkpoints_stored"`
}

type BackfillStats struct {
	BackfilledBoots     int `json:"backfilled_boots"`
	BackfilledRecords   int `json:"backfilled_records"`
	DistinctGenerations int `json:"distinct_generations"`
	LateRejected        int `json:"late_rejected"`
}

type SmallModelStat struct {
	InterleavePermutations int  `json:"interleave_permutations"`
	DistinctFinalStates    int  `json:"distinct_final_states"`
	Converged              bool `json:"converged"`
	ResetFirstOK           bool `json:"reset_first_ok"`
	CrashConflictOK        bool `json:"crash_conflict_ok"`
}

func main() {
	records := flag.Int("records", 100000, "目标总记录条数")
	devices := flag.Int("devices", 8, "合成设备数（设备间严格隔离）")
	seed := flag.Int64("seed", 20261128, "确定性随机种子")
	out := flag.String("out", "", "可选：把生成的输入信封写到该 JSON 文件")
	reportPath := flag.String("report", "", "可选：把统计 JSON 写到该文件")
	flag.Parse()

	start := time.Now()
	ds := gen.GenerateByRecordCount(*devices, *records, *seed)
	actual := gen.CountRecords(ds)
	if actual != *records {
		fatal("记录数不精确: want=%d got=%d", *records, actual)
	}

	stats := Stats{
		TargetRecords: *records,
		ActualRecords: actual,
		Devices:       *devices,
		StatusCounts:  map[string]int{},
		RuleCounts:    map[string]int{},
		DualWriter:    map[string]string{},
	}

	// 小模型穷举。
	outcome, err := smallmodel.RunExhaustive()
	must(err)
	stats.SmallModel = SmallModelStat{
		InterleavePermutations: outcome.Permutations,
		DistinctFinalStates:    outcome.Distinct,
		Converged:              outcome.AllConverged,
		ResetFirstOK:           smallmodel.RunResetFirst() == nil,
		CrashConflictOK:        smallmodel.RunCrashAndConflict() == nil,
	}
	if !stats.SmallModel.ResetFirstOK || !stats.SmallModel.CrashConflictOK {
		fatal("小模型重置/崩溃断言失败")
	}

	backfillGens := map[int]bool{}
	env := envelope.File{Synthetic: true, Arrivals: map[string][]orchestrator.Arrival{}}

	for di := range ds.Devices {
		dd := &ds.Devices[di]
		env.Ledgers = append(env.Ledgers, dd.Ledger)
		env.Arrivals[dd.Ledger.DeviceID] = dd.Arrivals
		stats.TotalBoots += len(dd.Ledger.Boots)

		// 1) 整次重放 vs 增量检查点：逐设备比对，全部必须一致。
		cache := store.NewCache()
		if err := cache.VerifyEquivalence(dd.Ledger, dd.Arrivals); err != nil {
			stats.EquivalenceFailures++
			fmt.Fprintf(os.Stderr, "一致性失败 %s: %v\n", dd.Ledger.DeviceID, err)
		}
		stats.EquivalenceChecks++
		stats.Cache.ColdMisses += cache.Misses
		stats.Cache.CheckpointsStored += cache.Len()

		// 2) 缓存复用：同一输入第二次增量处理必须命中并复用全部启动。
		warm := cache.IncrementalProcess(dd.Ledger, dd.Arrivals)
		if warm.Reused {
			stats.Cache.WarmHits++
			stats.Cache.ReusedBootsOnWarm += warm.ReusedPrefix
		}

		// 3) 篡改一条记录后，内容键必须变化 → 缓存不可复用（新 miss）。
		mutated := mutateOneRecord(dd.Ledger)
		mutRes := cache.IncrementalProcess(mutated, dd.Arrivals)
		if mutRes.Reused {
			fatal("设备 %s 篡改后仍命中缓存，内容寻址失效", dd.Ledger.DeviceID)
		}
		stats.Cache.MutatedMisses++

		// 4) 真值核对与计数。
		rep := orchestrator.Process(dd.Ledger, dd.Arrivals)
		if len(rep.Boots) != len(dd.Truth) {
			stats.TruthMismatches++
			fmt.Fprintf(os.Stderr, "真值条数不符 %s: %d vs %d\n",
				dd.Ledger.DeviceID, len(rep.Boots), len(dd.Truth))
		}
		for i, br := range rep.Boots {
			stats.StatusCounts[string(br.Status)]++
			for _, c := range br.Conflicts {
				stats.RuleCounts[string(c.Rule)]++
			}
			if i < len(dd.Truth) {
				t := dd.Truth[i]
				if br.Status != t.WantStatus {
					stats.TruthMismatches++
					fmt.Fprintf(os.Stderr, "真值不符 %s boot=%d: got=%s want=%s fault=%s\n",
						dd.Ledger.DeviceID, t.BootNumber, br.Status, t.WantStatus, t.Fault)
				}
				if t.WantRule != "" && !containsRule(br.Conflicts, t.WantRule) {
					stats.TruthMismatches++
					fmt.Fprintf(os.Stderr, "规则缺失 %s boot=%d: 未发现 %s\n",
						dd.Ledger.DeviceID, t.BootNumber, t.WantRule)
				}
			}
		}
		stats.Backfill.BackfilledBoots += rep.BackfilledBoots
		stats.Backfill.BackfilledRecords += rep.BackfilledRecords
		stats.Backfill.LateRejected += rep.LateRejected
		for _, t := range dd.Truth {
			if t.Fault == gen.FaultBackfill {
				backfillGens[t.Generation] = true
			}
		}
	}
	stats.Backfill.DistinctGenerations = len(backfillGens)
	stats.ElapsedMS = time.Since(start).Milliseconds()

	// 双写者三场景确定性验证。
	stats.DualWriter = runDualWriterScenarios()

	if *out != "" {
		must(os.MkdirAll(filepath.Dir(*out), 0o755))
		writeJSON(*out, env)
	}
	writeIndented(os.Stdout, stats)
	if *reportPath != "" {
		must(os.MkdirAll(filepath.Dir(*reportPath), 0o755))
		writeJSON(*reportPath, stats)
	}
	if stats.EquivalenceFailures != 0 || stats.TruthMismatches != 0 {
		os.Exit(1)
	}
	fmt.Println("\n结论: 增量检查点与整次重放 100% 一致；缓存在冷启动/篡改时 miss，" +
		"仅在输入前缀字节级不变时 warm hit 复用。")
}

// mutateOneRecord 复制账并翻转一条扩展记录摘要的一个字节。
func mutateOneRecord(l meas.Ledger) meas.Ledger {
	cp := meas.Ledger{DeviceID: l.DeviceID, Boots: append([]meas.Boot(nil), l.Boots...)}
	for bi := range cp.Boots {
		for ri := range cp.Boots[bi].Records {
			r := &cp.Boots[bi].Records[ri]
			if r.Op == meas.OpExtend && len(r.Digest) > 0 {
				records := append([]meas.Record(nil), cp.Boots[bi].Records...)
				digest := append([]byte(nil), r.Digest...)
				digest[0] ^= 0xa5
				records[ri].Digest = digest
				cp.Boots[bi].Records = records
				return cp
			}
		}
	}
	return cp
}

func containsRule(conflicts []meas.Evidence, rule meas.ConflictCode) bool {
	for _, c := range conflicts {
		if c.Rule == rule {
			return true
		}
	}
	return false
}

// runDualWriterScenarios 验证三种确定性崩溃/恢复场景的结论。
func runDualWriterScenarios() map[string]string {
	res := map[string]string{}
	alg := meas.AlgSHA256
	records := []meas.Record{
		{Seq: 1, Op: meas.OpExtend, Mask: meas.Mask(meas.MaskF0), Digest: meas.DigestSum(alg, []byte("fw"))},
		{Seq: 2, Op: meas.OpExtend, Mask: meas.Mask(meas.MaskC0), Digest: meas.DigestSum(alg, []byte("cfg"))},
		{Seq: 3, Op: meas.OpReset, Mask: meas.Mask(meas.MaskA0)},
		{Seq: 4, Op: meas.OpExtend, Mask: meas.Mask(meas.MaskA0), Digest: meas.DigestSum(alg, []byte("app"))},
	}
	b := meas.Boot{
		DeviceID: "DW-DEV", BootNumber: 1, Generation: 1, Algorithm: alg,
		ActiveMask: meas.Mask(meas.MaskAll), Records: records,
	}
	b.Seal = meas.GenerationSeal{
		DeviceID: "DW-DEV", BootNumber: 1, Generation: 1, Algorithm: alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		SealValue:  replay.Boot(b).RecomputedSeal, Sealed: true,
	}

	// 场景一：重置先落盘——恢复后按逻辑序号归并，结论与正常次序一致。
	dw := store.NewDualWriter(store.CrashResetFirst)
	pb := dw.Commit(b)
	recovered, _ := dw.Recover(pb, nil)
	base := mustReplay(b)
	recRep := mustReplay(recovered)
	res[string(store.CrashResetFirst)] = ternary(
		eq(recRep.Registers, base.Registers), "OK 物理交错合流", "FAIL 合流破坏")

	// 场景二：库写完、总签封未落盘——状态 UNSEALED 而非冲突。
	dw2 := store.NewDualWriter(store.CrashUnsealed)
	pb2 := dw2.Commit(b)
	rep2 := mustReplay(pb2.Boot)
	res[string(store.CrashUnsealed)] = ternary(
		!pb2.Boot.Seal.Sealed && rep2.Status == meas.BootUnsealed,
		"OK 标记 UNSEALED（无法证实签封，非前后冲突）", "FAIL 崩溃语义错误")

	// 场景三：恢复重新签封后出现迟到事件——必须被拒绝为 LATE_AFTER_SEALED。
	late := meas.Record{Seq: 5, Op: meas.OpExtend, Mask: meas.Mask(meas.MaskA1),
		Digest: meas.DigestSum(alg, []byte("late"))}
	_, arrivals := dw2.Recover(pb2, []meas.Record{late})
	dr := orchestrator.Process(meas.Ledger{DeviceID: "DW-DEV", Boots: []meas.Boot{b}}, arrivals)
	gotRule := ""
	if len(dr.CrossBootConflicts) > 0 {
		gotRule = string(dr.CrossBootConflicts[0].Rule)
	}
	res[string(store.CrashLateEvent)] = ternary(
		gotRule == string(meas.ConfLateAfterSealed), "OK 迟到事件被拒绝", "FAIL 迟到事件被吞掉: "+gotRule)
	return res
}

func mustReplay(b meas.Boot) meas.BootReport {
	return replay.Boot(b)
}

func eq(a, b [meas.NumRegs]meas.RegisterReport) bool {
	for i := range a {
		if a[i].Status != b[i].Status || !hexEqual(a[i].Final, b[i].Final) {
			return false
		}
	}
	return true
}

func ternary(ok bool, a, bad string) string {
	if ok {
		return a
	}
	return bad
}

func hexEqual(x, y []byte) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func writeJSON(path string, v any) {
	fh, err := os.Create(path)
	must(err)
	defer fh.Close()
	enc := json.NewEncoder(fh)
	enc.SetIndent("", "  ")
	must(enc.Encode(v))
}

func writeIndented(f *os.File, v any) {
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	must(enc.Encode(v))
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "genbench: "+format+"\n", a...)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}
