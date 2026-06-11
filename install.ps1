# install.ps1 -- PowerShell installer for guild (Windows)
#
# Mirror of install.sh for Windows. Downloads the matching release zip
# + checksums.txt from GitHub, verifies SHA256 BEFORE extraction, and
# installs the binary to %LOCALAPPDATA%\Programs\guild by default.
#
# Supported: windows x amd64, arm64. Works on Windows PowerShell 5.1
# and PowerShell 7+. This file is deliberately ASCII-only: PowerShell
# 5.1 reads BOM-less scripts as ANSI, which mangles multibyte chars.
#
# Usage:
#   irm https://github.com/mathomhaus/guild/releases/latest/download/install.ps1 | iex
#   .\install.ps1 [-Version vX.Y.Z] [-Prefix DIR]
# Environment overrides (useful with irm | iex, which takes no args):
#   GUILD_VERSION         -- same as -Version
#   GUILD_INSTALL_PREFIX  -- same as -Prefix
#
# Signature (cosign) verification is deliberately NOT performed here;
# this script checks SHA256 only -- same policy as install.sh. See
# SECURITY.md and .goreleaser.yml for the cosign verify-blob steps.
#
# No telemetry, no phone-home. The only network calls are to
# api.github.com (to resolve the latest tag) and to
# github.com/mathomhaus/guild/releases/download/... (for the zip and
# checksums.txt).

param(
    [string]$Version = $env:GUILD_VERSION,
    [string]$Prefix = $env:GUILD_INSTALL_PREFIX
)

$ErrorActionPreference = 'Stop'

$Repo = 'mathomhaus/guild'
$BinName = 'guild'

if (-not $Prefix) {
    $Prefix = Join-Path $env:LOCALAPPDATA 'Programs\guild'
}

# Windows PowerShell 5.1 defaults to TLS 1.0; GitHub requires >= 1.2.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# --- platform detection ----------------------------------------------
$archRaw = $env:PROCESSOR_ARCHITECTURE
switch ($archRaw) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { throw "unsupported architecture: $archRaw (supported: AMD64, ARM64)" }
}

$tmpDir = Join-Path $env:TEMP "guild-install-$([System.IO.Path]::GetRandomFileName())"
New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

try {
    # --- version resolution ------------------------------------------
    if (-not $Version) {
        Write-Host "resolving latest release from github.com/$Repo..."
        try {
            $latest = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
        } catch {
            throw "failed to query the GitHub API ($($_.Exception.Message)). If you are rate-limited, set `$env:GUILD_VERSION='vX.Y.Z' to bypass it."
        }
        $Version = $latest.tag_name
        if (-not $Version) {
            throw "could not read tag_name from the GitHub API response. Set `$env:GUILD_VERSION='vX.Y.Z' to bypass the API."
        }
    }

    # goreleaser uses .Version (without the leading v) in name_template.
    $versionNum = $Version -replace '^v', ''

    # --- download ----------------------------------------------------
    $zipName = "${BinName}_${versionNum}_windows_${arch}.zip"
    $baseUrl = "https://github.com/$Repo/releases/download/$Version"
    $zipPath = Join-Path $tmpDir $zipName
    $checksumsPath = Join-Path $tmpDir 'checksums.txt'

    Write-Host "downloading $zipName ($Version)..."
    try {
        Invoke-WebRequest "$baseUrl/$zipName" -OutFile $zipPath -UseBasicParsing
    } catch {
        throw "failed to download $baseUrl/$zipName -- does that release exist for windows/$arch?"
    }

    Write-Host 'downloading checksums.txt...'
    Invoke-WebRequest "$baseUrl/checksums.txt" -OutFile $checksumsPath -UseBasicParsing

    # --- verify SHA256 BEFORE extracting -----------------------------
    $expectedLine = Get-Content $checksumsPath | Where-Object { $_ -match "\s+$([regex]::Escape($zipName))$" } | Select-Object -First 1
    if (-not $expectedLine) {
        throw "no checksum entry for $zipName in checksums.txt"
    }
    $expected = ($expectedLine -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -ne $actual) {
        throw "SHA256 mismatch for ${zipName}: expected $expected, got $actual"
    }
    Write-Host 'sha256 verified'

    # --- extract + install -------------------------------------------
    $extractDir = Join-Path $tmpDir 'extract'
    Expand-Archive $zipPath -DestinationPath $extractDir -Force

    $binSrc = Join-Path $extractDir "$BinName.exe"
    if (-not (Test-Path $binSrc)) {
        throw "binary '$BinName.exe' not found inside $zipName"
    }

    New-Item -ItemType Directory -Path $Prefix -Force | Out-Null
    $installPath = Join-Path $Prefix "$BinName.exe"
    # Windows locks running executables against overwrite/delete but
    # allows rename. If guild.exe is running (e.g. as an MCP server),
    # rename it aside, move the new binary in, then best-effort delete
    # the old one (next install cleans it up if it is still locked).
    Get-ChildItem -Path $Prefix -Filter "$BinName.exe.old-*" -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
    Copy-Item $binSrc "$installPath.tmp" -Force
    $oldPath = "$installPath.old-" + [System.IO.Path]::GetRandomFileName()
    if (Test-Path $installPath) {
        Move-Item $installPath $oldPath
    }
    Move-Item "$installPath.tmp" $installPath
    Remove-Item $oldPath -Force -ErrorAction SilentlyContinue

    # --- post-install ------------------------------------------------
    Write-Host ''
    Write-Host "installed $BinName $Version -> $installPath"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $onPath = ($env:Path -split ';') -contains $Prefix -or ($userPath -split ';') -contains $Prefix
    if (-not $onPath) {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$Prefix", 'User')
        Write-Host "added $Prefix to your user PATH -- open a new terminal to pick it up"
    }

    Write-Host ''
    Write-Host 'next step:'
    Write-Host "  $BinName mcp install   # register guild with your MCP client"
    Write-Host ''
    Write-Host 'note: semantic (vector) retrieval is currently unavailable on'
    Write-Host 'Windows; search runs keyword (BM25) only. See'
    Write-Host 'internal/lore/embed/assets/README.md for why.'
} finally {
    Remove-Item $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
