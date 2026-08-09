param(
    [Parameter(Mandatory = $true)]
    [string]$OutputDir
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

function Write-AsciiLfFile {
    param(
        [string]$Path,
        $Lines
    )
    $content = ([string[]]$Lines -join "`n") + "`n"
    [System.IO.File]::WriteAllText($Path, $content, [System.Text.Encoding]::ASCII)
}

$setup = @("mkdir -p '+META1/catalog'")
for ($dir = 0; $dir -lt 64; $dir++) {
    $setup += "mkdir -p '+META1/catalog/d{0:d3}'" -f $dir
}
$setup += "exit"
Write-AsciiLfFile (Join-Path $OutputDir "create-0000-dirs.txt") $setup

$stages = @(
    @(1, 1900, "create-1900.txt"),
    @(1901, 2000, "create-2000.txt"),
    @(2001, 4000, "create-4000.txt"),
    @(4001, 4100, "create-4100.txt"),
    @(4101, 5200, "create-5200.txt")
)

foreach ($stage in $stages) {
    $lines = [System.Collections.Generic.List[string]]::new()
    for ($id = $stage[0]; $id -le $stage[1]; $id++) {
        $dir = ($id - 1) % 64
        $lines.Add(("create asmfile '+META1/catalog/d{0:d3}/f{1:d6}.dat' size 1 external redundancy striping 0" -f $dir, $id))
    }
    $lines.Add("exit")
    Write-AsciiLfFile (Join-Path $OutputDir $stage[2]) $lines
}
