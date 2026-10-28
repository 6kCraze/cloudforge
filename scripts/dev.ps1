[CmdletBinding()]
param(
    [Parameter(Position = 0)][ValidateSet('init', 'setup', 'infra', 'migrate', 'start', 'status', 'stop')][string]$Action = 'status',
    [string]$EnvFile = '.env',
    [switch]$PullImages
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'common.ps1')
$runtimeDir = Join-Path $script:CloudForgeRoot '.cloudforge'
$sessionFile = Join-Path $runtimeDir 'session.json'
$originalEnvironment = @{}

function Get-OwnedServices {
    Get-CloudOwnedServices $sessionFile
}

function Start-CloudInfrastructure {
    Require-CloudCommand docker 'Install Docker Desktop, enable its Linux backend, and start Docker Desktop.'
    $backend = & docker info --format '{{.OSType}}' 2>$null
    if ($LASTEXITCODE -ne 0 -or $backend -ne 'linux') { throw 'Start Docker Desktop with its Linux container backend, then retry.' }
    if ($PullImages) { Invoke-CloudCommand docker @('compose', '--env-file', $EnvFile, 'pull', 'db', 'rabbitmq') }
    foreach ($image in @('postgres:17', 'rabbitmq:4-management')) {
        & docker image inspect $image --format '{{.Id}}' *> $null
        if ($LASTEXITCODE -ne 0) { throw "Image '$image' is not cached. Run .\scripts\dev.ps1 infra -PullImages once to download the required images." }
    }
    Invoke-CloudCommand docker @('compose', '--env-file', $EnvFile, 'up', '-d', '--wait', '--pull', 'never', 'db', 'rabbitmq')
}

Push-Location $script:CloudForgeRoot
try {
    if ($Action -in @('setup', 'migrate') -and @(Get-OwnedServices).Count) {
        throw 'Stop the application with Ctrl+C before applying migrations. Back up existing databases before upgrades.'
    }
    if ($Action -in @('init', 'setup')) {
        if ($EnvFile -ne '.env') { throw 'init/setup use the repository .env; use an explicit infra/migrate/start action for another configuration.' }
        New-CloudEnv
        if ($Action -eq 'init') { return }
    }
    $config = Read-CloudEnv $EnvFile
    $knownKeys = @(@((Read-CloudEnv '.env.example').Keys) + @($config.Keys) | Select-Object -Unique)
    foreach ($key in $knownKeys) {
        $originalEnvironment[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
        Set-CloudProcessEnvironment $key $null
    }
    $null = Import-CloudEnv $EnvFile
    foreach ($required in @('DATABASE_URL', 'RABBITMQ_URL')) {
        if (-not $config[$required]) { throw "$required is missing in '$EnvFile'. See .env.example." }
    }
    $port = if ($config['HTTP_PORT']) { [int]$config['HTTP_PORT'] } else { 8080 }
    if ($port -lt 1 -or $port -gt 65535) { throw 'HTTP_PORT must be between 1 and 65535.' }
    $browserURL = "http://localhost:$port"
    switch ($Action) {
        { $_ -in @('infra', 'setup') } {
            Start-CloudInfrastructure
            if ($Action -eq 'infra') { break }
            Require-CloudCommand go 'Install the Go version specified by go.mod.'
            Invoke-CloudCommand go @('run', './cmd/migrate', 'up')
            Write-Host 'Setup complete. Run .\scripts\dev.ps1 start.'
        }
        'migrate' {
            if (@(Get-OwnedServices).Count) { throw 'Stop the application with Ctrl+C before applying migrations. Back up an existing database before upgrades.' }
            Require-CloudCommand go 'Install the Go version specified by go.mod.'
            Invoke-CloudCommand go @('run', './cmd/migrate', 'up')
        }
        'start' {
            Require-CloudCommand go 'Install the Go version specified by go.mod.'
            if (@(Get-OwnedServices).Count) { throw 'This checkout already has a running dev session. Use its terminal and .\scripts\dev.ps1 status.' }
            $probe = New-Object Net.Sockets.TcpClient
            try {
                try { $probe.Connect('localhost', $port) } catch { }
                if ($probe.Connected) { throw "Port $port is occupied. Stop the existing API or set HTTP_PORT in '$EnvFile'." }
            } finally { $probe.Dispose() }
            $null = New-Item -ItemType Directory -Path $runtimeDir -Force
            $lock = [IO.File]::Open((Join-Path $runtimeDir 'session.lock'), [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::Write, [IO.FileShare]::None)
            $children = @()
            try {
                $suffix = if ([Environment]::OSVersion.Platform -eq 'Win32NT') { '.exe' } else { '' }
                foreach ($service in @('api', 'dispatcher', 'worker')) {
                    $binary = Join-Path $runtimeDir ($service + $suffix)
                    Invoke-CloudCommand go @('build', '-p', '1', '-o', $binary, "./cmd/$service")
                }
                foreach ($service in @('api', 'dispatcher', 'worker')) {
                    $binary = Join-Path $runtimeDir ($service + $suffix)
                    $info = New-Object Diagnostics.ProcessStartInfo
                    $info.FileName = $binary
                    $info.WorkingDirectory = $script:CloudForgeRoot
                    $info.UseShellExecute = $false
                    # Same foreground console: Ctrl+C reaches the existing Go signal handlers.
                    $child = [Diagnostics.Process]::Start($info)
                    $children += [PSCustomObject]@{ service = $service; process_id = $child.Id; started_utc = $child.StartTime.ToUniversalTime().ToString('o'); executable = $binary; handle = $child }
                }
                $record = @{ services = @($children | Select-Object service, process_id, started_utc, executable) }
                $record | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $sessionFile -Encoding UTF8
                $deadline = [DateTime]::UtcNow.AddSeconds(20)
                $ready = $false
                while ([DateTime]::UtcNow -lt $deadline) {
                    foreach ($child in $children) { if ($child.handle.HasExited) { throw "$($child.service) exited. Check its structured log output above and verify infrastructure/migrations." } }
                    try { $ready = (Invoke-RestMethod "$browserURL/readyz" -TimeoutSec 2).status -eq 'ready' } catch { }
                    if ($ready) { break }
                    Start-Sleep -Milliseconds 250
                }
                if (-not $ready) { throw 'API readiness timed out. Check the database, migrations, and log output.' }
                Write-Host "CloudForge API ready: $browserURL - one worker process. Use status to inspect broker consumers."
                Write-Host 'Keep this terminal open. Press Ctrl+C once to stop the application gracefully, then run .\scripts\dev.ps1 stop.'
                while ($true) {
                    foreach ($child in $children) { if ($child.handle.HasExited) { throw "$($child.service) exited unexpectedly. Restart the dev session after reviewing its logs." } }
                    Start-Sleep -Milliseconds 500
                }
            } finally {
                # Normally all children receive the console interrupt together.
                foreach ($child in $children) {
                    if (-not $child.handle.HasExited -and -not $child.handle.WaitForExit(15000)) {
                        Write-Warning "$($child.service) did not exit after the grace period; stopping only this owned process. Any unfinished job remains recoverable by its lease."
                        $child.handle.Kill()
                        $child.handle.WaitForExit()
                    }
                    $child.handle.Dispose()
                }
                if (Test-Path -LiteralPath $sessionFile) { Remove-Item -LiteralPath $sessionFile }
                $lock.Dispose()
            }
        }
        'status' {
            Require-CloudCommand docker 'Install and start Docker Desktop.'
            Invoke-CloudCommand docker @('compose', '--env-file', $EnvFile, 'ps', '-a')
            $services = @(Get-OwnedServices)
            foreach ($service in $services) { Write-Host "$($service.Service): process $($service.Process.Id) is running" }
            if (-not $services.Count) { Write-Host 'No application session owned by this checkout.' }
            try {
                $state = Invoke-RestMethod "$browserURL/readyz" -TimeoutSec 2
                Write-Host "API database readiness: $($state.status) ($browserURL)"
            } catch { Write-Host "API is not ready at $browserURL. Start infrastructure/migrate/start as needed." }
            $broker = [Uri]$config['RABBITMQ_URL']
            $vhost = [Uri]::UnescapeDataString($broker.AbsolutePath.TrimStart('/'))
            if (-not $vhost) { $vhost = '/' }
            $runningBroker = @(& docker compose --env-file $EnvFile ps --status running -q rabbitmq)
            if ($LASTEXITCODE -ne 0) { throw 'Could not inspect the broker. Check Docker Desktop.' }
            if (($runningBroker -join '').Trim()) {
                Invoke-CloudCommand docker @('compose', '--env-file', $EnvFile, 'exec', '-T', 'rabbitmq', 'rabbitmqctl', '-q', 'list_queues', '-p', $vhost, 'name', 'messages_ready', 'messages_unacknowledged', 'consumers')
            } else { Write-Host 'Broker is stopped; no current queue snapshot is available.' }
        }
        'stop' {
            Require-CloudCommand docker 'Install and start Docker Desktop.'
            if (@(Get-OwnedServices).Count) { throw 'Press Ctrl+C in the dev start terminal first. This command preserves running application processes rather than abruptly disconnecting their database/broker.' }
            try { $null = Invoke-RestMethod "$browserURL/healthz" -TimeoutSec 1; $apiRunning = $true } catch { $apiRunning = $false }
            if ($apiRunning) { throw 'An API is still running. Stop it gracefully in its terminal before stopping infrastructure.' }
            $runningBroker = @(& docker compose --env-file $EnvFile ps --status running -q rabbitmq)
            if ($LASTEXITCODE -ne 0) { throw 'Could not inspect the broker. Check Docker Desktop.' }
            if (($runningBroker -join '').Trim()) {
                $connections = @(& docker compose --env-file $EnvFile exec -T rabbitmq rabbitmqctl -q list_connections name)
                if ($LASTEXITCODE -ne 0) { throw 'Could not verify idle broker clients; infrastructure was not stopped.' }
                if (@($connections | Where-Object { $_.Trim() -and $_.Trim() -ne 'name' }).Count) {
                    throw 'AMQP clients are still connected. Stop dispatcher/workers in their own terminals before stopping the shared broker.'
                }
            }
            Invoke-CloudCommand docker @('compose', '--env-file', $EnvFile, 'stop', 'db', 'rabbitmq')
            Write-Host 'Infrastructure stopped. Database records, uploaded files, and named volumes remain in place.'
        }
    }
} finally {
    foreach ($key in $originalEnvironment.Keys) { Set-CloudProcessEnvironment $key $originalEnvironment[$key] }
    Pop-Location
}
