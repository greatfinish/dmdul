# 兼容性与解析加固实测记录

本文记录 v0.10.0 的实现依据，初次实验日期为 2026-09-06。

## 环境与隔离

| 实例 | Build | 用途 |
| --- | --- | --- |
| DM8 独立 UNDOLAB，端口 5439 | `03134284336-20250117-257733-20132` | HUGE、列权限、系统权限、已有 Undo 差分 |
| DM9 独立矩阵实例，端口 5540 | `03151060506-20260417-322930-20218` | 4 KiB、CASE_SENSITIVE=0、PAGE_CHECK、SM3 |

实验不停止两台主机原有主实例。各实验实例正常关闭后才读取 DBF，导回阶段只重启自己的
实例。DM9 权限名映射另外使用了一次主实例只读查询；没有修改主实例配置或业务对象。

## 已验证结果

### DM9

详见 [矩阵明细](dm9-compatibility.md)。3 行小表分别覆盖 4 KiB 的 PAGE_CHECK 0/1/3、
GB18030/UTF-8/EUC-KR 和 8/16/32 KiB 的 SM3。DMP 导回双向 MINUS 均为 0；
三字符集的 4 KiB 样本额外做了中文/韩文数据往返。

发现并修复：

- HASH 页的中间 sector 摘要会覆盖字典行，原值在页尾备份。必须恢复后才能解析 slot/行。
- 原 check 文件枚举沿用了用户数据导出的过滤条件，可能跳过 SYSTEM；现在包含系统文件。
- 原 check 使用了已还原保护字节的页，导致校验误报；现在校验原始字节后再检查结构。
- 文件头/部分 SYSTEM 元数据页不具备普通 PAGE_CHECK 布局，单独记录“不适用”，仍检查身份。

4 KiB + SM3/SHA256 在初始化阶段被数据库拒绝，不是 dmdul 的解码失败。
`/opt/dm9_20260525_x86_uos20_64.iso` 中的服务器与已安装服务器为同一个 build，
第二 build 尚未验证，不能按 ISO 文件名推断版本。

### HUGE

新建 `SYSDBA.HFS_PROBE`，2500 行，两个 1024 行 HFS section 加 452 行 RAUX，
7 列包含 INT、可空 INT/BIGINT、SMALLINT、DATE、DOUBLE 和 VARCHAR。
SQL 导回普通对照表后 missing=0、extra=0，总行数=2500、NI 非空=1667、NB 非空=1875。
见 [HFS 物理布局](huge-tables.md#6-定长列补充实验v0100)。

此次只确认未压缩、未加密 section。HFS CHKSUM 已取得数值样本，但算法未确认，
压缩、多 HFS path、原始 ASM HFS 不在本次通过清单。

### 授权

- `SYSDBA.GRANT_PROBE`：表级 SELECT；列 ID 的 REFERENCES，VAL/OTHER_VAL 的 UPDATE。
  `tab_privs.tsv` 保留列号、列名和授权人，DMP column-grant record 类型 17 与官方 dexp
  样本字节一致。dimp REMAP_SCHEMA 导回后 DBA_COL_PRIVS、DBA_TAB_PRIVS 一致。
- `SYSPRIV_TEST`：CREATE SESSION、CREATE TABLE、带 ADMIN OPTION 的 CREATE VIEW，
  并授予 GRANT_PROBE_R。OWNER DMP 导出后删除此实验用户，再导入重建，三项权限和
  ADMIN_OPTION 与原值一致，表内 ID=7 保留，dimp 报无告警。
- 系统权限来自 `SYSGRANTS` 中 OBJID=-1、COLID=-1 的真实行。DM8/DM9 对照了
  `SF_GET_SYS_PRIV(4096..4351)`；只使用 172 项两者同名的已验证映射，未知编号仍写 TSV。
  不再用无条件 CREATE SESSION 冒充源权限；内置账户/角色保留目标库的安全策略。

`sys_privs.tsv` 字段为 `grantee, privilege_id, privilege, admin_option`。
旧字典缺少此文件仍可加载；有文件时以其内容为准。SQL 对未知编号写警告而不生成 GRANT，
DMP 则明确停止，要求核实字典后重试。自定义角色定义目前仍需原始系统字典，单独的
sys_privs.tsv 不是完整的角色元数据备份。表级导出不附带用户系统权限。

## 代码验证

- Windows：Go 1.22.12、Go 1.26.1 的完整测试与 `go vet ./...` 通过。
- Linux：Go 1.26.1 的完整测试、vet 和 race 检查通过。
- 五个 fuzz 入口分别运行 10 秒，覆盖页大小、slot、行 metadata、DMP 和 DMASM；
  均通过，但短时间 fuzz 不代表穷尽所有损坏输入。
- `govulncheck -show verbose ./...` 未报告可达符号漏洞；仍有依赖包/模块级提示，
  不能表述为“所有依赖无漏洞”。发布工具链建议及扫描边界见 [开发说明](development.md)。
- CI 工作流随 v0.10.0 提交；以上 2026-09-06 的结论来自本地和实验机执行，不冒充远端 CI 结果。

初次实验使用 `bin/dmdul-dev.exe`。正式构建与发版复测另见
[v0.10.0 发布验证](release-v0.10.0-validation.md)。

## 实验资产

项目只保存必要结论和最小样例页；完整 DBF 与实验日志保留在测试环境，不提交。

| 位置 | 内容 |
| --- | --- |
| DM9 `/dmdata/dmdulmatrix0906/<case>/` | init/setup/offline/import/compare 日志与关闭的实例 |
| DM9 各 case 的 `localized-*.log` | 4 KiB 三字符集补充往返 |
| DM8 `/dmdata/dmdulundolab/huge-*.log` | HUGE 2500 行结果 |
| DM8 `/dmdata/dmdulundolab/column-grants-import.log` | 列级授权导回 |
| DM8 `/dmdata/dmdulundolab/system-privilege-*.log` | 系统授权提取和导回 |
| `research/compatibility_matrix_probe.py` | 可复跑的独立 DM9 参数矩阵脚本，密码从环境变量读取 |
| `internal/dm/testdata/` | 内置字典 SM3 页、合成业务 HFS INT section、SHA256 清单 |

## 未完成项

事务 committed-only、完整 PRE IMAGE、DM9 第二 build、HUGE 压缩/其余类型/多路径/校验和、
DMASM 超 65535 AU 单文件/REDO/已删除文件、物化视图/自定义类型/目录/作业/策略，均未在
此次工作中完成，详见 [路线图](roadmap.md)。源码拆分和已有测试通过不能替代这些物理实验。
