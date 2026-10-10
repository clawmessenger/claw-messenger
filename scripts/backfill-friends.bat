@echo off
REM backfill-friends.bat - Windows wrapper for the friend-relationship backfill.
REM
REM Repairs RongCloud friend edges for devices bound BEFORE the bind flow started
REM writing them (that is why the bound device user was missing from 好友列表).
REM Idempotent; skips the machine infra node and the ops node; dry-run by default.
REM
REM Usage (from anywhere in the claw-messenger repo):
REM   scripts\backfill-friends.bat --all-users
REM   scripts\backfill-friends.bat --all-users --commit
REM   scripts\backfill-friends.bat --user 100123,100456 --commit
REM   scripts\backfill-friends.bat --sync-names --commit
REM
setlocal enabledelayedexpansion

set "SCRIPT_DIR=%~dp0"
for %%I in ("%SCRIPT_DIR%..") do set "REPO_ROOT=%%~fI"
set "SERVER_DIR=%REPO_ROOT%\server"

set "ENV_FILE="
if exist "%REPO_ROOT%\.env" set "ENV_FILE=%REPO_ROOT%\.env"
if not defined ENV_FILE if exist "%REPO_ROOT%\..\.env" set "ENV_FILE=%REPO_ROOT%\..\.env"
if not defined ENV_FILE if exist "%SERVER_DIR%\.env" set "ENV_FILE=%SERVER_DIR%\.env"

echo ==^> repo:     %REPO_ROOT%
echo ==^> server:   %SERVER_DIR%
if defined ENV_FILE (
  echo ==^> env file: %ENV_FILE%
) else (
  echo ==^> env file: ^(not found; using current environment only^)
)
echo.

set "HAS_ENV="
for %%A in (%*) do (
  if /I "%%~A"=="--env-file" set "HAS_ENV=1"
  if /I "%%~A"=="-env-file" set "HAS_ENV=1"
)

cd /d "%SERVER_DIR%"
if defined ENV_FILE if not defined HAS_ENV (
  go run ./cmd/backfill-friends --env-file "%ENV_FILE%" %*
) else (
  go run ./cmd/backfill-friends %*
)

endlocal
