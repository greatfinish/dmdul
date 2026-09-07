# DM8/DM9 PAGE_CHECK 页校验实验

本文记录 2026-07-15 在独立 DM8 实例上对 `PAGE_CHECK=0/1/2/3` 的页级差分结果。实验使用相同
的 8 KiB 页、表结构和三条数据，并在 `CHECKPOINT(100)` 和正常关闭后读取同一 `MAIN.DBF` page 16。

达梦官方定义：模式 0 禁用校验；模式 1 使用标准 CRC32；模式 2 使用 `PAGE_HASH_NAME` 指定的
HASH；模式 3 使用快速 CRC32C。参考：[dminit 参数详解](https://eco.dameng.com/document/dm/zh-cn/pm/dminit-parameters.html)。

## CRC 模式

模式 1 和模式 3 的校验值都位于页头 `0x18..0x1B`，按 little-endian `u32` 保存。计算步骤为：

```text
work_page = raw_on_disk_page
work_page[0x18:0x1C] = 00 00 00 00
payload = work_page[0 : page_size - 8]

PAGE_CHECK=1: checksum = CRC32/IEEE(payload)
PAGE_CHECK=3: checksum = CRC32C/Castagnoli(payload)
```

实机 8 KiB 样本：

| 模式 | 页头存储值 | 独立重算 |
| ---: | ---: | ---: |
| 0 | `0x00000000` | 不校验 |
| 1 | `0xF3B8E936` | CRC32=`0xF3B8E936` |
| 3 | `0x6EA5FEFA` | CRC32C=`0x6EA5FEFA` |

模式 0、1、3 的目标页除 `0x18..0x1B` 外完全一致，因此该字段定位不是相关性猜测。

## HASH 模式

实验使用 `PAGE_CHECK=2 PAGE_HASH_NAME=SHA256`，在线参数返回：

```text
ENABLE_PAGE_CHECK = 2
PAGE_CHECK_ID     = 2304
CYT_NAME          = SHA256
```

该早期整页 HASH 样本不写入 `0x18`。其位置和输入范围由摘要长度决定：

```text
hash_offset = page_size - digest_size - 8
stored_hash = page[hash_offset : hash_offset + digest_size]
calculated  = HASH(page[0 : hash_offset])
```

8 KiB + SHA256 的 `hash_offset=0x1FD8`，实机保存值与
`SHA256(page[0:0x1FD8])` 完全一致。HASH 占用页尾空间，因此 slot/空闲边界会相应前移。

对应 slot 目录公式为：

```text
slot_start = page_size - digest_size - 8 - n_slot * 2
```

固定使用 `page_size - 8 - n_slot * 2` 会把 SHA256 摘要误读为 slot。DMDUL 的 SYSTEM 字典页、
分区字典页和用户数据页现已统一使用 HASH 感知的 slot 起点。

## DMDUL 实现

`internal/dm/page_check.go` 已实现：

- CRC32/IEEE；
- CRC32C/Castagnoli；
- MD5、SHA1、SHA224、SHA256、SHA384、SHA512、SM3 页 HASH；
- 校验值不匹配和单字节损坏测试。

v0.10.0 通过 `github.com/tjfoc/gmsm/sm3` 支持 `SM3/OPENSSL_SM3`，包括标准已知答案测试、
损坏输入和 DM9 实机样本。未知算法在扫描开始前报错，不会静默跳过校验。对于摘要损坏的页面，
DMDUL 仍会根据 slot 目录结构在
`16/20/28/32/48/64` 字节候选中保守推断摘要长度，以便救援模式继续定位 slot；该推断不等于校验
通过。页校验失败不应自动丢弃救援数据，后续接入导出日志时应分别记录“校验失败”和“仍尝试恢复”。

实机回归还直接使用模式 0 和 SHA256 模式的完整 `SYSTEM.DBF`、`MAIN.DBF` 执行当前 DMDUL。两者的
标准两阶段 bootstrap 都恢复 `1063` 个对象，随后对 `SYSDBA.PC_T` 生成一个计划页、直接读取一个页、
无 fallback，三条数据全部导出且字段值与在线插入值一致。这既验证了 HASH 尾部处理，也验证了
模式 0 不会被结构推断误判成 HASH 页。

## DM9 分 sector HASH 与备份字节

2026-09-06 在 DM9 `03151060506-20260417-322930-20218` 上补测 SM3 时发现，
8/16/32 KiB HASH 页还有另一种布局：每 4 KiB sector 分别存摘要，中间 sector 被覆盖的
原始字节集中备份在页尾。不能将整页 HASH 公式套用到所有 build。

```text
sector_count = page_size / 4096
digest_size = 32                         # SM3 / SHA256
backup_start = page_size - 8 - sector_count * digest_size
slot_start = backup_start - n_slot * 2

中间 sector: HASH(page[sector_start : sector_end - digest_size])
最后 sector: HASH(page[sector_start : page_size - digest_size - 8])
```

8 KiB SM3 的具体证据：

| 范围 | 含义 |
| --- | --- |
| `0xFE0..0xFFF` | 第一 sector 的 SM3 摘要，输入为 `page[0:0xFE0]` |
| `0x1FB8..0x1FD7` | 第一 sector 被覆盖的 32 字节原值 |
| `0x1FD8..0x1FF7` | 最后一 sector 摘要，输入为 `page[0x1000:0x1FD8]` |
| 最后 8 字节 | 固定尾部，不纳入上述摘要 |

`SYSTEM.DBF` page 304 的一条字典行头从 `0xFE6` 开始，只有恢复备份字节后才能正确读取。
对应内置字典页已保存为 `internal/dm/testdata/dm9_sm3_system_page304.bin`，不含用户业务数据。

实现先在**原始磁盘字节**上校验全部 sector，再在副本中恢复边界字节用于行解析；还原后的
页面不能当作原始 checksum 证据。重复还原必须幂等。8/16/32 KiB 的合成损坏测试和实机
bootstrap/检查/DMP 导回覆盖这条路径。4 KiB + SM3/SHA256 在此 build 初始化阶段即因
剩余页空间不足被拒绝，不能记录为解析器支持。

## 校验覆盖说明

文件头及 SYSTEM 前部特定元数据页没有普通数据页的 PAGE_CHECK 布局。检查仍核对其身份，
并单独累计 `checksum not applicable`，不把它们统计成“摘要已验证”。模式 0 同样不提供摘要证据。
完整检查读取原始页；仅在进入结构解析时还原保护字节，避免“先还原再校验”造成误报。

默认文件集合包含 SYSTEM、ROLL、TEMP 和用户 DBF，不沿用数据卸载仅选用户表空间的过滤条件。
`check pages SYSTEM.DBF;` 必须实际读到该文件；文件过滤没有匹配项时返回错误，不输出零文件的成功报告。

## check pages 影响报告

`check pages` 不只验证 PAGE_CHECK。扫描顺序是页头自描述、PAGE_CHECK 校验和、行页结构，
每页只记录最基础的首个失败原因：`HEADER_INVALID`、`CHECKSUM_FAIL` 或
`STRUCTURE_INVALID`。文件大小不是页大小整数倍时另记为 `FILE_INVALID`。

扫描器保留两套输出路径：

1. 终端和 `dul.log` 保留每文件最多 4096 条坏页，适合快速观察且内存有界。
2. `check_bad_pages.tsv` 通过同步回调逐行写盘，覆盖全部坏页。损坏分类计数和对象影响也在
   扫描过程中聚合，因此不受 4096 条上限影响。

字典归属按证据强度执行：

| 依据 | 类型 | 置信度 | 说明 |
| --- | --- | --- | --- |
| 主 `storage_id` | `TABLE` | `HIGH` | 页头 storage 与表主 storage 唯一匹配 |
| 辅助 `storage_id` | `TABLE_ASSIST` | `HIGH` | 能证明父表，不能继续猜测 INDEX/LOB/分区类型 |
| 唯一段范围 | `TABLE` | `MEDIUM` | storage_id 被清零或未知时，用 group/file/page 范围回退 |
| 无匹配或存在歧义 | `UNATTRIBUTED` | `NONE` | 保留具体原因，不解释为空闲页 |

每次成功检查会覆盖生成 `output/check_summary.md`、`output/check_bad_pages.tsv` 和
`output/check_affected_objects.tsv`。汇总报告可与官方 `dmdbchk` 交叉核对，但不能替代官方工具。

## 与 bootstrap 的执行顺序

`bootstrap` 在读取系统字典前自动执行一次完整 SYSTEM.DBF 纯物理预检。该预检使用统一页检查
内核，但不加载任何字典，因此不会受到旧 `dmdul_dict` 影响；文件系统 SYSTEM 和 DMASM 逻辑
SYSTEM 使用相同规则。

预检发现损坏时，bootstrap 继续尝试恢复尚可读取的字典，最终状态标记为
`SUCCESS_WITH_WARNINGS`。只有需要扫描全部数据文件并分析对象影响时，才在 bootstrap 后运行
独立 `check pages;`。独立检查只采用当前会话已经 bootstrap 或显式加载的字典，不会自动加载
目录残留字典。
