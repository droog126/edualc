param(
    [string]$Account = "default"
)
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

Write-Host "[login] 确保隔离桌面环境就绪..."
docker compose up -d desktop | Out-Null
Write-Host ">>> 请将下方授权链接复制到控制台「隔离浏览器」页（http://127.0.0.1:8317/ui）或 http://127.0.0.1:8320/edualc-desktop.html 完成登录"
Write-Host ""
docker compose exec -it relay /usr/local/bin/claude-login $Account
