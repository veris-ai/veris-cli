# veris installer for Windows.
#
#   powershell -c "irm https://raw.githubusercontent.com/veris-ai/veris-cli/main/scripts/install.ps1 | iex"
#
# Installs the release binary to $env:VERIS_INSTALL_DIR (default
# %LOCALAPPDATA%\Programs\veris-proxy). No admin rights, no package manager.
# The directory keeps its pre-rename name on purpose: it is already on the
# PATH of every machine that ran an earlier installer, and an upgrade has to
# land on top of that install rather than beside it.
#
#   $env:VERIS_PROXY_VERSION = "v0.8.0"   pin a version (default: latest)
#
# Releases up to v0.8.1 publish the binary as veris-proxy-windows-amd64.exe;
# later ones as veris-windows-amd64.exe. Both names are tried, so a pinned
# older version and the latest release install the same way.

$ErrorActionPreference = "Stop"

$repo = "veris-ai/veris-cli"
$version = if ($env:VERIS_PROXY_VERSION) { $env:VERIS_PROXY_VERSION } else { "latest" }
$installDir = if ($env:VERIS_INSTALL_DIR) { $env:VERIS_INSTALL_DIR } else {
  Join-Path $env:LOCALAPPDATA "Programs\veris-proxy"
}

# Only amd64 binaries are published for Windows; an arm64 machine runs them
# under emulation, which is fine for a proxy.
$asset = "veris-windows-amd64.exe"
$legacyAsset = "veris-proxy-windows-amd64.exe"
function Release-Url($name) {
  if ($version -eq "latest") {
    "https://github.com/$repo/releases/latest/download/$name"
  } else {
    "https://github.com/$repo/releases/download/$version/$name"
  }
}

New-Item -ItemType Directory -Force -Path $installDir | Out-Null
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "veris-$([guid]::NewGuid()).exe"

# The new asset name first, then the name releases carried before the rename.
$url = Release-Url $asset
Write-Host "downloading $url"
try {
  Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
} catch {
  $firstError = $_.Exception.Message
  $url = Release-Url $legacyAsset
  Write-Host "downloading $url"
  try {
    Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
  } catch {
    throw "veris install: could not download $asset ($version) -- $firstError; nor $legacyAsset -- $($_.Exception.Message)"
  }
}

# Refuse to install something that is not the expected binary.
& $tmp version *> $null
if ($LASTEXITCODE -ne 0) {
  Remove-Item $tmp -Force -ErrorAction SilentlyContinue
  throw "veris install: downloaded file is not a runnable veris binary"
}

$target = Join-Path $installDir "veris.exe"
Move-Item -Force $tmp $target
Write-Host "installed $(& $target version) to $target"

# The binary used to be called veris-proxy, and scripts, skills and CI
# configs still invoke it by that name. A .cmd shim beside the binary keeps
# every one of them working; %~dp0 is the shim's own directory, so it finds
# its sibling whether or not $installDir is on the PATH yet.
#
# An earlier installer left veris-proxy.exe here, and PATHEXT resolves .EXE
# before .CMD: with the old binary still beside it, `veris-proxy` would keep
# running the stale version and the shim would never be reached.
$stale = Join-Path $installDir "veris-proxy.exe"
if (Test-Path $stale) {
  Remove-Item $stale -Force
  Write-Host "removed the previous $stale; veris-proxy now runs through the shim"
}
$shim = Join-Path $installDir "veris-proxy.cmd"
Set-Content -Path $shim -Value "@echo off`r`n`"%~dp0veris.exe`" %*" -Encoding ASCII
Write-Host "installed a veris-proxy shim to $shim"

# Persist the install dir on the user's PATH when it is not already there.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$installDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
  Write-Host "added $installDir to your PATH; open a new terminal to pick it up"
}
