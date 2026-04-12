@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

rem This repo is public — no proxy URL is committed here.
rem Paste the URL your admin gave you between the quotes (once), save, then run this file.
rem Example shapes: http://proxy.example.com:8888   or   http://USER:PASS@host:8888
rem Password special chars: prefer a URL-encoded password; in batch avoid & ^ | < > inside the value.
set "PROXY_URL="

if "!PROXY_URL!"=="" (
  echo Edit run-with-proxy.cmd: set PROXY_URL to your proxy URL on the "set" line above, save, then run again.
  pause
  exit /b 1
)

set "HTTPS_PROXY=!PROXY_URL!"
set "HTTP_PROXY=!PROXY_URL!"

"%~dp0go-frog-windows-amd64.exe"
exit /b !ERRORLEVEL!
