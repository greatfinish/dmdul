# DMASM LAB04-REBALANCENORMAL 实验记录

LAB04 验证镜像版 DMASM NORMAL 磁盘组在 ADD、REBALANCE、OFFLINE、RECONNECT 和
REPLACE 期间如何更新 AU 地址与成员元数据。实验还暴露并修复了一个恢复风险：替换后的
旧盘重新出现时，仅按 group id 聚合成员会把旧 AU 当成第三份副本。

实验时间：2026-08-06 至 2026-08-09。

## 结论

- AU 1 `+0x06` 是 32 位小端组头 generation。成员变化和健康状态变化都会推进它。
- AU 2 从 `+0x00` 开始保存 128 字节成员记录。实测状态码为 `1=NORMAL`、
  `5=OFFLINE/RECONNECT`、`0=DELETED`。
- generation 不是每块活跃盘都会同步更新。必须从最高 generation 的可读组头副本取得
  当前成员表，再按成员状态、disk id、磁盘名和 AU 数筛选输入盘。
- 32 字节 AU 描述符的 logical sequence 是 `+0x1B` 的 16 位小端值；`+0x1D` 是副本
  失败位图。此前把 `+0x1D` 拼入 sequence 会产生 `131073` 之类的伪序号。
- NORMAL 组在成员 I/O 失败后仍可从剩余副本读取。短窗口 ONLINE 会进入
  `RECONNECT/RESYNC`；超过修复期限后，DMASM 会删除旧成员并把全部缺失副本恢复到
  其他成员，此时原盘不能再按 reconnect 使用。
- REPLACE 不保留旧 disk id。本次旧 D 为 `disk_id=3`，备用 R 入组后为 `disk_id=9`，
  但继承 `RBL_FG4`。

## 实验环境

两节点为 `192.168.17.107/.108`。六块 8 GiB VMware `monolithicFlat` 共享盘均格式化为
6000 MiB，AU 为 4 MiB。

| 设备 | 初始用途 | failure group |
| --- | --- | --- |
| `rbln4a` | 初始成员 A | `RBL_FG1` |
| `rbln4b` | 初始成员 B | `RBL_FG2` |
| `rbln4c` | 初始成员 C | `RBL_FG3` |
| `rbln4d` | 初始成员 D | `RBL_FG4` |
| `rbln4e` | 第五块扩容盘 E | 最终加入 `RBL_FG1` |
| `rbln4r` | REPLACE 专用备用盘 R | 替换后继承 `RBL_FG4` |

`RBLN4` 中写入 14 个 512 MiB 确定性文件，共 7 GiB 逻辑数据、14 GiB NORMAL 副本。
每个 4 KiB 页包含文件标签、逻辑偏移和固定填充值。另有 256 MiB 数据文件和
`ASMTEST.T_REBALANCE_NORMAL`，在线基线为 20000 行、payload 字符数 10240000。

## 阶段结果

| 阶段 | 关键结果 | generation |
| --- | --- | ---: |
| 四盘高占用基线 | A-D 为四个 failure group | 23 |
| E 作为独立第五 failure group | E 入组，但多档占用率均无重平衡计划 | 24 |
| E 重新加入 `RBL_FG1` | E 获得 `disk_id=8`，重平衡实际迁移样本副本 | 26 |
| C OFFLINE | 触碰 `fill11` 后产生 64 条失败 AU | 27 |
| C 超时自动恢复 | C 变为 DELETED，`RECOVER 930/930`，失败 AU 清零 | 30 |
| D OFFLINE | 触碰 `fill11` 后产生 64 条失败 AU | 31 |
| D RECONNECT | 中间态为 `RESYNC RUN 7/71`，完成后 `71/71` | 31 -> 32 |
| D 再次 OFFLINE | 为 REPLACE 建立故障样本 | 33 |
| R 替换 D | `REPLACE 932/932`，R 为 `disk_id=9` | 38 |

generation 取自同一阶段最高版本的 AU 1 组头。E 的本地组头长期停留在 24，但最新成员表
始终把 `disk_id=8` 标为 NORMAL。这证明不能逐盘比较 generation 后直接丢弃所有旧版本盘。

## ADD 与 REBALANCE

E 作为新的第五 failure group 加入时，在旧成员占用率约 8%、16%、25% 和 31-33% 的
四次实验中均得到 `no plan`。每次 fully drop、重新格式化和加入都会分配新成员号，E
先后使用过 `disk_id=4/5/6/7`。

将 E 加入已有 `RBL_FG1` 后，E 获得 `disk_id=8`，伙伴为 B/C/D。`V$ASM_OPERATION`
记录 `BALANCE 248/248`；确定性样本中有 232 份物理 AU 从 A 迁移到 E：前七个文件各
32 份，`fill5` 为 8 份。其余工作量来自 ASM 元数据和非样本文件。

随后 C 超时退出并完成全组恢复，14 个样本在四个当前成员 `0/1/3/8` 上各占 64 份，
每个逻辑 AU 仍严格为两副本。

## OFFLINE 与健康位

仅在一个节点删除 SCSI 设备不会改变全局状态。两节点都失去成员后，缓存查询仍显示
NORMAL；对该成员持有副本的 `fill11` 执行写入后，状态才变为 OFFLINE。

OFFLINE 阶段有 64 个失败 AU，全部属于 `fill11`。存活副本的 32 字节描述符只改变
`+0x1D` 和校验字段：

| 描述符字段 | 结果 |
| --- | --- |
| `+0x1B..+0x1C` | 16 位 logical sequence，保持不变 |
| `+0x1D=0x01` | logical mirror slot 0 失败 |
| `+0x1D=0x02` | logical mirror slot 1 失败 |
| `+0x1E..+0x1F` | 随健康位变化的校验字段，算法未解码 |

C 故障时，D 上偶数 sequence 为 `0x01`、奇数为 `0x02`；D 故障时，E 上相同 sequence
得到相反位值。这与 `V$ASM_FAIL_AU` 的失败盘和 AU 地址逐项对应。重连、超时恢复或替换
完成后，相关描述符的 `+0x1D` 均恢复为 0。

## RECONNECT

C 的首次 OFFLINE 跨越了修复期限。设备再次出现时，前 48 MiB SHA-256 与离线前一致，
但 `V$ASMDISK` 已不再列出 `disk_id=2`，历史记录为 `RECOVER 930/930`。因此该盘是携带
旧元数据的迟到盘，不能再执行 ONLINE。

为获得确定的 reconnect 样本，实验改用 D：

1. 两节点删除 D 的 SCSI 设备，并写 `fill11` 触发 OFFLINE。
2. 两节点重新扫描到同一块未变磁盘，前 48 MiB 哈希一致。
3. 执行 `ONLINE ... POWER 8`，抓到 `RECONNECT` 和 `RESYNC RUN 7/71`。
4. RESYNC 完成后 D 恢复 NORMAL，失败 AU 从 64 清零。

恢复后的 14 个文件共 7 GiB 均完成逐 4 KiB 页校验，AU 地址分布未改变。

## REPLACE 与旧成员排除

D 再次 OFFLINE 后，以从未入组的 R 执行 REPLACE。操作历史为 `REPLACE 932/932`：

- D 的成员记录变为 `state=0, disk_id=3`；
- R 的成员记录为 `state=1, disk_id=9`；
- R 继承 D 的 `RBL_FG4`；
- 14 个样本原来位于 disk 3 的副本全部改为 disk 9；
- 失败 AU 为 0，数据库表仍为 20000 行。

旧 D 重新扫描出现后，官方视图仍只列出 `0/1/8/9`。修复前的 dmdul 会把旧 D 的
描述符再次聚合，报告“逻辑 AU 有 3 个副本”；修复后从 generation 38 的 AU 2 成员表
排除 `state=0` 的 disk 3。把 A/B/旧D/E/R 五个设备同时传给解析器后，7 GiB 全页校验
通过，AU 分布只包含 `0/1/8/9`。

## 已确认的物理字段

### AU 1 组头

| 偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 2 | group id |
| `0x06` | 4 | group-header generation，小端 |
| `0x0A` | 2 | 副本数；NORMAL 为 2 |
| `0x0C` | 32 | 组名 |
| `0x34` | 2 | 当前成员数 |
| `0x36` | 2 | 当前 failure group 数 |
| `0x40` | 4 | 下一个成员号；替换后从 9 推进为 10 |

### AU 2 成员记录

记录长度为 128 字节，从 AU 2 的 `+0x00` 连续排列。

| 记录偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 2 | 实测状态：0 DELETED、1 NORMAL、5 OFFLINE/RECONNECT |
| `0x02` | 2 | disk id |
| `0x08` | 4 | 成员可用 AU 数 |
| `0x0C` | 2 | failure group id |
| `0x2C` | 32 | failure group 名 |
| `0x4C` | 32 | `DMASM...` 磁盘名 |
| `0x7C` | 4 | 校验/尾标记；算法未解码 |

## dmdul 解析规则

镜像组打开时按以下顺序选择成员：

1. 读取所有输入盘的 AU 1，选择最高 generation 的可读组头副本。
2. 从该 generation 的 AU 2 读取成员表，并比较同 generation 副本的一致性。
3. 仅保留状态 1、disk id 存在、磁盘名一致且 AU 数匹配的输入盘。
4. OFFLINE/RECONNECT 和 DELETED 成员不参与普通读取；缺失副本地址仍作为元数据证据保留。
5. 读取数据时继续按当前副本数组重试，所有地址都校验 file id 和 logical sequence。

合成回归测试覆盖旧 generation 的 DELETED 盘和状态 5 成员；LAB04 真盘测试覆盖
“当前成员 + 替换后的旧成员”并存场景。

## 证据目录

实验机证据位于：

```text
/dmdata/DMASM_MIRROR/lab04-rebalance/evidence
```

关键目录为 `03-rebalance-same-fg`、`04-offline`、`05-timeout-recovered`、
`05-offline-d-reconnect`、`05-reconnect-running`、`05-reconnected-d`、
`06-offline-d-replace` 和 `06-replaced-d-r`。每个静态阶段包含成员盘前 48 MiB、SHA-256、
官方工具输出、动态视图结果和 dmdul 校验日志。

仓库内的 [generation-stages.tsv](generation-stages.tsv) 和
[member-state-stages.tsv](member-state-stages.tsv) 汇总了可重复比较的 generation 与状态证据。

## 安全边界

- 实验只操作 `.107/.108`，未修改 `.101/.102`。
- SCSI 删除只让操作系统暂时失去设备，不改写 VMDK；ONLINE 使用同一块原盘。
- REPLACE 只使用从未加入组的 R 盘。
- 运行中快照只用于状态差分，格式结论以操作完成后的静态副本为准。
- 当前结论适用于实测 DM8 build `20260707` 的镜像版 `0x3001`。其他 build 需重新验证。
