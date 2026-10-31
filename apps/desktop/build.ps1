param(
  [switch]$SelfContained,
  [string]$OutDir = (Join-Path $PSScriptRoot '..\dist-desktop\win-x64')
)

$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
  Write-Host '== 1/2 构建网页资源（apps/mobile/dist）=='
  npm --prefix .. run build
  if ($LASTEXITCODE -ne 0) { throw '网页资源构建失败' }

  if (Test-Path $OutDir) { Remove-Item $OutDir -Recurse -Force }

  Write-Host '== 2/2 发布桌面端 =='
  # Framework-dependent by default: the target needs the .NET 8 Desktop Runtime
  # (this machine already has it). Add -SelfContained for a machine without it.
  dotnet publish StarStack.Desktop.csproj -c Release -r win-x64 --self-contained:$SelfContained -o $OutDir
  if ($LASTEXITCODE -ne 0) { throw 'dotnet publish 失败' }

  Write-Host "完成：$OutDir"
  Write-Host '安装：powershell -ExecutionPolicy Bypass -File .\install.ps1'
}
finally {
  Pop-Location
}
