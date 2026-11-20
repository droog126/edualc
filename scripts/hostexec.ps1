param(
    [string]$Action = "start"
)
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        if ($_ -match "^\s*([^#=\s]+)=(.*)$") {
            [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2].Trim(), "Process")
        }
    }
}

New-Item -ItemType Directory -Force -Path ".bin", "data\runtime" | Out-Null
$pidFile = "data\runtime\hostexec.pid"
$logFile = "data\runtime\hostexec.log"
$bin = ".bin\hostexec.exe"

if (Test-Path $pidFile) {
    $oldPid = (Get-Content $pidFile -ErrorAction SilentlyContinue).Trim()
    if ($oldPid) { Stop-Process -Id ([int]$oldPid) -Force -ErrorAction SilentlyContinue }
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
}

if ($Action -eq "stop") {
    Write-Host "[hostexec] 已停止。"
    exit 0
}

go build -trimpath -ldflags="-s -w" -o $bin ./cmd/hostexec
$proc = Start-Process -FilePath $bin -RedirectStandardOutput $logFile -RedirectStandardError ($logFile + ".err") -WindowStyle Hidden -PassThru
$proc.Id | Out-File -Encoding ascii $pidFile

$port = if ($env:HOSTEXEC_PORT) { $env:HOSTEXEC_PORT } else { "8325" }
for ($i = 0; $i -lt 20; $i++) {
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/health" | Out-Null
        Write-Host "[hostexec] 已启动 (PID=$($proc.Id), 127.0.0.1:$port)"
        exit 0
    } catch { Start-Sleep -Milliseconds 200 }
}
Write-Error "[hostexec] 启动失败，请查看 $logFile"
exit 1
