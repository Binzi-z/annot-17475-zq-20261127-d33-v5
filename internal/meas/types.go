// Package meas 实现完全虚构的“启动度量账”（BootLedger）离线重放引擎。
//
// 模型与任何真实 TPM/PCR 实现无关，仅借鉴“摘要寄存器 + 扩展链”的概念：
//   - 每个合成设备有 3 个摘要库（bank）：固件库 FW、配置库 CFG、应用库 APP，
//     每个库固定 2 个寄存器，共 6 个寄存器，由 6 位掩码寻址；
//   - 固件库寄存器不可重置，配置/应用库寄存器可被明确声明的重置指令归零；
//   - 摘要算法随模拟固件换代（generation）切换，同一次启动内所有记录必须
//     与换代签封声明的算法一致；
//   - 每次启动由有序记录、库掩码与换代签封构成，引擎只做确定性重放，
//     不访问任何外部服务。
package meas

import "encoding/hex"

// AlgID 标识虚构固件代使用的摘要算法。
type AlgID string

const (
	AlgSHA256 AlgID = "SHA256"
	AlgSHA384 AlgID = "SHA384"
	AlgSHA512 AlgID = "SHA512"
)

// Size 返回算法摘要长度（字节）。
func (a AlgID) Size() int {
	switch a {
	case AlgSHA256:
		return 32
	case AlgSHA384:
		return 48
	case AlgSHA512:
		return 64
	default:
		return -1
	}
}

// GenAlgorithm 把模拟固件代映射到摘要算法：G1→SHA-256，G2→SHA-384，
// G3→SHA-512，之后按 3 代循环。该映射是整个系统唯一的换代规则来源。
func GenAlgorithm(generation int) AlgID {
	if generation < 1 {
		return ""
	}
	switch (generation - 1) % 3 {
	case 0:
		return AlgSHA256
	case 1:
		return AlgSHA384
	default:
		return AlgSHA512
	}
}

// 寄存器位掩码：bit0..bit5 对应 F0 F1 C0 C1 A0 A1。
const (
	MaskF0  uint8 = 1 << 0
	MaskF1  uint8 = 1 << 1
	MaskC0  uint8 = 1 << 2
	MaskC1  uint8 = 1 << 3
	MaskA0  uint8 = 1 << 4
	MaskA1  uint8 = 1 << 5
	MaskAll uint8 = MaskF0 | MaskF1 | MaskC0 | MaskC1 | MaskA0 | MaskA1
	MaskFW  uint8 = MaskF0 | MaskF1
	MaskCFG uint8 = MaskC0 | MaskC1
	MaskAPP uint8 = MaskA0 | MaskA1
)

// 库编号：寄存器按 (bank,index) 定位，库内固定两个寄存器。
const (
	BankFW     = 0
	BankCFG    = 1
	BankAPP    = 2
	NumBanks   = 3
	RegPerBank = 2
	NumRegs    = NumBanks * RegPerBank
)

// RegisterName 返回寄存器的虚构名称（F0..A1）。
func RegisterName(reg int) string {
	if reg < 0 || reg >= NumRegs {
		return "?"
	}
	return []string{"F0", "F1", "C0", "C1", "A0", "A1"}[reg]
}

// BankName 返回库名称。
func BankName(bank int) string {
	return []string{"FW", "CFG", "APP"}[bank]
}

// RegBank / RegIndex 把寄存器序号映射回库与库内序号。
func RegBank(reg int) int  { return reg / RegPerBank }
func RegIndex(reg int) int { return reg % RegPerBank }

// BankMask 返回某库对应的寄存器位掩码。
func BankMask(bank int) uint8 {
	switch bank {
	case BankFW:
		return MaskFW
	case BankCFG:
		return MaskCFG
	case BankAPP:
		return MaskAPP
	default:
		return 0
	}
}

// RegMask 返回单个寄存器的位掩码。
func RegMask(reg int) uint8 { return 1 << uint(reg) }

// Resettable 报告寄存器是否接受重置指令（固件库永不可重置）。
func Resettable(reg int) bool { return RegBank(reg) != BankFW }

// HexBytes 是带固定 JSON 十六进制编解码的字节切片，用于摘要与证据。
type HexBytes []byte

func (h HexBytes) MarshalJSON() ([]byte, error) {
	return []byte(`"` + hex.EncodeToString(h) + `"`), nil
}

func (h *HexBytes) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return &HexDecodeError{string(data)}
	}
	raw, err := hex.DecodeString(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*h = raw
	return nil
}

func (h HexBytes) String() string { return hex.EncodeToString(h) }

// HexDecodeError 表示非法的十六进制 JSON 字符串。
type HexDecodeError struct{ Raw string }

func (e *HexDecodeError) Error() string { return "meas: 非法十六进制串: " + e.Raw }

// Mask 是带 JSON 编解码的 6 位寄存器掩码（JSON 中以 0x.. 十六进制字符串表示，
// 同时兼容纯数字写法）。
type Mask uint8

func (m Mask) MarshalJSON() ([]byte, error) {
	return []byte(`"0x` + hex.EncodeToString([]byte{byte(m)}) + `"`), nil
}

func (m *Mask) UnmarshalJSON(data []byte) error {
	if len(data) >= 2 && data[0] == '"' {
		s := string(data[1 : len(data)-1])
		v, err := parseMaskString(s)
		if err != nil {
			return err
		}
		*m = Mask(v)
		return nil
	}
	var n uint8
	if _, err := parseInt(data, &n); err != nil {
		return err
	}
	*m = Mask(n)
	return nil
}

// Op 表示记录的操作类型。
type Op string

const (
	OpExtend    Op = "extend"     // 扩展：对掩码命中的每个寄存器做 H(旧值‖摘要)
	OpReset     Op = "reset"      // 重置：命中的可重置寄存器归零（命中固件位即冲突）
	OpEmptyBank Op = "empty_bank" // 空库声明：该库本次启动显式无任何记录
)

// Record 是一次启动内的一条有序度量记录。
type Record struct {
	// Seq 是启动内的逻辑序号，从 1 开始且预期连续；重复即冲突。
	Seq int `json:"seq"`
	// Op 为 extend / reset / empty_bank。
	Op Op `json:"op"`
	// Mask 为命中的寄存器位掩码（empty_bank 时必须恰好等于某个库掩码）。
	Mask Mask `json:"mask"`
	// Digest 为被度量内容的摘要（extend 必填，长度需匹配换代算法）。
	Digest HexBytes `json:"digest,omitempty"`
	// Expected 可选：记录方声称执行后命中寄存器应有的值。
	// 与重算值不一致时构成 EVIDENCE_MISMATCH 冲突，用于表达“前后冲突”，
	// 与“缺记录无法证实”严格区分。
	Expected HexBytes `json:"expected,omitempty"`
	// RefBoot 非 0 时表示这是一条历史补记/迟到记录，引用同设备的历史启动号。
	RefBoot int `json:"ref_boot,omitempty"`
}

// GenerationSeal 是换代/启动签封：把本次启动绑定到设备、启动号、固件代、
// 算法与上一枚签封，形成哈希链。
type GenerationSeal struct {
	DeviceID   string   `json:"device_id"`
	BootNumber int      `json:"boot_number"`
	Generation int      `json:"generation"`
	Algorithm  AlgID    `json:"algorithm"`
	ActiveMask Mask     `json:"active_mask"` // 本次启动参与度量的寄存器
	EmptyBanks []int    `json:"empty_banks"` // 显式声明为空的库编号
	PrevSeal   HexBytes `json:"prev_seal"`   // 上一枚签封值（首启为全零）
	SealValue  HexBytes `json:"seal_value"`  // 记录方落盘的签封值（重算不符即冲突）
	Sealed     bool     `json:"sealed"`      // 总签封是否已落盘（双写者崩溃时为 false）
}

// Boot 是一次启动的完整输入包。
type Boot struct {
	DeviceID   string         `json:"device_id"`
	BootNumber int            `json:"boot_number"`
	Generation int            `json:"generation"`
	Algorithm  AlgID          `json:"algorithm"`
	ActiveMask Mask           `json:"active_mask"`
	EmptyBanks []int          `json:"empty_banks,omitempty"`
	Records    []Record       `json:"records"`
	Seal       GenerationSeal `json:"seal"`
}

// Ledger 是一个合成设备的全部启动序列（按 BootNumber 升序）。
type Ledger struct {
	DeviceID string `json:"device_id"`
	Boots    []Boot `json:"boots"`
}

// InputFile 是离线校验器的顶层输入：多设备隔离的多个账。
type InputFile struct {
	Synthetic bool     `json:"synthetic"`
	Ledgers   []Ledger `json:"ledgers"`
}
