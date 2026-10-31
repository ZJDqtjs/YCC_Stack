@echo off
rem Build the desktop client on a provider machine:
rem   dotnet publish (self-contained single file) -> out\StarStack.Desktop.exe
rem The .NET SDK is installed into %LOCALAPPDATA%\starstack-sdk when missing,
rem so a fresh node only needs network access, not a preinstalled toolchain.
setlocal
set "PROCESSOR_ARCHITECTURE=AMD64"
cd /d "%~dp0"
echo [build] cwd=%CD%
powershell -NoProfile -ExecutionPolicy Bypass -Command "& { $ErrorActionPreference='Stop'; try { [Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12 } catch {}; $local=$env:LOCALAPPDATA; if (-not $local) { $local=[Environment]::GetFolderPath('LocalApplicationData') }; $tmp=$env:TEMP; if (-not $tmp) { $tmp=[IO.Path]::GetTempPath() }; $sdk=Join-Path $local 'starstack-sdk'; $dn=Join-Path $sdk 'dotnet.exe'; $have=$false; if (Test-Path $dn) { $have=[bool](& $dn --list-sdks 2>$null) }; if (-not $have) { $sys=Get-Command dotnet -ErrorAction SilentlyContinue; if ($sys) { $have=[bool](& $sys.Source --list-sdks 2>$null) } }; if (-not $have) { Write-Host '[auto-provision] installing .NET 8 SDK'; $p=Join-Path $tmp 'dotnet-install.ps1'; $ProgressPreference='SilentlyContinue'; try { Invoke-WebRequest -UseBasicParsing -Uri 'https://dot.net/v1/dotnet-install.ps1' -OutFile $p } catch { Write-Host ('iwr failed: ' + $_.Exception.Message); & curl.exe -fsSL 'https://dot.net/v1/dotnet-install.ps1' -o $p; if ($LASTEXITCODE -ne 0) { throw 'download dotnet-install.ps1 failed' } }; & $p -Channel 8.0 -Architecture x64 -InstallDir $sdk; if (-not (Test-Path $dn)) { throw '.NET SDK install failed' } }; if (Test-Path $dn) { $env:PATH=$sdk + ';' + $env:PATH }; dotnet --version; if ($LASTEXITCODE -ne 0) { exit 3 }; dotnet publish StarStack.Desktop.csproj -c Release -r win-x64 --self-contained true -p:PublishSingleFile=true -p:IncludeNativeLibrariesForSelfExtract=true -p:EnableCompressionInSingleFile=true -p:DebugType=None -p:DebugSymbols=false -o out; exit $LASTEXITCODE }"
if errorlevel 1 exit /b 3
dir out
