# Installs ccgp and ccgpy from the GitHub release for this CPU (Windows).
#
#   irm https://raw.githubusercontent.com/PHPCraftdream/cc-go-proxy/main/install.ps1 | iex
#
# Environment:
#   CCGP_VERSION      version to install, e.g. 0.1.1 (default: the latest release)
#   CCGP_INSTALL_DIR  where to put the binaries (default: %LOCALAPPDATA%\Programs\ccgp)
#   CCGP_NO_PATH=1    do not add the directory to the user PATH
#
# The archive is checked against the release's SHA256SUMS before anything is
# installed. For signed build provenance see docs/releasing.md.

function Install-Ccgp {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with a progress bar
    try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }

    $repo = 'PHPCraftdream/cc-go-proxy'
    $base = "https://github.com/$repo/releases"

    $arch = switch ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
        'X64'   { 'amd64' }
        'Arm64' { 'arm64' }
        default { throw "unsupported CPU $_" }
    }

    $version = $env:CCGP_VERSION
    if ($version) { $version = $version.TrimStart('v') }
    if (-not $version) {
        $latest = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'ccgp-install' }
        $version = ([string]$latest.tag_name).TrimStart('v')
        if (-not $version) { throw 'cannot find the latest release' }
    }

    $dir = $env:CCGP_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'Programs\ccgp' }

    $name = "ccgp_${version}_windows_${arch}"
    $archive = "$name.zip"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("ccgp-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Installing ccgp $version (windows/$arch) into $dir"
        Invoke-WebRequest -UseBasicParsing -Uri "$base/download/v$version/$archive" -OutFile (Join-Path $tmp $archive)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/download/v$version/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')

        $want = $null
        foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -eq 2 -and $parts[1].Trim() -eq $archive) { $want = $parts[0].ToLowerInvariant() }
        }
        if (-not $want) { throw "$archive is not listed in SHA256SUMS" }
        $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLowerInvariant()
        if ($got -ne $want) { throw "checksum mismatch for $archive (expected $want, got $got)" }

        Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
        foreach ($exe in 'ccgp.exe', 'ccgpy.exe') {
            $src = Join-Path (Join-Path $tmp $name) $exe
            if (-not (Test-Path $src)) { throw "$exe is missing from the archive" }
            $dst = Join-Path $dir $exe
            if (Test-Path $dst) {
                # A running exe cannot be overwritten but can be renamed away.
                $old = "$dst.old"
                Remove-Item $old -Force -ErrorAction SilentlyContinue
                Move-Item $dst $old -Force
            }
            Copy-Item $src $dst -Force
        }
    }
    finally {
        Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    Write-Host "Installed: $dir\ccgp.exe, $dir\ccgpy.exe"

    if ($env:CCGP_NO_PATH -ne '1') {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = @(); if ($userPath) { $entries = $userPath -split ';' | Where-Object { $_ } }
        if ($entries -notcontains $dir) {
            [Environment]::SetEnvironmentVariable('Path', (($entries + $dir) -join ';'), 'User')
            Write-Host "Added $dir to your user PATH; open a new terminal to use it."
        }
    }
    Write-Host "Next: ccgp --help   (you also need Claude Code: 'claude' on PATH)"
}

Install-Ccgp
