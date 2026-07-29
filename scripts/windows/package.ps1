# package.ps1 (Task 12 Step 2): 生成 dist/windows/exam-system 生产发布包.
# 仅白名单复制 exam-server.exe / config.production.example.yaml / frontend /
# scoring_worker / scripts/windows/* / docs/*.md; 防止 backend/ data/ 等
# Python 老栈混入. 末段 manifest.sha256 校验.
[CmdletBinding()]
param(
    [string]$OutDir = "dist/windows/exam-system",
    [string]$SourceRoot = ".",
    [string]$ExamServerExe = ""
)
$ErrorActionPreference = 'Stop'
# 1) 干净 outdir (Build dest 隔离为全新 dist/windows/exam-system 真空)
if (Test-Path $OutDir) {
    Remove-Item -Recurse -Force $OutDir
}
New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
# 2) 找 exam-server.exe; 默认源 build/bin/, 或参数 -ExamServerExe 显式指定
$exePath = $ExamServerExe
if (-not $exePath) {
    $exePath = Join-Path $SourceRoot "build/bin/exam-server.exe"
}
if (-not (Test-Path $exePath)) {
    Write-Error "exam-server.exe 未找到: $exePath; 需先 GOOS=windows GOARCH=amd64 go build -o build/bin/exam-server.exe ./cmd/exam-server"
    exit 1
}
Write-Host "==> 复制白名单: exam-server.exe / config / frontend / scoring_worker / scripts / docs"
Copy-Item $exePath (Join-Path $OutDir "exam-server.exe")
Copy-Item (Join-Path $SourceRoot "config.production.example.yaml") $OutDir
# frontend 静态资源 (Go serve 必备)
$frontendSrc = Join-Path $SourceRoot "frontend"
if (Test-Path $frontendSrc) {
    Copy-Item $frontendSrc (Join-Path $OutDir "frontend") -Recurse -Filter:$false
} else {
    Write-Warning "frontend 不存在, dist 不打包前端. 检查 build 流程"
}
# scoring_worker (Python 评分 worker)
$swSrc = Join-Path $SourceRoot "scoring_worker"
if (Test-Path $swSrc) {
    Copy-Item $swSrc (Join-Path $OutDir "scoring_worker") -Recurse
} else {
    Write-Warning "scoring_worker 不存在"
}
# scripts/windows (start/stop + package 本身; install/uninstall 已按用户决断删除)
$scriptsDest = Join-Path $OutDir "scripts/windows"
New-Item -ItemType Directory -Path $scriptsDest -Force | Out-Null
Get-ChildItem -Path (Join-Path $SourceRoot "scripts/windows") -Filter *.ps1 | ForEach-Object {
    Copy-Item $_.FullName $scriptsDest
}
# docs (deployment-go-pg.md 等)
$docsDest = Join-Path $OutDir "docs"
New-Item -ItemType Directory -Path $docsDest -Force | Out-Null
Get-ChildItem -Path (Join-Path $SourceRoot "docs") -Filter *.md -ErrorAction SilentlyContinue | ForEach-Object {
    Copy-Item $_.FullName $docsDest
}
# 3) 防 Python 老栈混入: dist 不应含 backend/ data/ *.db (sqlite) *.pyc __pycache__
$forbidden = @("backend", "data", "config.yaml")
foreach ($bad in $forbidden) {
    $badPath = Join-Path $OutDir $bad
    if (Test-Path $badPath) {
        Write-Error "禁入文件混入 dist: $badPath — package 中止"
        exit 1
    }
}
Get-ChildItem -Path $OutDir -Recurse -Include *.db,*.pyc -ErrorAction SilentlyContinue | ForEach-Object {
    Write-Error "禁入文件混入 dist: $($_.FullName)"
    exit 1
}
Write-Host "==> 白名单复制 + 防混入 OK"
# 4) manifest.sha256: dist 内所有文件 sha256 hex 清单, 部署点用于完整性核对 (无 install.ps1, 手动核对)
$manifestPath = Join-Path $OutDir "manifest.sha256"
$sb = [System.Text.StringBuilder]::new()
Get-ChildItem -Path $OutDir -Recurse -File | Sort-Object FullName | ForEach-Object {
    $hash = (Get-FileHash -Path $_.FullName -Algorithm SHA256).Hash
    $rel = $_.FullName.Substring($OutDir.Length + 1).Replace("\", "/")
    [void]$sb.AppendLine("$hash  $rel")
}
[System.IO.File]::WriteAllText($manifestPath, $sb.ToString())
Write-Host "==> manifest.sha256 已生成: $manifestPath"
Write-Host "==> dist windows build complete: $OutDir"
