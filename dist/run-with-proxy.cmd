@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

rem Proxy URL is not stored in this repo (public). Your admin gives you the URL.
rem Option A: set PROXY_URL or HTTPS_PROXY in the environment, then run this script.
rem Option B: run this script and paste the URL when prompted.

if not "!PROXY_URL!"=="" goto have_proxy
if not "!HTTPS_PROXY!"=="" (
  set "PROXY_URL=!HTTPS_PROXY!"
  goto have_proxy
)

echo Your admin should give you the proxy URL for example http://host:8888
echo or http://user:pass@host:8888 — special characters in the password must be URL-encoded.
set /p "PROXY_URL=Proxy URL: "

:have_proxy
if "!PROXY_URL!"=="" (
  echo No proxy URL. Set PROXY_URL or HTTPS_PROXY, or enter one when prompted.
  pause
  exit /b 1
)

set "HTTPS_PROXY=!PROXY_URL!"
set "HTTP_PROXY=!PROXY_URL!"

"%~dp0go-frog-windows-amd64.exe"
exit /b !ERRORLEVEL!
