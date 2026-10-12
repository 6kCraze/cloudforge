$testEnvPath = Join-Path $PSScriptRoot '..\.env.test'
if (-not (Test-Path -LiteralPath $testEnvPath)) {
    throw 'Missing .env.test. Copy .env.test.example to .env.test and set up the isolated targets first.'
}
$env:TEST_DATABASE_URL = $null
$env:TEST_RABBITMQ_URL = $null
foreach ($line in Get-Content -LiteralPath $testEnvPath) {
    if ($line -match '^\s*(TEST_DATABASE_URL|TEST_RABBITMQ_URL)=(.*)$') {
        [Environment]::SetEnvironmentVariable($matches[1], $matches[2].Trim(), 'Process')
    }
}
if (-not $env:TEST_DATABASE_URL -or -not $env:TEST_RABBITMQ_URL) {
    throw 'Both TEST_DATABASE_URL and TEST_RABBITMQ_URL must be configured.'
}
Write-Host 'Loaded isolated integration-test configuration.'
