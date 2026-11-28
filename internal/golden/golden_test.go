// Package golden 维护“独立真值小样本”：补记、空库、异常重置三个场景，
// 以确定性 JSON 落在 testdata/，并附带人工可读的预期结论。
// 常规测试直接读盘比对；加 -update 运行可重新生成金样本。
package golden

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"example.invalid/local-fixture/internal/envelope"
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
	"example.invalid/local-fixture/internal/testhelp"
)

var update = flag.Bool("update", false, "重新生成 testdata 金样本")

// Expectation 是金样本中每个启动的人工真值。
type Expectation struct {
	DeviceID   string                `json:"device_id"`
	BootNumber int                   `json:"boot_number"`
	Status     meas.BootStatus       `json:"status"`
	Rule       meas.ConflictCode     `json:"rule,omitempty"`
	Reason     meas.UnprovableReason `json:"reason,omitempty"`
}

// GoldenBundle 是落盘的金样本输入与预期。
type GoldenBundle struct {
	Input        envelope.File `json:"input"`
	Expectations []Expectation `json:"expectations"`
}

var goldenPath = filepath.Join("..", "..", "testdata", "golden_bundle.json")

func buildBundle() GoldenBundle {
	alg := meas.AlgSHA256
	var bundle GoldenBundle
	arrivals := map[string][]orchestrator.Arrival{}

	// 场景一：补记——启动缺一条记录且未签封，同代补记到达后链补齐。
	full := []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "GOLDEN", 1, 1),
		testhelp.Extend(2, meas.MaskC0, alg, "GOLDEN", 1, 1),
		testhelp.Extend(3, meas.MaskA0, alg, "GOLDEN", 1, 1),
	}
	backfillBoot := testhelp.Unsealed(testhelp.Boot("GOLDEN", 1, 1, nil,
		[]meas.Record{full[0], full[2]}))
	arrivals["GOLDEN"] = append(arrivals["GOLDEN"], orchestrator.Arrival{
		Record: full[1], RefBoot: 1, Generation: 1,
	})
	bundle.Expectations = append(bundle.Expectations, Expectation{
		DeviceID: "GOLDEN", BootNumber: 1, Status: meas.BootUnsealed,
	})

	// 场景二：空库——配置库显式声明为空，其余库正常，整体 OK。
	emptyBoot := testhelp.BootWithEmpty("GOLDEN", 2, 1, backfillBoot.Seal.PrevSeal,
		[]int{meas.BankCFG}, []meas.Record{
			testhelp.Extend(1, meas.MaskF0, alg, "GOLDEN", 2, 1),
			testhelp.Extend(2, meas.MaskA0, alg, "GOLDEN", 2, 1),
		})
	bundle.Expectations = append(bundle.Expectations, Expectation{
		DeviceID: "GOLDEN", BootNumber: 2, Status: meas.BootOK,
	})

	// 场景三：异常重置——对固件库下重置指令，必须判 FIRMWARE_RESET_REJECTED。
	fwResetBoot := testhelp.Boot("GOLDEN", 3, 1, emptyBoot.Seal.SealValue, []meas.Record{
		testhelp.Extend(1, meas.MaskF0, alg, "GOLDEN", 3, 1),
		testhelp.Reset(2, meas.MaskFW),
	})
	bundle.Expectations = append(bundle.Expectations, Expectation{
		DeviceID: "GOLDEN", BootNumber: 3, Status: meas.BootConflicted,
		Rule: meas.ConfFWReset,
	})

	bundle.Input = envelope.File{
		Synthetic: true,
		Ledgers: []meas.Ledger{{
			DeviceID: "GOLDEN",
			Boots:    []meas.Boot{backfillBoot, emptyBoot, fwResetBoot},
		}},
		Arrivals: arrivals,
	}
	return bundle
}

func TestGoldenBundle(t *testing.T) {
	if *update {
		bundle := buildBundle()
		must(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		raw, err := json.MarshalIndent(bundle, "", "  ")
		must(t, err)
		must(t, os.WriteFile(goldenPath, raw, 0o644))
		t.Logf("已写出金样本: %s", goldenPath)
	}

	raw, err := os.ReadFile(goldenPath)
	must(t, err)
	var bundle GoldenBundle
	must(t, json.Unmarshal(raw, &bundle))

	ledger := bundle.Input.Ledgers[0]
	rep := orchestrator.Process(ledger, bundle.Input.Arrivals["GOLDEN"])
	if len(rep.Boots) != len(bundle.Expectations) {
		t.Fatalf("启动数不符: got=%d want=%d", len(rep.Boots), len(bundle.Expectations))
	}
	for i, want := range bundle.Expectations {
		got := rep.Boots[i]
		if got.BootNumber != want.BootNumber || got.Status != want.Status {
			t.Fatalf("启动 %d 状态不符: got=(%d,%s) want=(%d,%s)",
				i, got.BootNumber, got.Status, want.BootNumber, want.Status)
		}
		if want.Rule != "" {
			found := false
			for _, c := range got.Conflicts {
				if c.Rule == want.Rule {
					found = true
					// 短证据必须自包含且可独立重算。
					if c.RecordCanonical == "" || len(c.RecordHash) == 0 {
						t.Fatalf("规则 %s 的证据不自包含", want.Rule)
					}
				}
			}
			if !found {
				t.Fatalf("启动 %d 缺少预期规则 %s", want.BootNumber, want.Rule)
			}
		}
	}

	// 补记量化：恰好影响 1 个启动代、1 条记录。
	if rep.BackfilledBoots != 1 || rep.BackfilledRecords != 1 {
		t.Fatalf("补记计数错误: boots=%d records=%d", rep.BackfilledBoots, rep.BackfilledRecords)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
