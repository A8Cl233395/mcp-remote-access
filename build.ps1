param(
    [string]$DefaultServer = "",
    [switch]$SkipHook
)

$ErrorActionPreference = "Stop"

$RootDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir = Join-Path $RootDir "bin"
$ReleasesDir = Join-Path $RootDir "releases"
$Win7ToolchainDir = Join-Path $RootDir ".toolchain\go-win7"

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
New-Item -ItemType Directory -Force -Path $ReleasesDir | Out-Null

# Load .env file if present
$EnvFile = Join-Path $RootDir ".env"
if (Test-Path $EnvFile) {
    Write-Host "==> Loading build configuration from .env..." -ForegroundColor Gray
    Get-Content $EnvFile | ForEach-Object {
        $line = $_.Trim()
        if ($line -and -not $line.StartsWith("#") -and $line.Contains("=")) {
            $parts = $line.Split("=", 2)
            $k = $parts[0].Trim()
            $v = $parts[1].Trim().Trim('"').Trim("'")
            if (-not [string]::IsNullOrEmpty($k)) {
                [System.Environment]::SetEnvironmentVariable($k, $v, "Process")
            }
        }
    }
}

if ([string]::IsNullOrEmpty($DefaultServer)) {
    if ($env:DEFAULT_SERVER) {
        $DefaultServer = $env:DEFAULT_SERVER
    } elseif ($env:DEFAULT_SERVER_ADDR) {
        $DefaultServer = $env:DEFAULT_SERVER_ADDR
    } elseif ($env:MCP_SERVER_ADDR) {
        $DefaultServer = $env:MCP_SERVER_ADDR
    }
}

$LdFlags = "-s -w"
if ($DefaultServer -ne "") {
    $LdFlags = "$LdFlags -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=$DefaultServer"
    Write-Host "==> Embedding DefaultServerAddr: $DefaultServer" -ForegroundColor Yellow
} else {
    Write-Host "==> WARNING: DefaultServerAddr not specified (using 127.0.0.1:20055). Create .env or pass -DefaultServer." -ForegroundColor DarkYellow
}

Write-Host "==> Building Windows (amd64) binaries with standard Go..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"

go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "relay-server.exe") ./cmd/relay-server
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "dev-client.exe") ./cmd/dev-client
go build -trimpath -ldflags="$LdFlags -H=windowsgui" -o (Join-Path $BinDir "user-client.exe") ./cmd/user-client

Write-Host "==> Building Linux (amd64 & arm64) binaries with standard Go..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"

go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "relay-server-linux-amd64") ./cmd/relay-server
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "dev-client-linux-amd64") ./cmd/dev-client
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "user-client-linux-amd64") ./cmd/user-client

$env:GOARCH = "arm64"
go build -trimpath -ldflags="$LdFlags" -o (Join-Path $BinDir "user-client-linux-arm64") ./cmd/user-client

# Build dedicated Windows 7 32-bit (windows7-i386) user-client using XTLS/go-win7
$Win7GoExe = Join-Path $Win7ToolchainDir "bin\go.exe"
if (-not (Test-Path $Win7GoExe)) {
    $ZipUrl = "https://github.com/XTLS/go-win7/releases/download/patched-1.27.4/go-for-win7-windows-amd64.zip"
    $ExpectedSHA = "2fccdac532798af3e7ffe73a18e25f0763d22d4c87f963075c0f59fcef8e3645"
    $TempZip = Join-Path $env:TEMP "go-for-win7-windows-amd64.zip"
    Write-Host "==> Downloading XTLS/go-win7 toolchain (patched-1.27.4) for windows7-i386 build..." -ForegroundColor Yellow
    New-Item -ItemType Directory -Force -Path $Win7ToolchainDir | Out-Null
    curl.exe -fSL $ZipUrl -o $TempZip
    $ActualSHA = (Get-FileHash $TempZip -Algorithm SHA256).Hash.ToLower()
    if ($ActualSHA -ne $ExpectedSHA) {
        throw "go-win7 SHA256 mismatch: expected $ExpectedSHA, got $ActualSHA"
    }
    tar.exe -xf $TempZip -C $Win7ToolchainDir
}

Write-Host "==> Building Windows (386 / Win7+ compatible) user-client with go-win7..." -ForegroundColor Magenta
$env:GOROOT = $Win7ToolchainDir
$env:GOTOOLCHAIN = "local"
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "386"

& $Win7GoExe build -trimpath -ldflags="$LdFlags -H=windowsgui" -o (Join-Path $BinDir "user-client-windows-386.exe") ./cmd/user-client

# Restore host env
Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\GOROOT -ErrorAction SilentlyContinue
Remove-Item Env:\GOTOOLCHAIN -ErrorAction SilentlyContinue

Write-Host "==> Populating ./releases/ for user-client auto-update..." -ForegroundColor Cyan
Copy-Item -Force (Join-Path $BinDir "user-client.exe") (Join-Path $ReleasesDir "user-client-windows-amd64.exe")
Copy-Item -Force (Join-Path $BinDir "user-client-windows-386.exe") (Join-Path $ReleasesDir "user-client-windows-386.exe")
Copy-Item -Force (Join-Path $BinDir "user-client-linux-amd64") (Join-Path $ReleasesDir "user-client-linux-amd64")
Copy-Item -Force (Join-Path $BinDir "user-client-linux-arm64") (Join-Path $ReleasesDir "user-client-linux-arm64")

Write-Host "==> Build complete! Artifacts in ./bin and ./releases:" -ForegroundColor Green
Get-ChildItem $BinDir, $ReleasesDir | Select-Object FullName, Length

if (-not $SkipHook -and $env:SKIP_POST_BUILD_HOOK -ne "1") {
    $HookScript = if ($env:POST_BUILD_HOOK) { $env:POST_BUILD_HOOK } else { Join-Path $RootDir "post-build.ps1" }
    if (Test-Path $HookScript) {
        Write-Host "==> Running post-build hook: $HookScript" -ForegroundColor Cyan
        $env:MCP_ROOT_DIR = $RootDir
        $env:MCP_BIN_DIR = $BinDir
        $env:MCP_RELEASES_DIR = $ReleasesDir
        $global:LASTEXITCODE = 0
        & $HookScript
        if ($LASTEXITCODE -ne 0) {
            throw "Post-build hook failed with exit code $LASTEXITCODE"
        }
    }
}
