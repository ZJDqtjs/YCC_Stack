$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$out = Join-Path $root 'out'
New-Item -ItemType Directory -Force $out | Out-Null
$log = Join-Path $out 'gradle.log'
$errLog = Join-Path $out 'gradle.err.log'

$jdk = Get-ChildItem 'C:\Program Files\Microsoft\jdk-17*' -Directory -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $jdk) { $jdk = Get-ChildItem 'C:\Program Files\Eclipse Adoptium\jdk-17*' -Directory -ErrorAction SilentlyContinue | Select-Object -First 1 }
if (-not $jdk) { $jdk = Get-ChildItem 'C:\Program Files\Java\jdk-17*' -Directory -ErrorAction SilentlyContinue | Select-Object -First 1 }
if (-not $jdk) { Write-Host '[ERROR] JDK 17 not found'; exit 2 }
$env:JAVA_HOME = $jdk.FullName
Write-Host "[build] JAVA_HOME=$env:JAVA_HOME"

$sdk = Join-Path $env:LOCALAPPDATA 'Android\Sdk'
$env:ANDROID_HOME = $sdk
$env:ANDROID_SDK_ROOT = $sdk
New-Item -ItemType Directory -Force $sdk | Out-Null

$sdkmanager = Join-Path $sdk 'cmdline-tools\latest\bin\sdkmanager.bat'
if (-not (Test-Path $sdkmanager)) {
  Write-Host '[provision] installing cmdline-tools'
  $zip = Join-Path $out 'cmdline-tools.zip'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://dl.google.com/android/repository/commandlinetools-win-11076708_latest.zip' -OutFile $zip
  $ct = Join-Path $out 'ct'
  if (Test-Path $ct) { Remove-Item $ct -Recurse -Force }
  Expand-Archive -Path $zip -DestinationPath $ct -Force
  $latest = Join-Path $sdk 'cmdline-tools\latest'
  if (Test-Path $latest) { Remove-Item $latest -Recurse -Force }
  New-Item -ItemType Directory -Force (Split-Path $latest) | Out-Null
  Move-Item (Join-Path $ct 'cmdline-tools') $latest
}

if (-not (Test-Path (Join-Path $sdk 'platforms\android-34'))) {
  Write-Host '[provision] accepting licenses'
  $yes = 1..60 | ForEach-Object { 'y' }
  $yes | & $sdkmanager --sdk_root="$sdk" --licenses | Out-Host
  Write-Host '[provision] installing packages'
  $yes | & $sdkmanager --sdk_root="$sdk" 'platform-tools' 'platforms;android-34' 'build-tools;34.0.0' | Out-Host
}
if (-not (Test-Path (Join-Path $sdk 'platforms\android-34'))) { Write-Host '[ERROR] SDK platform 34 missing'; exit 4 }

Write-Host '[build] gradle assembleDebug'
$android = Join-Path $root 'android'
$p = Start-Process -FilePath 'cmd.exe' -ArgumentList '/c','gradlew.bat assembleDebug --no-daemon' -WorkingDirectory $android -NoNewWindow -Wait -PassThru -RedirectStandardOutput $log -RedirectStandardError $errLog
if ($p.ExitCode -ne 0) {
  Write-Host "[build] gradle exited $($p.ExitCode); last lines:"
  if (Test-Path $log) { Get-Content $log -Tail 25 | Out-Host }
  if (Test-Path $errLog) { Get-Content $errLog -Tail 25 | Out-Host }
  exit 5
}

$apk = Join-Path $android 'app\build\outputs\apk\debug\app-debug.apk'
if (-not (Test-Path $apk)) {
  Write-Host '[ERROR] APK not produced'
  if (Test-Path $log) { Get-Content $log -Tail 20 | Out-Host }
  exit 6
}
Copy-Item $apk (Join-Path $out 'StarStack.apk') -Force
Write-Host '[build] done'
exit 0
