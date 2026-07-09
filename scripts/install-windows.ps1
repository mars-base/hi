# hi — Windows installer (PowerShell)
# Usage: irm https://raw.githubusercontent.com/mars-base/hi/main/install-windows.ps1 | iex

$ErrorActionPreference = "Continue"

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {}

# Get latest release information from GitHub.
$Repo = "mars-base/hi"
$Bin = "hi.exe"
$Tag = $null
try {
    $Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -ErrorAction Stop
    $Tag = $Release.tag_name
} catch {
    Write-Host "ERROR: Could not reach GitHub. Check your internet connection."
}

if ($Tag) {
    $Version = $Tag -replace '^v', ''
    $Arch = "amd64"
    $InstallDir = "$env:USERPROFILE\.local\bin"
    $Dest = Join-Path $InstallDir $Bin

    $currentVersion = ""
    try { $currentVersion = (& $Dest --version 2>$null) -replace '^hi ', '' } catch {}

    if ($currentVersion -eq $Version) {
        Write-Host "hi $Tag is already installed and up to date."
    } else {
        if ($currentVersion) { Write-Host "Upgrading hi: $currentVersion -> $Version" }
        else { Write-Host "Installing hi $Tag ..." }

        $Archive = "hi-${Version}-windows-${Arch}.zip"
        $Url = "https://github.com/$Repo/releases/download/$Tag/$Archive"
        $TmpDir = "$env:TEMP\hi-install-$Version"

        try {
            New-Item -ItemType Directory -Path $TmpDir -Force -ErrorAction SilentlyContinue | Out-Null
            Invoke-WebRequest -Uri $Url -OutFile (Join-Path $TmpDir $Archive) -ErrorAction Stop
            Expand-Archive -Path (Join-Path $TmpDir $Archive) -DestinationPath $TmpDir -Force -ErrorAction Stop

            $running = Get-Process hi -ErrorAction SilentlyContinue
            if ($running) {
                Write-Host ""
                Write-Host "WARNING: hi is running (PID: $($running.Id)). Stop it first (Ctrl+C), then re-run."
            } else {
                if (-not (Test-Path $InstallDir)) {
                    New-Item -ItemType Directory -Path $InstallDir -Force -ErrorAction SilentlyContinue | Out-Null
                }
                Remove-Item $Dest -Force -ErrorAction SilentlyContinue
                Move-Item -Force (Join-Path $TmpDir $Bin) $Dest -ErrorAction Stop

                Write-Host ""
                Write-Host "hi $Tag installed to $Dest"
                $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
                if ($UserPath -notlike "*$InstallDir*") {
                    Write-Host "Adding $InstallDir to PATH ..."
                    [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
                    $env:Path = "$env:Path;$InstallDir"
                }
                Write-Host ""
                Write-Host "Downloading helper scripts..."

                $ScriptBase = "https://raw.githubusercontent.com/$Repo/main/scripts"
                $Scripts = @("hi-statusline.py", "hi-statusline.bat")
                foreach ($Script in $Scripts) {
                    $ScriptDest = Join-Path $InstallDir $Script
                    try {
                        Invoke-WebRequest -Uri "$ScriptBase/$Script" -OutFile $ScriptDest -ErrorAction Stop
                        Write-Host "  + $Script"
                    } catch {
                        Write-Host "  ! $Script : download failed ($($_.Exception.Message))"
                    }
                }

                Write-Host ""
                Write-Host "Quick start:"
                Write-Host "  1. hi init-config      (auto-detects settings)"
                Write-Host "  2. Edit ~/.hi/config.yaml if needed"
                Write-Host "  3. hi                   (proxy + Claude Code)"
                Write-Host "  Or: hi proxy & hi cc    (standalone proxy + attach)"
                Write-Host ""
                Write-Host "Run: hi"

                # ─── Install counter (fire-and-forget) ─────────────────────
                # Anonymous install count for adoption tracking. No PII collected.
                # Query stats: curl https://runaifs.pages.dev/api/stats?project=hi
                try {
                    Invoke-RestMethod -Uri "https://runaifs.pages.dev/api/install?os=windows&project=hi" -Method Get -TimeoutSec 5 | Out-Null
                } catch {}
            }
        } catch {
            Write-Host ""
            Write-Host "ERROR: $($_.Exception.Message)"
        } finally {
            Remove-Item $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
