$ErrorActionPreference = 'Stop'

$base = if ($env:CINDERSTACK_DOWNLOAD_URL) { $env:CINDERSTACK_DOWNLOAD_URL } else { 'https://github.com/rodo-software/cinderstack-runner/releases/latest/download' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE; expected AMD64 or ARM64" }
}

$file = "cinderstack_windows_$arch.zip"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -UseBasicParsing -OutFile (Join-Path $tmp $file) "$base/$file"
    Invoke-WebRequest -UseBasicParsing -OutFile (Join-Path $tmp 'checksums.txt') "$base/checksums.txt"

    $expected = $null
    Get-Content (Join-Path $tmp 'checksums.txt') | ForEach-Object {
        $parts = $_ -split '\s+', 3
        if ($parts.Count -ge 2 -and $parts[1] -eq $file) {
            $expected = $parts[0]
        }
    }

    if (-not $expected) {
        throw "checksum: no checksum found for $file in checksums.txt; refusing to install"
    }

    $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $file)).Hash
    if ($actual -ne $expected) {
        # throw, not exit: under irm | iex, exit would close the user's PowerShell
        throw "checksum mismatch for ${file}: refusing to install"
    }

    Expand-Archive -Path (Join-Path $tmp $file) -DestinationPath $tmp

    $installDir = Join-Path $env:LOCALAPPDATA 'Programs\cinderstack'
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Copy-Item (Join-Path $tmp 'cinderstack.exe') $installDir -Force

    $installed = Join-Path $installDir 'cinderstack.exe'
    Write-Output "Installed cinderstack to $installed"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($userPath -split ';' -notcontains $installDir) {
        Write-Output "Warning: $installDir is not in your PATH."
        Write-Output 'Run this command to add it:'
        Write-Output ""
        Write-Output "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';$installDir', 'User')"
    }

    Write-Output ""
    Write-Output "To check the build provenance: gh attestation verify $file --repo rodo-software/cinderstack-runner"
    Write-Output "(Download the archive first with: Invoke-WebRequest -UseBasicParsing -OutFile `"$file`" `"$base/$file`")"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
