param(
    [string]$Account = "default"
)
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

Write-Host "[login] 确保隔离桌面环境就绪..."
docker compose up -d desktop | Out-Null
Write-Host ">>> 可在控制台「Claude 桌面端」页右键打开隔离浏览器，或访问 http://127.0.0.1:8318/vnc.html?autoconnect=true&resize=remote"
Write-Host ""
docker compose exec -it relay /usr/local/bin/claude-login $Account
