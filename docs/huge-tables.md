# DM8 HUGE 列存储表离线恢复

本文记录 dmdul 对 DM8 HUGE（列存储）表的当前支持范围、离线文件要求和验证证据。
实现依据是达梦官方的 HFS/AUX 模型，并结合一致性快照做物理差分与回灌验证。

官方文档：[管理列存储表](https://eco.dameng.com/document/dm/zh-cn/pm/manage-column-tables.html)

## 1. 为什么 HUGE 表不能按普通 DBF 表处理

普通行表的数据位于 DBF 的段、簇和数据页中。HUGE 表建立在混合表空间上，完整区数据位于
Huge File System（HFS）目录；事务型 HUGE 表的增删改则位于普通 DBF 中的辅助表。因此，
只扫描 `MAIN.DBF` 会漏掉已经进入 HFS section 的主体数据，只读取 HFS 文件又会漏掉尚未
整理的增量。

```text
SYSTEM.DBF dictionary
        |
        +-- HUGE main table
        +-- table$AUX   section metadata
        +-- table$RAUX  rows not yet moved into a full section
        +-- table$DAUX  logical delete ranges
        +-- table$UAUX  updated column values
                 |
                 v
HFS root/SCH#########/TAB####/COL####_##########.dta
                 |
                 v
        merged logical table rows
```

HFS 目录的典型命名为：

```text
HMAIN/
  SCH000001500/
    TAB1063/
      COL0000_0000000000.dta
      COL0001_0000000000.dta
```

每个 `.dta` 文件先有文件头，后续 section 按 4 KiB 对齐。`$AUX` 的 `COLID`、
`SEC_ID`、`FILE_ID`、`OFFSET`、`COUNT`、`N_LEN`、`CPR_FLAG` 和 `ENC_FLAG`
给出每个列 section 的物理位置和编码状态。

## 2. dmdul 当前恢复路径

`bootstrap;` 会执行以下工作：

1. 识别 HUGE 主表及 `$AUX/$RAUX/$DAUX/$UAUX` 内部对象；
2. 从 `SYSOBJECTS.INFO3` 恢复 `SECTION` 和 `FILESIZE`，从辅助对象判断
   `WITH DELTA` / `WITHOUT DELTA`；
3. 对外只保留 HUGE 主表，内部辅助表不计入 `list table` 和普通用户表数量；
4. 把 HUGE 标志、存储参数和四个辅助表 ID 写入 `dmdul_dict/tables.tsv`。

`unload table` 会按以下顺序恢复数据：

1. 从 `$AUX` 获取每列、每个 section 的 HFS 文件号、偏移、行数和长度；
2. 按 HFS section 流式读取列数据，不把整个 `.dta` 文件读入内存；
3. 沿普通 DBF 的 storage root/page plan 读取 `$RAUX/$DAUX/$UAUX`；
4. 合并 HFS 主体行、RAUX 尾部行、DAUX 删除范围和 UAUX 更新值；
5. 把同一逻辑结果写成 SQL、dmfldr 文本或 DMP。

DDL 会生成 `CREATE HUGE TABLE`，并恢复已经验证的表级存储参数：

```sql
CREATE HUGE TABLE SYSDBA.DMDUL_HUGE_SIMPLE (
    ID INT NOT NULL,
    VAL VARCHAR(20),
    TAG CHAR(1)
)
STORAGE(SECTION(1024), FILESIZE(16), WITH DELTA, ON MAIN);
```

## 3. 离线文件准备

恢复 HUGE 表时，普通 DBF 和 HFS 目录必须来自同一个停库时点或存储一致性快照：

```text
/recover/snap/
  SYSTEM.DBF
  MAIN.DBF
  other_tablespace.DBF
  dm.ctl                 # 可选，仅作映射核对
  HMAIN/                 # MAIN 混合表空间的 HFS 根
  other_hfs_root/        # 目标 HUGE 表使用时必须提供
```

`data_dir` 应指向上述共同父目录：

```text
DMDUL> set system /recover/snap/SYSTEM.DBF;
DMDUL> set data_dir /recover/snap;
DMDUL> bootstrap;
DMDUL> describe SYSDBA.DMDUL_HUGE_SIMPLE;
DMDUL> unload table SYSDBA.DMDUL_HUGE_SIMPLE;
```

`describe` 会显示：

```text
storage= HUGE
huge= SECTION(1024), FILESIZE(16 MiB), WITH DELTA, aux_ids= 1064/1065/1066/1067
```

卸载统计会额外显示 `HUGE tables selected`、`HUGE sections read` 和
`HUGE HFS files read`。

## 4. 已验证范围

| 能力 | 当前状态 |
| --- | --- |
| HUGE 主表与四类辅助对象识别 | 已验证 |
| `CREATE HUGE TABLE`、SECTION、FILESIZE、WITH DELTA、ON 表空间 | 已验证 |
| 非空 `INT` HFS 定长列 | 已验证 |
| 可空/非空 `VARCHAR`、`CHAR` HFS 变长列 | 已验证 |
| `$RAUX` 未满 section 行 | 已验证 |
| `$DAUX` DELETE 与 `$UAUX` UPDATE 合并 | 已验证 |
| SQL 导出和回灌 | 已验证 |
| DMP 导出并由官方 `dimp` 导入 | 已验证 |
| dmfldr 导出与官方 dmfldr 装载 | 已验证 |
| WITHOUT DELTA 数据 | 已实现同一路径，仍缺独立初始化参数样本 |

实机使用 1024 行 section、16 MiB 文件、`WITH DELTA` 的 ARM64 DM8 样本验证：

- 一个完整 HFS section 加 `$RAUX` 共恢复 1499 行；
- 仅有 `$RAUX`、尚未形成完整 HFS section 的小表恢复 100 行，SQL 与 dmfldr 回灌
  双向 `MINUS` 均为 0；
- 包含两个完整 HFS section 加尾部 RAUX 的表恢复 2500 行，SQL 回灌双向 `MINUS` 为 0；
- `$DAUX` 删除和 `$UAUX` 更新后的 SQL 回灌双向 `MINUS` 为 0；
- 表级 DMP 经官方 `dimp REMAP_SCHEMA` 导入 1499 行，与同一快照的 SQL 回灌表
  双向 `MINUS` 均为 0。

## 5. 安全边界

当前实现坚持“不能证明就不解码”：

- `CPR_FLAG != N` 的压缩 section 会在写出第一行前明确报错；
- `ENC_FLAG != N` 的加密 section 会明确报错；
- 尚未验证 NULL bitmap 的可空定长列会明确报错；
- 已验证的 HFS 定长类型目前只有非空 `INT`，其他数值、日期、时间和 INTERVAL
  需要按物理样本逐类补充；
- 当前要求目标表在 `data_dir` 下只有一个匹配的 HFS 表目录；同一表跨多个 HFS path
  的 `FILE_ID -> path` 规则尚未解码；
- 原始 DMASM 成员盘中的 HFS 文件尚未接入 ASM 逻辑 Reader，当前 DMASM 路径只覆盖 DBF；
- `check pages` 当前检查 DBF 页，不检查 `.dta` section 校验和；
- 表/列级 `STAT NONE`、压缩级别、压缩算法、加密和 HUGE 日志属性尚未恢复到 DDL。

为避免异常元数据或超大增量把进程内存耗尽，单次 `$UAUX` 加载最多保留 200 万个唯一
更新且解码值合计不超过 256 MiB；单个 section 的全部变长列 offset 表合计不超过
256 MiB。超过限制时工具会停止并报告原因，不会自动扩大内存上限。

遇到上述边界时，不要手工删除错误继续导入。应保存 `SYSTEM.DBF`、普通 DBF、完整 HFS
目录、`dmdul_dict` 和 `dul.log`，用最小样本补充格式证据后再扩展解析器。
