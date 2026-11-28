package fixture

import (
	"encoding/json"
	"os"
	"testing"

	"example.invalid/local-fixture/internal/envelope"
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
)

// fixture.json 是一个可直接重放的虚构 BootLedger 信封（GOLDEN 设备，
// 覆盖补记、空库、异常重置三类场景）。本测试固定其结构与重放结论。
func TestSyntheticInput(t *testing.T) {
	data, err := os.ReadFile("fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var input envelope.File
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	if !input.Synthetic || len(input.Ledgers) != 1 {
		t.Fatal("输入布局非法")
	}
	ledger := input.Ledgers[0]
	if ledger.DeviceID != "GOLDEN" || len(ledger.Boots) != 3 {
		t.Fatalf("账布局不符: %+v", ledger)
	}

	rep := orchestrator.Process(ledger, input.Arrivals["GOLDEN"])
	wantStatus := []meas.BootStatus{meas.BootUnsealed, meas.BootOK, meas.BootConflicted}
	if len(rep.Boots) != 3 {
		t.Fatalf("启动数错误: %d", len(rep.Boots))
	}
	for i, want := range wantStatus {
		if rep.Boots[i].Status != want {
			t.Fatalf("启动 %d 状态 got=%s want=%s", i+1, rep.Boots[i].Status, want)
		}
	}
	// 第三个启动必须是固件库异常重置，且带可独立核验的短证据。
	last := rep.Boots[2]
	if last.FirstConflict == nil || last.FirstConflict.Rule != meas.ConfFWReset ||
		last.FirstConflict.RecordCanonical == "" {
		t.Fatalf("固件重置短证据缺失: %+v", last.FirstConflict)
	}
	// 补记恰好补 1 个启动、1 条记录。
	if rep.BackfilledBoots != 1 || rep.BackfilledRecords != 1 {
		t.Fatalf("补记计数错误: %+v", rep)
	}
}
