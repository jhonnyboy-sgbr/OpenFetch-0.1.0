# Requisitos do desenvolvedor: Windows 10/11, Go 1.23+, Wails 2, Inno Setup 6 para o instalador.
# Uso: powershell -ExecutionPolicy Bypass -File .\scripts\build-windows.ps1
param([switch]$SkipToolDownload, [switch]$SkipInstaller)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

function Step($Message) { Write-Host "`n[OpenFetch] $Message" -ForegroundColor Magenta }
function Need($Exe, $Help) {
  if (-not (Get-Command $Exe -ErrorAction SilentlyContinue)) { throw "$Exe nao encontrado. $Help" }
}
Need 'go' 'Instale o Go em https://go.dev/dl/'
Step 'Instalando/atualizando o Wails v2.17.0 para este usuario...'
go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0
if ($LASTEXITCODE -ne 0) { throw 'Falha ao instalar Wails' }
$GoPath = (& go env GOPATH | Select-Object -First 1).Trim()
$Wails = Join-Path $GoPath 'bin\wails.exe'
if (-not (Test-Path $Wails)) { throw "Wails CLI ausente: $Wails" }

Step 'Verificando as dependencias Go...'
go mod tidy
if ($LASTEXITCODE -ne 0) { throw 'Falha no go mod tidy' }
Step 'Compilando o aplicativo nativo...'
& $Wails build -platform windows/amd64 -webview2 embed
if ($LASTEXITCODE -ne 0) { throw 'Falha ao compilar OpenFetch' }
$Output = Join-Path $Root 'build\bin'
$Tools = Join-Path $Output 'tools'
New-Item -Force -ItemType Directory $Tools | Out-Null

if (-not $SkipToolDownload) {
  Step 'Baixando componentes oficiais para o instalador...'
  $Yt = Join-Path $Tools 'yt-dlp.exe'
  Invoke-WebRequest 'https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe' -OutFile $Yt
  $Hashes = Join-Path $env:TEMP 'openfetch_yt_hashes.txt'
  Invoke-WebRequest 'https://github.com/yt-dlp/yt-dlp/releases/latest/download/SHA2-256SUMS' -OutFile $Hashes
  $Line = Get-Content $Hashes | Where-Object { $_ -match '\syt-dlp\.exe$' } | Select-Object -First 1
  if (-not $Line) { throw 'Checksum do yt-dlp nao encontrado' }
  $Expected = ($Line -split '\s+')[0].ToUpper()
  $Actual = (Get-FileHash $Yt -Algorithm SHA256).Hash.ToUpper()
  if ($Expected -ne $Actual) { throw 'Checksum do yt-dlp difere do divulgado pelo projeto!' }

  $FfmpegZip = Join-Path $env:TEMP 'openfetch_ffmpeg_essentials.zip'
  Invoke-WebRequest 'https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip' -OutFile $FfmpegZip
  $Extract = Join-Path $env:TEMP ('openfetch_ffmpeg_' + [guid]::NewGuid().ToString('N'))
  New-Item -Force -ItemType Directory $Extract | Out-Null
  Expand-Archive -Path $FfmpegZip -DestinationPath $Extract -Force
  $Exe = Get-ChildItem -Path $Extract -Recurse -Filter 'ffmpeg.exe' -File | Select-Object -First 1
  if (-not $Exe) { throw 'ffmpeg.exe nao encontrado dentro do arquivo' }
  Copy-Item $Exe.FullName (Join-Path $Tools 'ffmpeg.exe') -Force
  $Probe = Get-ChildItem -Path $Extract -Recurse -Filter 'ffprobe.exe' -File | Select-Object -First 1
  if ($Probe) { Copy-Item $Probe.FullName (Join-Path $Tools 'ffprobe.exe') -Force }
  Remove-Item -Recurse -Force $Extract

  # yt-dlp 2026 usa um runtime JS para os desafios do YouTube.
  $DenoZip = Join-Path $env:TEMP 'openfetch_deno.zip'
  $DenoName = 'deno-x86_64-pc-windows-msvc.zip'
  $DenoURL = 'https://github.com/denoland/deno/releases/latest/download/' + $DenoName
  Invoke-WebRequest $DenoURL -OutFile $DenoZip
  $DenoSHAURL = $DenoURL + '.sha256sum'
  $DenoSHAFile = Join-Path $env:TEMP 'openfetch_deno_sha256sum.txt'
  Invoke-WebRequest $DenoSHAURL -OutFile $DenoSHAFile
  $DenoText = (Get-Content $DenoSHAFile | Out-String)
  $Match = [regex]::Match($DenoText, '(?i)\b[a-f0-9]{64}\b')
  if (-not $Match.Success) { throw 'SHA-256 oficial do Deno nao encontrado' }
  if ((Get-FileHash $DenoZip -Algorithm SHA256).Hash.ToUpper() -ne $Match.Value.ToUpper()) { throw 'SHA-256 do Deno incorreto' }
  $DenoExtract = Join-Path $env:TEMP ('openfetch_deno_' + [guid]::NewGuid().ToString('N'))
  Expand-Archive -Path $DenoZip -DestinationPath $DenoExtract -Force
  Copy-Item (Join-Path $DenoExtract 'deno.exe') (Join-Path $Tools 'deno.exe') -Force
  Remove-Item -Recurse -Force $DenoExtract

  Step 'Validando executaveis baixados...'
  & (Join-Path $Tools 'deno.exe') --version | Select-Object -First 1
  if ($LASTEXITCODE -ne 0) { throw 'Deno invalido' }
  & (Join-Path $Tools 'yt-dlp.exe') --version
  if ($LASTEXITCODE -ne 0) { throw 'yt-dlp invalido' }
  & (Join-Path $Tools 'ffmpeg.exe') -version | Select-Object -First 1
  if ($LASTEXITCODE -ne 0) { throw 'FFmpeg invalido' }
}

Step 'Preparando pacote portatil...'
$Portable = Join-Path $Root 'release\OpenFetch-0.1.0-portable'
New-Item -ItemType Directory -Force $Portable | Out-Null
Copy-Item (Join-Path $Output 'OpenFetch.exe') $Portable -Force
Copy-Item $Tools (Join-Path $Portable 'tools') -Recurse -Force
Copy-Item (Join-Path $Root 'docs\LICENCAS-E-CREDITOS.txt') $Portable -Force
if (-not $SkipInstaller) {
  $Iscc = Get-Command 'ISCC.exe' -ErrorAction SilentlyContinue
  if (-not $Iscc) {
    $Candidate = @("${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe")
    foreach($C in $Candidate) { if (Test-Path $C) { $Iscc = $C; break } }
  }
  if ($Iscc) {
    Step 'Gerando instalador com Inno Setup...'
    & $Iscc (Join-Path $Root 'installer\OpenFetch.iss')
    if ($LASTEXITCODE -ne 0) { throw 'Erro ao gerar instalador' }
  } else {
    Write-Warning 'Inno Setup nao encontrado: arquivo portatil gerado; instalador nao gerado.'
  }
}
Step 'Pronto. Consulte as pastas release e build/bin.'
