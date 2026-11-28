package store

import (
	"crypto/sha256"

	"example.invalid/local-fixture/internal/meas"
	"example.invalid/local-fixture/internal/orchestrator"
)

// 检查点是“内容寻址”的：键只取决于输入前缀的规范编码，与进程内存无关。
// 因此同一份输入前缀在任意时刻、任意进程里都命中同一检查点；
// 一旦前缀内任何记录/头/签封改变，键立即变化，缓存不可复用。

// Checkpoint 是某设备启动前缀的可复用重放快照。
type Checkpoint struct {
	Key       string
	PrefixLen int
	// TailSeal 是前缀最后一枚有效落盘签封，供后续启动续链。
	TailSeal []byte
	// TailGeneration 是前缀最后一个正常启动的固件代，供换代核验。
	TailGeneration int
	// 前缀的完整结论，合并尾段时原样保留。
	Report orchestrator.DeviceReport
}

// Cache 是按前缀键组织的检查点集合，并记录复用计数。
type Cache struct {
	points map[string]Checkpoint
	Hits   int
	Misses int
}

// NewCache 构造空检查点缓存。
func NewCache() *Cache { return &Cache{points: map[string]Checkpoint{}} }

// Len 返回已缓存检查点数量。
func (c *Cache) Len() int { return len(c.points) }

// PrefixKey 计算设备前 n 次启动的内容键（SHA-256，十六进制）。
// 键吸收每次启动的规范头、每条规范记录与落盘签封，任一字节变化都会换键。
func PrefixKey(l meas.Ledger, n int) string {
	h := sha256.New()
	h.Write([]byte("BLC1")) // BootLedger Checkpoint v1
	h.Write([]byte(l.DeviceID))
	for i := 0; i < n && i < len(l.Boots); i++ {
		b := l.Boots[i]
		h.Write(meas.CanonicalHeader(b))
		for _, r := range b.Records {
			h.Write(meas.CanonicalRecord(r))
		}
		h.Write(b.Seal.SealValue)
	}
	sum := h.Sum(nil)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(sum)*2)
	for i, v := range sum {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}

// IncrementalResult 是增量重放的结果与复用说明。
type IncrementalResult struct {
	Report       orchestrator.DeviceReport
	ReusedPrefix int // 复用检查点覆盖的启动数（0 表示全量重放）
	CacheKey     string
	Reused       bool
}

// IncrementalProcess 尽量从检查点续算：找到最长的已缓存前缀后，
// 仅重放新增启动；找不到则整次重放并写回检查点。
func (c *Cache) IncrementalProcess(l meas.Ledger, arrivals []orchestrator.Arrival) IncrementalResult {
	// 从最长前缀向下找可复用检查点。
	best := -1
	var cp Checkpoint
	for n := len(l.Boots); n >= 1; n-- {
		key := PrefixKey(l, n)
		if got, ok := c.points[key]; ok {
			best, cp = n, got
			break
		}
	}
	key := PrefixKey(l, len(l.Boots))
	if best < 0 {
		c.Misses++
		full := orchestrator.Process(l, arrivals)
		c.points[key] = Checkpoint{
			Key: key, PrefixLen: len(l.Boots), Report: full,
			TailSeal: tailSeal(full.Boots), TailGeneration: lastGeneration(l.Boots),
		}
		return IncrementalResult{Report: full, ReusedPrefix: 0, CacheKey: key}
	}

	c.Hits++
	// 尾段：只重放未覆盖的新启动，到达流过滤为目标启动号落在尾段的项。
	tailLedger := meas.Ledger{DeviceID: l.DeviceID, Boots: append([]meas.Boot(nil), l.Boots[best:]...)}
	minBoot := 0
	if len(tailLedger.Boots) > 0 {
		minBoot = tailLedger.Boots[0].BootNumber
	}
	var tailArrivals []orchestrator.Arrival
	for _, ar := range arrivals {
		if ar.RefBoot == 0 || ar.RefBoot >= minBoot {
			tailArrivals = append(tailArrivals, ar)
		}
	}
	// 注入前缀签封与代，保证尾段的签封链/换代核验与整次重放等价。
	tail := orchestrator.ProcessWithSeal(tailLedger, tailArrivals, cp.TailSeal, cp.TailGeneration)

	merged := orchestrator.DeviceReport{
		DeviceID:           l.DeviceID,
		BootCount:          len(cp.Report.Boots) + len(tail.Boots),
		Boots:              append(append([]meas.BootReport(nil), cp.Report.Boots...), tail.Boots...),
		CrossBootConflicts: append(append([]meas.Evidence(nil), cp.Report.CrossBootConflicts...), tail.CrossBootConflicts...),
		BackfilledBoots:    cp.Report.BackfilledBoots + tail.BackfilledBoots,
		BackfilledRecords:  cp.Report.BackfilledRecords + tail.BackfilledRecords,
		LateRejected:       cp.Report.LateRejected + tail.LateRejected,
	}
	merged.FirstConflict = pickEarliest(merged.Boots)
	c.points[key] = Checkpoint{
		Key: key, PrefixLen: len(l.Boots), Report: merged,
		TailSeal: tailSeal(merged.Boots), TailGeneration: lastGeneration(l.Boots),
	}
	return IncrementalResult{Report: merged, ReusedPrefix: best, CacheKey: key, Reused: true}
}

// VerifyEquivalence 对同一输入分别整次重放与增量重放，逐字段比对结论。
// 任何不一致都返回描述首个差异的错误，供十万条规模的一致性断言使用。
func (c *Cache) VerifyEquivalence(l meas.Ledger, arrivals []orchestrator.Arrival) error {
	full := orchestrator.Process(l, arrivals)
	inc := c.IncrementalProcess(l, arrivals)
	return compareReports(full, inc.Report)
}

func tailSeal(reports []meas.BootReport) []byte {
	for i := len(reports) - 1; i >= 0; i-- {
		if reports[i].Sealed && reports[i].SealOK {
			return append([]byte(nil), reports[i].ClaimedSeal...)
		}
	}
	return nil
}

// lastGeneration 取输入账最后一个启动的固件代（前缀代上下文）。
func lastGeneration(boots []meas.Boot) int {
	if len(boots) == 0 {
		return 0
	}
	return boots[len(boots)-1].Generation
}

func pickEarliest(reports []meas.BootReport) *meas.Evidence {
	var best *meas.Evidence
	for i := range reports {
		fc := reports[i].FirstConflict
		if fc == nil {
			continue
		}
		if best == nil || fc.BootNumber < best.BootNumber ||
			(fc.BootNumber == best.BootNumber && fc.Seq < best.Seq) {
			cp := *fc
			best = &cp
		}
	}
	return best
}
