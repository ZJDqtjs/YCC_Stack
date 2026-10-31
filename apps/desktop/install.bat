@echo off
rem Fallback installer. The single-file exe runs directly, so this is only for
rem people who also want a Start Menu entry and a copy under LOCALAPPDATA.
setlocal
if not exist "%~dp0StarStack.Desktop.exe" (
  echo StarStack.Desktop.exe not found next to this script.
  pause
  exit /b 1
)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" -Source "%~dp0"
pause
