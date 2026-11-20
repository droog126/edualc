param(
    [string]$Prompt = "你好，请只回复 OK。",
    [string]$Model = "sonnet"
)
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$port = "8317"
$apiKey = ""
if (Test-Path ".env") {
    $pLine = Get-Content ".env" | Where-Object { $_ -match "^HOST_PORT=(.+)$" } | Select-Object -First 1
    if ($pLine) { $port = $Matches[1].Trim() }
    $kLine = Get-Content ".env" | Where-Object { $_ -match "^RELAY_API_KEY=(.+)$" } | Select-Object -First 1
    if ($kLine) { $apiKey = $Matches[1].Trim() }
}

Write-Host "[test] 检查安全门控状态 (/health):"
try {
    Invoke-RestMethod -Uri "http://127.0.0.1:$port/health" | ConvertTo-Json -Depth 5
} catch {
    Write-Host $_.Exception.Message
}

Write-Host "`n[test] 发送一句话测试 (/v1/chat/completions):"
$headers = @{ "Content-Type" = "application/json" }
if ($apiKey) { $headers["Authorization"] = "Bearer $apiKey" }
$body = @{
    model = $Model
    stream = $false
    messages = @(@{ role = "user"; content = $Prompt })
} | ConvertTo-Json -Depth 5

Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$port/v1/chat/completions" -Headers $headers -Body $body | ConvertTo-Json -Depth 6
