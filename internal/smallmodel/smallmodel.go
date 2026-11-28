// Package smallmodel 用最小化模型穷举各摘要库记录的物理交错，验证：
//   - 合流性：无论库日志以何种物理次序落盘，只要逻辑序号一致，
//     重放得到的最终寄存器值与冲突结论必须完全相同；
//   - 重置先发生：重置记录被排到扩展之前不会改变最终链；
//   - 崩溃缺库与异常重置在所有交错下都给出稳定结论。
package smallmodel

import (
	"errors"
	"fmt"
	"sort"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/replay"
)

// InterleaveResult 是穷举的一次结果。
type InterleaveResult struct {
	Index       int
	Order       []int // 逻辑记录序号的一种排列
	Status      meas.BootStatus
	FinalDigest []byte
}

// Outcome 汇总穷举结论。
type Outcome struct {
	Permutations int
	Distinct     int
	AllConverged bool
	Results      []InterleaveResult
	Divergence   string
}

// bankStreams 按库准备逻辑记录：每个库 2 条记录，共 6 条，
// 包含一次合法的应用库重置，使交错空间具有真实语义。
func bankStreams(alg meas.AlgID) [][]meas.Record {
	mk := func(seq int, op meas.Op, mask uint8, salt int) meas.Record {
		r := meas.Record{Seq: seq, Op: op, Mask: meas.Mask(mask)}
		if op == meas.OpExtend {
			h := meas.NewHasher(alg)
			fmt.Fprintf(h, "smallmodel|%d", salt)
			r.Digest = h.Sum(nil)
		}
		return r
	}
	return [][]meas.Record{
		{mk(1, meas.OpExtend, meas.MaskF0, 1), mk(4, meas.OpExtend, meas.MaskF1, 4)},
		{mk(2, meas.OpExtend, meas.MaskC0, 2), mk(5, meas.OpExtend, meas.MaskC1, 5)},
		{mk(3, meas.OpExtend, meas.MaskA0, 3), mk(6, meas.OpReset, meas.MaskA0, 6)},
	}
}

// enumerate 递归枚举保持“各库内部次序”的所有全局交错。
func enumerate(streams [][]meas.Record) [][]meas.Record {
	idx := make([]int, len(streams))
	var out [][]meas.Record
	var cur []meas.Record
	var walk func()
	walk = func() {
		advanced := false
		for b := range streams {
			if idx[b] < len(streams[b]) {
				advanced = true
				r := streams[b][idx[b]]
				idx[b]++
				cur = append(cur, r)
				walk()
				cur = cur[:len(cur)-1]
				idx[b]--
			}
		}
		if !advanced {
			cp := append([]meas.Record(nil), cur...)
			out = append(out, cp)
		}
	}
	walk()
	return out
}

func minimalBoot(records []meas.Record) meas.Boot {
	gen := 2 // SHA-384，覆盖非默认算法
	alg := meas.GenAlgorithm(gen)
	b := meas.Boot{
		DeviceID:   "SMALL-MODEL",
		BootNumber: 1,
		Generation: gen,
		Algorithm:  alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		Records:    records,
	}
	rep := replay.Boot(b)
	b.Seal = meas.GenerationSeal{
		DeviceID: "SMALL-MODEL", BootNumber: 1, Generation: gen, Algorithm: alg,
		ActiveMask: meas.Mask(meas.MaskAll),
		SealValue:  rep.RecomputedSeal, Sealed: true,
	}
	return b
}

// stateDigest 把 6 个寄存器最终值与状态压成一个合流指纹。
func stateDigest(rep meas.BootReport) []byte {
	h := meas.NewHasher(rep.Algorithm)
	for reg := range rep.Registers {
		rr := rep.Registers[reg]
		h.Write([]byte(rr.Status))
		h.Write(rr.Final)
	}
	return h.Sum(nil)
}

// RunExhaustive 穷举全部交错并重放，检查合流性。
func RunExhaustive() (Outcome, error) {
	alg := meas.GenAlgorithm(2)
	streams := bankStreams(alg)
	perms := enumerate(streams)
	out := Outcome{Permutations: len(perms), AllConverged: true}
	seen := map[string]bool{}
	for i, perm := range perms {
		// 物理交错后必须按逻辑序号归并回有序流。
		sorted := append([]meas.Record(nil), perm...)
		sortRecords(sorted)
		b := minimalBoot(sorted)
		rep := replay.Boot(b)
		fp := stateDigest(rep)
		seen[string(fp)] = true
		order := make([]int, len(perm))
		for j, r := range perm {
			order[j] = r.Seq
		}
		out.Results = append(out.Results, InterleaveResult{
			Index: i, Order: order, Status: rep.Status, FinalDigest: fp,
		})
	}
	out.Distinct = len(seen)
	if len(seen) != 1 {
		out.AllConverged = false
		out.Divergence = fmt.Sprintf("出现 %d 种不同最终状态，逻辑序号合流性被破坏", len(seen))
		return out, errors.New(out.Divergence)
	}
	return out, nil
}

// RunResetFirst 穷举“重置先于扩展落盘”的交错，断言重放结论与
// 逻辑次序完全一致（引擎只信任逻辑序号）。
func RunResetFirst() error {
	alg := meas.GenAlgorithm(2)
	streams := bankStreams(alg)
	perms := enumerate(streams)
	// 基线：严格逻辑次序。
	base := mergeSorted(streams)
	baseRep := replay.Boot(minimalBoot(base))
	for _, perm := range perms {
		// 模拟重置记录物理上最先落盘。
		shuffled := append([]meas.Record(nil), perm...)
		sort.SliceStable(shuffled, func(i, j int) bool {
			return shuffled[i].Op == meas.OpReset && shuffled[j].Op != meas.OpReset
		})
		sortRecords(shuffled) // 重放前按逻辑序号归并
		rep := replay.Boot(minimalBoot(shuffled))
		if string(stateDigest(rep)) != string(stateDigest(baseRep)) {
			return fmt.Errorf("重置先落盘交错产生了不同最终状态")
		}
	}
	return nil
}

// RunCrashAndConflict 对每个交错注入：缺库（无法证实）、固件重置（冲突）、
// 证据篡改（冲突），断言结论在所有交错下稳定。
func RunCrashAndConflict() error {
	alg := meas.GenAlgorithm(2)
	streams := bankStreams(alg)
	perms := enumerate(streams)

	cases := []struct {
		name      string
		mutate    func([]meas.Record) []meas.Record
		want      meas.BootStatus
		wantRule  meas.ConflictCode
		wantRange meas.UnprovableReason
	}{
		{
			name: "missing-app-bank",
			mutate: func(rs []meas.Record) []meas.Record {
				var out []meas.Record
				for _, r := range rs {
					if uint8(r.Mask)&meas.MaskAPP == 0 {
						out = append(out, r)
					}
				}
				return out
			},
			want:      meas.BootUnprovable,
			wantRange: meas.UnprovBankMissing,
		},
		{
			name: "firmware-reset",
			mutate: func(rs []meas.Record) []meas.Record {
				return append(rs, meas.Record{Seq: 99, Op: meas.OpReset, Mask: meas.Mask(meas.MaskFW)})
			},
			want:     meas.BootConflicted,
			wantRule: meas.ConfFWReset,
		},
		{
			name: "tampered-evidence",
			mutate: func(rs []meas.Record) []meas.Record {
				for i := range rs {
					if rs[i].Op == meas.OpExtend && rs[i].Mask == meas.Mask(meas.MaskF0) {
						rs[i].Expected = meas.ZeroDigest(alg) // 与重算值必然不同
					}
				}
				return rs
			},
			want:     meas.BootConflicted,
			wantRule: meas.ConfEvidenceMismatch,
		},
	}

	for _, tc := range cases {
		for _, perm := range perms {
			rs := append([]meas.Record(nil), perm...)
			sortRecords(rs)
			rs = tc.mutate(rs)
			rep := replay.Boot(minimalBoot(rs))
			if rep.Status != tc.want {
				return fmt.Errorf("%s: 交错下状态漂移 got=%s want=%s", tc.name, rep.Status, tc.want)
			}
			if tc.wantRule != "" {
				if rep.FirstConflict == nil || rep.FirstConflict.Rule != tc.wantRule {
					return fmt.Errorf("%s: 冲突规则漂移", tc.name)
				}
			}
			if tc.wantRange != "" {
				found := false
				for _, rg := range rep.Unprovable {
					if rg.Reason == tc.wantRange {
						found = true
					}
				}
				if !found {
					return fmt.Errorf("%s: 无法证实范围丢失", tc.name)
				}
			}
		}
	}
	return nil
}

func mergeSorted(streams [][]meas.Record) []meas.Record {
	var all []meas.Record
	for _, s := range streams {
		all = append(all, s...)
	}
	sortRecords(all)
	return all
}

func sortRecords(rs []meas.Record) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j-1].Seq > rs[j].Seq; j-- {
			rs[j-1], rs[j] = rs[j], rs[j-1]
		}
	}
}
