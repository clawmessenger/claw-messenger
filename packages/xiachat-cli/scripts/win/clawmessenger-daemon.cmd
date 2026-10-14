@echo off
rem clawmessenger background daemon - thin wrapper that forwards to the
rem PowerShell script sitting next to it. See CLAWMESSENGER_GUIDE.zh.md (Appendix A).
rem
rem   clawmessenger-daemon.cmd start            start in background (survives closing the window)
rem   clawmessenger-daemon.cmd stop             stop everything
rem   clawmessenger-daemon.cmd restart          restart
rem   clawmessenger-daemon.cmd status           show status
rem   clawmessenger-daemon.cmd logs             tail the log
rem   clawmessenger-daemon.cmd install-startup  silent auto-start on logon (no admin needed)
rem   clawmessenger-daemon.cmd install          scheduled-task auto-start (needs admin)
rem   clawmessenger-daemon.cmd uninstall        remove auto-start
setlocal
set "SCRIPT=%~dp0clawmessenger-daemon.ps1"
if not exist "%SCRIPT%" (
  echo [ERROR] clawmessenger-daemon.ps1 not found next to this file: "%SCRIPT%"
  exit /b 1
)
if "%~1"=="" (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT%" -Action status
  echo.
  echo usage: clawmessenger-daemon.cmd start / stop / restart / status / logs
  echo        clawmessenger-daemon.cmd install-startup / install / uninstall
  echo        extra args are passed through, e.g.:  ... start -Model gpt-5
  pause
  exit /b 0
)
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT%" %*
exit /b %errorlevel%
