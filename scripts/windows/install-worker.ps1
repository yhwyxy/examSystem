# install-worker.ps1: Windows 部署机离线安装 scoring_worker 依赖.
# 全部 wheel 自带于 packages/ (subjective-scoring v0.1.13 + 依赖 + psycopg),
# 全程 --no-index --find-links 本地解析, 不访问 GitHub / PyPI.
[CmdletBinding()]
param(
    [string]$InstallRoot = ".",
    [string]$VenvDir = "C:\exam-venv",
    [switch]$WithLocalModel
)
$ErrorActionPreference = 'Stop'

# 1) 确认 packages/ 存在
$pkgs = Join-Path $InstallRoot "packages"
if (-not (Test-Path $pkgs)) {
    Write-Error "packages/ 不存在于 $InstallRoot; 请确认随发布包拷贝了离线 wheel 目录"
    exit 1
}

# 2) 创建 venv (要求 Python 3.12; scoring_worker/pyproject.toml 限定 >=3.12,<3.13)
$venvPy = Join-Path $VenvDir "Scripts\python.exe"
if (-not (Test-Path $venvPy)) {
    Write-Host "==> 创建 venv: $VenvDir (Python 3.12)"
    py -3.12 -m venv $VenvDir
    if ($LASTEXITCODE -ne 0) {
        Write-Error "venv 创建失败; 请确认已安装 Python 3.12 且 py -3.12 可用"
        exit 1
    }
} else {
    Write-Host "==> venv 已存在: $VenvDir (复用)"
}

# 3) 离线安装依赖 (与 scoring_worker/pyproject.toml 依赖清单保持一致)
$deps = @(
    "psycopg[binary]>=3.2,<4",
    "pyyaml>=6.0,<7",
    "python-dotenv>=1.0,<2",
    "subjective-scoring[text,sql,code,remote]"
)
if ($WithLocalModel) {
    $deps += "sentence-transformers>=3.0", "torch>=2.2,<3"
    Write-Host "==> 含本地语义模型依赖 (sentence-transformers + torch, 体积较大)"
}
Write-Host "==> 离线安装 (--no-index --find-links packages):"
& $venvPy -m pip install --no-index --find-links $pkgs @deps
if ($LASTEXITCODE -ne 0) {
    Write-Error "离线安装失败; 检查 packages/ 是否完整 (缺 wheel 时在联网机器上用 pip download 补齐)"
    exit 1
}

# 4) 验证关键包
Write-Host "==> 验证关键包版本:"
& $venvPy -c "from importlib.metadata import version; print('subjective-scoring', version('subjective-scoring'))"
& $venvPy -c "import psycopg; print('psycopg', psycopg.__version__)"
if ($WithLocalModel) {
    & $venvPy -c "import sentence_transformers; print('sentence-transformers', sentence_transformers.__version__)"
}
Write-Host "==> install-worker.ps1 完成."
Write-Host "    下一步健康自检: $venvPy -m scoring_worker --preflight (需先设好 DATABASE_URL / RERANKER_MODEL)"
