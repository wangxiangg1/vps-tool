[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$RuntimeRoot = Join-Path $PSScriptRoot "runtime"

function Stop-RecordedProcess([string]$PidPath) {
    if (-not (Test-Path -LiteralPath $PidPath)) { return }
    $raw = (Get-Content -Raw -LiteralPath $PidPath).Trim()
    $recordedPid = 0
    if ([int]::TryParse($raw, [ref]$recordedPid)) {
        $process = Get-Process -Id $recordedPid -ErrorAction SilentlyContinue
        if ($process) {
            Stop-Process -Id $recordedPid -Force -ErrorAction SilentlyContinue
            Write-Output "Stopped $($process.ProcessName) ($recordedPid)"
        }
    }
    Remove-Item -LiteralPath $PidPath -Force -ErrorAction SilentlyContinue
}

Stop-RecordedProcess (Join-Path $RuntimeRoot "mock-agent.pid")
Stop-RecordedProcess (Join-Path $RuntimeRoot "server.pid")
Write-Output "Demo processes stopped. Disposable files remain under $RuntimeRoot."
