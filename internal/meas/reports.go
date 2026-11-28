package meas

// 冲突规则码：每条规则对应一种“前后不一致”，与 UnprovableReason 严格区分。
type ConflictCode string

const (
	ConfHeaderMaskInvalid ConflictCode = "HEADER_MASK_INVALID"     // 启动头掩码越界
	ConfAlgGeneration     ConflictCode = "ALG_GENERATION_MISMATCH" // 算法与固件代/签封不一致
	ConfSeqDuplicate      ConflictCode = "SEQ_DUPLICATE"           // 同启动序号重复
	ConfSeqOutOfOrder     ConflictCode = "SEQ_OUT_OF_ORDER"        // 序号倒退
	ConfMaskOutOfActive   ConflictCode = "MASK_OUT_OF_ACTIVE"      // 命中未参与本次启动的寄存器
	ConfBadDigestLen      ConflictCode = "BAD_DIGEST_LENGTH"       // 摘要长度不匹配换代算法
	ConfFWReset           ConflictCode = "FIRMWARE_RESET_REJECTED" // 固件库拒绝重置
	ConfEvidenceMismatch  ConflictCode = "EVIDENCE_MISMATCH"       // 声称值与重算值前后冲突
	ConfEmptyBankBadMask  ConflictCode = "EMPTY_BANK_BAD_MASK"     // 空库声明掩码非法
	ConfEmptyBankViolated ConflictCode = "EMPTY_BANK_VIOLATED"     // 空库却出现度量记录
	ConfUnknownOp         ConflictCode = "UNKNOWN_OP"              // 未知操作
	ConfMisroutedHistory  ConflictCode = "MISROUTED_HISTORY_REF"   // 历史引用未经过编排路由
	ConfSealMismatch      ConflictCode = "SEAL_MISMATCH"           // 总签封重算不符
	ConfSealChainBroken   ConflictCode = "SEAL_CHAIN_BROKEN"       // PrevSeal 与上一枚签封断链
	ConfBootNumberRepeat  ConflictCode = "BOOT_NUMBER_REPEATED"    // 历史启动编号重复
	ConfGenRegression     ConflictCode = "GENERATION_REGRESSION"   // 固件代倒退
	ConfCrossGenRef       ConflictCode = "CROSS_GENERATION_REF"    // 跨启动代引用同名编号
	ConfRefUnknownBoot    ConflictCode = "REF_UNKNOWN_BOOT"        // 引用不存在的历史启动
	ConfLateAfterSealed   ConflictCode = "LATE_AFTER_SEALED"       // 签封落盘后的迟到事件
	ConfBackfillDuplicate ConflictCode = "BACKFILL_DUPLICATE"      // 补记与既有序号冲突
)

// UnprovableReason 表示“证据不足”，而非“证据互相矛盾”。
type UnprovableReason string

const (
	UnprovBankMissing   UnprovableReason = "BANK_NO_RECORD"     // 参与启动的库整库缺记录
	UnprovSeqGap        UnprovableReason = "SEQ_GAP"            // 启动内序号缺口，链不完整
	UnprovUnsealedCrash UnprovableReason = "SEAL_NOT_PERSISTED" // 总签封未落盘（崩溃恢复）
)

// RegStatus 是寄存器重放状态。
type RegStatus string

const (
	RegExcluded   RegStatus = "EXCLUDED"   // 不参与本次启动
	RegEmpty      RegStatus = "EMPTY_BANK" // 所在库显式声明为空
	RegOK         RegStatus = "OK"         // 链完整且重算通过
	RegReset      RegStatus = "RESET"      // 最后一次有效操作是重置
	RegConflicted RegStatus = "CONFLICTED" // 存在前后冲突
	RegUnprovable RegStatus = "UNPROVABLE" // 缺记录/缺口，无法证实
)

// Evidence 是每个不一致点的可独立验证短证据。
// 字段自包含：仅凭证据内的规范记录文本、算法与前后值即可离线重算。
type Evidence struct {
	Rule            ConflictCode `json:"rule"`
	DeviceID        string       `json:"device_id"`
	BootNumber      int          `json:"boot_number"`
	Generation      int          `json:"generation"`
	Seq             int          `json:"seq"` // 头/签封级冲突为 0
	Regs            []string     `json:"regs,omitempty"`
	Algorithm       AlgID        `json:"algorithm"`
	RecordCanonical string       `json:"record_canonical,omitempty"`
	RecordHash      HexBytes     `json:"record_hash,omitempty"`
	Before          HexBytes     `json:"before,omitempty"`
	InputDigest     HexBytes     `json:"input_digest,omitempty"`
	Recomputed      HexBytes     `json:"recomputed,omitempty"`
	Claimed         HexBytes     `json:"claimed,omitempty"`
	HeaderCanonical string       `json:"header_canonical,omitempty"`
	PrevSeal        HexBytes     `json:"prev_seal,omitempty"`
	Note            string       `json:"note"`
}

// RegisterRange 描述一段无法证实的寄存器范围。
type RegisterRange struct {
	Bank     int              `json:"bank"`
	BankName string           `json:"bank_name"`
	Regs     []string         `json:"regs"`
	Reason   UnprovableReason `json:"reason"`
	FromSeq  int              `json:"from_seq"` // 0 表示整库缺失
}

// RegisterReport 是单个寄存器的重放结论。
type RegisterReport struct {
	Name              string    `json:"name"`
	Bank              int       `json:"bank"`
	Status            RegStatus `json:"status"`
	LastValid         HexBytes  `json:"last_valid"`     // 最后一个有效度量值（冲突时冻结）
	Final             HexBytes  `json:"final"`          // 重放最终值（可能与 LastValid 不同）
	LastEventSeq      int       `json:"last_event_seq"` // 最后作用于该寄存器的事件序号
	UnprovableFromSeq int       `json:"unprovable_from_seq"`
}

// BootStatus 是启动总体状态。
type BootStatus string

const (
	BootOK         BootStatus = "OK"         // 完整、签封一致
	BootUnsealed   BootStatus = "UNSEALED"   // 重放通过但总签封未落盘
	BootUnprovable BootStatus = "UNPROVABLE" // 有库无法证实（无前后冲突）
	BootConflicted BootStatus = "CONFLICTED" // 存在确认的前后冲突
)

// BootReport 是一次启动的离线重放报告。
type BootReport struct {
	DeviceID       string                  `json:"device_id"`
	BootNumber     int                     `json:"boot_number"`
	Generation     int                     `json:"generation"`
	Algorithm      AlgID                   `json:"algorithm"`
	ActiveMask     Mask                    `json:"active_mask"`
	Status         BootStatus              `json:"status"`
	Sealed         bool                    `json:"sealed"`
	SealOK         bool                    `json:"seal_ok"`
	RecomputedSeal HexBytes                `json:"recomputed_seal"`
	ClaimedSeal    HexBytes                `json:"claimed_seal"`
	Registers      [NumRegs]RegisterReport `json:"registers"`
	FirstConflict  *Evidence               `json:"first_conflict,omitempty"`
	Conflicts      []Evidence              `json:"conflicts"`
	Unprovable     []RegisterRange         `json:"unprovable_ranges"`
	EmptyBanks     []int                   `json:"empty_banks"`
	RecordCount    int                     `json:"record_count"`
	AppliedCount   int                     `json:"applied_count"`
}
