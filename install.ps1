# Installs Houston on Windows from the latest GitHub release, runs it in the
# background at startup, and opens the dashboard.
#
#   irm https://raw.githubusercontent.com/Svemeh/houston-we-have-a-packet-loss/main/install.ps1 | iex
#
# Run it again to update. To remove (your data is kept):
#
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/Svemeh/houston-we-have-a-packet-loss/main/install.ps1))) -Uninstall
#
# Needs admin once (Windows asks): Houston runs as a SYSTEM scheduled task at
# startup, so it keeps measuring with nobody logged in.
#   app   C:\Program Files\Houston\houston.exe
#   data  C:\ProgramData\Houston\data

param([switch]$Uninstall)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar makes downloads crawl
# Windows PowerShell 5.1 can default to TLS 1.0, which GitHub refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'Svemeh/houston-we-have-a-packet-loss'
$ScriptUrl = "https://raw.githubusercontent.com/$Repo/main/install.ps1"
$DownloadUrl = if ($env:HOUSTON_DOWNLOAD_URL) { $env:HOUSTON_DOWNLOAD_URL } else { "https://github.com/$Repo/releases/latest/download" }
$Port = 2101
$TaskName = 'Houston'
$AppDir = Join-Path $env:ProgramFiles 'Houston'
$DataDir = Join-Path $env:ProgramData 'Houston'
$Exe = Join-Path $AppDir 'houston.exe'

# ---- Admin ------------------------------------------------------------------
# Piped through iex there is no script file to re-run, so the elevated window
# downloads this script again.
$identity = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host 'Houston needs admin rights once to install. Windows will ask.'
    $command = "& ([scriptblock]::Create((irm '$ScriptUrl')))"
    if ($Uninstall) { $command += ' -Uninstall' }
    $command += "; Write-Host; Read-Host 'Done. Press Enter to close'"
    Start-Process powershell -Verb RunAs -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $command)
    return
}

function Stop-Houston {
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    }
    Get-Process -Name houston -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $Exe } |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
}

# ---- Uninstall --------------------------------------------------------------

if ($Uninstall) {
    Stop-Houston
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $AppDir -ErrorAction SilentlyContinue
    Write-Host "Houston removed. Your data is still in $DataDir\data"
    return
}

# ---- Download ---------------------------------------------------------------

$arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($arch) {
    'AMD64' { $asset = 'houston_Windows_x86_64.zip' }
    'ARM64' { $asset = 'houston_Windows_arm64.zip' }
    default { throw "Unsupported CPU: $arch" }
}

$work = Join-Path ([IO.Path]::GetTempPath()) ("houston-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null
try {
    Write-Host "Downloading $asset ..."
    $zip = Join-Path $work $asset
    Invoke-WebRequest -UseBasicParsing -Uri "$DownloadUrl/$asset" -OutFile $zip
    $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$DownloadUrl/checksums.txt").Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }

    $expected = $null
    foreach ($line in ($sums -split "`n")) {
        $fields = $line.Trim() -split '\s+'
        if ($fields.Count -eq 2 -and $fields[1] -eq $asset) { $expected = $fields[0] }
    }
    if (-not $expected) { throw "$asset is not in checksums.txt" }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash
    if ($actual -ne $expected) { throw "Checksum mismatch for $asset, not installing" }

    Expand-Archive -Path $zip -DestinationPath $work -Force

    # ---- Install --------------------------------------------------------------

    Stop-Houston
    New-Item -ItemType Directory -Force -Path $AppDir, $DataDir | Out-Null
    Copy-Item -Force (Join-Path $work 'houston.exe') $Exe
}
finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

# Runs as SYSTEM at boot, restarts if it crashes, never times out. Priority 4
# is normal; the task default (7) is below normal, which would delay the
# replies Houston is timing. The working directory is where data\ ends up.
$action = New-ScheduledTaskAction -Execute $Exe -WorkingDirectory $DataDir
$trigger = New-ScheduledTaskTrigger -AtStartup
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -Priority 4
Register-ScheduledTask -TaskName $TaskName -Description 'Houston, We Have a Packet Loss (Starlink monitor)' `
    -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $TaskName

$version = & $Exe -version
Write-Host "Installed $version. Starts when Windows starts."
Write-Host "  data: $DataDir\data"

$up = $false
for ($i = 0; $i -lt 15 -and -not $up; $i++) {
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "http://localhost:$Port/" -TimeoutSec 2 | Out-Null
        $up = $true
    }
    catch { Start-Sleep -Seconds 1 }
}
if ($up) {
    Start-Process "http://localhost:$Port/"
}
else {
    Write-Host "Houston did not answer yet. Is something else using port $Port?"
}
Write-Host "Dashboard: http://localhost:$Port"
