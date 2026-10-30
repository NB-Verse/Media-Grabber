@echo off
REM Media Grabber - Windows starter. Double-click this file.
cd /d "%~dp0"

where python >nul 2>nul
if errorlevel 1 (
  echo Python is not installed. Get it from https://www.python.org/downloads/ and tick "Add python.exe to PATH".
  pause
  exit /b 1
)

where ffmpeg >nul 2>nul
if errorlevel 1 (
  echo FFmpeg not found. Installing it with winget...
  winget install --id=Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements
  set JUST_INSTALLED=1
)

where deno >nul 2>nul
if errorlevel 1 (
  echo Deno not found ^(needed for YouTube^). Installing it with winget...
  winget install --id=DenoLand.Deno -e --accept-source-agreements --accept-package-agreements
  set JUST_INSTALLED=1
)
if defined JUST_INSTALLED (
  echo.
  echo New tools were installed. Close this window and double-click start.bat again so Windows can find them.
  pause
  exit /b 0
)

if not exist .venv (
  echo Setting up for the first time...
  python -m venv .venv
)
call .venv\Scripts\activate.bat

echo Updating the downloader to the latest version...
python -m pip install -q --upgrade pip
python -m pip install -q --upgrade -r requirements.txt

start "" http://127.0.0.1:8000
python app.py
pause
