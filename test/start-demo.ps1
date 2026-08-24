[CmdletBinding()]
param(
    [int]$Port = 8000
)

$ErrorActionPreference = "Stop"
$ProjectRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$RuntimeRoot = Join-Path $PSScriptRoot "runtime"
$DbPath = Join-Path $RuntimeRoot "vps-tool-demo.sqlite3"
$FixturePath = Join-Path $RuntimeRoot "agents.json"
$ServerPidPath = Join-Path $RuntimeRoot "server.pid"
$AgentPidPath = Join-Path $RuntimeRoot "mock-agent.pid"
$ServerLogPath = Join-Path $RuntimeRoot "server.log"
$ServerErrorPath = Join-Path $RuntimeRoot "server.error.log"
$AgentLogPath = Join-Path $RuntimeRoot "mock-agent.log"
$AgentErrorPath = Join-Path $RuntimeRoot "mock-agent.error.log"

New-Item -ItemType Directory -Force -Path $RuntimeRoot | Out-Null
$Python = (Get-Command python -ErrorAction Stop).Source
$existingListener = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($existingListener) {
    throw "port $Port is already in use; stop the existing local service or choose another port"
}

function Stop-RecordedProcess([string]$PidPath) {
    if (-not (Test-Path -LiteralPath $PidPath)) { return }
    $raw = (Get-Content -Raw -LiteralPath $PidPath).Trim()
    $recordedPid = 0
    if ([int]::TryParse($raw, [ref]$recordedPid)) {
        $process = Get-Process -Id $recordedPid -ErrorAction SilentlyContinue
        if ($process) {
            Stop-Process -Id $recordedPid -Force -ErrorAction SilentlyContinue
        }
    }
    Remove-Item -LiteralPath $PidPath -Force -ErrorAction SilentlyContinue
}

Stop-RecordedProcess $AgentPidPath
Stop-RecordedProcess $ServerPidPath

$env:VPS_TOOL_ADMIN_USER = "demo"
$env:VPS_TOOL_ADMIN_PASSWORD = "demo-local-panel-2026"
$env:VPS_TOOL_COOKIE_SECURE = "false"
$env:VPS_TOOL_DB_PATH = $DbPath
$env:VPS_TOOL_HEARTBEAT_TIMEOUT = "90"
$env:VPS_TOOL_SCHEDULER_INTERVAL = "5"
$demoEnvironment = @{
    VPS_TOOL_ADMIN_USER = "demo"
    VPS_TOOL_ADMIN_PASSWORD = "demo-local-panel-2026"
    VPS_TOOL_COOKIE_SECURE = "false"
    VPS_TOOL_DB_PATH = $DbPath
    VPS_TOOL_HEARTBEAT_TIMEOUT = "90"
    VPS_TOOL_SCHEDULER_INTERVAL = "5"
    PYTHONUNBUFFERED = "1"
}

$serverArguments = @(
    (Join-Path $PSScriptRoot "run_server.py"),
    "--db", $DbPath,
    "--port", "$Port"
)
$server = Start-Process -FilePath $Python -ArgumentList $serverArguments -WorkingDirectory $ProjectRoot -Environment $demoEnvironment -RedirectStandardOutput $ServerLogPath -RedirectStandardError $ServerErrorPath -WindowStyle Hidden -PassThru
$server.Id | Set-Content -LiteralPath $ServerPidPath -Encoding ascii

try {
    $ready = $false
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $health = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/api/health" -TimeoutSec 2
            if ($health.ok) {
                $ready = $true
                break
            }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $ready) {
        $errorTail = if (Test-Path -LiteralPath $ServerErrorPath) { Get-Content -Tail 20 -LiteralPath $ServerErrorPath | Out-String } else { "" }
        throw "control plane did not become ready. $errorTail"
    }

    & $Python (Join-Path $PSScriptRoot "seed_demo.py") --db $DbPath --fixture $FixturePath
    if ($LASTEXITCODE -ne 0) { throw "demo data seeding failed" }

    $agentArguments = @(
        (Join-Path $PSScriptRoot "mock_agent.py"),
        "--url", "ws://127.0.0.1:$Port/agent",
        "--fixture", $FixturePath
    )
    $agent = Start-Process -FilePath $Python -ArgumentList $agentArguments -WorkingDirectory $ProjectRoot -Environment @{ PYTHONUNBUFFERED = "1" } -RedirectStandardOutput $AgentLogPath -RedirectStandardError $AgentErrorPath -WindowStyle Hidden -PassThru
    $agent.Id | Set-Content -LiteralPath $AgentPidPath -Encoding ascii

    $online = $false
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        try {
            $health = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/api/health" -TimeoutSec 2
            if ([int]$health.online_agents -ge 2) {
                $online = $true
                break
            }
        } catch {
            # Keep polling while the mock Agents complete their handshake.
        }
        Start-Sleep -Milliseconds 500
    }
    if (-not $online) { throw "mock Agents did not connect; inspect $AgentLogPath" }

    Write-Output "Demo panel is ready: http://127.0.0.1:$Port/"
    Write-Output "Demo login user: demo"
    Write-Output "Demo login password: demo-local-panel-2026"
    Write-Output "Stop: pwsh -File .\test\stop-demo.ps1"
    Write-Output "Runtime: $RuntimeRoot"
} catch {
    Stop-RecordedProcess $AgentPidPath
    Stop-RecordedProcess $ServerPidPath
    throw
}
