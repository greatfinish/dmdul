# DM8 普通行页格式与 DMDUL 解析边界

本文记录 DMDUL 对 DM8 普通行存储页的当前认识、实现映射和证据等级。内容来自：

- 达梦官方公开文档；
- 多个离线 `SYSTEM.DBF` 和用户表空间 DBF 样本；
- `oldpro` 受控测试表；
- 2026-07-15 在 DM8 `03134284336-20250117-257733-20132` 上完成的事务、页校验和行增长差分实验；
- DMDUL 自动化测试。

达梦官方没有公开完整磁盘页格式，因此本文不会把单一样本或 AI 推断写成产品级规范。

## 证据等级

| 等级 | 含义 |
| --- | --- |
| 官方 | 达梦官方文档明确说明 |
| 已验证 | 多个样本或受控差分实验一致，且已进入解析器和测试 |
| 样本观察 | 当前样本稳定，但尚未覆盖足够版本、页大小或表组织 |
| 待验证 | 只有候选解释，不能据此过滤或恢复数据 |

## 官方边界

达梦官方 `dminit` 文档确认：

- `PAGE_SIZE` 支持 4、8、16、32 KB，默认 8 KB；
- `EXTENT_SIZE` 支持 16、32、64 页；
- `PAGE_CHECK` 支持 0、1、2、3，非 0 时会把数据页校验值写入页头。

参考：[dminit 参数详解](https://eco.dameng.com/document/dm/zh-cn/pm/dminit-parameters.html)。

官方文档没有公开本文下面的页内偏移、行头位分配和事务尾布局。这些内容均属于离线样本逆向结果。

## 页面头

普通行页当前使用以下布局：

| 偏移 | 长度 | 字节序 | DMDUL 名称 | 证据 | 说明 |
| --- | ---: | --- | --- | --- | --- |
| `0x00` | 2 | LE | `group_id` | 已验证 | 表空间/group 编号 |
| `0x02` | 2 | LE | `file_id` | 已验证 | group 内文件号，不只是 hint |
| `0x04` | 4 | LE | `page_no` | 已验证 | 文件内页号 |
| `0x08` | 6 | LE | `prev_ref` | 已验证 | `u16 file_id + u32 page_no` |
| `0x0E` | 6 | LE | `next_ref` | 已验证 | `u16 file_id + u32 page_no` |
| `0x14` | 4 | LE | `page_kind` | 已验证 | 页面角色 |
| `0x18` | 4 | LE | `page_checksum` | 已验证 | `PAGE_CHECK=1/3` 的 CRC32/CRC32C；模式 0 和 SHA256 样本为 0 |
| `0x1C` | 4 | 未定 | 未命名 | 待验证 | SCN/修改序号仅为候选解释 |
| `0x24` | 2 | LE | `n_slot` | 已验证 | slot 条目数，不等于活动记录数 |
| `0x26` | 2 | LE | `free_end` | 已验证 | 行区结束/空闲边界 |
| `0x2C` | 2 | LE | `n_rec` | 已验证 | 目录记录计数；删除提交后可暂时滞后，不是严格可见行数 |
| `0x2E` | 2 | LE | `free_row_head` | 已验证 | 可复用删除行链头；`0xFFFF` 表示当前无链头 |
| `0x38` | 2 | LE | `tree_level` | 已验证 | leaf 普遍为 0，internal/root 样本为非 0 |
| `0x3A` | 4 | LE | `storage_id` | 已验证 | 页面所属 storage/assist 对象 |

页面绝对身份使用 `(group_id, file_id, page_no)`。前后页引用只有 file/page，group 继承当前 storage。
六字节全 `FF` 表示空引用。

当前已识别的 `page_kind`：

| 值 | 当前解释 |
| ---: | --- |
| `0x14` | BTree leaf / 普通行数据候选页 |
| `0x15` | BTree root/internal 页 |
| `0x16` | 行 overflow 候选页 |
| `0x20` | LOB 数据页 |
| `0x22` | Long Row 数据页 |

仅凭 `page_kind=0x14` 不能判定页面属于某张表，还必须校验页身份、`storage_id`、page plan、
slot 结构和目标表行格式。

## Slot 目录

普通页至少存在三类已经观察到的尾部布局：

```text
fixed_slot_start      = page_size - 8 - n_slot * 2
hash_slot_start       = page_size - hash_size - 8 - n_slot * 2
protected_slot_start  = page_size - protection_tail_size - n_slot * 2
slot[i]                = little-endian u16 row_offset

protection_tail_size  = (page_size / 4096 - 1) * 4 + 12
```

重要边界：

- slot 大小为 2 字节；
- `PAGE_CHECK=2` 的 HASH 摘要位于 slot 之后，不能按固定 8 字节尾部读取；
- 32 KiB 保护页的 `protection_tail_size=40`，其中包含七组边界原字节、4 字节保护字段和
  8 字节固定尾；按固定 8 字节计算会让整个 slot 目录错移 32 字节；
- slot 顺序与行的物理偏移顺序不一定一致；
- `n_slot` 可能包含空闲、控制或无效条目，不能等同于活动行数；
- `n_rec` 通常小于等于 `n_slot`；
- DELETE 提交后，slot 可能暂时仍指向带删除标志的旧行；后续 DML 才重排 slot 并把旧行挂到 `0x2E`；
- DMDUL 会综合 `n_slot/n_rec/free_end`、row offset、两字节行长/状态和可解码行数给候选
  布局评分，再把胜出的 slot 视为可解析行入口；
- “可解析物理行”不等于“事务可见活动行”。

`oldpro` 样本：

| 文件/页 | kind | n_slot | n_rec | 有效行偏移 |
| --- | ---: | ---: | ---: | --- |
| `MAIN.DBF` page 16 | `0x14` | 6 | 4 | `0x62/0xA1/0xE0/0x11F` |
| `MAIN.DBF` page 176 | `0x14` | 4 | 2 | `0x62/0x7C` |

普通 `0x14` 样本常从 `0x62` 开始放行，但这不是所有 DM 页面类型的通用常量。DMDUL 只把它用于
当前普通行页兼容路径，不把它用于 BTree internal、LOB 或 Long Row 页。

## 行记录

当前普通行记录按以下顺序解析：

```text
u16 big-endian physical_row_length_and_delete_flag
2-bit-per-column metadata
fixed-width columns in storage order
variable-width columns in storage order
19-byte transaction/MVCC/Undo control tail
```

### 两字节行长

`row[0:2]` 是大端行长/状态字：

```text
physical_length = u16be(row[0:2]) & 0x7FFF
deleted         = u16be(row[0:2]) & 0x8000 != 0
```

必须特别避免以下错误解释：

```text
00 2C  -> 长度 44
00 2E  -> 长度 46
```

它们不是“正常行 0x2C”和“删除行 0x2E”状态。

2026-07-15 对 `oldpro/MAIN.DBF` 的复核证据：

- page 976：`page_kind=0x14`、`storage_id=33555550`；
- 字典映射：`JYC.T_ROW_SNAP100`；
- `n_slot=53`、`n_rec=51`；
- 51 条 slot 引用记录的行头为 `00 2E 00`；
- 表字段为 `INT + ROWID + BIGINT`，其行布局可解释为：

```text
2 字节行长 + 1 字节 metadata + 4 + 12 + 8 字节列值 + 19 字节尾部 = 46 字节
```

因此，把 `0x2E` 当删除标志会把整页活动记录全部误删，解析器禁止采用该规则。

2026-07-15 DELETE 差分进一步确认：一条 105 字节行在删除前为 `00 69`，DELETE 落盘后为
`80 69`，Rollback 后恢复为 `00 69`，Commit 后保持 `80 69`。删除位是 16 位状态字的最高位，
`0x2E` 本身仍可能只是合法长度 46。

### 2-bit 列 metadata

metadata 长度为：

```text
ceil(storage_column_count / 4)
```

当前状态解释：

| 2-bit 值 | 当前处理 | 证据 |
| --- | --- | --- |
| `00` | 行内值 | 已验证 |
| `01` | special/out-of-line；先尝试行内 envelope，再尝试 21 字节 locator | 已验证 |
| `10` | 拒绝并报告 `unsupported row metadata state 10` | 未观察到，语义待验证 |
| `11` | NULL | 已验证 |

列状态按物理 storage order 解释，而不是简单按 DDL 展示顺序；当前实现先排定长列，再排变长列。

状态 `10` 的定向验证覆盖普通 NULL/空值、固定列、LOB、`STORAGE(USING LONG ROW)`、
`ALTER TABLE ADD DEFAULT`、`ALTER TABLE DROP COLUMN`、更新行和 `oldpro` 真实活动行。DROP COLUMN
实验同时检查了删除列前的旧物理页和删除列后的当前 root/leaf 页，两者仍只出现 `00`。
`oldpro` 扫描统计为 `00=18252`、
`11=26`、`01=0`、`10=0`；Long Row 样本稳定产生 `01`，仍未产生 `10`。因此当前不能给
`10` 编造“行内值”语义，也禁止再进入启发式偏移解析。

### 变长值

当前已验证编码：

| marker | 解释 |
| --- | --- |
| `0x80` | 空值，payload 长度 0 |
| `0x81..0xFE` | 短值，长度为 `marker - 0x80` |
| `< 0x80` | marker 起始的两字节大端长度，随后为 payload |

NULL 由列 metadata 表达，不能与长度为 0 的空字符串/空二进制混淆。

### 行尾控制区

普通 CLUSTERBTR 行的 19 字节尾部已通过在线 `V$TRX` 和 `ROLL.DBF` 指针闭环：

| 相对偏移 | 长度 | 字节序 | 含义 |
| ---: | ---: | --- | --- |
| `0` | 6 | LE | `clu_rowid`，48 位聚簇行号 |
| `6` | 1 | - | `roll_file` |
| `7` | 4 | LE | `roll_page` |
| `11` | 2 | LE | `roll_offset` |
| `13` | 6 | LE | `trx_id`，48 位事务号 |

未挂 Undo 时使用 `file=0xFF/page=0x7FFFFFFF/offset=0xFFFF`。一个未提交 DELETE 样本的
尾部为：

```text
08 00 00 00 00 00 | 00 | 10 04 00 00 | 6A 00 | DB 12 02 00 00 00
clu_rowid=8            file=0 page=1040   off=106  trx_id=135899
```

同一时刻在线 `V$TRX.ID=135899`，且指针能准确读取 `ROLL.DBF` page 1040 offset 106 的 Undo
记录。DMDUL 已能解码这 19 字节结构，但尚未离线解码事务表状态和完整 PRE IMAGE 链，因此不能仅凭
尾部判断事务最终提交/回滚，也不会宣称恢复结果具有在线数据库的一致性读语义。

这也意味着 slot-only 不是 committed-only：未提交 INSERT 可能已经占用活动 slot；未提交 DELETE
可能已经设置删除位。当前普通卸载会按落盘 slot/删除位处理，不能在缺少离线事务状态时模拟数据库
一致性读。

## 页保护与页校验

文章只讨论普通行和 slot 还不够。DM 页面可能在 4 KiB 扇区边界使用保护字节，并把原始字节保存
到页尾。DMDUL 先证明页面采用保护尾布局，再恢复跨边界的活动行字节；固定尾和 HASH 尾页面
不会进入这条路径。详细规则见
[SYSTEM.DBF 离线扫描笔记](offline-system-scan.md#page-protection-bytes)。

四个独立实例已确认 `PAGE_CHECK=0/1/2/3` 的字段和算法，详见
[DM8 PAGE_CHECK 页校验实验](page-check.md)。DMDUL 已有 CRC32、CRC32C 和标准 HASH 的页校验器。

## 当前解析路径

普通表正常卸载：

```text
storage root -> internal refs -> leaf chain -> page plan
page plan 完整：ReadAt 计划页
计划失败：扫描同 group 并匹配 storage_id
仍未定位：读取字典段范围
recover table：才允许全文件残留页扫描
```

进入行解析后：

1. 校验 `(group,file,page)`、`page_kind`、`storage_id`；
2. 校验 `n_slot/n_rec/free_end/tree_level`；
3. 从页尾 slot 目录取得行偏移；
4. 按大端两字节状态字取低 15 位行长，普通模式跳过最高位为 1 的删除行；
5. 优先使用显式 2-bit metadata 解码；
6. 普通 `unload` 只读取 slot 指向的行；同 group `storage_id` 或段范围回退也不扫描页内空洞；
7. 只有 `recover table` 才额外遍历 `0x62..free_end` 物理行区，包含无 slot 和删除残留；
8. metadata `10` 直接停止该行，不允许启发式兜底；其他旧格式失败仍保留受限兼容解析；
9. 21 字节 locator 从当前行出发读取 `0x20` LOB 或 `0x22` Long Row 页链。

## 仍需完善的解析能力

### 已完成：slot/free/delete 状态差分实验

同一页已保存并比较以下阶段：

```text
INSERT + CHECKPOINT
DELETE 未提交 + CHECKPOINT
ROLLBACK（未 Checkpoint 时磁盘页暂未恢复）
ROLLBACK + CHECKPOINT
DELETE COMMIT
DELETE COMMIT + CHECKPOINT
后续 INSERT 复用空间
```

结论：DELETE 设置行长状态字高位；Rollback 后经 Checkpoint 清除；Commit 后经 Checkpoint 仍保留。
删除提交后 slot 和 `n_rec` 可暂时不变，后续 INSERT 会重排 slot，并令 `0x2E` 指向已删除物理行；
再后续 INSERT 可在该偏移覆盖旧行。

### 已完成：19 字节事务尾结构

字段边界、Undo 文件/页/偏移和 48 位事务号已经确认。剩余工作是离线事务表状态和 PRE IMAGE 链，
在此完成前恢复仍只能保证“物理可解析”，不能保证“事务可见”。

### 已完成：普通卸载与物理残留扫描分流

正常 `unload` 已收紧为 slot-only。page plan 失败时仍允许按同 group `storage_id` 或段范围定位页面，
但页内不会遍历无 slot 物理行。空洞、删除行和旧版本物理扫描只在显式 `recover table` 中启用。

### P1：metadata 状态 `10`

已覆盖 Long Row、LOB、特殊 NULL、ADD DEFAULT、DROP COLUMN 和更新场景，但当前 DM8 构建仍未观察到 `10`。解析器
保守拒绝该状态；只有取得可重复物理样本后才会增加语义。

### 已完成：页面校验字段和算法

模式 0、CRC32、SHA256 和 CRC32C 的实机页均已按独立实现重算成功。损坏页如何在普通/救援模式
中分级处理仍需接入导出诊断，校验公式本身已经确认。

### P2：迁移行和链式行

8K 页普通表把单行增长到 6500 字节时报 `[-2665]`。在近满 leaf 中把 100 字节 VARCHAR 更新到
3000 字节时，DM 执行 BTree leaf split：旧 page 161 拆出 page 163，活动 slot 仍指向完整 3039 字节
行，没有 forwarding/fragment locator。现有实验仍未证明普通 BTREE 表使用 Oracle 式跨页 chained
row；除 LOB/Long Row 外，不根据相邻页自动拼接普通行。

## 自动化测试

当前测试已覆盖：

- `00 2C`、`00 2E` 按 44/46 字节物理行长解析；
- `80 69` 按“删除标志 + 105 字节长度”解析；
- 4K、8K、16K、32K page size 下按固定尾、HASH 尾或保护尾公式读取 slot；
- 32K 保护页识别 40 字节保留尾，并恢复跨 4 KiB 边界的行字节；
- `n_slot > n_rec` 且存在无效 slot 的 heap/row page；
- 正常模式 slot-only、恢复模式包含删除 slot 和无 slot 物理空洞；
- 显式 2-bit NULL metadata；
- metadata `10` 不进入启发式解析；
- 19 字节 `clu_rowid/rollback address/trx_id` 解码；
- PAGE_CHECK CRC32、SHA256、CRC32C 重算与单字节损坏检测；
- 21 字节 LOB/Long Row locator；
- page plan、断链和分层 fallback。

真实多版本和多页大小样本仍应继续加入测试资产，合成页测试不能替代实机差分证据。
