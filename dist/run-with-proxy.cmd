@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

rem === Allowlisted proxy (edit this line before you commit / ship) ===
set "PROXY_URL=http://203.0.113.10:3128"

if "%PROXY_URL%"=="" (
  echo PROXY_URL is empty in run-with-proxy.cmd
  pause
  exit /b 1
)

set "HTTPS_PROXY=%PROXY_URL%"
set "HTTP_PROXY=%PROXY_URL%"

"%~dp0go-frog-windows-amd64.exe"
exit /b !ERRORLEVEL!
