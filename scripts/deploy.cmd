@echo off
rem Build ccgp and ccgpy, install them and restart the background server.
rem Run it from a plain terminal, not under wnice / capc: the restarted server
rem would stay in that job (Idle priority, CPU and memory caps).
rem   scripts\deploy.cmd
rem Installs into %CCGP_BIN%, default %USERPROFILE%\bin.
setlocal
cd /d "%~dp0.."

set "BIN=%CCGP_BIN%"
if not defined BIN set "BIN=%USERPROFILE%\bin"

if not exist dist mkdir dist
echo == build
go build -ldflags "-s -w" -o dist\ccgp.exe .\cmd\ccgp || goto fail
go build -ldflags "-s -w" -o dist\ccgpy.exe .\cmd\ccgpy || goto fail

rem HHMMSS for the name of the replaced binaries (running ones cannot be overwritten, only renamed)
set "t=%TIME: =0%"
set "ts=%t:~0,2%%t:~3,2%%t:~6,2%"

echo == install into %BIN%
if not exist "%BIN%" mkdir "%BIN%"
for %%n in (ccgp ccgpy) do (
  if exist "%BIN%\%%n.exe" ren "%BIN%\%%n.exe" "%%n.old-%ts%.exe" || goto fail
  copy /y "dist\%%n.exe" "%BIN%\%%n.exe" >nul || goto fail
)

echo == restart
"%BIN%\ccgp.exe" --restart || goto fail
"%BIN%\ccgp.exe" --status
exit /b 0

:fail
echo deploy failed 1>&2
exit /b 1
