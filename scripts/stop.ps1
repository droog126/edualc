$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
& (Join-Path $PSScriptRoot "hostexec.ps1") -Action stop
docker compose down
Write-Host "[stop] edualc 已停止。"
