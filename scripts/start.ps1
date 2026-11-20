$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

if (-not (Test-Path ".env")) {
    Copy-Item ".env.example" ".env"
}
if (-not (Test-Path "workspace")) {
    New-Item -ItemType Directory -Path "workspace" | Out-Null
}

& (Join-Path $PSScriptRoot "hostexec.ps1") -Action start
docker compose up -d --build gateway relay desktop

$port = "8317"
$dport = "8318"
$tport = "8319"
foreach ($line in (Get-Content ".env")) {
    if ($line -match "^HOST_PORT=(.+)$") { $port = $Matches[1].Trim() }
    if ($line -match "^DESKTOP_VNC_PORT=(.+)$") { $dport = $Matches[1].Trim() }
    if ($line -match "^TERMINAL_PORT=(.+)$") { $tport = $Matches[1].Trim() }
}

Write-Host ""
Write-Host "=========================================================="
Write-Host "  edualc 已启动！"
Write-Host "  控制台           : http://127.0.0.1:$port/ui"
Write-Host "  Claude 桌面端    : http://127.0.0.1:$dport/edualc-desktop.html"
Write-Host "  容器 Web 终端    : http://127.0.0.1:$tport"
Write-Host "  OpenAI API       : http://127.0.0.1:$port/v1"
Write-Host "  Anthropic API    : http://127.0.0.1:$port"
Write-Host "=========================================================="
