@echo off
REM 停止 TanGIS 桌面单机版（Windows）
taskkill /IM tangis.exe /F >nul 2>&1
if %ERRORLEVEL%==0 (echo TanGIS stopped.) else (echo No running TanGIS found.)
