# DMASM LAB03-HIGH 实验记录

LAB03 验证镜像版 DMASM 的 HIGH 冗余。实验不只检查文件目录和 AU 地址，还要求
`dmdul` 从裸成员盘完成 `SYSTEM.DBF bootstrap -> 用户表 page plan -> 数据卸载`。

实验日期：2026-08-05 至 2026-08-06。

## 实验目标

1. 解码 HIGH 文件每个逻辑 AU 的三个物理副本地址。
2. 验证 4 MiB AU、4-AU group、粗粒度和 32 KiB 细粒度条带。
3. 验证副本数组、故障组和 partner 关系不会改变逻辑文件字节流。
4. 对 HIGH 表空间中的真实 DBF 执行 Standard Bootstrap 和 page-plan 卸载。
5. 制作所有成员盘的同一时点冷副本，并在 Windows 上直接读取 `*-flat.vmdk`。

## 环境布局

`HIGH4` 使用四块 12 GiB VMware `monolithicFlat` 共享盘。每块盘格式化为 10,000 MiB
逻辑容量，AU 为 4 MiB；四块盘分别属于不同故障组。

| VMDK | group | disk | AU | 故障组 |
| --- | ---: | ---: | ---: | --- |
| `lab03_high4a-flat.vmdk` | 3 | 0 | 4 MiB | `HIGH_FG1` |
| `lab03_high4b-flat.vmdk` | 3 | 1 | 4 MiB | `HIGH_FG2` |
| `lab03_high4c-flat.vmdk` | 3 | 2 | 4 MiB | `HIGH_FG3` |
| `lab03_high4d-flat.vmdk` | 3 | 3 | 4 MiB | `HIGH_FG4` |

在线视图记录的磁盘组真值：

| 项目 | 值 |
| --- | --- |
| group id / name | `3 / HIGH4` |
| redundancy | `HIGH` |
| AU size | `4,194,304` bytes |
| members / failure groups | `4 / 4` |
| partner rows | `12`，每块盘与其他三块盘互为 partner |
| redundancy lowered | `N` |

## 确定性文件

实验程序按 4 KiB 页写入文件标签、逻辑偏移和固定填充值。这样可以直接判断读到的是
正确逻辑位置，而不是只验证文件可打开。

| ASM 文件 | 大小 | 条带 | 标签 | 填充值 |
| --- | ---: | ---: | ---: | ---: |
| `+HIGH4/high_stripe0_au4.dat` | 256 MiB | coarse | `0x4849474830000001` | `0x48` |
| `+HIGH4/high_stripe32_au4.dat` | 256 MiB | 32 KiB | `0x4849474833320002` | `0x49` |

官方 `check` 对两个文件各列出 64 个逻辑 AU。每个 AU 都有三个物理地址，文件头显示
`n_copy=3`、`r_copy=0`，检查结果均为 `0 mirs err`。

`TestRawASMHighDeterministicFiles` 对两份文件的全部 131,072 个 4 KiB 页逐页校验，覆盖
512 MiB 逻辑数据。文件标签、逻辑偏移和填充值全部一致。

## 数据库对象

数据库在 HIGH 组中创建 256 MiB、32 KiB 条带、HIGH 冗余的数据文件：

```text
+HIGH4/data/MIRRORDB/TS_HIGH4_01.DBF
```

`ASMTEST.T_HIGH_COPY` 包含 20,000 行。在线真值为：

| 指标 | 值 |
| --- | ---: |
| `COUNT(*)` | 20,000 |
| `MIN(ID)` / `MAX(ID)` | 1 / 20,000 |
| `SUM(LENGTH(PAYLOAD))` | 10,240,000 |
| storage root | file 0 / page 1040 |
| storage id | 33,555,491 |

为了把 ASM 映射与数据库行解析分开验证，使用 `dmasmtoolm cp` 将该 DBF 导出为普通
256 MiB 文件。`dmdul` 从四块裸盘重建的逻辑 DBF 与官方复制结果逐字节一致。

## 32 KiB 页保护问题

第一次在线裸盘卸载得到 17,492 行，另有 1,420 行解析失败。ASM 确定性文件和真实 DBF
逐字节对照均已通过，因此故障不在 HIGH 副本或条带映射。

真实 32 KiB 数据页的尾部布局为：

```text
slot directory
7 * 4-byte sector-boundary backups
4-byte protection field
8-byte fixed trailer
```

页尾总保留长度为 40 字节。旧代码有时按固定 8 字节尾部计算 slot 起点，把 32 字节
保护区误读成 slot；行跨越 4 KiB 边界时，页内保护值也会污染字符串。修复后，解析器
根据 `n_slot/n_rec/free_end` 和可解码行头识别保护布局，再从页尾恢复七组边界原字节。

修复后的在线卸载结果：

| 指标 | 结果 |
| --- | ---: |
| planned pages | 351 |
| direct pages read | 351 |
| fallback pages scanned | 0 |
| rows exported / failed | 20,000 / 0 |
| ID 缺失 / 重复 | 0 / 0 |
| 标记或 payload 不一致 | 0 |

## 冷副本

两节点按数据库、ASM、CSS 顺序停止，再关闭虚拟机。随后复制 12 条共享 VMDK 链：

- 9 块用户 ASM 成员盘；
- 3 块 DCR/VOTE 成员盘；
- 24 个 descriptor/flat 文件，共 208 GiB。

冷副本目录为 `D:\temp\dmasm-lab03-high-cold-20260806-000152`。源与副本 SHA-256、
VMDK 链和纯离线恢复均已验证：

| 检查项 | 结果 |
| --- | --- |
| 源/副本文件 | 24 / 24 |
| 总字节数 | 223,338,305,041 |
| SHA-256 差异 | 0 |
| VMDK descriptor 链 | 12 / 12 consistent |
| 裸盘发现 | 5 个磁盘组、12 个成员、73 个 ASM 文件 |
| 数据文件发现 | 9 个 DBF，全部可读且按 32 KiB 页对齐 |
| Standard Bootstrap | 约 6 秒；1169 个对象、2 个用户、6 张表、37 列 |
| `T_HIGH_COPY` page plan | planned/direct 351/351，fallback 0 |
| 导出结果 | 20,000 行，失败 0 |

冷副本 SQL 与在线裸盘 SQL 的 SHA-256 均为
`3A9EE8F6419F8932EEA9FE36676DC92E5B792F4C61BF894B097D867C8535DF12`。
独立检查确认 20,000 个 ID 唯一且连续，无缺失、重复或畸形 SQL；`STRIPE_MARK`、
512 字节 `PAYLOAD` 均符合生成规则，payload 总长度为 10,240,000。

验证结束后恢复两台虚拟机。两节点 CSS、ASM 和数据库服务均为 `active`，无 failed unit；
MDSC07 与 MDSC08 都处于 `OPEN`，两端在线查询结果仍为 20,000 行、ID 1..20,000、
payload 总长度 10,240,000。

## 研究文件

| 文件 | 用途 |
| --- | --- |
| `format-high4-disks.txt` | 四块成员盘格式化命令 |
| `create-high4.txt` | HIGH4 磁盘组和两个确定性文件 |
| `dmasm_fill_high.c` | 写入确定性页标签、偏移和填充值 |
| `create-db-objects.sql` | HIGH 表空间、测试表和 20,000 行数据 |
| `collect-online.sql` | 磁盘组、成员、partner、文件和表数据真值 |
| `check-high4.txt` | 官方文件一致性检查命令 |

这些结论来自 DM8 build `03134284604-20260707-335949-20228` 的受控实验，不代表达梦
公开的磁盘格式规范。正式恢复仍应使用停写后的完整副本，并在隔离数据库中验证输出。
