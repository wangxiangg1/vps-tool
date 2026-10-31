$root = Split-Path -Parent $PSScriptRoot
$source = Join-Path $root 'web'
$target = Join-Path $PSScriptRoot 'web'
New-Item -ItemType Directory -Path $target -Force | Out-Null
Copy-Item -Path (Join-Path $source '*') -Destination $target -Recurse -Force
