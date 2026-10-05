@echo off
REM TanGIS 桌面单机版启动脚本（Windows）
REM 双击运行；默认监听固定端口 8080，启动后自动打开浏览器。

setlocal
cd /d "%~dp0"
if "%PORT%"=="" set PORT=8080
set TANGIS_MODE=desktop

echo TanGIS is starting on port %PORT% ...

curl -s -f http://127.0.0.1:%PORT%/healthz >nul 2>&1
if %ERRORLEVEL%==0 (
  echo TanGIS is already running, opening console.
) else (
  start "TanGIS" /b tangis.exe > tangis.log 2>&1
  ping -n 8 127.0.0.1 >nul
)

curl -s -f http://127.0.0.1:%PORT%/healthz >nul 2>&1
if NOT %ERRORLEVEL%==0 (
  echo Failed to start. See tangis.log
  exit /b 1
)

echo ---------------------------------------------
echo  TanGIS is ready
echo  Console : http://127.0.0.1:%PORT%/
echo  Stop    : stop.bat
echo ---------------------------------------------
start "" http://127.0.0.1:%PORT%/
endlocal
