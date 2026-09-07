# 本地开发、测试、构建说明

## 代码结构

```text
cmd/dmdul/          CLI 入口
internal/cli/       命令行解析、参数默认值、控制台输出
internal/dm/        达梦文件解析、DDL 导出、数据导出
internal/version/   版本字符串
docs/               用户文档和逆向研究笔记
research/           临时实验脚本和研究材料
```

## 常用开发命令

运行测试：

```powershell
go test ./...
```

构建 Windows 可执行文件：

```powershell
go build -o .\bin\dmdul.exe .\cmd\dmdul
```

查看帮助：

```powershell
.\bin\dmdul.exe help
```

启动交互式界面：

```powershell
.\bin\dmdul.exe
```

## 本地验证建议

### 自动化质量检查

`.github/workflows/quality.yml` 在 main 推送、PR 和手动触发时执行：

- Windows/Linux：Go 1.22 与 stable 的 `go vet`、不使用缓存的测试、构建。
- Linux：启用 CGO 的 race 检测。
- Linux：五个 fuzz 入口各运行 30 秒、2 个 worker；普通单测也会运行种子样本。

工作流只读取仓库，不发布二进制、不连接私有测试库、不需要数据库密码。它产生检查结果，
不会自动设置 GitHub 分支保护；需要强制合并门禁时由维护者在仓库设置中要求这些检查通过。

本地示例：

```powershell
go vet ./...
go test -count=1 ./...
go test ./internal/dm -run '^$' -fuzz '^FuzzDataRowMetadata$' -fuzztime=30s -parallel=2
```

Linux race：

```bash
CGO_ENABLED=1 go test -race -count=1 ./...
```

其余 fuzz 入口为 `FuzzPageGeometry`、`FuzzDataPageSlots`、`FuzzDMPContainer`、
`FuzzDMASMMetadata`。每次只能选择一个入口；短时 fuzz 是冒烟检查，不代表所有格式已验证。
失败后保留 Go 自动生成的最小复现输入，先排除真实业务数据和凭据再纳入版本管理。

### 实例样例

建议准备一个不提交到 Git 的样例目录，例如：

```text
oldpro/
  SYSTEM.DBF
  dm.ctl
  MAIN.DBF
  TBS_BIN_TEST01.DBF
```

执行交互式验证：

```text
DMDUL> set system oldpro\SYSTEM.DBF;
DMDUL> set data_dir oldpro;
DMDUL> bootstrap;
DMDUL> list user;
DMDUL> list table SYSDBA;
DMDUL> unload table SYSDBA.T;
```

如果样例表名不同，可以先用 `list table <owner>;` 找到实际表名，再执行 `unload table`。

功能验证统一通过交互式命令完成。底层解析器应直接由 Go 单元测试或集成测试覆盖，
不再通过一次性功能子命令暴露。

## 版本信息

`internal/version/version.go` 里的 `Version` 是没有 `-ldflags` 注入时的兜底值，发版时
同步成当前 tag。正式发布构建通过 `-ldflags` 注入版本号、提交号和构建日期。

### 发布构建

**`-s -w` 不能漏。** 它去掉符号表和 DWARF 调试信息，二进制从约 6.2 MB 降到约 4.5 MB，
压缩包从约 3.4 MB 降到约 1.9 MB（v0.6.4 实测）。历史发布包都是带这两个 flag 构建的，
漏掉会让新版本看起来凭空"胖"了一大圈。`-s -w` 与 `-X` 注入不冲突，`dmdul version`
照常打印完整版本串。

先在干净工作区完成版本提交并创建 tag。以下命令从 `HEAD` 上的精确 tag 读取版本号，
如果尚未打 tag 会立即停止，避免二进制、压缩包名称和源码版本不一致。三段命令要在
**同一个 PowerShell 会话**里依次执行，后两段复用第一段定义的 `$ldflags`：

```powershell
$ver = (git describe --tags --exact-match HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or !$ver) { throw "HEAD must have an exact release tag" }
$commit = git rev-parse --short HEAD
$buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$ldflags = "-s -w -X dmdul/internal/version.Version=$ver -X dmdul/internal/version.Commit=$commit -X dmdul/internal/version.BuildTime=$buildTime"
go build -trimpath -ldflags $ldflags -o bin\dmdul.exe .\cmd\dmdul
if ($?) { .\bin\dmdul.exe version }
```

```powershell
$env:CGO_ENABLED="0"
$env:GOOS="linux"
$env:GOARCH="amd64"
go build -trimpath -ldflags $ldflags -o bin\dmdul .\cmd\dmdul
Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH
```

```powershell
New-Item -ItemType Directory -Force .\bin\licenses | Out-Null
Copy-Item -LiteralPath LICENSE, THIRD_PARTY_NOTICES.md -Destination .\bin -Force
Copy-Item -LiteralPath .\docs\licenses\go-LICENSE.txt, .\docs\licenses\x-text-LICENSE.txt, .\docs\licenses\gmsm-LICENSE.txt -Destination .\bin\licenses -Force
Compress-Archive -Path .\bin\dmdul.exe, .\bin\LICENSE, .\bin\THIRD_PARTY_NOTICES.md, .\bin\licenses -DestinationPath .\bin\dmdul_windows_amd64_$ver.zip -Force
tar -czf bin\dmdul_linux_amd64_$ver.tar.gz -C bin dmdul LICENSE THIRD_PARTY_NOTICES.md licenses
if ($LASTEXITCODE -ne 0) { throw "Linux packaging failed" }
Get-FileHash .\bin\dmdul_windows_amd64_$ver.zip, .\bin\dmdul_linux_amd64_$ver.tar.gz -Algorithm SHA256 | Format-List Hash, Path
```

发布包必须保留 `LICENSE`、`THIRD_PARTY_NOTICES.md` 和 `licenses/`，不要只打包可执行文件。

Windows PowerShell 5.1 没有 `&&` 和三元运算符，串联用 `;`，条件串联用 `if ($?) { }`。

`bin/` 在 `.gitignore` 里，构建产物不会进仓库。

### 本地开发构建

日常构建不需要注入，直接：

```powershell
go build -o bin\dmdul.exe .\cmd\dmdul
```

## 测试覆盖方向

### 代码职责

`data.go` 保留导出编排；页计划算法在 `data_page_plan.go`，候选匹配与回退在
`data_plan_candidates.go`，行布局在 `data_row.go`，标量在 `data_scalar.go`，
LOB 在 `data_lob.go`，输出路由在 `data_writer.go`。VECTOR/JSON/SQL 值渲染有各自文件。
`ddl.go` 保留导出编排与模型；字典行解析在 `ddl_catalog.go`，对象、分区和约束等渲染
分别在 `ddl_objects.go`、`ddl_partition.go`、`ddl_schema_render.go`。

本轮按 Go AST 移动声明并逐个验证等价，没有重新实现旧算法。后续变更应先补最小回归样本，
再改所属模块；不要因为文件拆分完成就跳过跨模块测试。

### 依赖与漏洞检查

v0.10.0 将 `golang.org/x/text` 从 v0.5.0 升到 v0.22.0，保留 Go 1.22 最低编译要求。
v0.23.0 的模块已要求 Go 1.23，不能只改版本号而不跑最低工具链。
SM3 使用 `github.com/tjfoc/gmsm v1.4.1` 的 `sm3` 包，没有引入 CGO。

```powershell
go install golang.org/x/vuln/cmd/govulncheck@v1.7.0
govulncheck -show verbose ./...
```

2026-09-06 本地 Go 1.26.1 的结果为 0 个可达调用链漏洞。报告仍列出旧标准库公告以及
x/text 模块的 [GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970)，后者涉及未被当前
解码路径调用的 `unicode/norm`。因此不能写成“所有依赖无漏洞”。增加包或调用符号后必须
重跑；CI 使用 stable Go 执行 govulncheck，发布也应使用受维护的最新补丁工具链。
Go 1.22 只保留源码兼容性测试，不作为生产二进制推荐构建工具链。

v0.10.0 发布复测使用 Go 1.27.1，Windows 完整测试/vet 通过；此次 govulncheck 结果为
0 个可达符号漏洞、0 个已导入包漏洞，以及上述 1 个未被调用的 x/text 模块级公告。
构建时使用 `-trimpath -s -w` 并注入 tag、提交号和 UTC 构建时间。

当前重点测试方向：

- `SYSTEM.DBF` 页大小、字符集、页保护字节。
- `dm.ctl` 控制文件解析。
- 字典表对象、字段、索引、约束解析。
- DDL 类型格式化。
- 数据页 slot array、树表、堆表、行长、NULL、长 varchar、NUMBER、DATE/DATETIME 解码。
- CLI 默认参数和错误提示。

新增一种行格式或数据类型时，建议先在 `internal/dm/data_test.go` 或相关测试文件中添加最小样本，再实现解析逻辑。

## 逆向研究约定

- 临时脚本放在 `research/`。
- 已验证并进入主流程的规则记录到 `docs/offline-system-scan.md`。
- 系统字典字段含义记录到 `docs/system-dictionary-fields.md`。
- 不要把生产库文件、导出 SQL、含密码的配置、VM 配置或虚拟磁盘提交到仓库。

## 发布前检查清单

```powershell
go test ./...
go build -o .\bin\dmdul.exe .\cmd\dmdul
.\bin\dmdul.exe help
.\bin\dmdul.exe version
```

如果有样例文件，再执行：

```text
DMDUL> set system oldpro\SYSTEM.DBF;
DMDUL> set data_dir oldpro;
DMDUL> bootstrap;
DMDUL> list user;
DMDUL> unload user SYSDBA;
```
