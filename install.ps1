# remote-cc installer for Windows (PowerShell 5.1+ or 7).
#
#   irm https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.ps1 | iex
#
# Environment (all optional):
#   RCC_VERSION       release tag to install, e.g. v0.3.2 (default: latest)
#   RCC_INSTALL_DIR   target directory (default: %USERPROFILE%\.local\bin)
#   RCC_NAME          command name (default: remote-cc)
#   RCC_REPO          GitHub owner/repo (default: NotDesigned/remote-cc)

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is very slow with the progress bar
    try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch {}

    function Say($m) { Write-Host "remote-cc: $m" }
    function Env($n, $d) { $v = [Environment]::GetEnvironmentVariable($n); if ($v) { $v } else { $d } }

    $repo = Env 'RCC_REPO' 'NotDesigned/remote-cc'
    $dir = Env 'RCC_INSTALL_DIR' (Join-Path $env:USERPROFILE '.local\bin')
    $name = Env 'RCC_NAME' 'remote-cc'
    $version = Env 'RCC_VERSION' 'latest'

    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "unsupported architecture $($env:PROCESSOR_ARCHITECTURE)" }
    }
    $asset = "rcc-windows-$arch.exe"
    if ($version -eq 'latest') {
        $base = "https://github.com/$repo/releases/latest/download"
    } else {
        if (-not $version.StartsWith('v')) { $version = "v$version" }
        $base = "https://github.com/$repo/releases/download/$version"
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("remote-cc-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force $tmp | Out-Null
    try {
        Say "downloading $asset ($version) from github.com/$repo"
        try { Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile "$tmp\SHA256SUMS" }
        catch { throw "could not download SHA256SUMS (does release $version exist?): $($_.Exception.Message)" }
        try { Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile "$tmp\$asset" }
        catch { throw "could not download $asset`: $($_.Exception.Message)" }

        $want = $null
        foreach ($line in Get-Content "$tmp\SHA256SUMS") {
            $f = $line -split '\s+'
            if ($f.Count -ge 2 -and ($f[1] -eq $asset -or $f[1] -eq "*$asset")) { $want = $f[0].ToLower() }
        }
        if (-not $want) { throw "no checksum for $asset in SHA256SUMS" }
        $got = (Get-FileHash -Algorithm SHA256 "$tmp\$asset").Hash.ToLower()
        if ($got -ne $want) { throw "checksum mismatch for $asset (got $got, want $want)" }

        New-Item -ItemType Directory -Force $dir | Out-Null
        $exe = Join-Path $dir "$name.exe"
        if (Test-Path $exe) {
            # A running .exe cannot be overwritten, but it can be renamed aside.
            $old = "$exe.old"
            Remove-Item $old -Force -ErrorAction SilentlyContinue
            if (Test-Path $old) { $old = "$exe.$([DateTimeOffset]::Now.ToUnixTimeSeconds()).old" }
            Move-Item $exe $old -Force
        }
        Move-Item "$tmp\$asset" $exe -Force
        Say "installed $(& $exe version) to $exe"
    } finally {
        Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not (($userPath -split ';') -contains $dir)) {
        $new = if ($userPath) { $userPath.TrimEnd(';') + ';' + $dir } else { $dir }
        [Environment]::SetEnvironmentVariable('Path', $new, 'User')
        $env:Path = if ($env:Path) { $env:Path.TrimEnd(';') + ';' + $dir } else { $dir }
        Say "added $dir to your user PATH (new terminals pick it up; this one already has it)"
    }

    if (-not (Get-Command ssh -ErrorAction SilentlyContinue)) {
        Say "warning: no ssh client found. Install it: Settings > System > Optional features > OpenSSH Client"
    }
    $gitBash = $false
    $git = Get-Command git -ErrorAction SilentlyContinue
    if ($git) { $gitBash = Test-Path (Join-Path (Split-Path (Split-Path $git.Source)) 'bin\bash.exe') }
    foreach ($p in @($env:ProgramFiles, ${env:ProgramFiles(x86)}, (Join-Path $env:LOCALAPPDATA 'Programs'))) {
        if ($p -and (Test-Path (Join-Path $p 'Git\bin\bash.exe'))) { $gitBash = $true }
    }
    if (-not $gitBash) {
        Say "warning: Git Bash not found. Commands from Claude run in Git Bash on this machine: install Git for Windows (https://git-scm.com/download/win)"
    }
    Say "next: $name server setup user@your-server   (see https://github.com/$repo#readme)"
}
