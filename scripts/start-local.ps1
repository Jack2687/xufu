param([switch]$SkipBuild)
$ErrorActionPreference='Stop'
$xupayRoot=Split-Path -Parent $PSScriptRoot
if(Test-Path (Join-Path $xupayRoot '.local/migrated-to-production.json')){
 throw 'This database was migrated to production. Local workers remain stopped to prevent duplicate processing. Use a separate development database before starting locally.'
}
& (Join-Path $PSScriptRoot 'setup-local.ps1')
# Local Windows development only. Raw server/Docker startup never reads this
# registry setting, so production connects to configured proxies directly.
if(!$env:XUPAY_LOCAL_PROXY){
 $xupayWindowsProxy=Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' -ErrorAction SilentlyContinue
 if($xupayWindowsProxy.ProxyEnable -eq 1 -and $xupayWindowsProxy.ProxyServer){
  $xupayProxyAddress=[string]$xupayWindowsProxy.ProxyServer
  if($xupayProxyAddress.Contains('=')){
   $xupayProxyMap=@{}
   $xupayProxyAddress.Split(';') | ForEach-Object { $xupayProxyPart=$_.Split('=',2);if($xupayProxyPart.Length -eq 2){$xupayProxyMap[$xupayProxyPart[0]]=$xupayProxyPart[1]} }
   $xupayProxyAddress=if($xupayProxyMap['https']){$xupayProxyMap['https']}else{$xupayProxyMap['http']}
  }
  if($xupayProxyAddress -and !$xupayProxyAddress.Contains('://')){$xupayProxyAddress='http://'+$xupayProxyAddress}
  if($xupayProxyAddress){$env:XUPAY_LOCAL_PROXY=$xupayProxyAddress}
 }
}
if($env:XUPAY_LOCAL_PROXY -and $env:XUPAY_LOCAL_PROXY -ne 'off'){Write-Output 'XUPAY local proxy chain enabled for configured proxy connections'}
Push-Location $xupayRoot
try{
 if(!(Test-Path (Join-Path $xupayRoot 'runtime/node_modules/playwright/package.json'))){npm --prefix runtime ci --no-audit --no-fund;if($LASTEXITCODE -ne 0){throw 'Payment runtime setup failed'}}
 if(!$SkipBuild){npm --prefix frontend ci --no-audit --no-fund;if($LASTEXITCODE -ne 0){throw 'npm ci failed'};npm --prefix frontend run build;if($LASTEXITCODE -ne 0){throw 'Frontend build failed'};go build -o .local/xupay-server.exe ./cmd/server;if($LASTEXITCODE -ne 0){throw 'Go build failed'}}
 if(Get-NetTCPConnection -State Listen -LocalPort ([int]$env:PORT) -ErrorAction SilentlyContinue){throw "Port $env:PORT occupied; no existing process was stopped"}
 $p=Start-Process -FilePath (Join-Path $xupayRoot '.local/xupay-server.exe') -WorkingDirectory $xupayRoot -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $xupayRoot '.local/server.out.log') -RedirectStandardError (Join-Path $xupayRoot '.local/server.err.log')
 $p.Id | Set-Content (Join-Path $xupayRoot '.local/server.pid')
 $ready=$false
 for($i=0;$i -lt 50;$i++){$p.Refresh();if($p.HasExited){throw 'Application exited; inspect .local/server.err.log'};try{$h=Invoke-RestMethod "http://127.0.0.1:$env:PORT/health/ready" -TimeoutSec 2;if($h.status -eq 'ready'){$ready=$true;break}}catch{};Start-Sleep -Milliseconds 200}
 if(!$ready){throw 'Health check timeout'}
 Write-Output "XUPAY started: http://127.0.0.1:$env:PORT ; credentials: .local/access.md"
}finally{Pop-Location}
