Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Get-GatewayRoot {
  return Split-Path -Parent $PSScriptRoot
}

function Get-GatewayBaseUrl {
  param(
    [Parameter(Mandatory = $true)]
    [string]$ListenHost,
    [Parameter(Mandatory = $true)]
    [int]$ListenPort
  )

  return "http://{0}:{1}" -f $ListenHost, $ListenPort
}

function Get-GatewayStatePaths {
  param(
    [string]$StateRoot = (Join-Path $HOME ".codex-retry-gateway")
  )

  return [pscustomobject]@{
    StateRoot  = $StateRoot
    ConfigDir  = Join-Path $StateRoot "config"
    LogDir     = Join-Path $StateRoot "logs"
    BackupDir  = Join-Path $StateRoot "backups"
    ConfigPath = Join-Path $StateRoot "config\config.json"
    LogPath    = Join-Path $StateRoot "logs\gateway.log"
    StatePath  = Join-Path $StateRoot "state.json"
    PidPath    = Join-Path $StateRoot "gateway.pid"
  }
}

function Get-GatewayBaseUrlFromConfig {
  param(
    $GatewayConfig
  )

  if ($null -eq $GatewayConfig) {
    return $null
  }

  if ([string]::IsNullOrWhiteSpace([string]$GatewayConfig.listen_host) -or $null -eq $GatewayConfig.listen_port) {
    return $null
  }

  return Get-GatewayBaseUrl `
    -ListenHost ([string]$GatewayConfig.listen_host) `
    -ListenPort ([int]$GatewayConfig.listen_port)
}

function Ensure-Directory {
  param(
    [Parameter(Mandatory = $true)]
    [string]$Path
  )

  New-Item -ItemType Directory -Path $Path -Force | Out-Null
}

function Write-Utf8NoBomFile {
  param(
    [Parameter(Mandatory = $true)]
    [string]$Path,
    [Parameter(Mandatory = $true)]
    [string]$Content
  )

  $parent = Split-Path -Parent $Path
  if ($parent) {
    Ensure-Directory -Path $parent
  }

  $encoding = [System.Text.UTF8Encoding]::new($false)
  [System.IO.File]::WriteAllText($Path, $Content, $encoding)
}

function Read-JsonFile {
  param(
    [Parameter(Mandatory = $true)]
    [string]$Path
  )

  if (-not (Test-Path -LiteralPath $Path)) {
    return $null
  }

  $raw = [System.IO.File]::ReadAllText($Path, [System.Text.Encoding]::UTF8)
  if ([string]::IsNullOrWhiteSpace($raw)) {
    return $null
  }

  return $raw | ConvertFrom-Json
}

function Write-JsonFile {
  param(
    [Parameter(Mandatory = $true)]
    [string]$Path,
    [Parameter(Mandatory = $true)]
    $Value
  )

  $json = $Value | ConvertTo-Json -Depth 20
  Write-Utf8NoBomFile -Path $Path -Content ($json + "`n")
}

function Get-CodexProviderContext {
  param(
    [Parameter(Mandatory = $true)]
    [string]$CodexConfigPath
  )

  $content = Get-Content -LiteralPath $CodexConfigPath -Raw
  $providerMatch = [regex]::Match($content, '(?m)^\s*model_provider\s*=\s*"([^"]+)"\s*$')
  if (-not $providerMatch.Success) {
    throw "model_provider was not found in $CodexConfigPath"
  }

  $providerName = $providerMatch.Groups[1].Value
  $sectionPattern = "(?ms)^\[model_providers\." + [regex]::Escape($providerName) + "\]\s*$.*?(?=^\[|\z)"
  $sectionMatch = [regex]::Match($content, $sectionPattern)
  if (-not $sectionMatch.Success) {
    throw "[model_providers.$providerName] was not found in $CodexConfigPath"
  }

  $sectionText = $sectionMatch.Value
  $baseUrlMatch = [regex]::Match($sectionText, '(?m)^\s*base_url\s*=\s*"([^"]+)"\s*$')
  if (-not $baseUrlMatch.Success) {
    throw "base_url was not found in [model_providers.$providerName]"
  }

  return [pscustomobject]@{
    Content         = $content
    ProviderName    = $providerName
    SectionText     = $sectionText
    SectionIndex    = $sectionMatch.Index
    SectionLength   = $sectionMatch.Length
    CurrentBaseUrl  = $baseUrlMatch.Groups[1].Value
    BaseUrlLineText = $baseUrlMatch.Value
  }
}

function Set-CodexProviderBaseUrl {
  param(
    [Parameter(Mandatory = $true)]
    [string]$CodexConfigPath,
    [Parameter(Mandatory = $true)]
    [string]$ProviderName,
    [Parameter(Mandatory = $true)]
    [string]$NewBaseUrl
  )

  $context = Get-CodexProviderContext -CodexConfigPath $CodexConfigPath
  if ($context.ProviderName -ne $ProviderName) {
    throw "model_provider changed unexpectedly: expected $ProviderName, actual $($context.ProviderName)"
  }

  $updatedSection = [regex]::Replace(
    $context.SectionText,
    '(?m)^(\s*base_url\s*=\s*")([^"]*)("\s*)$',
    {
      param($match)
      return $match.Groups[1].Value + $NewBaseUrl + $match.Groups[3].Value
    },
    1
  )

  $updatedContent =
    $context.Content.Substring(0, $context.SectionIndex) +
    $updatedSection +
    $context.Content.Substring($context.SectionIndex + $context.SectionLength)

  Write-Utf8NoBomFile -Path $CodexConfigPath -Content $updatedContent
}

function Test-ProcessAlive {
  param(
    [Parameter(Mandatory = $true)]
    [int]$ProcessId
  )

  try {
    $null = Get-Process -Id $ProcessId -ErrorAction Stop
    return $true
  } catch {
    return $false
  }
}

function Wait-GatewayHealth {
  param(
    [Parameter(Mandatory = $true)]
    [string]$ListenHost,
    [Parameter(Mandatory = $true)]
    [int]$ListenPort,
    [Parameter(Mandatory = $true)]
    [string]$HealthPath,
    [int]$TimeoutSeconds = 10
  )

  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  $healthUrl = "http://{0}:{1}{2}" -f $ListenHost, $ListenPort, $HealthPath

  while ((Get-Date) -lt $deadline) {
    try {
      $response = Invoke-WebRequest -Uri $healthUrl -UseBasicParsing -TimeoutSec 2
      if ($response.StatusCode -eq 200) {
        return $response
      }
    } catch {
      Start-Sleep -Milliseconds 200
    }
  }

  throw "Gateway health check timed out: $healthUrl"
}

function Normalize-IntArray {
  param(
    $Values,
    [int[]]$Default = @(516, 1034, 1552)
  )

  if ($null -eq $Values) {
    return ,@($Default)
  }

  $queue = New-Object System.Collections.Generic.List[object]
  foreach ($item in @($Values)) {
    $queue.Add($item)
  }

  $normalized = @()
  foreach ($value in $queue) {
    if ($null -eq $value) {
      continue
    }
    if ($value -is [System.Collections.IEnumerable] -and -not ($value -is [string])) {
      foreach ($nestedValue in @($value)) {
        if ($null -eq $nestedValue) {
          continue
        }
        $normalized += [int]$nestedValue
      }
      continue
    }
    $normalized += [int]$value
  }

  if ($normalized.Count -eq 0) {
    return ,@($Default)
  }

  return ,@($normalized)
}

function Normalize-StringArray {
  param(
    $Values,
    [string[]]$Default
  )

  if ($null -eq $Values) {
    return ,@($Default)
  }

  $normalized = @()
  foreach ($value in @($Values)) {
    if ($value -is [System.Collections.IEnumerable] -and -not ($value -is [string])) {
      foreach ($nestedValue in @($value)) {
        if ([string]::IsNullOrWhiteSpace([string]$nestedValue)) {
          continue
        }
        foreach ($part in ([string]$nestedValue).Split(@(" ", "`t", "`r", "`n"), [System.StringSplitOptions]::RemoveEmptyEntries)) {
          $normalized += $part
        }
      }
      continue
    }
    if ([string]::IsNullOrWhiteSpace([string]$value)) {
      continue
    }
    foreach ($part in ([string]$value).Split(@(" ", "`t", "`r", "`n"), [System.StringSplitOptions]::RemoveEmptyEntries)) {
      $normalized += $part
    }
  }

  if ($normalized.Count -eq 0) {
    return ,@($Default)
  }

  return ,@($normalized)
}
