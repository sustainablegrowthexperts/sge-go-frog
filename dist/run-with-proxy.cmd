@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

if not "%~1"=="" (
  set "PROXY_URL=%~1"
  goto have_proxy
)

echo Enter your HTTPS proxy URL, for example: http://203.0.113.10:3128
set /p "PROXY_URL=Proxy URL: "

:have_proxy
if "!PROXY_URL!"=="" (
  echo No proxy URL given. From a terminal you can use: run-with-proxy.cmd http://HOST:PORT
  pause
  exit /b 1
)

set "HTTPS_PROXY=!PROXY_URL!"
set "HTTP_PROXY=!PROXY_URL!"

"%~dp0go-frog-windows-amd64.exe"
set "ERR=!ERRORLEVEL!"
exit /b !ERR!
