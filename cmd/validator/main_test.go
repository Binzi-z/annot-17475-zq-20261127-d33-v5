package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"example.invalid/local-fixture/internal/envelope"
	"example.invalid/local-fixture/internal/meas"
)

var goldenInput = filepath.Join("..", "..", "testdata", "golden_bundle.json")

// writeEnvelopeFromGolden 把金样本包裹里的纯信封写到临时文件，供 CLI 消费。
func writeEnvelopeFromGolden(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(goldenInput)
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Input envelope.File `json:"input"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "envelope.json")
	envRaw, err := json.MarshalIndent(bundle.Input, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, envRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// validate 子命令必须正确分类金样本三场景，并产出报告文件。
func TestCLIValidateGolden(t *testing.T) {
	input := writeEnvelopeFromGolden(t)
	out := filepath.Join(t.TempDir(), "report.json")
	runValidate([]string{"-input", input, "-out", out})

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report envelope.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.DeviceCount != 1 || len(report.BootReports["GOLDEN"]) != 3 {
		t.Fatalf("报告设备/启动数错误: %+v", report)
	}
	boots := report.BootReports["GOLDEN"]
	wantStatus := []meas.BootStatus{meas.BootUnsealed, meas.BootOK, meas.BootConflicted}
	for i, want := range wantStatus {
		if boots[i].Status != want {
			t.Fatalf("启动 %d 状态 got=%s want=%s", i+1, boots[i].Status, want)
		}
	}
	// 异常重置必须带可独立核验的短证据。
	fw := boots[2]
	if fw.FirstConflict == nil || fw.FirstConflict.Rule != meas.ConfFWReset {
		t.Fatalf("固件重置证据缺失: %+v", fw.FirstConflict)
	}
}

// evidence 子命令仅凭证据文件即可离线核验通过。
func TestCLIEvidenceSelfContained(t *testing.T) {
	input := writeEnvelopeFromGolden(t)
	out := filepath.Join(t.TempDir(), "report.json")
	runValidate([]string{"-input", input, "-out", out})
	raw, _ := os.ReadFile(out)
	var report envelope.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	ev := report.BootReports["GOLDEN"][2].FirstConflict
	evPath := filepath.Join(t.TempDir(), "evidence.json")
	evRaw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, evRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	// 捕获标准输出后核验。
	rescue := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runEvidence([]string{"-evidence", evPath})
	w.Close()
	os.Stdout = rescue
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if got := string(buf[:n]); !contains(got, "独立核验通过") {
		t.Fatalf("证据独立核验未通过，输出: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
