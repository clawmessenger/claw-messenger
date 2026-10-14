#requires -Version 5.1
<#
  clawmessenger 后台守护工具（Windows）

  为什么需要它：
    `clawmessenger run`（不带 --agent 就是 supervisor 模式）只有前台模式，
    进程一退，它拉起的所有 agent 子进程一起消失 -> 智能体全部下线。
    CLI 里没有 --daemon / service 之类的参数，所以只能在 OS 层把它放到后台。

  本脚本做的事：
    start            隐藏窗口 + 独立控制台，把 supervisor 丢到后台跑（关掉 cmd 不影响）
    stop             taskkill /T 连子进程一起结束（Windows 上 Stop-Process 收不到 SIGTERM）
    restart          停掉再后台起
    status           看有没有在跑、PID、起了几个 agent 子进程、自启有没有配
    logs             看最新一份日志的尾巴
    run              前台阻塞跑（给计划任务/启动文件夹内部调用，不建议手动用）
    install          注册计划任务（登录自启 + 崩溃自动重试）**需要管理员**
    install-startup  写一个 .vbs 到「启动」文件夹（登录静默自启，**不需要管理员**）
    uninstall        摘掉上面两种自启配置（不删 exe、不删日志）

  用法：
    powershell -NoProfile -ExecutionPolicy Bypass -File clawmessenger-daemon.ps1 -Action start
    或直接双击同目录的 clawmessenger-daemon.cmd

  可选参数：
    -Exe <路径>      显式指定 clawmessenger.exe（默认从 PATH / %LOCALAPPDATA%\clawmessenger 找）
    -Model <名字>    透传给 `clawmessenger run --model <名字>`
    -WorkDir <路径>  supervisor 的工作目录，也是 agent 轮次的 cwd（默认 = exe 所在目录）
    -TaskName <名字> 计划任务名，默认 "ClawMessenger Agents"

  注意：必须在**同一个 Windows 用户**下运行。凭据在 %USERPROFILE%\.clawmessenger ，
  换成别的账号（比如以 SYSTEM 跑服务）会找不到凭据、变成另一批节点。
#>
[CmdletBinding()]
param(
  [ValidateSet('start', 'stop', 'restart', 'status', 'run', 'logs', 'install', 'install-startup', 'uninstall')]
  [string]$Action = 'status',
  [string]$Exe,
  [string]$Model,
  [string]$WorkDir,
  [string]$TaskName = 'ClawMessenger Agents'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$InstallDir = Join-Path $env:LOCALAPPDATA 'clawmessenger'
$StateDir   = Join-Path $env:USERPROFILE '.clawmessenger'
$LogDir     = Join-Path $StateDir 'logs'
$PidFile    = Join-Path $StateDir 'supervisor.pid'
$SelfCopied = Join-Path $InstallDir 'clawmessenger-daemon.ps1'
$StartupVbs = Join-Path ([Environment]::GetFolderPath('Startup')) 'clawmessenger.vbs'

# ---------------------------------------------------------------- 基础工具

function Write-Ok($msg)   { Write-Host $msg -ForegroundColor Green }
function Write-Warn2($msg) { Write-Host $msg -ForegroundColor Yellow }
function Write-Err2($msg)  { Write-Host $msg -ForegroundColor Red }

# Windows PowerShell 5.1 的 Start-Process 会把当前进程环境重建进一个**大小写不敏感**
# 的 StringDictionary；一旦环境里同时存在 http_proxy 与 HTTP_PROXY 这种仅大小写不同的
# 变量（Clash/v2ray 之类的代理工具很常见），就会抛 ArgumentException「已添加项」。
# 这里先把「忽略大小写后同名」的变量合并成一个（值保留先遇到的那个），
# 只影响本启动器进程，子进程拿到的是去重后的环境。
function Optimize-ProcessEnvironment {
  for ($pass = 0; $pass -lt 5; $pass++) {
    $keys = @([Environment]::GetEnvironmentVariables('Process').Keys)
    $seen = New-Object 'System.Collections.Generic.HashSet[string]' ([StringComparer]::OrdinalIgnoreCase)
    $removed = 0
    foreach ($k in $keys) {
      if (-not $seen.Add($k)) {
        [Environment]::SetEnvironmentVariable($k, $null, 'Process')
        $removed++
      }
    }
    if ($removed -eq 0) { return }
  }
}

function Resolve-CmExe {
  if ($Exe) {
    if (-not (Test-Path -LiteralPath $Exe)) { throw "-Exe 指定的文件不存在: $Exe" }
    return (Resolve-Path -LiteralPath $Exe).Path
  }
  $cmd = Get-Command clawmessenger -CommandType Application -ErrorAction SilentlyContinue |
         Where-Object { $_.Extension -eq '.exe' } |
         Select-Object -First 1
  if ($cmd) { return $cmd.Source }
  $fallback = Join-Path $InstallDir 'clawmessenger.exe'
  if (Test-Path -LiteralPath $fallback) { return $fallback }
  throw "找不到 clawmessenger.exe。请先在这台设备上执行 `clawmessenger pair`，或用 -Exe <路径> 指定。"
}

function Resolve-WorkingDir([string]$ExePath) {
  if ($WorkDir) {
    if (-not (Test-Path -LiteralPath $WorkDir)) { throw "-WorkDir 指定的目录不存在: $WorkDir" }
    return (Resolve-Path -LiteralPath $WorkDir).Path
  }
  return (Split-Path -Parent $ExePath)
}

# supervisor 是通过 pid 文件记录的；进程名必须对得上，避免 PID 被回收后误判。
# 没有 pid 文件时（比如历史上是用 bat 手动启动的）回退到命令行特征识别：
# supervisor 的 argv 是 `run`，它的 agent 子进程是 `run --agent <名字>`。
function Find-SupervisorByCommandLine {
  $cand = Get-CimInstance -ClassName Win32_Process -Filter "Name='clawmessenger.exe'" -ErrorAction SilentlyContinue |
          Where-Object {
            $_.CommandLine -and
            $_.CommandLine -match '\srun(\s|$)' -and
            $_.CommandLine -notmatch '--agent'
          } |
          Select-Object -First 1
  if (-not $cand) { return $null }
  return (Get-Process -Id $cand.ProcessId -ErrorAction SilentlyContinue)
}

function Get-SupervisorProcess {
  if (Test-Path -LiteralPath $PidFile) {
    $raw = ''
    try { $raw = [string](Get-Content -LiteralPath $PidFile -Raw -ErrorAction Stop) } catch { $raw = '' }
    $parsed = 0
    if ([int]::TryParse($raw.Trim(), [ref]$parsed)) {
      $p = Get-Process -Id $parsed -ErrorAction SilentlyContinue
      if ($p -and $p.ProcessName -like 'clawmessenger*') { return $p }
    }
  }
  return (Find-SupervisorByCommandLine)
}

function Get-AgentChildren([int]$ParentProcessId) {
  $all = @(Get-CimInstance -ClassName Win32_Process -Filter "ParentProcessId=$ParentProcessId" -ErrorAction SilentlyContinue |
           Where-Object { $_.Name -like 'clawmessenger*' })
  return $all
}

function New-LogPaths {
  if (-not (Test-Path -LiteralPath $LogDir)) {
    New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
  }
  $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
  return @{
    Out = Join-Path $LogDir "supervisor-$stamp.out.log"
    Err = Join-Path $LogDir "supervisor-$stamp.err.log"
  }
}

function Get-RunArgs {
  $list = @('run')
  if ($Model) { $list += @('--model', $Model) }
  return $list
}

# ---------------------------------------------------------------- 动作实现

# 后台启动：-WindowStyle Hidden 会给 exe 分配一个**独立的隐藏控制台**，
# 因此它不属于启动它的那个窗口 —— 关掉 cmd / PowerShell 都不会牵连它。
function Start-CmBackground {
  $exePath = Resolve-CmExe
  $running = Get-SupervisorProcess
  if ($running) {
    Write-Warn2 "检测到已有 supervisor 在运行（PID $($running.Id)），不重复启动 —— 两个 supervisor 会重复拉起同一批 agent。"
    Write-Host  '  想改成后台常驻模式：先 -Action stop，再 -Action start。'
    return
  }
  $logs = New-LogPaths
  $wd = Resolve-WorkingDir $exePath
  $proc = Start-Process -FilePath $exePath -ArgumentList (Get-RunArgs) -WorkingDirectory $wd `
            -WindowStyle Hidden `
            -RedirectStandardOutput $logs.Out -RedirectStandardError $logs.Err `
            -PassThru
  Set-Content -LiteralPath $PidFile -Value $proc.Id -Encoding ASCII
  Start-Sleep -Seconds 3
  if ($proc.HasExited) {
    Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
    Write-Err2 "启动后立刻退出了（exit=$($proc.ExitCode)）。日志："
    Write-Host "  $($logs.Out)"
    if (Test-Path -LiteralPath $logs.Err) {
      Get-Content -LiteralPath $logs.Err -Tail 20 | ForEach-Object { Write-Host "  $_" }
    }
    return
  }
  Write-Ok "已后台启动：supervisor PID $($proc.Id)"
  Write-Host "  工作目录 : $wd"
  Write-Host "  日志     : $($logs.Out)"
  Write-Host "  查看状态 : -Action status ；停止：-Action stop"
  Write-Host "  提示：心跳/上线要几秒到十几秒，稍后在管理端刷新设备列表。"
}

# 前台阻塞运行：给计划任务 / 启动文件夹内部用。
# 任务动作必须**阻塞**在 supervisor 上，任务生命周期才能跟进程一致，
# 「失败后自动重启」才有意义。
function Run-CmForeground {
  $exePath = Resolve-CmExe
  $logs = New-LogPaths
  $wd = Resolve-WorkingDir $exePath
  $proc = Start-Process -FilePath $exePath -ArgumentList (Get-RunArgs) -WorkingDirectory $wd `
            -NoNewWindow `
            -RedirectStandardOutput $logs.Out -RedirectStandardError $logs.Err `
            -PassThru
  Set-Content -LiteralPath $PidFile -Value $proc.Id -Encoding ASCII
  $proc.WaitForExit()
  Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
  exit $proc.ExitCode
}

function Stop-CmSupervisor {
  $running = Get-SupervisorProcess
  if (-not $running) {
    if (Test-Path -LiteralPath $PidFile) { Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue }
    Write-Warn2 '没有在后台运行的 clawmessenger supervisor。'
    return
  }
  $supId = $running.Id
  $children = Get-AgentChildren $supId
  Write-Host "正在停止 supervisor PID $supId（连带 $($children.Count) 个 agent 子进程）..."
  # Windows 上没有 POSIX 信号：Stop-Process 走 TerminateProcess，Node 收不到
  # SIGTERM，supervisor 的优雅关闭和子进程清理都不会执行，会留下孤儿 agent。
  # 所以必须用 taskkill /T 把整棵进程树一起结束。
  & taskkill.exe /PID $supId /T /F | Out-Null
  Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 800
  if (Get-Process -Id $supId -ErrorAction SilentlyContinue) {
    Write-Err2 "停止失败，PID $supId 仍然存在。可以再执行一次，或在任务管理器里结束。"
  } else {
    Write-Ok '已停止。子进程心跳停下后，管理端大约 90 秒后显示离线。'
  }
}

function Show-CmStatus {
  $exePath = $null
  try { $exePath = Resolve-CmExe } catch { }
  if ($exePath) { Write-Host "exe      : $exePath" }
  else          { Write-Warn2 'exe      : 未找到 clawmessenger.exe' }

  Write-Host '自启     :'
  $autoFound = $false
  $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if ($task) {
    Write-Host "  计划任务「$TaskName」已注册（状态 $($task.State)）"
    $autoFound = $true
  }
  if (Test-Path -LiteralPath $StartupVbs) {
    Write-Host "  启动文件夹 : $StartupVbs"
    $autoFound = $true
  }
  if (-not $autoFound) { Write-Host '  未配置（用 -Action install-startup 免管理员配置，或 install 配计划任务）' }

  $running = Get-SupervisorProcess
  if (-not $running) {
    Write-Warn2 '运行状态 : 未在后台运行（智能体应为离线）'
    return
  }
  $uptime = (Get-Date) - $running.StartTime
  $up = '{0}天{1}时{2}分' -f $uptime.Days, $uptime.Hours, $uptime.Minutes
  Write-Ok "运行状态 : 后台运行中（PID $($running.Id)，已运行 $up）"
  if (-not (Test-Path -LiteralPath $PidFile)) {
    Write-Warn2 "  注意：没有 pid 文件，说明它是在 cmd/bat 里直接前台启动的（关掉那个窗口就会掉线）。"
    Write-Warn2 "  想让它真正脱离窗口常驻：先 -Action stop，再 -Action start。"
  }
  $children = Get-AgentChildren $running.Id
  Write-Host "agent 子进程 : $($children.Count) 个"
  foreach ($c in $children) {
    Write-Host ("  - PID {0}  {1}" -f $c.ProcessId, $c.CommandLine)
  }
  Write-Host "日志目录 : $LogDir"
}

function Show-CmLogs {
  if (-not (Test-Path -LiteralPath $LogDir)) {
    Write-Warn2 "还没有日志目录：$LogDir"
    return
  }
  $file = Get-ChildItem -LiteralPath $LogDir -Filter 'supervisor-*.log' -File -ErrorAction SilentlyContinue |
          Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if (-not $file) {
    Write-Warn2 '没有找到 supervisor 日志。'
    return
  }
  Write-Host "最新日志：$($file.FullName)（$($file.LastWriteTime)）" -ForegroundColor Cyan
  Get-Content -LiteralPath $file.FullName -Tail 40
}

# 把本脚本复制到安装目录，让自启项不依赖仓库/下载目录的位置。
function Get-DaemonScriptPath {
  $src = $PSCommandPath
  if (-not $src) { $src = $SelfCopied }
  if (-not (Test-Path -LiteralPath $src)) {
    throw '无法定位本脚本自身路径，请用 -File 方式调用（例如通过同目录的 clawmessenger-daemon.cmd）。'
  }
  $resolvedSrc = (Resolve-Path -LiteralPath $src).Path
  if (-not (Test-Path -LiteralPath $InstallDir)) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  }
  $needCopy = $true
  if (Test-Path -LiteralPath $SelfCopied) {
    if ((Resolve-Path -LiteralPath $SelfCopied).Path -eq $resolvedSrc) { $needCopy = $false }
  }
  if ($needCopy) { Copy-Item -LiteralPath $resolvedSrc -Destination $SelfCopied -Force }
  # 顺手把同目录的 .cmd 包装也复制过去，安装目录里就有个可直接调用/双击的短入口。
  $srcCmd = Join-Path (Split-Path -Parent $resolvedSrc) 'clawmessenger-daemon.cmd'
  $dstCmd = Join-Path $InstallDir 'clawmessenger-daemon.cmd'
  if (Test-Path -LiteralPath $srcCmd) {
    $resolvedCmdSrc = (Resolve-Path -LiteralPath $srcCmd).Path
    if (-not (Test-Path -LiteralPath $dstCmd) -or ((Resolve-Path -LiteralPath $dstCmd).Path -ne $resolvedCmdSrc)) {
      Copy-Item -LiteralPath $resolvedCmdSrc -Destination $dstCmd -Force
    }
  }
  return $SelfCopied
}

# 统一的「让 powershell 以隐藏窗口跑 run 动作」参数串。
# 注意：只有在单引号/双引号拼接时才能正确保留内层引号，不要写成 ""$x""（PS 会把它
# 当成转义的单个引号吞掉）——这正是 VBS 版本曾经生成出非法脚本的原因。
function Get-RunArgLine([string]$ScriptPath, [string]$ExePath, [string]$WorkingDir) {
  $q = [char]34
  $line = "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File ${q}${ScriptPath}${q} -Action run -Exe ${q}${ExePath}${q} -WorkDir ${q}${WorkingDir}${q}"
  if ($Model) { $line += " -Model ${q}${Model}${q}" }
  return $line
}

function Install-CmTask {
  $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
               [Security.Principal.WindowsBuiltInRole]::Administrator)
  if (-not $isAdmin) {
    throw "注册计划任务需要管理员权限。请用『以管理员身份运行』的 PowerShell 再执行；或改用免管理员的 -Action install-startup。"
  }
  $scriptPath = Get-DaemonScriptPath
  $exePath = Resolve-CmExe
  $wd = Resolve-WorkingDir $exePath
  $psExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
  $argLine = Get-RunArgLine $scriptPath $exePath $wd

  $taskAction = New-ScheduledTaskAction -Execute $psExe -Argument $argLine -WorkingDirectory $wd
  $who = $env:USERNAME
  if ($env:USERDOMAIN) { $who = "$env:USERDOMAIN\$env:USERNAME" }
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $who
  $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew

  Register-ScheduledTask -TaskName $TaskName -Action $taskAction -Trigger $trigger -Settings $settings `
    -Description 'clawmessenger 智能体 supervisor：登录后隐藏后台常驻，异常退出自动重试。' -Force | Out-Null

  Write-Ok "已注册计划任务「$TaskName」"
  Write-Host '  触发   : 当前用户登录时'
  Write-Host '  行为   : 隐藏窗口后台常驻；异常退出后每分钟自动重试'
  Write-Host '  生效   : 下次登录自动生效；现在想立刻跑请执行 -Action start'
  Write-Host "  取消   : -Action uninstall"
}

function Install-CmStartup {
  $scriptPath = Get-DaemonScriptPath
  $exePath = Resolve-CmExe
  $wd = Resolve-WorkingDir $exePath
  $psExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
  $argLine = Get-RunArgLine $scriptPath $exePath $wd

  # VBS 字符串字面量里的引号必须写成两个连续引号（""），所以先转义再拼行。
  $vbsArg = $argLine -replace '"', '""'

  # VBS 的 0 = 隐藏窗口、False = 不等待 -> 完全不闪窗。VBS 按 ANSI 读取，用 Default 编码写。
  $lines = @(
    "' clawmessenger 智能体 supervisor：登录时后台静默启动。删除本文件即可取消自启。",
    'Set s = CreateObject("WScript.Shell")',
    ('s.CurrentDirectory = "' + $wd + '"'),
    ('s.Run "' + $psExe + ' ' + $vbsArg + '", 0, False')
  )
  Set-Content -LiteralPath $StartupVbs -Value $lines -Encoding Default

  Write-Ok "已写入登录自启：$StartupVbs"
  Write-Host '  行为 : 登录时隐藏窗口后台常驻（不需要管理员权限）'
  Write-Host '  生效 : 下次登录自动生效；现在想立刻跑请执行 -Action start'
  Write-Host '  取消 : -Action uninstall'
}

function Uninstall-CmAutoStart {
  $removed = @()
  $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if ($task) {
    try {
      Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
      $removed += "计划任务「$TaskName」"
    } catch {
      Write-Warn2 "删除计划任务失败（可能需要在管理员 PowerShell 下执行）：$($_.Exception.Message)"
    }
  }
  if (Test-Path -LiteralPath $StartupVbs) {
    Remove-Item -LiteralPath $StartupVbs -Force
    $removed += $StartupVbs
  }
  if ($removed.Count -eq 0) {
    Write-Warn2 '没有发现已配置的自启项。'
  } else {
    Write-Ok ('已移除：' + ($removed -join '、'))
  }
  Write-Host '（正在后台跑的进程没动；要停请执行 -Action stop）'
}

# ---------------------------------------------------------------- 入口

# 必须在任何 Start-Process 之前执行，否则会被上面注释里的环境变量大小写冲突炸掉。
Optimize-ProcessEnvironment

switch ($Action) {
  'start'           { Start-CmBackground }
  'stop'            { Stop-CmSupervisor }
  'restart'         { Stop-CmSupervisor; Start-Sleep -Seconds 2; Start-CmBackground }
  'status'          { Show-CmStatus }
  'run'             { Run-CmForeground }
  'logs'            { Show-CmLogs }
  'install'         { Install-CmTask }
  'install-startup' { Install-CmStartup }
  'uninstall'       { Uninstall-CmAutoStart }
}
