$ErrorActionPreference = 'Stop'
Push-Location (Join-Path $PSScriptRoot '..')
try {
    . .\scripts\load-test-env.ps1
    $dbTarget = [Uri]$env:TEST_DATABASE_URL
    $brokerTarget = [Uri]$env:TEST_RABBITMQ_URL
    foreach ($target in @($dbTarget, $brokerTarget)) {
        if ($target.Host -notin @('localhost', '127.0.0.1') -or
            $target.AbsolutePath -ne '/cloudforge_test' -or
            $target.UserInfo.Split(':')[0] -ne 'cloudforge_test') {
            throw 'Setup requires localhost cloudforge_test targets and cloudforge_test accounts.'
        }
    }
    if ($dbTarget.Scheme -notin @('postgres', 'postgresql') -or
        $brokerTarget.Scheme -ne 'amqp' -or
        $dbTarget.Query -notin @('', '?sslmode=disable') -or $brokerTarget.Query) {
        throw 'Use the local test URLs in .env.test.example without target overrides.'
    }
    $dbPassword = [Uri]::UnescapeDataString(($dbTarget.UserInfo -split ':', 2)[1])
    $brokerPassword = [Uri]::UnescapeDataString(($brokerTarget.UserInfo -split ':', 2)[1])
    if (-not $dbPassword -or -not $brokerPassword) { throw 'Test passwords are required.' }
    function Invoke-TestDocker {
        $input | & docker @args
        if ($LASTEXITCODE -ne 0) { throw 'Docker test infrastructure command failed.' }
    }
    # Compose expands only the development admin identity; never truncate it.
    $adminUser = (Get-Content .env | Where-Object { $_ -match '^POSTGRES_USER=' }) -replace '^POSTGRES_USER=', ''
    if (-not $adminUser) { throw 'POSTGRES_USER is required in .env for provisioning.' }
    $escapedPassword = $dbPassword.Replace("'", "''")
    $sql = @"
SELECT 'CREATE ROLE cloudforge_test LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'cloudforge_test') \gexec
ALTER ROLE cloudforge_test WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '$escapedPassword';
SELECT 'CREATE DATABASE cloudforge_test OWNER cloudforge_test' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'cloudforge_test') \gexec
"@
    $sql | Invoke-TestDocker compose exec -T db psql -v ON_ERROR_STOP=1 -U $adminUser.Trim() -d postgres
    Invoke-TestDocker compose exec -T rabbitmq rabbitmqctl add_vhost cloudforge_test
    $users = Invoke-TestDocker compose exec -T rabbitmq rabbitmqctl -q list_users
    if ($users -match '^cloudforge_test\s') {
        Invoke-TestDocker compose exec -T rabbitmq rabbitmqctl change_password cloudforge_test $brokerPassword
    } else {
        Invoke-TestDocker compose exec -T rabbitmq rabbitmqctl add_user cloudforge_test $brokerPassword
    }
    Invoke-TestDocker compose exec -T rabbitmq rabbitmqctl set_user_tags cloudforge_test
    Invoke-TestDocker compose exec -T rabbitmq rabbitmqctl set_permissions -p cloudforge_test cloudforge_test '.*' '.*' '.*'
    # Migration command uses DATABASE_URL; restore the caller's development value.
    $savedDatabaseURL = $env:DATABASE_URL
    try {
        $env:DATABASE_URL = $env:TEST_DATABASE_URL
        go run ./cmd/migrate up
        if ($LASTEXITCODE -ne 0) { throw 'Test database migrations failed.' }
    } finally { $env:DATABASE_URL = $savedDatabaseURL }
    Write-Host 'Isolated test database and RabbitMQ vhost are ready. Development volumes are preserved.'
} finally { Pop-Location }
