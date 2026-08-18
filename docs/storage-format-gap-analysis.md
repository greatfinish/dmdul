# DM8 存储格式阶段性总结对照笔记

本文根据 `DM8_STORAGE_FORMAT_SUMMARY_2026-07-03_CN.md` 对当前 DMDUL 实现做差距分析，用于安排后续解析增强。

> **2026-07-13 状态更新**：本文第 1～4 节保留的是当时的差距记录，不再代表当前
> 实现。标准两阶段 bootstrap、按页流式扫描、显式 2-bit NULL metadata、常规标量
> 类型、21 字节 LOB locator、`0x20` LOB 页链和 `0x22` Long Row 页链均已落地。
> 当前类型矩阵和实机结果以 [DM8 数据类型支持矩阵](data-types.md) 为准；仍未完成的
> 主要方向是 HUGE 表、自定义/集合类型和无 SYSTEM 字典的独立 storage scan。
>
> **2026-07-15 行页复核**：页头、slot、行长、2-bit metadata 的当前证据和实现边界，
> 统一记录在 [DM8 普通行页格式与 DMDUL 解析边界](row-page-format.md)。特别注意：
> `00 2C`、`00 2E` 是两字节大端行长样本，不是正常/删除状态标志；真正删除位是
> 大端状态字的 `0x8000`。PAGE_CHECK 四模式见 [页校验实验](page-check.md)。

## 已经基本吻合的部分

### 页头与 page plan

当前实现已经使用以下页头信息：

| 信息 | 当前代码 |
| --- | --- |
| `group_id/file_id/page_no` | `pageHeaderMatchesRef` 校验页头 `0x00/0x02/0x04` |
| `page_kind` | `dmPageKindOff = 0x14` |
| BTREE leaf/data 页 | `page_kind = 0x14` |
| BTREE root/internal 页 | `page_kind = 0x15` |
| storage/assist id | `dataPageAssistIndexOff = 0x3A` |
| leaf next 链 | `dmPageNextRefOff = 0x0E` |
| internal 左孩子 | `dmBTreeLeftmostChildOff = 0x52` |

普通表数据导出已经优先使用 `storage root -> internal -> leaf chain`。这与总结中的主路径一致。当前还补充了 internal 左孩子不可用时沿 internal next 链继续下降的逻辑。

### 同名表与 owner 区分

当前 `tableNameMatcher`、`ownerMatcher`、`DictionaryTable` 均保留 owner 信息。数据导出时也已经修复过同名表不同 owner 的误匹配问题，核心方向正确。

### DROP / TRUNCATE 恢复

当前已有 `recover table` 和 `-recover`，会在显式恢复模式下按 storage/assist id 扫描残留页。这个边界与总结一致：默认导出尊重当前字典入口，恢复扫描必须显式执行。

### 行内短 LOB

当前 `unwrapInlineLOBPayload` 已支持 13 字节 envelope 的短内联 LOB，适合小 `CLOB/TEXT/BLOB` 初步恢复。

## 需要优先增强的部分

### 1. Bootstrap 不应长期依赖全 SYSTEM.DBF 扫描

当前 `LoadDictionary` 仍会对整个 `SYSTEM.DBF` 调用 `scanDictionaryObjects`、`iterDictionaryRows` 等全文件扫描函数。

建议后续改为：

```text
SYSTEM.DBF page 0 + 0x80 -> SYSOBJECTS root
SYSTEM.DBF page 0 + 0x7c -> SYSINDEXES root
下载 SYSOBJECTS
从 SYSOBJECTS/SYSINDEXES 定位 SYSCOLUMNS/SYSTEXTS/SYSHPARTTABLEINFO
每张字典表复用标准 page plan 下载
```

全文件 marker 扫描保留为 fallback，并在 `dul.log` 中明确记录 fallback 原因。

### 2. 行解析缺少 NULL metadata 的严格 2-bit 模型

总结中记录普通行 metadata 是 storage-order 每列 2 bit：

```text
00 = 非 NULL
01 = out-of-line locator
11 = NULL
```

当前实现仍主要通过尝试不同 `start` 偏移、固定列 sentinel、变长字段读取失败后推断尾部 NULL。它能覆盖不少样例，但对于字段多、固定可空列多、Long Row locator、多版本残留页，误判风险较高。

建议新增显式 metadata 解码：

- 按列数计算 `ceil(column_count / 4)` metadata 字节数；
- 以 storage order 判断每列状态；
- fixed NULL 仍消耗固定宽度；
- variable NULL 不读取 length/payload；
- `01` 状态进入 LOB/Long Row locator 路径。

### 3. 标量类型支持不足

总结中已验证的类型远多于当前 `fixedDataSize` / `parseFixedDataValue` 支持范围。

当前明显缺口：

| 类型 | 总结中的编码 | 当前风险 |
| --- | --- | --- |
| `REAL` | 4 字节 IEEE754 | 当前不支持 |
| `FLOAT` | length=4 时 4 字节，否则 8 字节 | 当前不支持 |
| `DOUBLE` | 8 字节 IEEE754 | 当前不支持 |
| `TIME` | 5 字节 packed time | 当前按 8 字节 datetime 解析，需重新验证并修正 |
| `TIME WITH TIME ZONE` | 7 字节 | 当前不支持 |
| `DATETIME/TIMESTAMP WITH TIME ZONE` | 10 字节 | 当前不支持 |
| `TIMESTAMP WITH LOCAL TIME ZONE` | 8 字节 | DDL scale 需要特殊处理 |
| `INTERVAL DAY TO SECOND` | 24 字节 | 当前不支持 |
| `ROWID` | 12 字节 | 当前不支持 |

DDL 生成也需要同步收敛：时间精度建议限制在 `1..6`，带时区类型要生成达梦可接受的语法。

### 4. 行外 LOB 与 Long Row 还不是完整恢复

当前只支持短内联 LOB。总结中已明确：

- out-of-line LOB locator 为 21 字节；
- LOB 数据页 `page_kind=0x20`；
- `STORAGE(USING LONG ROW)` 使用类似 locator，但 payload 页为 `page_kind=0x22`；
- `0x22` 页可能一页存多个 payload record。

建议拆成三步实现：

1. 解析 21 字节 locator，区分 `0x20` LOB 和 `0x22` Long Row；
2. 从当前活动行 locator 出发追页链，禁止全文件扫描所有 LOB 页；
3. SQL/CSV 输出中先写外置文件引用，后续再实现 row archive。

### 5. HUGE TABLE 已落地受限恢复路径

总结中 HUGE 表特别重要：

- 主表 storage 可能是 `ROOTFILE=-1/ROOTPAGE=-1`；
- `QUERY LOW` 某些场景可用 `$RAUX` 代理恢复；
- `QUERY HIGH` 会进入 `HMAIN/SCH.../TAB.../COL*.dta` 列存文件和压缩 section；
- 不能把 `$RAUX` 部分行误认为完整恢复。

当前 DMDUL 已完成第一阶段专门处理：

- bootstrap 识别 HUGE 主表及 `$AUX/$RAUX/$DAUX/$UAUX`，并把 SECTION、FILESIZE、
  WITH DELTA 和辅助表 ID 写入磁盘字典；
- DDL 使用 `CREATE HUGE TABLE`，恢复已经验证的表级存储参数；
- unload 从 `$AUX` 定位 HFS 列 section，并合并 `$RAUX` 尾部行、`$DAUX` 删除范围和
  `$UAUX` 更新值，而不是把 RAUX 当成完整表；
- HFS 列文件按 section 流式读取，SQL 与 DMP 实机回灌双向 `MINUS` 为 0；
- 压缩、加密、可空定长列及未验证类型会明确失败，不做启发式猜测。

后续缺口是压缩 section、更多定长类型、多个 HFS path、HFS 校验和和 DMASM 裸盘 HFS
映射。详细证据与边界见 [DM8 HUGE 列存储表离线恢复](huge-tables.md)。

### 6. 无 SYS 字典的 storage_scan 救援模式尚未落地

总结建议的 `storage_scan.dict` 很适合 SYSTEM.DBF 丢失场景。当前文档已有方向，但代码尚未实现独立模式。

建议模式：

```text
扫描所有 DBF 页头
按 group/file/storage_id 聚合 page_kind=0x14 页
输出 storage_scan.tsv
输出 sample raw row、offset、length、ASCII hint
允许用户后续按 storage id + 人工列定义恢复
```

这个模式不应伪造 owner/table/column，只能输出 `SCAN.TAB_<storage_id>` 这类占位对象或 raw row。

## 暂不建议贸然修改的部分

### 普通 chained row

总结和我们自己的实验都没有证明普通 BTREE 表会自动生成 Oracle 式跨块 chained row。当前不建议实现普通行跨页拼接。后续如果出现可验证样本，再按行片段指针设计。

### 行迁移

当前观察更接近“物理位置变化 / 页级重组”。默认导出按页尾 slot 目录取活动行是正确方向。后续需要增强的是：

- 不按物理连续旧行导出；
- 遇到 active slot 指向疑似迁移指针时输出诊断；
- recovery 模式保留页号、slot、row offset，便于人工筛选。

## 建议优先级

1. **P0：显式 NULL metadata 解码**，降低多字段表、尾部 NULL、Long Row locator 的误判。
2. **P0：补齐基础标量类型**，尤其 `REAL/FLOAT/DOUBLE/TIME/带时区时间/INTERVAL/ROWID`。
3. **P1：行外 LOB 和 Long Row locator/page chain**。
4. **P1：bootstrap 标准表下载化**，降低大 SYSTEM.DBF 的扫描成本和误识别风险。
5. **P1：storage_scan 救援模式**，支持 SYSTEM.DBF 不可用时先保全 raw 行。
6. **P2：HUGE 压缩 section、更多标量类型、多 HFS path 与 HFS 校验和**。
7. **P2：row archive 输出格式**，用于跨机器重装载和 LOB payload 一体化保存。
