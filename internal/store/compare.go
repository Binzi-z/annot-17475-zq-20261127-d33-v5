package store

import (
	"fmt"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
)

// compareReports 逐启动、逐寄存器比对整次重放与增量重放的结论。
// 只比较“结论性”字段，避免缓存拼接造成的切片容量差异干扰。
func compareReports(want, got orchestrator.DeviceReport) error {
	if want.DeviceID != got.DeviceID {
		return fmt.Errorf("device_id 不一致: %q != %q", want.DeviceID, got.DeviceID)
	}
	if want.BootCount != got.BootCount || len(want.Boots) != len(got.Boots) {
		return fmt.Errorf("启动数不一致: want=%d got=%d", want.BootCount, got.BootCount)
	}
	for i := range want.Boots {
		if err := compareBoot(want.Boots[i], got.Boots[i]); err != nil {
			return fmt.Errorf("启动#%d: %w", i, err)
		}
	}
	if len(want.CrossBootConflicts) != len(got.CrossBootConflicts) {
		return fmt.Errorf("跨启动冲突数不一致: want=%d got=%d",
			len(want.CrossBootConflicts), len(got.CrossBootConflicts))
	}
	if want.BackfilledRecords != got.BackfilledRecords ||
		want.BackfilledBoots != got.BackfilledBoots ||
		want.LateRejected != got.LateRejected {
		return fmt.Errorf("补记/迟到计数不一致: want=(%d/%d/%d) got=(%d/%d/%d)",
			want.BackfilledBoots, want.BackfilledRecords, want.LateRejected,
			got.BackfilledBoots, got.BackfilledRecords, got.LateRejected)
	}
	return nil
}

func compareBoot(want, got meas.BootReport) error {
	if want.BootNumber != got.BootNumber {
		return fmt.Errorf("boot_number %d != %d", want.BootNumber, got.BootNumber)
	}
	if want.Status != got.Status {
		return fmt.Errorf("boot %d 状态不一致: want=%s got=%s",
			want.BootNumber, want.Status, got.Status)
	}
	if want.SealOK != got.SealOK || want.Sealed != got.Sealed {
		return fmt.Errorf("boot %d 签封结论不一致: sealOK=%v/%v sealed=%v/%v",
			want.BootNumber, want.SealOK, got.SealOK, want.Sealed, got.Sealed)
	}
	if !hexEqual(want.RecomputedSeal, got.RecomputedSeal) {
		return fmt.Errorf("boot %d 重算签封不一致", want.BootNumber)
	}
	if len(want.Conflicts) != len(got.Conflicts) {
		return fmt.Errorf("boot %d 冲突条数不一致: want=%d got=%d",
			want.BootNumber, len(want.Conflicts), len(got.Conflicts))
	}
	for i := range want.Conflicts {
		if want.Conflicts[i].Rule != got.Conflicts[i].Rule ||
			want.Conflicts[i].Seq != got.Conflicts[i].Seq {
			return fmt.Errorf("boot %d 第 %d 条冲突不一致: %s/%d vs %s/%d",
				want.BootNumber, i, want.Conflicts[i].Rule, want.Conflicts[i].Seq,
				got.Conflicts[i].Rule, got.Conflicts[i].Seq)
		}
	}
	if len(want.Unprovable) != len(got.Unprovable) {
		return fmt.Errorf("boot %d 无法证实范围数不一致", want.BootNumber)
	}
	for reg := range want.Registers {
		w, g := want.Registers[reg], got.Registers[reg]
		if w.Status != g.Status || w.LastEventSeq != g.LastEventSeq ||
			w.UnprovableFromSeq != g.UnprovableFromSeq || !hexEqual(w.LastValid, g.LastValid) {
			return fmt.Errorf("boot %d 寄存器 %s 结论不一致: %s/%x vs %s/%x",
				want.BootNumber, w.Name, w.Status, w.LastValid, g.Status, g.LastValid)
		}
	}
	return nil
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
