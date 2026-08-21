# veris-proxy installer for Windows.
#
#   powershell -c "irm https://raw.githubusercontent.com/veris-ai/veris-proxy/main/scripts/install.ps1 | iex"
#
# Installs the release binary to $env:VERIS_INSTALL_DIR (default
# %LOCALAPPDATA%\Programs\veris-proxy). No admin rights, no package manager.
#
#   $env:VERIS_PROXY_VERSION = "v0.8.0"   pin a version (default: latest)

$ErrorActionPreference = "Stop"

$repo = "veris-ai/veris-proxy"
$version = if ($env:VERIS_PROXY_VERSION) { $env:VERIS_PROXY_VERSION } else { "latest" }
$installDir = if ($env:VERIS_INSTALL_DIR) { $env:VERIS_INSTALL_DIR } else {
  Join-Path $env:LOCALAPPDATA "Programs\veris-proxy"
}

# Only amd64 binaries are published for Windows; an arm64 machine runs them
# under emulation, which is fine for a proxy.
$asset = "veris-proxy-windows-amd64.exe"
$url = if ($version -eq "latest") {
  "https://github.com/$repo/releases/latest/download/$asset"
} else {
  "https://github.com/$repo/releases/download/$version/$asset"
}

New-Item -ItemType Directory -Force -Path $installDir | Out-Null
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "veris-proxy-$([guid]::NewGuid()).exe"

Write-Host "downloading $url"
try {
  Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
} catch {
  throw "veris-proxy install: could not download $asset ($version) from $url -- $($_.Exception.Message)"
}

# Refuse to install something that is not the expected binary.
& $tmp version *> $null
if ($LASTEXITCODE -ne 0) {
  Remove-Item $tmp -Force -ErrorAction SilentlyContinue
  throw "veris-proxy install: downloaded file is not a runnable veris-proxy binary"
}

$target = Join-Path $installDir "veris-proxy.exe"
Move-Item -Force $tmp $target
Write-Host "installed $(& $target version) to $target"

# Persist the install dir on the user's PATH when it is not already there.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$installDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
  Write-Host "added $installDir to your PATH; open a new terminal to pick it up"
}
