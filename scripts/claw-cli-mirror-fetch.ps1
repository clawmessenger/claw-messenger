<#
claw-cli-mirror-fetch.ps1 —— 在「能上网」的本机把 CLI Release 资产下全、校验、打成一个包，
供服务器离线激活。适用前提：服务器自己连不上 GitHub（本项目当前就是这种情况）。

为什么需要它：claw-cli-mirror-sync.sh 默认是「服务器自己去 GitHub 拉」。服务器出不了网时，
那条路整条失效，只剩 --from-dir（本机下好传上来再激活）。本脚本就是 --from-dir 的供料端，
把「下 6 个文件 + 校验 + 打包 + 给服务器命令」合成一条命令。

用法（PowerShell 5.1 / 7 均可）：
  powershell -ExecutionPolicy Bypass -File claw-cli-mirror-fetch.ps1
  powershell -ExecutionPolicy Bypass -File claw-cli-mirror-fetch.ps1 -Proxy http://127.0.0.1:7890
  powershell -ExecutionPolicy Bypass -File claw-cli-mirror-fetch.ps1 -OutDir D:\claw-mirror
  powershell -ExecutionPolicy Bypass -File claw-cli-mirror-fetch.ps1 -Only clawmessenger-windows-x64.exe
  powershell -ExecutionPolicy Bypass -File claw-cli-mirror-fetch.ps1 -Tag clawmessenger-v1.3.4

参数：
  -OutDir   下载与暂存目录（默认 %TEMP%\claw-mirror）
  -Only     只下这几个资产（逗号分隔）；服务器激活时要带同样的 --only，或依赖包内 MANIFEST 自动对齐
  -Proxy    显式代理地址，如 http://127.0.0.1:7890 或 socks5h://127.0.0.1:1080
  -Tag      解析不到 releases/latest 时手动指定版本号
  -Bases    自定义下载基址（默认 github 直连 + 三个公共加速前缀）
  -NoPackage 只下载校验，不打包

产物：<OutDir 的上一级>\claw-cli-mirror-<tag>.tar.gz
  内含 6 个资产 + SHA256SUMS + VERSION + MANIFEST + 激活脚本，传到服务器解包即可 --from-dir。
#>

[CmdletBinding()]
param(
  [string]$OutDir = (Join-Path $env:TEMP 'claw-mirror'),
  [string[]]$Only = @(),
  [string]$Proxy = '',
  [string]$Tag = '',
  [string[]]$Bases = @(),
  [switch]$NoPackage
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'clawmessenger/claw-messenger'
$defaultAssets = @(
  'clawmessenger-windows-x64.exe',
  'clawmessenger-windows-x64-daemon-scripts.zip',
  'clawmessenger-linux-x64',
  'clawmessenger-linux-arm64',
  'clawmessenger-linux-daemon-scripts.tar.gz',
  'clawmessenger-darwin-arm64'
)

function Write-Step($msg) { Write-Host "==> $msg" }
function Write-Warn($msg) { Write-Host "warn: $msg" -ForegroundColor Yellow }
function Fail($msg) { Write-Host "error: $msg" -ForegroundColor Red; exit 1 }

# curl.exe 是 Windows 10 1803+ 自带的真 curl。PowerShell 里的 curl 是 Invoke-WebRequest 别名，
# 不能带这些参数，所以一律写全名。
$curl = (Get-Command curl.exe -ErrorAction SilentlyContinue)
if (-not $curl) { Fail "找不到 curl.exe（Windows 10 1803 之后自带；老系统装一个再跑）" }

$assets = if ($Only.Count -gt 0) { $Only } else { $defaultAssets }

if ($Bases.Count -eq 0) {
  $prefixes = @(
    'https://github.com',
    'https://ghfast.top/https://github.com',
    'https://ghproxy.net/https://github.com',
    'https://gh-proxy.com/https://github.com'
  )
  $Bases = $prefixes | ForEach-Object { "$_/$repo/releases/latest/download" }
}

$curlProxy = @()
if ($Proxy) { $curlProxy = @('-x', $Proxy) }

function Get-Url($url, $dest) {
  $cargs = @('-fsSL', '--retry', '2', '--connect-timeout', '15', '--max-time', '1800', '-o', $dest, $url)
  $cargs = $curlProxy + $cargs
  & curl.exe @cargs 2>$null
  return ($LASTEXITCODE -eq 0)
}

if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Force -Path $OutDir | Out-Null }
$OutDir = (Resolve-Path $OutDir).Path

# ---------------------------------------------------------------- 1. 版本号
# 只用 releases/latest 的 302 拿 tag，不碰 api.github.com（匿名额度 60/小时，共享出口必然 403）。
if (-not $Tag) {
  Write-Step '解析 GitHub 最新版本（releases/latest 跳转）'
  foreach ($b in $Bases) {
    $page = $b -replace '/download$', ''
    $cargs = @('-sIL', '-o', 'NUL', '-w', '%{url_effective}', '--max-time', '25', $page)
    $cargs = $curlProxy + $cargs
    $eff = & curl.exe @cargs 2>$null
    if ($eff -match '/releases/tag/([^/?#]+)') {
      $Tag = $Matches[1]
      Write-Host "    latest = $Tag  (via $b)"
      break
    }
  }
}
if (-not $Tag) {
  Fail '拿不到最新版本号（本地网络也不通？）。可以先用梯子，或加 -Tag clawmessenger-vX.Y.Z 手动指定。'
}

# ---------------------------------------------------------------- 2. SHA256SUMS
# 先拿清单：后面每个包下完都立刻比对，镜像给旧包/给错误页都能当场发现。
Write-Step '下载 SHA256SUMS'
$sumsPath = Join-Path $OutDir 'SHA256SUMS'
$sumsOk = $false
foreach ($b in $Bases) {
  if (Get-Url "$b/SHA256SUMS" $sumsPath) {
    $head = Get-Content -LiteralPath $sumsPath -TotalCount 1
    if ($head -match '^[0-9a-fA-F]{64}\s') { $sumsOk = $true; Write-Host "    <- $b"; break }
    Write-Warn "$b 返回的不是哈希清单（镜像错误页？），换下一个源"
  }
}
if (-not $sumsOk) { Fail 'SHA256SUMS 下载失败。没有哈希清单就无法校验，不做无校验安装。' }

$expected = @{}
Get-Content -LiteralPath $sumsPath | ForEach-Object {
  if ($_ -match '^([0-9a-fA-F]{64})\s+\*?(\S+)$') { $expected[$Matches[2]] = $Matches[1].ToLower() }
}
if ($expected.Count -eq 0) { Fail 'SHA256SUMS 解析不出任何条目，包可能不完整' }

# ---------------------------------------------------------------- 3. 逐个资产
$sizeHint = if ($Only.Count -eq 0) { '（全量约 432MB，打包后约 148MB；中断可重跑，已校验过的会跳过）' } else { '（中断可重跑，已校验过的会跳过）' }
Write-Step "下载 $($assets.Count) 个资产$sizeHint"
$okAssets = @()
foreach ($asset in $assets) {
  if (-not $expected.ContainsKey($asset)) { Fail "SHA256SUMS 里没有 $asset，检查 -Only 拼写" }
  $dest = Join-Path $OutDir $asset

  if (Test-Path $dest) {
    $h = (Get-FileHash -LiteralPath $dest -Algorithm SHA256).Hash.ToLower()
    if ($h -eq $expected[$asset]) { Write-Host "  跳过 $asset（已存在且哈希一致）"; $okAssets += $asset; continue }
    Write-Warn "$asset 已存在但哈希不符（上次没下完？），重下"
    Remove-Item -LiteralPath $dest -Force
  }

  $got = $false
  foreach ($b in $Bases) {
    Write-Host "  下载 $asset  <- $b"
    if (-not (Get-Url "$b/$asset" $dest)) { Write-Host '     不通，换下一个源'; continue }
    $h = (Get-FileHash -LiteralPath $dest -Algorithm SHA256).Hash.ToLower()
    if ($h -eq $expected[$asset]) { Write-Host "     OK  sha256:$($h.Substring(0,12))"; $got = $true; break }
    Write-Warn "    哈希不符（镜像缓存旧包或错误页），换下一个源"
    Remove-Item -LiteralPath $dest -Force
  }
  if (-not $got) { Fail "$asset 所有源都拿不到正确包。挂代理重试：-Proxy http://127.0.0.1:7890" }
  $okAssets += $asset
}

# ---------------------------------------------------------------- 4. VERSION / MANIFEST
# VERSION 让服务器激活后 dl/VERSION 显示正确 tag（离线时脚本自己推不出来）。
# MANIFEST 让服务器不必重复指定 --only，资产集合在包内自描述，避免「两边清单不一致」。
Set-Content -LiteralPath (Join-Path $OutDir 'VERSION') -Value $Tag -Encoding ASCII
Set-Content -LiteralPath (Join-Path $OutDir 'MANIFEST') -Value $okAssets -Encoding ASCII
Write-Host "    VERSION = $Tag"

# ---------------------------------------------------------------- 5. 打包
$bundle = $null
if (-not $NoPackage) {
  Write-Step '打包（含激活脚本，服务器只需传这一个文件）'
  $syncSrc = Join-Path $PSScriptRoot 'claw-cli-mirror-sync.sh'
  if (Test-Path $syncSrc) {
    Copy-Item -LiteralPath $syncSrc -Destination (Join-Path $OutDir 'claw-cli-mirror-sync.sh') -Force
  } else {
    Write-Warn "同目录下没找到 claw-cli-mirror-sync.sh，包里不含激活脚本（服务器上需另传）"
  }
  $parentDir = Split-Path $OutDir -Parent
  $tarExe = Get-Command tar.exe -ErrorAction SilentlyContinue
  if ($tarExe) {
    $bundle = Join-Path $parentDir ("claw-cli-mirror-" + ($Tag -replace '^clawmessenger-v', '') + ".tar.gz")
    if (Test-Path $bundle) { Remove-Item -LiteralPath $bundle -Force }
    & tar.exe -czf $bundle -C $OutDir .
    if ($LASTEXITCODE -ne 0) { Fail 'tar 打包失败' }
  } else {
    $bundle = Join-Path $parentDir ("claw-cli-mirror-" + ($Tag -replace '^clawmessenger-v', '') + ".zip")
    if (Test-Path $bundle) { Remove-Item -LiteralPath $bundle -Force }
    Compress-Archive -Path (Join-Path $OutDir '*') -DestinationPath $bundle -Force
    Write-Warn '没有 tar.exe，产出的是 .zip，服务器上用 unzip 解（本机建议装 tar 以产出 tar.gz）'
  }
}

# ---------------------------------------------------------------- 6. 汇总 + 下一步
$total = 0
foreach ($a in $okAssets) { $total += (Get-Item -LiteralPath (Join-Path $OutDir $a)).Length }
$totalText = if ($total -ge 1MB) { "{0:N1} MB" -f ($total / 1MB) } else { "{0:N0} KB" -f ($total / 1KB) }

$lines = New-Object System.Collections.Generic.List[string]
$lines.Add('')
$lines.Add("完成：$Tag   资产 $($okAssets.Count) 个，共 $totalText")
foreach ($a in $okAssets) {
  $f = Get-Item -LiteralPath (Join-Path $OutDir $a)
  $h = (Get-FileHash -LiteralPath $f.FullName -Algorithm SHA256).Hash.ToLower().Substring(0, 12)
  $lines.Add(("  {0,-46} {1,12} bytes  sha256:{2}" -f $a, $f.Length, $h))
}
if ($bundle) {
  $bs = if ((Get-Item -LiteralPath $bundle).Length -ge 1MB) {
    "{0:N1} MB" -f ((Get-Item -LiteralPath $bundle).Length / 1MB)
  } else { "{0:N0} KB" -f ((Get-Item -LiteralPath $bundle).Length / 1KB) }
  $lines.Add('')
  $lines.Add("上传这个文件：$bundle  ($bs)")
}

$bundleName = if ($bundle) { Split-Path $bundle -Leaf } else { '<bundle>.tar.gz' }
$appRoot = '/data/im/shrimp-says-test'
$webroot = "$appRoot/claw-web-product"
$mirrorDir = "$appRoot/claw-mirror"
$lines.Add('')
$lines.Add('── 接下来在服务器上执行（包内自带 VERSION / MANIFEST / 激活脚本）────────────')
$lines.Add('  ① 先只解出脚本装到 PATH（40KB，不用解 400MB）')
$lines.Add("     tar xzf /tmp/$bundleName -C /tmp ./claw-cli-mirror-sync.sh")
$lines.Add('     install -m 755 /tmp/claw-cli-mirror-sync.sh /usr/local/bin/claw-cli-mirror-sync')
$lines.Add('  ② 一条命令激活：解包、读包内清单、校验、切换、发布，全在里面')
$lines.Add("     claw-cli-mirror-sync --bundle /tmp/$bundleName")
$lines.Add('  ③ 自检')
$lines.Add("     claw-cli-mirror-sync --verify-only")
$lines.Add('     curl -s https://newsradar.dreamdt.cn/claw-web/dl/VERSION; echo')
$lines.Add('')
$lines.Add('（路径即线上实际布局；资产集合在 MANIFEST 里、版本号在 VERSION 里，都不用再传参）')
Write-Host ($lines -join "`n")
