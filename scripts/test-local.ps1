$ErrorActionPreference='Stop'
$xupayRoot=Split-Path -Parent $PSScriptRoot
& (Join-Path $PSScriptRoot 'setup-local.ps1')
$env:TEST_DATABASE_URL=$env:DATABASE_URL
Push-Location $xupayRoot
try{go test ./... -count=1 -v;if($LASTEXITCODE -ne 0){throw 'Go tests failed'};go vet ./...;if($LASTEXITCODE -ne 0){throw 'Go vet failed'};npm --prefix frontend run build;if($LASTEXITCODE -ne 0){throw 'Frontend checks failed'}}finally{Pop-Location;Remove-Item Env:TEST_DATABASE_URL -ErrorAction SilentlyContinue}
