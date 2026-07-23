$dest = "C:\opt\csweb-gui\data\cs_server\tools\cs-sync"
$plats = @("mswin.amd64","linux.amd64","linux.arm64","illumos.amd64","solaris.amd64","freebsd.amd64","darwin.amd64","darwin.arm64")
foreach ($p in $plats) {
    $url = "https://github.com/guenther-alka/cs-sync/releases/download/v1.3.1/cs-sync-$p.tar.gz"
    $tmp = "$env:TEMP\cs-sync-$p.tar.gz"
    Invoke-WebRequest -Uri $url -OutFile $tmp
    $outdir = Join-Path $dest $p
    Remove-Item "$outdir\*" -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $outdir | Out-Null
    tar -xzf $tmp -C $outdir --strip-components=1
    Remove-Item $tmp
    Write-Output "extracted $p"
}
