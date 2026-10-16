param(
    [Parameter(Mandatory=$true)][string]$FFmpeg,
    [string]$Output = "$PSScriptRoot\dist"
)
$ErrorActionPreference = 'Stop'
function Check-Exit { if ($LASTEXITCODE -ne 0) { throw "Build command failed: exit $LASTEXITCODE" } }
$Root = Split-Path $PSScriptRoot -Parent
$FFmpeg = (Resolve-Path -LiteralPath $FFmpeg).Path
New-Item -ItemType Directory -Force -Path $Output | Out-Null
$Output = (Resolve-Path -LiteralPath $Output).Path
& cmake -S $PSScriptRoot -B "$PSScriptRoot\build" -G 'Visual Studio 17 2022' -A x64
Check-Exit
& cmake --build "$PSScriptRoot\build" --config Release
Check-Exit
Push-Location $Root
$OldGOOS = $env:GOOS; $OldGOARCH = $env:GOARCH; $OldCGO = $env:CGO_ENABLED
try {
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    & go build -trimpath -o "$Output\sender.exe" ./cmd/sender
    Check-Exit
} finally {
    $env:GOOS = $OldGOOS; $env:GOARCH = $OldGOARCH; $env:CGO_ENABLED = $OldCGO
    Pop-Location
}
Copy-Item -LiteralPath "$PSScriptRoot\build\Release\capture-audio.exe" -Destination $Output
Copy-Item -LiteralPath $FFmpeg -Destination "$Output\ffmpeg.exe"
Copy-Item -LiteralPath "$PSScriptRoot\README.md" -Destination $Output
Write-Host "Built $Output. Keep all three executables together. Include FFmpeg license/source notices before redistribution."
