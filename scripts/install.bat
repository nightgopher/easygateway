@echo off
setlocal

set "LOCAL_INSTALLER=%~dp0install.ps1"
set "INSTALLER=%LOCAL_INSTALLER%"
set "DOWNLOADED_INSTALLER="
if not defined LLM_GATEWAY_INSTALLER_URL set "LLM_GATEWAY_INSTALLER_URL=https://raw.githubusercontent.com/nightgopher/litellm-gateway/main/scripts/install.ps1"

if exist "%INSTALLER%" goto run_installer

set "INSTALLER=%TEMP%\llm-gateway-install-%RANDOM%.ps1"
set "DOWNLOADED_INSTALLER=1"
echo   [i] Downloading the Windows installer...
curl.exe -fsSL "%LLM_GATEWAY_INSTALLER_URL%" -o "%INSTALLER%"
if errorlevel 1 (
    echo   [ERROR] Failed to download install.ps1.
    exit /b 1
)

:run_installer
where powershell.exe >nul 2>&1
if errorlevel 1 (
    echo   [ERROR] Windows PowerShell 5.1 or newer is required.
    exit /b 1
)

powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%INSTALLER%" %*
set "INSTALL_EXIT_CODE=%ERRORLEVEL%"

if defined DOWNLOADED_INSTALLER del /q "%INSTALLER%" >nul 2>&1
exit /b %INSTALL_EXIT_CODE%
