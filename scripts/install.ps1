#requires -version 5.1

[CmdletBinding()]
param(
    [string]$InstallRoot = $(if ($env:LLM_GATEWAY_HOME) {
        $env:LLM_GATEWAY_HOME
    } else {
        Join-Path $env:USERPROFILE '.llm-gateway'
    }),

    [switch]$Force,
    [switch]$NonInteractive,
    [switch]$Reconfigure,
    [switch]$SkipDownload,
    [switch]$SkipPath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version Latest

$script:Repository = 'nightgopher/litellm-gateway'
$script:BinaryName = 'gateway.exe'

function Write-InstallerMessage {
    param(
        [Parameter(Mandatory = $true)]
        [ValidateSet('Info', 'Success', 'Warning')]
        [string]$Level,

        [Parameter(Mandatory = $true)]
        [string]$Message
    )

    $prefix = switch ($Level) {
        'Info' { '[i]' }
        'Success' { '[OK]' }
        'Warning' { '[!]' }
    }
    Write-Host "  $prefix $Message"
}

function Merge-PathValue {
    param(
        [AllowEmptyString()]
        [string]$PathValue,

        [Parameter(Mandatory = $true)]
        [string]$RequiredEntry
    )

    $entries = New-Object 'System.Collections.Generic.List[string]'
    $seen = New-Object 'System.Collections.Generic.HashSet[string]' (
        [StringComparer]::OrdinalIgnoreCase
    )

    foreach ($rawEntry in ([string]$PathValue -split ';')) {
        $entry = $rawEntry.Trim()
        if ([string]::IsNullOrWhiteSpace($entry)) {
            continue
        }

        $comparisonKey = [Environment]::ExpandEnvironmentVariables(
            $entry
        ).TrimEnd('\', '/')
        if ($seen.Add($comparisonKey)) {
            $entries.Add($entry)
        }
    }

    $normalizedRequiredEntry = $RequiredEntry.Trim().TrimEnd('\', '/')
    $requiredComparisonKey = [Environment]::ExpandEnvironmentVariables(
        $normalizedRequiredEntry
    ).TrimEnd('\', '/')
    if ($seen.Add($requiredComparisonKey)) {
        $entries.Add($normalizedRequiredEntry)
    }

    return $entries -join ';'
}

function Set-GatewayUserPath {
    param(
        [Parameter(Mandatory = $true)]
        [string]$InstallDirectory
    )

    $environmentKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey(
        'Environment',
        $true
    )
    if ($null -eq $environmentKey) {
        throw 'Unable to open HKCU\Environment for writing.'
    }

    try {
        $currentUserPath = [string]$environmentKey.GetValue(
            'Path',
            '',
            [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
        )
        $newUserPath = Merge-PathValue `
            -PathValue $currentUserPath `
            -RequiredEntry $InstallDirectory

        if ($newUserPath -cne $currentUserPath) {
            $environmentKey.SetValue(
                'Path',
                $newUserPath,
                [Microsoft.Win32.RegistryValueKind]::ExpandString
            )
            Write-InstallerMessage Success 'User PATH updated and deduplicated.'
        } else {
            Write-InstallerMessage Success 'User PATH is already configured.'
        }
    }
    finally {
        $environmentKey.Dispose()
    }

    $env:Path = Merge-PathValue `
        -PathValue $env:Path `
        -RequiredEntry $InstallDirectory
}

function New-RandomMasterKey {
    $random = [Security.Cryptography.RandomNumberGenerator]::Create()
    $characters = New-Object Text.StringBuilder

    try {
        while ($characters.Length -lt 32) {
            $bytes = New-Object byte[] 32
            $random.GetBytes($bytes)
            $chunk = [Convert]::ToBase64String($bytes) -replace '[^A-Za-z0-9]', ''
            [void]$characters.Append($chunk)
        }
    }
    finally {
        $random.Dispose()
    }

    return 'sk-' + $characters.ToString().Substring(0, 32)
}

function Write-Utf8FileWithoutBom {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Content
    )

    [IO.File]::WriteAllText(
        $Path,
        $Content,
        (New-Object Text.UTF8Encoding($false))
    )
}

function Read-YesNo {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Message,

        [Parameter(Mandatory = $true)]
        [bool]$Default
    )

    $suffix = if ($Default) { '[Y/n]' } else { '[y/N]' }
    while ($true) {
        $answer = Read-Host "  $Message $suffix"
        if ([string]::IsNullOrWhiteSpace($answer)) {
            return $Default
        }
        if ($answer -match '^(?i:y|yes)$') {
            return $true
        }
        if ($answer -match '^(?i:n|no)$') {
            return $false
        }
        Write-InstallerMessage Warning 'Please enter y or n.'
    }
}

function ConvertFrom-MaskedInput {
    param(
        [Parameter(Mandatory = $true)]
        [Security.SecureString]$SecureValue
    )

    $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($SecureValue)
    try {
        return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)
    }
    finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer)
    }
}

function Read-ProviderApiKey {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ProviderName,

        [AllowEmptyString()]
        [string]$ExistingValue
    )

    $hasExistingValue = -not [string]::IsNullOrWhiteSpace($ExistingValue)
    $prompt = if ($hasExistingValue) {
        "  $ProviderName API Key (masked, Enter keeps existing)"
    } else {
        "  $ProviderName API Key (masked)"
    }

    $secureValue = Read-Host $prompt -AsSecureString
    $plainValue = ConvertFrom-MaskedInput -SecureValue $secureValue
    if ([string]::IsNullOrEmpty($plainValue) -and $hasExistingValue) {
        return $ExistingValue
    }
    if ([string]::IsNullOrEmpty($plainValue)) {
        Write-InstallerMessage Warning "$ProviderName is enabled without an API key."
    }
    return $plainValue
}

function Get-EnvFileValues {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    $values = @{}
    if (-not (Test-Path -LiteralPath $Path)) {
        return $values
    }

    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            $values[$matches[1]] = $matches[2]
        }
    }
    return $values
}

function Update-EnvFileValues {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [Collections.IDictionary]$Updates
    )

    $seen = New-Object 'System.Collections.Generic.HashSet[string]' (
        [StringComparer]::OrdinalIgnoreCase
    )
    $updatedLines = New-Object 'System.Collections.Generic.List[string]'

    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            $key = $matches[1]
            if ($Updates.Contains($key)) {
                $updatedLines.Add("$key=$($Updates[$key])")
                [void]$seen.Add($key)
                continue
            }
        }
        $updatedLines.Add($line)
    }

    foreach ($key in $Updates.Keys) {
        if ($seen.Add([string]$key)) {
            $updatedLines.Add("$key=$($Updates[$key])")
        }
    }

    Write-Utf8FileWithoutBom `
        -Path $Path `
        -Content (($updatedLines -join "`n") + "`n")
}

function Read-ProviderConfiguration {
    param(
        [Parameter(Mandatory = $true)]
        [Collections.IDictionary]$ExistingValues,

        [scriptblock]$YesNoReader,
        [scriptblock]$ApiKeyReader
    )

    if ($null -eq $YesNoReader) {
        $YesNoReader = {
            param($Message, $Default)
            Read-YesNo -Message $Message -Default $Default
        }
    }
    if ($null -eq $ApiKeyReader) {
        $ApiKeyReader = {
            param($ProviderName, $ExistingValue)
            Read-ProviderApiKey `
                -ProviderName $ProviderName `
                -ExistingValue $ExistingValue
        }
    }

    $providers = @(
        @{ Name = 'GLM'; Key = 'GLM_API_KEY' }
        @{ Name = 'Ali MaaS (qwen3.8-max-preview)'; Key = 'ALI_API_KEY' }
    )
    $updates = [ordered]@{}

    Write-Host ''
    Write-Host '  Configure LLM providers:'
    Write-Host '  - Answer y for every provider you want to enable.'
    Write-Host '  - API Key input is masked.'
    Write-Host '  - Press Enter to accept the displayed default.'
    Write-Host ''

    foreach ($provider in $providers) {
        $existingValue = if ($ExistingValues.Contains($provider.Key)) {
            [string]$ExistingValues[$provider.Key]
        } else {
            ''
        }
        $enabledByDefault = -not [string]::IsNullOrWhiteSpace($existingValue)
        $enabled = & $YesNoReader `
            "Enable $($provider.Name)?" `
            $enabledByDefault

        if ($enabled) {
            $updates[$provider.Key] = & $ApiKeyReader `
                $provider.Name `
                $existingValue
        } else {
            $updates[$provider.Key] = ''
        }
    }

    return $updates
}

function New-GatewayConfig {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ConfigPath,

        [switch]$NonInteractiveMode,
        [switch]$ForceReconfigure,
        [scriptblock]$YesNoReader,
        [scriptblock]$ApiKeyReader
    )

    if (Test-Path -LiteralPath $ConfigPath) {
        if ($NonInteractiveMode) {
            Write-InstallerMessage Success "Config already exists: $ConfigPath"
            return
        }

        $shouldReconfigure = $ForceReconfigure -or (
            Read-YesNo `
                -Message 'Config exists. Reconfigure provider API keys?' `
                -Default $false
        )
        if (-not $shouldReconfigure) {
            Write-InstallerMessage Success "Keeping existing config: $ConfigPath"
            return
        }

        $existingValues = Get-EnvFileValues -Path $ConfigPath
        $providerUpdates = Read-ProviderConfiguration `
            -ExistingValues $existingValues `
            -YesNoReader $YesNoReader `
            -ApiKeyReader $ApiKeyReader
        Update-EnvFileValues -Path $ConfigPath -Updates $providerUpdates
        Write-InstallerMessage Success "Provider config updated: $ConfigPath"
        return
    }

    $providerUpdates = [ordered]@{
        GLM_API_KEY = ''
        ALI_API_KEY = ''
    }
    if (-not $NonInteractiveMode) {
        $providerUpdates = Read-ProviderConfiguration `
            -ExistingValues @{} `
            -YesNoReader $YesNoReader `
            -ApiKeyReader $ApiKeyReader
    }

    $masterKey = New-RandomMasterKey
    $configLines = @(
        '# EasyGateway configuration'
        '# Gateway authentication token'
        "LITELLM_MASTER_KEY=$masterKey"
        ''
        '# Provider API keys'
        "GLM_API_KEY=$($providerUpdates['GLM_API_KEY'])"
        "ALI_API_KEY=$($providerUpdates['ALI_API_KEY'])"
        ''
        '# Gateway port'
        'PORT=4001'
        ''
        '# Log level'
        'LOG_LEVEL=info'
        ''
    )

    Write-Utf8FileWithoutBom `
        -Path $ConfigPath `
        -Content ($configLines -join "`n")
    Write-InstallerMessage Success "Config created: $ConfigPath"
}

function Invoke-CurlFileDownload {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Uri,

        [Parameter(Mandatory = $true)]
        [string]$Destination
    )

    $curl = Get-Command 'curl.exe' -ErrorAction SilentlyContinue
    if ($null -eq $curl) {
        throw 'curl.exe is not available.'
    }

    & $curl.Source `
        --fail `
        --location `
        --silent `
        --show-error `
        --retry 3 `
        --retry-delay 1 `
        --output $Destination `
        --url $Uri

    if ($LASTEXITCODE -ne 0) {
        throw "curl.exe failed with exit code $LASTEXITCODE while downloading $Uri"
    }
    if (-not (Test-Path -LiteralPath $Destination)) {
        throw "curl.exe completed without creating: $Destination"
    }
    if ((Get-Item -LiteralPath $Destination).Length -eq 0) {
        throw "curl.exe downloaded an empty file from: $Uri"
    }
}

function Invoke-FileDownload {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Uri,

        [Parameter(Mandatory = $true)]
        [string]$Destination
    )

    $temporaryPath = "$Destination.download"
    try {
        if (Get-Command 'curl.exe' -ErrorAction SilentlyContinue) {
            Invoke-CurlFileDownload -Uri $Uri -Destination $temporaryPath
        } else {
            Invoke-WebRequest `
                -UseBasicParsing `
                -Uri $Uri `
                -OutFile $temporaryPath
        }
        Move-Item -LiteralPath $temporaryPath -Destination $Destination -Force
    }
    finally {
        if (Test-Path -LiteralPath $temporaryPath) {
            Remove-Item -LiteralPath $temporaryPath -Force
        }
    }
}

function Write-InstallerFailure {
    param(
        [Parameter(Mandatory = $true)]
        [Management.Automation.ErrorRecord]$ErrorRecord
    )

    $message = $ErrorRecord.Exception.Message
    try {
        [Console]::Error.WriteLine('')
        [Console]::Error.WriteLine("  [ERROR] $message")
        [Console]::Error.WriteLine('')
    }
    catch {
        # Never let console rendering hide the original installer failure.
    }
}

function Install-GatewayBinary {
    param(
        [Parameter(Mandatory = $true)]
        [string]$BinaryPath,

        [switch]$ForceDownload,
        [switch]$NonInteractiveMode,
        [switch]$SkipBinaryDownload
    )

    if ($SkipBinaryDownload) {
        if (-not (Test-Path -LiteralPath $BinaryPath)) {
            throw "SkipDownload was specified but the binary is missing: $BinaryPath"
        }
        Write-InstallerMessage Success "Using existing binary: $BinaryPath"
        return
    }

    $shouldDownload = $ForceDownload -or -not (Test-Path -LiteralPath $BinaryPath)
    if (-not $shouldDownload -and -not $NonInteractiveMode) {
        $answer = Read-Host '  Gateway is already installed. Reinstall/upgrade? [y/N]'
        $shouldDownload = $answer -match '^(?i:y|yes)$'
    }

    if (-not $shouldDownload) {
        Write-InstallerMessage Success "Keeping existing binary: $BinaryPath"
        return
    }

    $architecture = if ($env:PROCESSOR_ARCHITEW6432) {
        $env:PROCESSOR_ARCHITEW6432
    } else {
        $env:PROCESSOR_ARCHITECTURE
    }
    if ($architecture -notmatch '^(?i:AMD64)$') {
        throw "Unsupported Windows architecture: $architecture"
    }

    $downloadUrl = (
        "https://github.com/$script:Repository/releases/latest/download/" +
        'gateway-windows-amd64.exe'
    )
    Write-InstallerMessage Info 'Downloading the latest gateway release...'
    Invoke-FileDownload -Uri $downloadUrl -Destination $BinaryPath
    Write-InstallerMessage Success 'Gateway binary downloaded.'
}

function Write-GatewayLauncher {
    param(
        [Parameter(Mandatory = $true)]
        [string]$LauncherPath
    )

    $launcher = @(
        '@echo off'
        'setlocal'
        'pushd "%~dp0.." || exit /b 1'
        '"%~dp0gateway.exe" %*'
        'set "gateway_exit_code=%errorlevel%"'
        'popd'
        'exit /b %gateway_exit_code%'
        ''
    ) -join "`r`n"

    Write-Utf8FileWithoutBom -Path $LauncherPath -Content $launcher
    Write-InstallerMessage Success "Launcher created: $LauncherPath"
}

function Invoke-GatewayInstaller {
    if ($env:OS -ne 'Windows_NT') {
        throw 'This installer supports Windows only. Use scripts/install.sh on Linux or macOS.'
    }

    [Net.ServicePointManager]::SecurityProtocol = (
        [Net.ServicePointManager]::SecurityProtocol -bor
        [Net.SecurityProtocolType]::Tls12
    )

    $resolvedInstallRoot = [IO.Path]::GetFullPath(
        [Environment]::ExpandEnvironmentVariables($InstallRoot)
    )
    $installDirectory = Join-Path $resolvedInstallRoot 'bin'
    $binaryPath = Join-Path $installDirectory $script:BinaryName
    $configPath = Join-Path $resolvedInstallRoot '.env'
    $providersPath = Join-Path $resolvedInstallRoot 'providers.yaml'
    $launcherPath = Join-Path $installDirectory 'llm-gateway.cmd'

    Write-Host ''
    Write-Host '  ========================================'
    Write-Host '       EasyGateway Windows Installer'
    Write-Host '  ========================================'
    Write-Host ''
    Write-InstallerMessage Info 'Platform: windows/amd64'

    New-Item -ItemType Directory -Path $installDirectory -Force | Out-Null
    Write-InstallerMessage Success "Install directory: $installDirectory"

    Install-GatewayBinary `
        -BinaryPath $binaryPath `
        -ForceDownload:$Force `
        -NonInteractiveMode:$NonInteractive `
        -SkipBinaryDownload:$SkipDownload

    if (-not $SkipPath) {
        Set-GatewayUserPath -InstallDirectory $installDirectory
    } else {
        Write-InstallerMessage Info 'Skipping PATH update.'
    }

    if (-not (Test-Path -LiteralPath $providersPath)) {
        Write-InstallerMessage Info 'Downloading providers.yaml...'
        Invoke-FileDownload `
            -Uri "https://raw.githubusercontent.com/$script:Repository/main/go-gateway/providers.yaml" `
            -Destination $providersPath
        Write-InstallerMessage Success 'providers.yaml downloaded.'
    } else {
        Write-InstallerMessage Success 'providers.yaml already exists.'
    }

    New-GatewayConfig `
        -ConfigPath $configPath `
        -NonInteractiveMode:$NonInteractive `
        -ForceReconfigure:$Reconfigure
    Write-GatewayLauncher -LauncherPath $launcherPath

    Write-Host ''
    Write-InstallerMessage Success 'Installation completed.'
    Write-Host '  Open a new terminal, then run: llm-gateway'
    Write-Host "  Config: $configPath"
    Write-Host ''
}

if ($env:LLM_GATEWAY_INSTALLER_TEST_MODE -ne '1') {
    try {
        Invoke-GatewayInstaller
    }
    catch {
        Write-InstallerFailure -ErrorRecord $_
        exit 1
    }
}
