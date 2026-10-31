param(
  [string]$Source = (Join-Path $PSScriptRoot '..\dist-desktop\win-x64'),
  [switch]$NoDesktopShortcut
)

$ErrorActionPreference = 'Stop'
$exeName = 'StarStack.Desktop.exe'
if (-not (Test-Path (Join-Path $Source $exeName))) {
  throw "找不到 $Source\$exeName，请先运行 build.ps1"
}

$dest = Join-Path $env:LOCALAPPDATA 'Programs\StarStack'
Get-Process StarStack.Desktop -ErrorAction SilentlyContinue | Stop-Process -Force

New-Item -ItemType Directory -Force -Path $dest | Out-Null
Copy-Item (Join-Path $Source '*') $dest -Recurse -Force

$exe = Join-Path $dest $exeName
$shell = New-Object -ComObject WScript.Shell

$startMenu = Join-Path ([Environment]::GetFolderPath('Programs')) '星栈算力.lnk'
$link = $shell.CreateShortcut($startMenu)
$link.TargetPath = $exe
$link.WorkingDirectory = $dest
$link.Description = '星栈算力'
$link.Save()

if (-not $NoDesktopShortcut) {
  $desktop = Join-Path ([Environment]::GetFolderPath('Desktop')) '星栈算力.lnk'
  $link = $shell.CreateShortcut($desktop)
  $link.TargetPath = $exe
  $link.WorkingDirectory = $dest
  $link.Description = '星栈算力'
  $link.Save()
}

Start-Process $exe
Write-Host "已安装到 $dest"
Write-Host '卸载：删除该目录与开始菜单/桌面快捷方式即可（用户数据在 %LOCALAPPDATA%\StarStack）。'
