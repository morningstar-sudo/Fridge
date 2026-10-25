# Fridge installer for Windows: runs install.sh inside WSL, where Docker and the swarm live.
#   irm https://raw.githubusercontent.com/morningstar-sudo/Fridge/main/install.ps1 | iex
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/morningstar-sudo/Fridge/main/install.ps1))) --admin alice
# Every argument is passed to install.sh (--admin NAME, --hosts A,B, --version TAG, --uninstall [--purge --yes], ...).
# Needs WSL with a Linux distribution that has Docker (Docker Desktop's WSL integration works).
$ErrorActionPreference = 'Stop'

if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
    throw 'WSL is not installed. Run "wsl --install" in an administrator terminal, install Docker in the distribution (or turn on Docker Desktop''s WSL integration) and run this again.'
}
$url = 'https://raw.githubusercontent.com/morningstar-sudo/Fridge/main/install.sh'
$quoted = ($args | ForEach-Object { "'" + ($_ -replace "'", "'\''") + "'" }) -join ' '
wsl.exe -- sh -c "curl -fsSL $url | sh -s -- $quoted"
exit $LASTEXITCODE
