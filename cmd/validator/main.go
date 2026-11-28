// validator 是 BootLedger 离线校验器命令行：
//
//	validator validate  -input ledger.json [-out report.json]  整次重放并输出报告
//	validator evidence  -evidence ev.json                       独立核验单条短证据
//	validator version                                            打印模型版本
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"example.invalid/local-fixture/internal/envelope"
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
)

const modelVersion = "BootLedger/BLR1+BLH1 (fictional)"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "validate":
		runValidate(os.Args[2:])
	case "evidence":
		runEvidence(os.Args[2:])
	case "version":
		fmt.Println(modelVersion)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: validator validate -input <ledger.json> [-out report.json]")
	fmt.Fprintln(os.Stderr, "      validator evidence -evidence <ev.json>")
	fmt.Fprintln(os.Stderr, "      validator version")
}

func runValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	in := fs.String("input", "", "输入账文件（envelope.File JSON）")
	out := fs.String("out", "", "可选：报告输出路径，缺省打印到标准输出")
	_ = fs.Parse(args)
	if *in == "" {
		fs.Usage()
		os.Exit(2)
	}
	raw, err := os.ReadFile(*in)
	must(err)
	var f envelope.File
	must(json.Unmarshal(raw, &f))

	report := envelope.Report{
		Synthetic:   f.Synthetic,
		DeviceCount: len(f.Ledgers),
		BootReports: map[string][]meas.BootReport{},
	}
	statusTmpl := map[string]int{}
	for _, l := range f.Ledgers {
		arrivals := f.Arrivals[l.DeviceID]
		dr := orchestrator.Process(l, arrivals)
		summary := envelope.DeviceSummary{
			DeviceID:          dr.DeviceID,
			BootCount:         dr.BootCount,
			StatusCounts:      map[string]int{},
			FirstConflict:     dr.FirstConflict,
			BackfilledBoots:   dr.BackfilledBoots,
			BackfilledRecords: dr.BackfilledRecords,
			LateRejected:      dr.LateRejected,
		}
		for _, br := range dr.Boots {
			summary.StatusCounts[string(br.Status)]++
			report.BootReports[l.DeviceID] = append(report.BootReports[l.DeviceID], br)
			report.TotalConflicts += len(br.Conflicts)
			if br.Status == meas.BootUnprovable {
				report.TotalUnprovable++
			}
		}
		_ = statusTmpl
		report.Devices = append(report.Devices, summary)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			must(err)
		}
		fh, err := os.Create(*out)
		must(err)
		defer fh.Close()
		enc = json.NewEncoder(fh)
		enc.SetIndent("", "  ")
	}
	must(enc.Encode(report))
}

// runEvidence 仅凭证据自身字段离线重算，验证短证据可独立核验。
func runEvidence(args []string) {
	fs := flag.NewFlagSet("evidence", flag.ExitOnError)
	path := fs.String("evidence", "", "单条证据 JSON（meas.Evidence）")
	_ = fs.Parse(args)
	if *path == "" {
		fs.Usage()
		os.Exit(2)
	}
	raw, err := os.ReadFile(*path)
	must(err)
	var ev meas.Evidence
	must(json.Unmarshal(raw, &ev))

	ok := true
	// 所有携带规范记录文本的证据：先重建记录，再核对记录哈希。
	if ev.RecordCanonical != "" {
		r, err := meas.ParseCanonicalRecord(ev.RecordCanonical)
		if err != nil {
			fail("规范记录无法解析: %v", err)
		}
		gotHash := meas.CanonicalRecordHash(ev.Algorithm, r)
		if !hexEqual(gotHash, ev.RecordHash) {
			ok = false
			fmt.Printf("记录哈希不符\n got=%x\nwant=%x\n", gotHash, ev.RecordHash)
		}
	}

	switch ev.Rule {
	case meas.ConfEvidenceMismatch:
		// 独立重算 H(before‖input)，必须等于 recomputed，且不等于 claimed。
		got := meas.Extend(ev.Algorithm, ev.Before, ev.InputDigest)
		if !hexEqual(got, ev.Recomputed) {
			ok = false
			fmt.Printf("重算值与证据 recomputed 不符\n got=%x\nwant=%x\n", got, ev.Recomputed)
		}
		if hexEqual(got, ev.Claimed) {
			ok = false
			fmt.Println("重算值竟然等于 claimed，冲突不成立")
		}
	case meas.ConfSealMismatch:
		// 签封冲突：recomputed 与 claimed 必须确实不同。
		if hexEqual(ev.Recomputed, ev.Claimed) {
			ok = false
			fmt.Println("重算签封与落盘签封相同，冲突不成立")
		}
	case meas.ConfSeqDuplicate, meas.ConfFWReset, meas.ConfLateAfterSealed,
		meas.ConfCrossGenRef, meas.ConfBootNumberRepeat, meas.ConfGenRegression,
		meas.ConfSealChainBroken, meas.ConfBackfillDuplicate,
		meas.ConfMaskOutOfActive, meas.ConfBadDigestLen, meas.ConfEmptyBankViolated:
		// 结构性规则：规范文本 + 记录哈希自洽即视为证据成立。
	default:
		fmt.Printf("规则 %s 按结构完整性核验\n", ev.Rule)
	}
	if ok {
		fmt.Printf("OK: 证据 %s @ %s boot=%d seq=%d 独立核验通过\n",
			ev.Rule, ev.DeviceID, ev.BootNumber, ev.Seq)
		return
	}
	os.Exit(1)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "validator: "+format+"\n", a...)
	os.Exit(1)
}

func hexEqual(a, b []byte) bool {
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

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "validator: ", err)
		os.Exit(1)
	}
}
