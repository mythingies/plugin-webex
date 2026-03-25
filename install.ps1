# plugin-webex installer for Windows
param(
    [string]$Version = "",
    [switch]$SkillOnly
)

$Repo = "mythingies/plugin-webex"
$ErrorActionPreference = "Stop"
$Binary = "webex-mcp"

# --- resolve version -------------------------------------------------------

function Resolve-LatestVersion {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing
    if (-not $release.tag_name) {
        throw "Could not determine latest release"
    }
    return $release.tag_name
}

# --- detect architecture ---------------------------------------------------

function Get-Arch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { return "amd64" }
        "ARM64" { return "arm64" }
        default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
    }
}

# --- install skill only ----------------------------------------------------

function Install-Skill {
    $skillDir = Join-Path $env:USERPROFILE ".claude\skills\webex"
    New-Item -ItemType Directory -Path $skillDir -Force | Out-Null
    $skillUrl = "https://raw.githubusercontent.com/$Repo/main/skills/webex/SKILL.md"
    Invoke-WebRequest -Uri $skillUrl -OutFile (Join-Path $skillDir "SKILL.md") -UseBasicParsing
    Write-Host ""
    Write-Host "Skill installed to $skillDir\SKILL.md"
    Write-Host ""
    Write-Host 'Set your token:  $env:WEBEX_TOKEN = "your-token"'
    Write-Host "Then use Claude Code normally - the skill is picked up automatically."
}

# --- install MCP binary ----------------------------------------------------

function Install-WebexMcp {
    $arch = Get-Arch

    if (-not $Version) {
        Write-Host "Resolving latest version..."
        $Version = Resolve-LatestVersion
    }

    $archive = "$Binary-windows-$arch.zip"
    $url = "https://github.com/$Repo/releases/download/$Version/$archive"
    $checksumsUrl = "https://github.com/$Repo/releases/download/$Version/checksums.txt"

    $installDir = Join-Path $env:LOCALAPPDATA "$Binary\bin"
    $tmpDir = Join-Path $env:TEMP "webex-mcp-install-$([System.Guid]::NewGuid())"

    try {
        New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

        Write-Host "Downloading $Binary $Version (windows/$arch)..."
        $archivePath = Join-Path $tmpDir $archive
        $checksumsPath = Join-Path $tmpDir "checksums.txt"

        Invoke-WebRequest -Uri $url -OutFile $archivePath -UseBasicParsing
        Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsPath -UseBasicParsing

        # verify checksum
        Write-Host "Verifying checksum..."
        $checksumLine = Get-Content $checksumsPath | Where-Object { $_ -like "*  $archive" }
        if (-not $checksumLine) {
            throw "No checksum found for $archive"
        }
        $expected = ($checksumLine -split '\s+')[0].ToLower()
        $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()

        if ($expected -ne $actual) {
            throw "Checksum mismatch: expected $expected, got $actual"
        }
        Write-Host "Checksum OK."

        # extract
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        Expand-Archive -Path $archivePath -DestinationPath $tmpDir -Force
        $binaryPath = Join-Path $tmpDir "$Binary.exe"
        Copy-Item -Path $binaryPath -Destination (Join-Path $installDir "$Binary.exe") -Force

        Write-Host ""
        Write-Host "Installed $Binary to $installDir\$Binary.exe"

        # add to PATH if not present
        $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
        if ($userPath -notlike "*$installDir*") {
            [Environment]::SetEnvironmentVariable("PATH", "$installDir;$userPath", "User")
            $env:PATH = "$installDir;$env:PATH"
            Write-Host "Added $installDir to your user PATH."
            Write-Host "Restart your terminal for PATH changes to take effect."
        }

        Write-Host ""
        Write-Host "Next: run the setup wizard in your project directory:"
        Write-Host ""
        Write-Host "  cd your-project"
        Write-Host "  $Binary --setup"
    }
    finally {
        if (Test-Path $tmpDir) {
            Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

# --- main ------------------------------------------------------------------

if ($SkillOnly) {
    Install-Skill
}
else {
    Write-Host "plugin-webex installer"
    Write-Host ""
    Write-Host "  1) MCP server  - full feature set (WebSocket, agent routing, priority inbox)"
    Write-Host "  2) Skill only  - lightweight, curl-based, no binary needed"
    Write-Host ""
    $choice = Read-Host "Choose [1/2] (default: 1)"

    switch ($choice) {
        "2" { Install-Skill }
        default { Install-WebexMcp }
    }
}
