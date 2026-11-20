param(
    [string]$Account = "default",
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$CommandArgs
)
$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot

$port = "8317"
$envFile = Join-Path $RootDir ".env"
if (Test-Path $envFile) {
    $line = Get-Content $envFile | Where-Object { $_ -match "^HOST_PORT=(.+)$" } | Select-Object -First 1
    if ($line) { $port = $Matches[1].Trim() }
}

if ($CommandArgs.Count -gt 0 -and $CommandArgs[0] -eq "status") {
    $d = Invoke-RestMethod -Uri "http://127.0.0.1:$port/admin/status"
    $r = $d.report
    $passed = ($r.checks | Where-Object { $_.state -eq "passed" }).Count
    Write-Host "门控状态 : $(if ($r.authorized) { '已放行' } else { '已熔断' }) ($passed/$($r.checks.Count))"
    if ($r.block_reason) { Write-Host "拦截原因 : $($r.block_reason)" }
    Write-Host "出口 IP  : $($r.exit_ip) ($($r.location.country_code) · $($r.location.city))"
    Write-Host "同步环境 : TZ=$($r.synced_tz)  LANG=$($r.synced_locale)"
    Write-Host "当前节点 : $($d.gateway.selected_node)"
    exit 0
}

if ($CommandArgs.Count -gt 0 -and $CommandArgs[0] -eq "browser") {
    $url = if ($CommandArgs.Count -gt 1) { $CommandArgs[1] } else { "https://claude.ai" }
    docker exec -d -u airlock -e DISPLAY=:79 -e HOME=/data/desktop -e XDG_RUNTIME_DIR=/run/airlock-desktop mini-cage-desktop /usr/local/bin/airlock-browser $url
    Write-Host "[edualc] 已在隔离浏览器中打开: $url"
    Write-Host "         查看画面: http://127.0.0.1:8320/edualc-desktop.html 或控制台「隔离浏览器」页"
    exit 0
}

if ($CommandArgs.Count -gt 0 -and $CommandArgs[0] -eq "login") {
    $target = if ($CommandArgs.Count -gt 1) { $CommandArgs[1] } else { $Account }
    & (Join-Path $PSScriptRoot "login.ps1") -Account $target
    exit $LASTEXITCODE
}

if ($CommandArgs.Count -gt 0 -and $CommandArgs[0] -eq "resume") {
    $rest = if ($CommandArgs.Count -gt 1) { $CommandArgs[1..($CommandArgs.Count - 1)] } else { @() }
    & python (Join-Path $PSScriptRoot "batch-resume.py") --account $Account @rest
    exit $LASTEXITCODE
}

if ($CommandArgs.Count -gt 0 -and $CommandArgs[0] -eq "claude") {
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/health" | Out-Null
    } catch {
        Write-Error "[edualc] 安全门控未放行，已拒绝启动 claude。请打开 http://127.0.0.1:$port/ui 查看原因。"
        exit 1
    }
}

$dockerArgs = @("exec", "-it", "-e", "ACCOUNT=$Account", "mini-cage-relay", "/usr/local/bin/airlock-shell.sh")
if ($CommandArgs.Count -gt 0) { $dockerArgs += $CommandArgs }
& docker @dockerArgs
