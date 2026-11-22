@echo off
cd /d "%~dp0"
if exist "..\next" (
  set "OUT=..\next\ForzaNext.exe"
) else (
  if not exist "dist" mkdir dist
  set "OUT=dist\ForzaNext.exe"
)
echo Building single-file %OUT% ...
go build -ldflags "-s -w" -o "%OUT%" .
if %ERRORLEVEL% neq 0 (
  echo [ERROR] Build failed!
  pause
  exit /b 1
)
echo [OK] Built %OUT%
