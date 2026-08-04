[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

if (-not $env:DATABASE_URL) { $env:DATABASE_URL = 'postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable' }
if (-not $env:WORKER_ID) { $env:WORKER_ID = 'dev-worker-1' }
if (-not $env:MULTIPLE_CHOICE_PARTIAL) { $env:MULTIPLE_CHOICE_PARTIAL = 'true' }
if (-not $env:RERANK_USE_REMOTE) { $env:RERANK_USE_REMOTE = 'false' }

$py = Join-Path (Get-Location) '.venv\Scripts\python.exe'
if (-not (Test-Path $py)) { $py = 'python' }
& $py -m scoring_worker @args
