# Shared repository-local tooling. Dot-sourcing this file does not change the machine.
$script:CloudForgeRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))

function Read-CloudEnv {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not [IO.Path]::IsPathRooted($Path)) { $Path = Join-Path $script:CloudForgeRoot $Path }
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Missing configuration file '$Path'. Run .\scripts\dev.ps1 init, or create the requested file from its example."
    }
    $values = @{}
    $lineNumber = 0
    foreach ($line in Get-Content -LiteralPath $Path) {
        $lineNumber++
        if ($line -match '^\s*(#.*)?$') { continue }
        if ($line -notmatch '^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            throw "Invalid KEY=value configuration at line $lineNumber in '$Path'."
        }
        $key, $value = $matches[1], $matches[2].Trim()
        if ($values.ContainsKey($key)) { throw "Duplicate configuration key '$key' in '$Path'." }
        if ($value.Length -ge 2 -and (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'")))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
        $values[$key] = $value
    }
    return $values
}

function Set-CloudProcessEnvironment {
    param([string]$Name, [AllowNull()][object]$Value)
    if ($null -eq $Value) { Remove-Item -LiteralPath ("Env:\" + $Name) -ErrorAction SilentlyContinue }
    else { [Environment]::SetEnvironmentVariable($Name, [string]$Value, 'Process') }
}

function Import-CloudEnv {
    param([string]$Path = '.env')
    $values = Read-CloudEnv $Path
    foreach ($key in $values.Keys) { [Environment]::SetEnvironmentVariable($key, $values[$key], 'Process') }
    return $values
}

function Require-CloudCommand {
    param([string]$Name, [string]$Help)
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) { throw "$Name is required. $Help" }
}

function Invoke-CloudCommand {
    param([string]$File, [string[]]$Arguments)
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$File failed (exit $LASTEXITCODE). See the command output above." }
}

function New-CloudEnv {
    param([switch]$Test)
    $name = if ($Test) { '.env.test' } else { '.env' }
    $path = Join-Path $script:CloudForgeRoot $name
    if (Test-Path -LiteralPath $path) { Write-Host "$name already exists; preserved."; return }
    # Refuse to invent replacement credentials for a checkout with existing data.
    if (-not $Test -and (Get-Command docker -ErrorAction SilentlyContinue)) {
        $project = if ($env:COMPOSE_PROJECT_NAME) { $env:COMPOSE_PROJECT_NAME } else { (Split-Path $script:CloudForgeRoot -Leaf).ToLowerInvariant() -replace '[^a-z0-9_-]', '' }
        $volumes = @(& docker volume ls --filter "label=com.docker.compose.project=$project" --format '{{.Name}}' 2>$null)
        if ($LASTEXITCODE -eq 0 -and $volumes.Count -gt 0) {
            throw 'Existing CloudForge volumes found but .env is missing. Restore their matching configuration instead of generating new credentials.'
        }
    }
    $text = [IO.File]::ReadAllText((Join-Path $script:CloudForgeRoot ($name + '.example')))
    $dbPassword = [guid]::NewGuid().ToString('N')
    $brokerPassword = [guid]::NewGuid().ToString('N')
    if ($Test) {
        $text = $text.Replace('cloudforge_test:cloudforge_test_only@localhost:5432', "cloudforge_test:${dbPassword}@localhost:5432")
        $text = $text.Replace('cloudforge_test:cloudforge_test_only@localhost:5672', "cloudforge_test:${brokerPassword}@localhost:5672")
    } else {
        $text = $text.Replace('POSTGRES_PASSWORD=cloudforge_dev_only', "POSTGRES_PASSWORD=$dbPassword")
        $text = $text.Replace('RABBITMQ_DEFAULT_PASS=cloudforge_dev_only', "RABBITMQ_DEFAULT_PASS=$brokerPassword")
        $text = $text.Replace('postgres://cloudforge:cloudforge_dev_only@', "postgres://cloudforge:${dbPassword}@")
        $text = $text.Replace('amqp://cloudforge:cloudforge_dev_only@', "amqp://cloudforge:${brokerPassword}@")
    }
    # CreateNew is deliberate: even a simultaneous initializer cannot overwrite a file.
    $file = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
    try {
        $bytes = (New-Object Text.UTF8Encoding($false)).GetBytes($text)
        $file.Write($bytes, 0, $bytes.Length)
    } finally { $file.Dispose() }
    Write-Host "Created $name with generated local credentials. Keep it private."
}

function Get-CloudOwnedServices {
    param([string]$SessionFile)
    if (-not (Test-Path -LiteralPath $SessionFile)) { return }
    $session = Get-Content -LiteralPath $SessionFile -Raw | ConvertFrom-Json
    foreach ($entry in $session.services) {
        try {
            $process = Get-Process -Id $entry.process_id -ErrorAction Stop
            # PS7 may parse ISO JSON dates as DateTime; PS5 leaves strings.
            $started = ([DateTime]$entry.started_utc).ToUniversalTime()
            if ($process.StartTime.ToUniversalTime().Ticks -eq $started.Ticks -and $process.Path -eq $entry.executable) {
                [PSCustomObject]@{ Service = $entry.service; Process = $process }
            }
        } catch { continue }
    }
}

function Assert-CloudTestTargets {
    param([hashtable]$Values)
    foreach ($name in @('TEST_DATABASE_URL', 'TEST_RABBITMQ_URL')) {
        $target = $null
        if (-not $Values[$name] -or -not [Uri]::TryCreate($Values[$name], [UriKind]::Absolute, [ref]$target)) {
            throw "$name must be an absolute local test URL."
        }
        if ($target.Host -notin @('localhost', '127.0.0.1') -or $target.AbsolutePath -ne '/cloudforge_test' -or
            $target.UserInfo.Split(':')[0] -ne 'cloudforge_test' -or $target.Fragment) {
            throw "$name must target localhost/cloudforge_test using the cloudforge_test account. Development fallback is forbidden."
        }
        if ($name -eq 'TEST_DATABASE_URL') {
            if ($target.Scheme -notin @('postgres', 'postgresql') -or $target.Query -notin @('', '?sslmode=disable')) { throw 'Unsafe PostgreSQL test URL options.' }
        } elseif ($target.Scheme -ne 'amqp' -or $target.Query) { throw 'Unsafe RabbitMQ test URL options.' }
        if ($target.UserInfo -notmatch ':.' ) { throw "$name requires a password." }
    }
}
