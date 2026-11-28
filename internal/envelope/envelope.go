// Package envelope 定义离线校验器的磁盘输入/输出信封。
package envelope

import (
	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
)

// File 是校验器顶层输入：多设备隔离的账与各自的到达流（补记/迟到）。
type File struct {
	Synthetic bool                              `json:"synthetic"`
	Ledgers   []meas.Ledger                     `json:"ledgers"`
	Arrivals  map[string][]orchestrator.Arrival `json:"arrivals,omitempty"`
}

// DeviceSummary 是单设备的精简结论。
type DeviceSummary struct {
	DeviceID          string         `json:"device_id"`
	BootCount         int            `json:"boot_count"`
	StatusCounts      map[string]int `json:"status_counts"`
	FirstConflict     *meas.Evidence `json:"first_conflict,omitempty"`
	BackfilledBoots   int            `json:"backfilled_boots"`
	BackfilledRecords int            `json:"backfilled_records"`
	LateRejected      int            `json:"late_rejected"`
}

// Report 是校验器的完整输出。
type Report struct {
	Synthetic       bool                         `json:"synthetic"`
	DeviceCount     int                          `json:"device_count"`
	Devices         []DeviceSummary              `json:"devices"`
	BootReports     map[string][]meas.BootReport `json:"boot_reports"`
	TotalConflicts  int                          `json:"total_conflicts"`
	TotalUnprovable int                          `json:"total_unprovable_boots"`
}
