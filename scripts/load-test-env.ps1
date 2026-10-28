param([string]$Path = '.env.test')
. (Join-Path $PSScriptRoot 'common.ps1')
$env:TEST_DATABASE_URL = $null
$env:TEST_RABBITMQ_URL = $null
$testValues = Read-CloudEnv $Path
Assert-CloudTestTargets $testValues
foreach ($key in @('TEST_DATABASE_URL', 'TEST_RABBITMQ_URL')) {
    [Environment]::SetEnvironmentVariable($key, $testValues[$key], 'Process')
}
Write-Host 'Loaded isolated integration-test configuration.'
