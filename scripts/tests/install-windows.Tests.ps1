$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$batchInstaller = Join-Path $repoRoot 'scripts\install.bat'
$powerShellInstaller = Join-Path $repoRoot 'scripts\install.ps1'
$unicodeDirectory = '{0}{1} space' -f [char]0x4E2D, [char]0x6587
$testRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'llm-gateway-installer-test-{0}-{1}' -f
    [Guid]::NewGuid().ToString('N'),
    $unicodeDirectory
)
$installRoot = Join-Path $testRoot '.llm-gateway'
$installBin = Join-Path $installRoot 'bin'

function Assert-True {
    param(
        [Parameter(Mandatory = $true)]
        [bool]$Condition,

        [Parameter(Mandatory = $true)]
        [string]$Message
    )

    if (-not $Condition) {
        throw "Assertion failed: $Message"
    }
}

function Assert-Equal {
    param(
        [AllowNull()]
        [object]$Expected,

        [AllowNull()]
        [object]$Actual,

        [Parameter(Mandatory = $true)]
        [string]$Message
    )

    if ($Expected -ne $Actual) {
        throw "Assertion failed: $Message`nExpected: $Expected`nActual:   $Actual"
    }
}

$originalUserProfile = $env:USERPROFILE
$originalPath = $env:Path
$originalTestMode = $env:LLM_GATEWAY_INSTALLER_TEST_MODE
$originalInstallerUrl = $env:LLM_GATEWAY_INSTALLER_URL
$originalModuleAnalysisCachePath = $env:PSModuleAnalysisCachePath

try {
    New-Item -ItemType Directory -Path $installBin -Force | Out-Null
    Copy-Item -LiteralPath $env:ComSpec -Destination (
        Join-Path $installBin 'gateway.exe'
    )
    [IO.File]::WriteAllText(
        (Join-Path $installRoot 'providers.yaml'),
        'providers: []',
        [Text.UTF8Encoding]::new($false)
    )

    $env:USERPROFILE = $testRoot
    $env:Path = "$installBin;$originalPath"
    $env:PSModuleAnalysisCachePath = Join-Path $testRoot 'ModuleAnalysisCache'

    $batchOutput = & cmd.exe /d /s /c (
        'echo n| call "{0}" -NonInteractive -SkipDownload -SkipPath' -f $batchInstaller
    ) 2>&1
    $batchExitCode = $LASTEXITCODE

    Assert-Equal 0 $batchExitCode (
        "CMD installer should complete successfully. Output:`n{0}" -f
        ($batchOutput -join [Environment]::NewLine)
    )

    $configPath = Join-Path $installRoot '.env'
    $launcherPath = Join-Path $installBin 'llm-gateway.cmd'
    Assert-True (Test-Path -LiteralPath $configPath) 'installer should create .env'
    Assert-True (Test-Path -LiteralPath $launcherPath) 'installer should create llm-gateway.cmd'

    $launcherText = [IO.File]::ReadAllText($launcherPath)
    Assert-True (
        $launcherText -match [regex]::Escape('"%~dp0gateway.exe" %*')
    ) 'launcher should resolve the binary relative to its own ASCII path'
    Assert-True (
        $launcherText -notmatch [regex]::Escape($testRoot)
    ) 'launcher should not embed a Unicode installation path'
    $launcherBytes = [IO.File]::ReadAllBytes($launcherPath)
    Assert-True (
        -not ($launcherBytes | Where-Object { $_ -gt 0x7F })
    ) 'launcher should contain ASCII bytes only for CMD code-page compatibility'

    $launcherOutput = & cmd.exe /d /s /c (
        'call "{0}" /d /c exit 0' -f $launcherPath
    ) 2>&1
    Assert-Equal 0 $LASTEXITCODE (
        "launcher should execute from a Unicode path. Output:`n{0}" -f
        ($launcherOutput -join [Environment]::NewLine)
    )

    $configBytes = [IO.File]::ReadAllBytes($configPath)
    $hasUtf8Bom = $configBytes.Length -ge 3 -and
        $configBytes[0] -eq 0xEF -and
        $configBytes[1] -eq 0xBB -and
        $configBytes[2] -eq 0xBF
    Assert-True (-not $hasUtf8Bom) '.env should be UTF-8 without BOM'

    $configText = [IO.File]::ReadAllText($configPath)
    Assert-True (
        $configText -match '(?m)^EASYGATEWAY_MASTER_KEY=sk-[A-Za-z0-9]{32}$'
    ) 'installer should generate a 32-character master key'
    Assert-True (
        $configText -match '(?m)^PORT=4001$'
    ) 'installer should configure the default port'
    Assert-True (
        $configText -match '(?m)^ALI_API_KEY=$'
    ) 'installer should use the documented ALI_API_KEY variable'

    Assert-True (
        (Test-Path -LiteralPath $powerShellInstaller)
    ) 'PowerShell installer should exist'

    $env:LLM_GATEWAY_INSTALLER_TEST_MODE = '1'
    . $powerShellInstaller

    $unicodeUser = '{0}{1} {2}{3}' -f (
        [char]0x6D4B,
        [char]0x8BD5,
        [char]0x7528,
        [char]0x6237
    )
    $requiredPath = "C:\Users\$unicodeUser\.llm-gateway\bin"
    $inputPath = (
        '%PNPM_HOME%;C:\Tools;{0};{1};;' -f
        $requiredPath,
        $requiredPath.ToLowerInvariant()
    )
    $mergedPath = Merge-PathValue -PathValue $inputPath -RequiredEntry $requiredPath
    Assert-Equal (
        "%PNPM_HOME%;C:\Tools;$requiredPath"
    ) $mergedPath 'PATH merge should remove duplicates case-insensitively'

    $appendedPath = Merge-PathValue `
        -PathValue '%JAVA_HOME%\bin;C:\Tools' `
        -RequiredEntry $requiredPath
    Assert-Equal (
        "%JAVA_HOME%\bin;C:\Tools;$requiredPath"
    ) $appendedPath 'PATH merge should append a missing entry without expanding variables'

    $variableInstallPath = '%USERPROFILE%\.llm-gateway\bin'
    $expandedInstallPath = Join-Path $env:USERPROFILE '.llm-gateway\bin'
    $variableMergedPath = Merge-PathValue `
        -PathValue "$variableInstallPath;C:\Tools" `
        -RequiredEntry $expandedInstallPath
    Assert-Equal (
        "$variableInstallPath;C:\Tools"
    ) $variableMergedPath (
        'PATH merge should compare expanded variables while preserving stored variables'
    )

    $downloadSource = Join-Path $testRoot 'download source.bin'
    $downloadDestination = Join-Path $testRoot 'download destination.bin'
    [IO.File]::WriteAllText(
        $downloadSource,
        'curl download regression content',
        [Text.UTF8Encoding]::new($false)
    )
    Invoke-CurlFileDownload `
        -Uri ([Uri]$downloadSource).AbsoluteUri `
        -Destination $downloadDestination
    Assert-True (
        (Test-Path -LiteralPath $downloadDestination)
    ) 'native curl download should create the destination file'
    Assert-Equal (
        'curl download regression content'
    ) ([IO.File]::ReadAllText($downloadDestination)) (
        'native curl download should preserve file contents'
    )

    $bootstrapDirectory = Join-Path $testRoot 'bootstrap-only'
    New-Item -ItemType Directory -Path $bootstrapDirectory -Force | Out-Null
    $bootstrapBatch = Join-Path $bootstrapDirectory 'install.bat'
    Copy-Item -LiteralPath $batchInstaller -Destination $bootstrapBatch

    $env:LLM_GATEWAY_INSTALLER_TEST_MODE = ''
    $env:LLM_GATEWAY_INSTALLER_URL = ([Uri]$powerShellInstaller).AbsoluteUri
    $bootstrapOutput = & cmd.exe /d /s /c (
        'call "{0}" -NonInteractive -SkipDownload -SkipPath' -f $bootstrapBatch
    ) 2>&1
    $bootstrapExitCode = $LASTEXITCODE
    Assert-Equal 0 $bootstrapExitCode (
        "downloaded CMD bootstrap should complete successfully. Output:`n{0}" -f
        ($bootstrapOutput -join [Environment]::NewLine)
    )

    $reconfigureRoot = Join-Path $testRoot 'reconfigure install'
    $reconfigureBin = Join-Path $reconfigureRoot 'bin'
    New-Item -ItemType Directory -Path $reconfigureBin -Force | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $reconfigureBin 'gateway.exe'),
        'test binary',
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $reconfigureRoot 'providers.yaml'),
        'providers: []',
        [Text.UTF8Encoding]::new($false)
    )
    $reconfigureConfig = @(
        'EASYGATEWAY_MASTER_KEY=sk-existing-master-key'
        'GLM_API_KEY=glm-existing-key'
        'PORT=4001'
        'LOG_LEVEL=debug'
        'CUSTOM_SETTING=keep-me'
        ''
    ) -join "`n"
    [IO.File]::WriteAllText(
        (Join-Path $reconfigureRoot '.env'),
        $reconfigureConfig,
        [Text.UTF8Encoding]::new($false)
    )

    $providerDecisions = New-Object 'System.Collections.Generic.Queue[bool]'
    $providerDecisions.Enqueue($true)
    $providerDecisions.Enqueue($true)
    $providerKeys = New-Object 'System.Collections.Generic.Queue[string]'
    $providerKeys.Enqueue('')
    $providerKeys.Enqueue('ali-new-secret')

    $yesNoReader = {
        param($Message, $Default)
        return $providerDecisions.Dequeue()
    }
    $apiKeyReader = {
        param($ProviderName, $ExistingValue)
        $value = $providerKeys.Dequeue()
        if ([string]::IsNullOrEmpty($value)) {
            return $ExistingValue
        }
        return $value
    }
    New-GatewayConfig `
        -ConfigPath (Join-Path $reconfigureRoot '.env') `
        -ForceReconfigure `
        -YesNoReader $yesNoReader `
        -ApiKeyReader $apiKeyReader

    $reconfiguredText = [IO.File]::ReadAllText(
        (Join-Path $reconfigureRoot '.env')
    )
    Assert-True (
        $reconfiguredText -match '(?m)^EASYGATEWAY_MASTER_KEY=sk-existing-master-key$'
    ) 'reconfiguration should preserve the existing master key'
    Assert-True (
        $reconfiguredText -match '(?m)^GLM_API_KEY=glm-existing-key$'
    ) 'a blank masked input should preserve an existing provider key'
    Assert-True (
        $reconfiguredText -match '(?m)^ALI_API_KEY=ali-new-secret$'
    ) 'reconfiguration should support enabling an additional provider'
    Assert-True (
        $reconfiguredText -match '(?m)^CUSTOM_SETTING=keep-me$'
    ) 'reconfiguration should preserve custom environment variables'
    Assert-True (
        ([IO.File]::ReadAllText($powerShellInstaller)) -match
        'Read-Host \$prompt -AsSecureString'
    ) 'interactive API key input should use masked secure-string input'

    $interactiveRoot = Join-Path $testRoot 'interactive install'
    $interactiveBin = Join-Path $interactiveRoot 'bin'
    New-Item -ItemType Directory -Path $interactiveBin -Force | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $interactiveBin 'gateway.exe'),
        'test binary',
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $interactiveRoot 'providers.yaml'),
        'providers: []',
        [Text.UTF8Encoding]::new($false)
    )

    $interactiveOutput = & cmd.exe /d /s /c (
        'call "{0}" -InstallRoot "{1}" -SkipDownload -SkipPath -NonInteractive' -f
        $batchInstaller,
        $interactiveRoot
    ) 2>&1
    $interactiveExitCode = $LASTEXITCODE
    Assert-Equal 0 $interactiveExitCode (
        "interactive CMD installer should accept provider input. Output:`n{0}" -f
        ($interactiveOutput -join [Environment]::NewLine)
    )
    Assert-True (
        (Test-Path -LiteralPath (Join-Path $interactiveRoot '.env'))
    ) 'CMD installer should create .env in non-interactive mode'

    Write-Host 'PASS: Windows installer integration and PATH regression tests'
}
finally {
    $env:USERPROFILE = $originalUserProfile
    $env:Path = $originalPath
    $env:LLM_GATEWAY_INSTALLER_TEST_MODE = $originalTestMode
    $env:LLM_GATEWAY_INSTALLER_URL = $originalInstallerUrl
    $env:PSModuleAnalysisCachePath = $originalModuleAnalysisCachePath

    if (Test-Path -LiteralPath $testRoot) {
        Remove-Item -LiteralPath $testRoot -Recurse -Force
    }
}
