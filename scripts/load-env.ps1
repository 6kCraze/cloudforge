. (Join-Path $PSScriptRoot 'common.ps1')
$null = Import-CloudEnv '.env'
Write-Host 'Loaded .env into this PowerShell session.'

