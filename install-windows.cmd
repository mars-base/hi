@echo off
REM hi — Windows installer (CMD)
REM Usage: curl -fsSL https://raw.githubusercontent.com/mars-base/hi/main/install-windows.cmd -o install.cmd && install.cmd && del install.cmd
REM
REM Supports fresh install and upgrade to latest release.

setlocal enabledelayedexpansion

set REPO=mars-base/hi
set BIN=hi.exe
set ARCH=amd64
set INSTALL_DIR=%USERPROFILE%\.local\bin
set DEST=%INSTALL_DIR%\%BIN%

REM Get latest release tag. Write to temp file first — pipes in for /f
REM are unreliable on some Windows versions.
set RELEASE_JSON=%TEMP%\_hi_release.json
curl -sL "https://api.github.com/repos/%REPO%/releases/latest" > "%RELEASE_JSON%"
for /f "tokens=*" %%i in ('findstr "tag_name" "%RELEASE_JSON%"') do set TAG_LINE=%%i
del "%RELEASE_JSON%" >nul 2>&1
if "%TAG_LINE%"=="" (
    echo Failed to fetch latest release from GitHub.
    echo This may be a rate-limit issue ^(too many requests^). Wait a few minutes and try again.
    echo If behind a proxy, set HTTP_PROXY and HTTPS_PROXY first.
    exit /b 1
)

REM Parse tag from JSON (minimal extraction).
for /f "tokens=2 delims=:" %%i in ('echo %TAG_LINE%') do set TAG=%%i
set TAG=%TAG:"=%
set TAG=%TAG: =%
set TAG=%TAG:,=%

set VERSION=%TAG:~1%

REM Check current version.
set CURRENT=
if exist "%DEST%" (
    for /f "tokens=*" %%i in ('"%DEST%" --version 2^>nul') do set CURRENT=%%i
    set CURRENT=!CURRENT:hi =!
    if "!CURRENT!"=="!VERSION!" (
        echo hi %TAG% is already installed and up to date.
        exit /b 0
    )
    if not "!CURRENT!"=="" (
        echo Upgrading hi: !CURRENT! -^> !VERSION!
    ) else (
        echo Installing hi %TAG% for windows/%ARCH%...
    )
) else (
    echo Installing hi %TAG% for windows/%ARCH%...
)

set ARCHIVE=hi-%VERSION%-windows-%ARCH%.zip
set URL=https://github.com/%REPO%/releases/download/%TAG%/%ARCHIVE%

echo Downloading...
curl -fsSL "%URL%" -o "%ARCHIVE%"

REM Extract using PowerShell.
powershell -Command "Expand-Archive -Path '%ARCHIVE%' -DestinationPath '.' -Force"
del "%ARCHIVE%"

REM Ensure install directory exists.
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"

REM Check for running hi processes — cannot replace if in use.
tasklist /fi "imagename eq hi.exe" 2>nul | find /i "hi.exe" >nul
if %errorlevel% equ 0 (
    echo.
    echo WARNING: hi.exe is currently running.
    echo Stop it first ^(Ctrl+C in the Claude Code terminal^), then run this script again.
    del "%BIN%" >nul 2>&1
    exit /b 1
)

REM Replace binary.
move /Y "%BIN%" "%DEST%" >nul 2>&1
if errorlevel 1 (
    echo.
    echo WARNING: Could not replace %DEST% - file may be in use.
    echo Close all Claude Code / hi sessions and run this script again.
    exit /b 1
)

echo.
echo hi %TAG% installed to %DEST%

REM Add to user PATH if not already present.
set "USER_PATH="
for /f "tokens=3" %%i in ('reg query "HKCU\Environment" /v PATH 2^>nul') do set USER_PATH=%%i
echo !USER_PATH! | findstr /c:"%INSTALL_DIR%" >nul
if errorlevel 1 (
    echo.
    echo Adding %INSTALL_DIR% to your user PATH...
    setx PATH "!USER_PATH!;%INSTALL_DIR%" >nul
    echo PATH updated. Restart your terminal to use 'hi'.
)

echo.
echo Downloading helper scripts...
set SCRIPT_BASE=https://raw.githubusercontent.com/%REPO%/main/scripts
curl -fsSL "%SCRIPT_BASE%/hi-statusline.py" -o "%INSTALL_DIR%\hi-statusline.py"
if exist "%INSTALL_DIR%\hi-statusline.py" (
    echo   + hi-statusline.py
) else (
    echo   ! hi-statusline.py : download failed
)
curl -fsSL "%SCRIPT_BASE%/hi-statusline.bat" -o "%INSTALL_DIR%\hi-statusline.bat"
if exist "%INSTALL_DIR%\hi-statusline.bat" (
    echo   + hi-statusline.bat
) else (
    echo   ! hi-statusline.bat : download failed
)

echo.
echo Quick start:
echo   1. hi init-config    ^(auto-detects settings^)
echo   2. Edit ~/.hi/config.yaml if needed
echo   3. hi                 ^(proxy + Claude Code^)
echo   Or: hi proxy ^& hi cc ^(standalone proxy + attach^)
echo.
echo Run: hi

REM ─── Install counter (fire-and-forget) ───────────────────────────
REM Anonymous install count for adoption tracking. No PII collected.
REM Query stats: curl https://runaifs.pages.dev/api/stats?project=hi
curl -fsSL --connect-timeout 3 --max-time 5 "https://runaifs.pages.dev/api/install?os=windows&project=hi" >nul 2>&1

endlocal
