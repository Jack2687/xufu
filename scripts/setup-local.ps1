param([string]$PostgresBin='C:\Program Files\PostgreSQL\18\bin')
$ErrorActionPreference='Stop'
$xupayRoot=Split-Path -Parent $PSScriptRoot
$xupayLocal=Join-Path $xupayRoot '.local'
$xupayData=Join-Path $xupayLocal 'postgres'
New-Item -ItemType Directory -Force -Path $xupayLocal | Out-Null
function New-XupaySecret([int]$Length=32){$b=New-Object byte[] $Length;[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b);[Convert]::ToBase64String($b)}
if(!(Test-Path (Join-Path $xupayRoot '.env'))){
 $dbPassword=(New-XupaySecret).Replace('+','a').Replace('/','b').Replace('=','')
 $adminPassword=(New-XupaySecret 18).Replace('+','a').Replace('/','b')
 $masterKey=New-XupaySecret
 @"
DATABASE_URL=postgres://xupay:${dbPassword}@127.0.0.1:18862/xupay?sslmode=disable
MASTER_KEY=$masterKey
PORT=18860
LISTEN_HOST=127.0.0.1
COOKIE_SECURE=false
BOOTSTRAP_EMAIL=admin@xupay.local
BOOTSTRAP_PASSWORD=$adminPassword
POSTGRES_PASSWORD=$dbPassword
"@ | Set-Content -Encoding utf8 (Join-Path $xupayRoot '.env')
 @"
# 序付本地管理员

地址：http://127.0.0.1:18860
后台：http://127.0.0.1:18860/#/admin
邮箱：admin@xupay.local
密码：$adminPassword

独立生成的本地凭据，不是参考站账号。此文件与 .env 已被 Git 忽略。
"@ | Set-Content -Encoding utf8 (Join-Path $xupayLocal 'access.md')
}
Get-Content (Join-Path $xupayRoot '.env') | ForEach-Object {if($_ -match '^([A-Z_]+)=(.*)$'){[Environment]::SetEnvironmentVariable($matches[1],$matches[2],'Process')}}
if(!(Test-Path (Join-Path $PostgresBin 'initdb.exe'))){throw 'PostgreSQL binaries are missing'}
if(!(Test-Path (Join-Path $xupayData 'PG_VERSION'))){
 if(Get-NetTCPConnection -State Listen -LocalPort 18862 -ErrorAction SilentlyContinue){throw 'Port 18862 is occupied by another instance'}
 $pwFile=Join-Path $xupayLocal 'init-password.tmp'
 try{[IO.File]::WriteAllText($pwFile,$env:POSTGRES_PASSWORD,[Text.UTF8Encoding]::new($false)); & (Join-Path $PostgresBin 'initdb.exe') -D $xupayData -U xupay --encoding=UTF8 --locale=C --auth=scram-sha-256 --pwfile=$pwFile; if($LASTEXITCODE -ne 0){throw 'Database initialization failed'}}finally{if(Test-Path $pwFile){Remove-Item -LiteralPath $pwFile}}
}
& (Join-Path $PostgresBin 'pg_ctl.exe') -D $xupayData status *> $null
if($LASTEXITCODE -ne 0){& (Join-Path $PostgresBin 'pg_ctl.exe') -D $xupayData -l (Join-Path $xupayLocal 'postgres.log') -o '-p 18862 -h 127.0.0.1' -w start;if($LASTEXITCODE -ne 0){throw 'Database startup failed'}}
$oldPassword=$env:PGPASSWORD
try{$env:PGPASSWORD=$env:POSTGRES_PASSWORD;$exists=& (Join-Path $PostgresBin 'psql.exe') -w -h 127.0.0.1 -p 18862 -U xupay -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='xupay'";if($LASTEXITCODE -ne 0){throw 'Database connection failed'};if($exists -ne '1'){& (Join-Path $PostgresBin 'createdb.exe') -w -h 127.0.0.1 -p 18862 -U xupay xupay;if($LASTEXITCODE -ne 0){throw 'Database creation failed'}}}finally{$env:PGPASSWORD=$oldPassword}
Write-Output 'XUPAY isolated PostgreSQL ready on 127.0.0.1:18862'
