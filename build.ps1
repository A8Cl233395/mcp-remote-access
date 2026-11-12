param(
    [string]$DefaultServer = ""
)

$ErrorActionPreference = "Stop"

$RootDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir = Join-Path $RootDir "bin"
$ReleasesDir = Join-Path $RootDir "releases"

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
New-Item -ItemType Directory -Force -Path $ReleasesDir | Out-Null

$LdFlags = "-s -w"
if ($DefaultServer -ne "") {
    $LdFlags = "$LdFlags -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=$DefaultServer"
    Write-Host "==> Embedding DefaultServerAddr: $DefaultServer" -ForegroundColor Yellow
}

Write-Host "==> Building Windows (amd64) binaries..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"

go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "relay-server.exe") ./cmd/relay-server
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "dev-client.exe") ./cmd/dev-client
go build -trimpath -ldflags="$LdFlags -H=windowsgui" -o (Join-Path $BinDir "user-client.exe") ./cmd/user-client

Write-Host "==> Building Linux (amd64) binaries..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"

go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "relay-server-linux-amd64") ./cmd/relay-server
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "dev-client-linux-amd64") ./cmd/dev-client
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "user-client-linux-amd64") ./cmd/user-client

# Restore host env
Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue

Write-Host "==> Populating ./releases/ for user-client auto-update..." -ForegroundColor Cyan
Copy-Item -Force (Join-Path $BinDir "user-client.exe") (Join-Path $ReleasesDir "user-client-windows-amd64.exe")
Copy-Item -Force (Join-Path $BinDir "user-client-linux-amd64") (Join-Path $ReleasesDir "user-client-linux-amd64")

Write-Host "==> Build complete! Artifacts in ./bin and ./releases:" -ForegroundColor Green
Get-ChildItem $BinDir, $ReleasesDir | Select-Object FullName, Length
