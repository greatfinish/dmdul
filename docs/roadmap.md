# dmdul 开发路线图

本页区分已有能力、本版新增实现和仍需逆向验证的功能。当前版本为 v0.10.0；
旧版二进制不包含本版新增能力，使用前请用 `version` 确认版本。

## 已有能力

| 范围 | 当前路径 | 证据与边界 |
| --- | --- | --- |
| 文件输入 | 离线 DBF、DMASM 逻辑 Reader、ASM `cp` | 不依赖启动数据库；不重放 ASM REDO |
| 标准字典 | SYSTEM 预检、两阶段 bootstrap、TSV 持久化与加载 | 不把动态 `V$` 视图当作磁盘系统表 |
| 表定位 | storage root、internal/leaf、next 链、计划页身份校验 | 失败时才用同 group storage/段范围；残留页扫描仅恢复模式 |
| 普通行 | slot-only、显式 2-bit metadata、已验证标量、LOB、Long Row | 不等于 committed-only；不猜测普通迁移行拼接 |
| 输出 | SQL、dmfldr、表/用户/全库 DMP | 有官方工具导回证据；不是全部数据库对象的无条件完整备份 |
| HUGE | HFS section 与 RAUX/DAUX/UAUX 合并 | 压缩、加密、多 HFS path 和原始 ASM HFS 仍有限制 |
| DM9 | 单个 build 的类型、对象、字符集、页大小矩阵 | 见 [DM9 验证](dm9-compatibility.md)，不能外推所有 build |

## v0.10.0 新增实现

- **页几何识别**：联合多页身份、页类型、checksum/结构证据识别 4/8/16/32 KiB；
  不再用文件大小猜 8 KiB，冲突或歧义时报错，拒绝 64 KiB。
- **无字典救援**：`scan storage`、样例行、人工列定义与 `recover storage`；不伪造对象归属。
- **Undo 证据**：19 字节行尾和部分 `0x1D` 公共头的有界追踪，仅诊断，不应用 PRE IMAGE。
- **DM9 页校验**：SM3、分 4 KiB sector 的 HASH 与覆盖字节备份恢复；校验先于字节还原。
  `check pages` 包含 SYSTEM/ROLL/TEMP，零匹配文件不再返回“检查通过”。
- **权限**：列级权限持久化与 SQL/DMP；真实系统权限 `sys_privs.tsv`、ADMIN OPTION，
  未知编号保留证据，不凭空补 GRANT。FULL 包含未授予用户的自建角色。
- **HUGE 定长列**：可空 INT/BIGINT、4 字节 HFS SMALLINT、DOUBLE、已验证 AD DATE；
  尾部 MSB-first presence bitmap 与 AUX N_NULL 交叉校验。
- **工程**：拆分 planner 候选、row/scalar/LOB、writer、DDL catalog/renderer；
  Go 1.22/stable 的 Windows/Linux 测试、vet、构建、Linux race、五组 fuzz、govulncheck。

## 仍需完成

| 优先级 | 工作 | 完成标准 |
| --- | --- | --- |
| P0 | 事务可见性与完整 Undo | 未提交/提交 INSERT、DELETE、连续 UPDATE、Rollback、Undo 复用矩阵；与在线提交视图逐行一致后才能提供 committed-only |
| P0 | DM9 第二个 build | 提供不同的完整 build 编号，重复冷快照、坏页注入、DDL/数据导回矩阵；安装包日期不同不算第二个 build |
| P1 | HUGE 压缩/编码/校验和 | 各压缩算法和级别的 section 样本、CHKSUM 重算、单字节破坏检测，输出前拒绝未知布局 |
| P1 | HUGE 更多类型/路径 | NUMBER、时间/时区、INTERVAL、二进制等独立物理布局；多 HFS path 的 FILE_ID 映射与同名冲突测试 |
| P1 | DMASM HFS | 从原始成员盘解析 HFS 文件，接入逻辑 Reader，与文件系统 HFS 导出逐字节/逐行比较 |
| P1 | DMASM 单文件超过 65535 AU | 映射条目跨界、首尾 AU、随机读取与官方复制的哈希一致；现有描述簇跨界测试不能替代单文件验证 |
| P1 | ASM REDO 与已删除文件 | 冷/非冷镜像、事务顺序和 generation、幂等重放；不写源盘，删除文件输出单列为候选证据 |
| P1 | 扩展整库对象 | 物化视图/日志、自定义类型、目录、作业、策略；分别验证系统表来源、依赖顺序、SQL/DMP 官方导回 |
| P2 | 迁移行/链式行 | 等待可重复普通行物理样本；禁止把 Long Row 或历史副本当作迁移链 |
| P2 | 降低维护成本 | 继续压缩 ExportData 编排复杂度；完善模糊输入语料和损坏字典回归，不在无测试时继续拆逻辑 |

## 实验原则

1. 每种未知布局先记录 build、初始化参数、最小建表/造数 SQL、在线基线和冷快照。
2. 正常实例与实验实例隔离，实验只操作自己的目录、端口和进程，不停止已有业务实例。
3. 先物理定位，再做离线导出，最后由官方工具导回并比对 NULL、长度、内容和对象定义。
4. 不以“无报错”“行数相等”代替内容一致；未知布局明确报错，保留证据。
5. 发布前运行完整质量门禁，文档将未验证项保留为边界，不提前声明支持。
