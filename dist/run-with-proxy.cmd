@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

rem === Allowlisted proxy (edit this line before you commit / ship) ===
set "PROXY_URL=http://128.199.217.199:7437"

if "%PROXY_URL%"=="" (
  echo PROXY_URL is empty in run-with-proxy.cmd
  pause
  exit /b 1
)

set "HTTPS_PROXY=%PROXY_URL%"
set "HTTP_PROXY=%PROXY_URL%"

"%~dp0go-frog-windows-amd64.exe"
exit /b !ERRORLEVEL!
