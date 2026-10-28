param([switch]$WithDatabase)
$ErrorActionPreference='Stop'
$xupayRoot=Split-Path -Parent $PSScriptRoot
$pidFile=Join-Path $xupayRoot '.local/server.pid'
if(Test-Path $pidFile){$serverID=[int](Get-Content $pidFile);$p=Get-Process -Id $serverID -ErrorAction SilentlyContinue;if($p){$expected=[IO.Path]::GetFullPath((Join-Path $xupayRoot '.local/xupay-server.exe'));if($p.Path -ne $expected){throw 'PID no longer belongs to XUPAY; refused to stop'};Stop-Process -Id $serverID; $p.WaitForExit(10000)|Out-Null};Remove-Item -LiteralPath $pidFile}
if($WithDatabase){& 'C:\Program Files\PostgreSQL\18\bin\pg_ctl.exe' -D (Join-Path $xupayRoot '.local/postgres') -m fast -w stop}
Write-Output 'XUPAY application stopped'
