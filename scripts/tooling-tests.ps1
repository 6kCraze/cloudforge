# No Pester installation or Docker dependency: exercise the tooling safety contracts.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'common.ps1')
foreach ($source in Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1') {
    $tokens = $null; $parseErrors = $null
    $null = [Management.Automation.Language.Parser]::ParseFile($source.FullName, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count) { throw "PowerShell syntax failed: $($source.Name): $parseErrors" }
}
function Assert-Tooling { param([bool]$Condition, [string]$Message) if (-not $Condition) { throw $Message } }
function Assert-Rejected { param([scriptblock]$Action) $rejected = $false; try { & $Action } catch { $rejected = $true }; Assert-Tooling $rejected 'Unsafe or invalid tooling input was accepted.' }
function docker { $global:LASTEXITCODE = 0 } # No native Docker calls during fixture initialization.
$originalRoot = $script:CloudForgeRoot
$originalFixtureEnvironment = [Environment]::GetEnvironmentVariable('CLOUDFORGE_TOOLING_FIXTURE', 'Process')
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('cloudforge-tooling-tests-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $fixture
try {
    Set-CloudProcessEnvironment 'CLOUDFORGE_TOOLING_FIXTURE' 'fixture-value'
    Set-CloudProcessEnvironment 'CLOUDFORGE_TOOLING_FIXTURE' $null
    Assert-Tooling ($null -eq [Environment]::GetEnvironmentVariable('CLOUDFORGE_TOOLING_FIXTURE', 'Process')) 'Null environment restoration left a value behind.'
    Copy-Item -LiteralPath (Join-Path $originalRoot '.env.example'), (Join-Path $originalRoot '.env.test.example') -Destination $fixture
    $script:CloudForgeRoot = $fixture
    Assert-Rejected { Read-CloudEnv 'missing.env' }
    New-CloudEnv
    $first = [IO.File]::ReadAllText((Join-Path $fixture '.env'))
    New-CloudEnv
    Assert-Tooling ($first -eq [IO.File]::ReadAllText((Join-Path $fixture '.env'))) 'Initializer overwrote .env.'
    $values = Read-CloudEnv '.env'
    Assert-Tooling ($values.POSTGRES_PASSWORD -match '^[0-9a-f]{32}$') 'Development password was not generated.'
    Assert-Tooling ($values.DATABASE_URL.Contains($values.POSTGRES_PASSWORD)) 'Generated database URL does not match credentials.'
    Assert-Tooling ($values.RABBITMQ_URL.Contains($values.RABBITMQ_DEFAULT_PASS)) 'Generated broker URL does not match credentials.'
    New-CloudEnv -Test
    $testValues = Read-CloudEnv '.env.test'
    Assert-CloudTestTargets $testValues
    foreach ($bad in @('postgres://cloudforge_test:p@localhost:5432/cloudforge', 'postgres://cloudforge_test:p@remote:5432/cloudforge_test', 'postgres://cloudforge_test:p@localhost:5432/cloudforge_test?user=cloudforge')) {
        Assert-Rejected { Assert-CloudTestTargets @{ TEST_DATABASE_URL = $bad; TEST_RABBITMQ_URL = $testValues.TEST_RABBITMQ_URL } }
    }
    Set-Content -LiteralPath (Join-Path $fixture 'quoted.env') -Value 'STORAGE_ROOT="C:\a path with spaces\storage"'
    Assert-Tooling ((Read-CloudEnv 'quoted.env').STORAGE_ROOT -eq 'C:\a path with spaces\storage') 'Quoted path parsing failed.'
    Set-Content -LiteralPath (Join-Path $fixture 'duplicate.env') -Value @('HTTP_PORT=8080', 'HTTP_PORT=8081')
    Assert-Rejected { Read-CloudEnv 'duplicate.env' }
    $currentProcess = Get-Process -Id $PID
    $entry = @{ service = 'fixture'; process_id = $PID; started_utc = $currentProcess.StartTime.ToUniversalTime().ToString('o'); executable = $currentProcess.Path }
    $manifest = Join-Path $fixture 'session.json'
    @{ services = @($entry) } | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifest
    Assert-Tooling (@(Get-CloudOwnedServices $manifest).Count -eq 1) 'Owned process identity did not survive a JSON roundtrip.'
    $entry.started_utc = $currentProcess.StartTime.ToUniversalTime().AddSeconds(-1).ToString('o')
    @{ services = @($entry) } | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifest
    Assert-Tooling (@(Get-CloudOwnedServices $manifest).Count -eq 0) 'A mismatched process start time was accepted.'
    $entry.started_utc = $currentProcess.StartTime.ToUniversalTime().ToString('o')
    $entry.executable = Join-Path $fixture 'unrelated.exe'
    @{ services = @($entry) } | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifest
    Assert-Tooling (@(Get-CloudOwnedServices $manifest).Count -eq 0) 'An unrelated executable was accepted.'
    Write-Host 'PowerShell syntax and tooling safety checks passed.'
} finally {
    Set-CloudProcessEnvironment 'CLOUDFORGE_TOOLING_FIXTURE' $originalFixtureEnvironment
    $script:CloudForgeRoot = $originalRoot
    Remove-Item Function:\docker
    $resolvedFixture = [IO.Path]::GetFullPath($fixture)
    $resolvedTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if ($resolvedFixture.StartsWith($resolvedTemp, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path $resolvedFixture -Leaf).StartsWith('cloudforge-tooling-tests-')) {
        Remove-Item -LiteralPath $resolvedFixture -Recurse -Force
    }
}
