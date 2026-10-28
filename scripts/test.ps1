[CmdletBinding()]
param([switch]$Integration, [switch]$Faults, [switch]$BrokerInterruption)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'common.ps1')
if (($Faults -or $BrokerInterruption) -and -not $Integration) { throw 'Fault checks require -Integration.' }
if ($BrokerInterruption -and -not $Faults) { throw 'Broker interruption requires -Integration -Faults -BrokerInterruption and an idle shared broker.' }
Require-CloudCommand go 'Install the Go version specified by go.mod.'
$savedFlags = @{}
foreach ($key in @('CLOUDFORGE_BROWSER_DEMO', 'CLOUDFORGE_WORKER_HELPER', 'CLOUDFORGE_LIVE_BROKER_STOP', 'GOMAXPROCS')) {
    $savedFlags[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
    Set-CloudProcessEnvironment $key $null
}
$env:GOMAXPROCS = '2'
Push-Location $script:CloudForgeRoot
try {
    if ($Integration) {
        if (@(Get-CloudOwnedServices (Join-Path $script:CloudForgeRoot '.cloudforge/session.json')).Count) {
            throw 'Stop the foreground application before running isolated integration checks.'
        }
        . .\scripts\load-test-env.ps1
        if ($Faults) {
            Invoke-CloudCommand go @('test', '-p', '1', '-tags=integration', './internal/system', '-run', '^TestV5RealWorkerProcessCrashRecoversImage$', '-count=1', '-timeout=90s', '-v')
            if ($BrokerInterruption) {
                $env:CLOUDFORGE_LIVE_BROKER_STOP = '1'
                try { Invoke-CloudCommand go @('test', '-p', '1', '-tags=integration', './internal/system', '-run', '^TestV5LiveBrokerInterruptionRetainsRecoveryIntent$', '-count=1', '-timeout=90s', '-v') }
                finally { $env:CLOUDFORGE_LIVE_BROKER_STOP = $null }
            }
        } else {
            # Keep real transactions, publication, competing workers, image work,
            # ownership/retries/recovery; reserve killed-process faults for opt-in.
            Invoke-CloudCommand go @('test', '-p', '1', '-tags=integration', './internal/database', './internal/jobs', './internal/dispatcher', './internal/worker', './internal/system', '-skip', 'TestV5RealWorkerProcessCrashRecoversImage', '-count=1', '-timeout=120s')
            Invoke-CloudCommand go @('vet', '-p', '1', '-tags=integration', './internal/database', './internal/jobs', './internal/dispatcher', './internal/worker', './internal/system')
        }
    } else {
        Require-CloudCommand node 'Install Node.js 24 LTS to run JavaScript syntax checks; Node is not required to run CloudForge.'
        $files = @(Get-ChildItem -Path $script:CloudForgeRoot -Filter '*.go' -Recurse -File | Where-Object { $_.FullName -notmatch '[\\/](\.git|\.cloudforge|vendor)[\\/]' } | ForEach-Object FullName)
        $candidates = @(& gofmt -l @files)
        if ($LASTEXITCODE -ne 0) { throw 'gofmt could not inspect Go sources.' }
        # Git normalizes text on commit; do not reject a Windows CRLF checkout.
        $unformatted = @(foreach ($path in $candidates) {
            $before = [IO.File]::ReadAllText($path).Replace("`r`n", "`n")
            $after = (@(& gofmt $path) -join "`n") + "`n"
            if ($LASTEXITCODE -ne 0 -or $before -ne $after) { $path }
        })
        if ($unformatted.Count) { throw ('Run gofmt on: ' + ($unformatted -join ', ')) }
        foreach ($file in Get-ChildItem internal/httpapi/web -Filter '*.js') { Invoke-CloudCommand node @('--check', $file.FullName) }
        & (Join-Path $PSScriptRoot 'tooling-tests.ps1')
        Invoke-CloudCommand go @('build', '-p', '1', './...')
        Invoke-CloudCommand go @('test', '-p', '1', './...')
        Invoke-CloudCommand go @('vet', '-p', '1', './...')
    }
} finally {
    Pop-Location
    foreach ($key in $savedFlags.Keys) { Set-CloudProcessEnvironment $key $savedFlags[$key] }
}
