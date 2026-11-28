# 模拟度量启动的多摘要账离线校验器

题面见 `task/PROMPT.txt`。本仓库用 **Go 标准库**实现一个完全虚构的“启动度量账
（BootLedger）”离线校验器：按输入说明重放各摘要库（bank）的事件顺序，输出
**最后有效度量**、**最早不一致事件**与**无法证实的寄存器范围**，并为每个不一致点
生成可独立重算的短证据。所有数据均为合成虚构，不对应任何真实 TPM/PCR 实现。

## 1. 虚构模型

| 库 bank | 寄存器 | 位掩码 | 可否重置 |
|---|---|---|---|
| 固件 `FW` | `F0 F1` | `0x03` | **永不可重置**，重置指令命中即冲突 |
| 配置 `CFG` | `C0 C1` | `0x0c` | 可重置 |
| 应用 `APP` | `A0 A1` | `0x30` | 可重置 |

共 6 个寄存器，由 6 位掩码寻址（`F0=bit0 … A1=bit5`，全掩码 `0x3f`）。

- **扩展** `extend`：对掩码命中的每个寄存器计算 `new = H(old ‖ eventDigest)`。
- **重置** `reset`：只对掩码**明确声明**的可重置寄存器归零；命中固件位被拒绝
  （`FIRMWARE_RESET_REJECTED`），固件寄存器保持原值。
- **空库** `empty_bank`：显式声明某库本次启动无记录，是合法状态（`EMPTY_BANK`），
  与“缺记录”严格区分；向空库写记录判 `EMPTY_BANK_VIOLATED`。
- **换代算法**：摘要算法随模拟固件代切换，唯一规则在 `meas.GenAlgorithm`：
  `G1→SHA-256`、`G2→SHA-384`、`G3→SHA-512`，之后按 3 代循环。启动字段、签封字段
  与代映射三者不一致判 `ALG_GENERATION_MISMATCH`。

## 2. 两类失败严格区分

- **无法证实（UNPROVABLE，证据不足）**：参与启动的库整库缺记录（`BANK_NO_RECORD`），
  或启动内序号有缺口（`SEQ_GAP`）。这不是矛盾，只是链不完整。
- **确认冲突（CONFLICTED，前后矛盾）**：序号重复/倒退、证据声称值与重算值不符
  （`EVIDENCE_MISMATCH`）、固件重置、签封重算不符、签封断链、历史启动编号重复、
  固件代倒退、跨代引用、签封后迟到事件等。

崩溃导致“库已写完但总签封未落盘”时，重放本身干净，状态为 **UNSEALED**
（无法证实签封），而**不是**冲突。

## 3. 跨启动语义（编排器 `orchestrator`）

- **历史启动编号重复**：重复槽仍保留在报告中并判 `BOOT_NUMBER_REPEATED`，
  且不推进签封链——编号不可被正常事件吞掉、不可复用。
- **签封哈希链**：每枚签封经 `PrevSeal` 指向上一枚有效落盘签封，断链即冲突。
- **补记 backfill**：仅允许填补**同代、未落盘**启动的紧邻序号缺口；同序号冲突
  判 `BACKFILL_DUPLICATE`。成功补记后重放，链恢复完整。
- **恢复后迟到事件**：目标启动已正确落盘签封后到达的事件，判 `LATE_AFTER_SEALED`
  并拒绝，不能并入历史。
- **跨代引用**：记录携带的固件代与目标启动代不同即判 `CROSS_GENERATION_REF`
  ——同名编号在不同启动代不能重新引用。

## 4. 可独立验证的短证据

每条证据自包含：规范记录文本 `BLR1|seq|op|mask|refboot|digest|expected`、
记录哈希、算法、前后值与重算值。`meas.ParseCanonicalRecord` 是规范编码的逆过程，
因此仅凭证据文件即可离线重算，无需原始数据库：

```
validator evidence -evidence <evidence.json>
```

## 5. 确定性双写者（`store`）

两条物理写路径：库写者先把记录追加到各库日志（按物理次序），签封写者最后落盘
总签封。三个确定性崩溃/恢复场景均有测试：

| 场景 | 行为 | 结论 |
|---|---|---|
| `reset_first` | 重置记录物理上先于扩展落盘 | 恢复按逻辑序号归并，90 种交错全部合流 |
| `banks_written_seal_missing` | 库写完、总签封未落盘 | 状态 `UNSEALED`，非冲突 |
| `late_after_recovery` | 恢复重新签封后出现迟到事件 | 判 `LATE_AFTER_SEALED` 并拒绝 |

## 6. 小模型穷举（`smallmodel`）

3 个库各 2 条记录（含一次合法应用库重置），保持各库内部次序的全局交错数为
`6!/(2!·2!·2!) = 90`。引擎只信任逻辑序号，因此 90 种物理交错只产生 **1 种**
最终寄存器状态（合流性）；缺库、固件重置、证据篡改三类注入在全部交错下结论稳定。

## 7. 增量检查点缓存何时可复用

检查点是**内容寻址**的：键 = SHA-256(设备ID ‖ 各启动规范头 ‖ 各规范记录 ‖ 落盘签封)。

- **冷 miss**：首次处理某设备，整次重放并写回检查点。
- **warm hit（可复用）**：输入前缀**字节级不变**时命中，仅重放新增启动；尾段重放
  注入前缀的最后签封与最后固件代，保证签封链/换代核验与整次重放等价。
- **篡改 miss（不可复用）**：前缀内任一记录/头/签封改变，内容键立即变化，
  强制整次重放，杜绝脏复用。

## 8. 输入信封格式

```json
{
  "synthetic": true,
  "ledgers": [{
    "device_id": "DEV-0001",
    "boots": [{
      "device_id": "DEV-0001", "boot_number": 1, "generation": 1,
      "algorithm": "SHA256", "active_mask": "0x3f", "empty_banks": [],
      "records": [
        {"seq": 1, "op": "extend", "mask": "0x01", "digest": "<hex>"},
        {"seq": 2, "op": "reset",  "mask": "0x30"}
      ],
      "seal": {"prev_seal": "<hex>", "seal_value": "<hex>", "sealed": true, "...": "..."}
    }]
  }],
  "arrivals": {"DEV-0001": [
    {"record": {"seq": 2, "op": "extend", "mask": "0x04", "digest": "<hex>"},
     "ref_boot": 1, "generation": 1}
  ]}
}
```

## 9. 命令

```bash
# 离线校验整份信封，输出报告（最后有效度量/最早冲突/无法证实范围）
go run ./cmd/validator validate -input fixture.json -out report.json

# 独立核验单条短证据
go run ./cmd/validator evidence -evidence evidence.json

# 生成 N 条多设备合成序列，验证检查点一致性并量化补记/缓存
go run ./cmd/genbench -records 100000 -devices 8 -out out/ledger.json -report out/stats.json
```

## 10. 测试

```bash
go test ./...        # 单元 + 金样本 + CLI 集成
go vet ./...         # 静态检查
go test ./internal/golden -update   # 重新生成 testdata 金样本
```

- NIST FIPS 180 已知答案向量（SHA-256/384/512 的 `abc`、空串）固定算法字节行为。
- 金样本 `testdata/golden_bundle.json` 覆盖**补记、空库、异常重置**三场景及人工真值。

## 11. 十万条实测（`bench/stats_100k.json`）

`genbench -records 100000 -devices 8` 实测（确定性种子 20261128，耗时约 67s）：

- 实际记录 **100,000 条**，分布在 **8 个隔离设备、16,482 次启动**；
- 增量检查点 vs 整次重放一致性检查 **8 次全部通过，0 失败**（100% 一致）；
- 缓存：**冷 miss 8、warm hit 8、篡改 miss 8**，warm hit 复用 **16,482** 个启动；
- 补记：影响 **385 个启动代、385 条记录**，跨 **190 个不同固件代**；
  迟到/跨代等被拒 **741** 条；
- 启动状态：OK 11,977 / CONFLICTED 2,965 / UNPROVABLE 786 / UNSEALED 754；
- 8 类冲突规则各有 363–378 个样本，真值不符 **0**；
- 小模型 90 交错合流、重置先落盘、崩溃/冲突稳定性全部通过。

## 12. 目录结构

```
cmd/validator      离线校验与证据核验 CLI
cmd/genbench       十万条量化基准 CLI
internal/meas      类型、掩码、哈希原语、规范编码、报告结构
internal/replay    单启动纯函数重放引擎
internal/seal      换代/启动签封哈希链
internal/orchestrator 跨启动规则（编号重复、换代、补记/迟到/跨代）
internal/store     确定性双写者 + 内容寻址检查点缓存
internal/gen       确定性多设备合成生成器（带独立真值）
internal/smallmodel  各摘要库记录交错穷举
internal/golden    独立真值小样本（补记/空库/异常重置）
internal/testhelp  测试构造器
testdata/          金样本 JSON
bench/             十万条统计快照
```
