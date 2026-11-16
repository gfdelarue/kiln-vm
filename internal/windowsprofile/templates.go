package windowsprofile

const autounattendTemplate = `<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="offlineServicing">
    <component name="Microsoft-Windows-LUA-Settings" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <EnableLUA>false</EnableLUA>
    </component>
  </settings>
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-PnpCustomizationsWinPE" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <DriverPaths>
{{DRIVER_PATHS}}
      </DriverPaths>
    </component>
    <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <SetupUILanguage>
        <UILanguage>en-US</UILanguage>
      </SetupUILanguage>
      <InputLocale>en-US</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <DiskConfiguration>
        <Disk wcm:action="add">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
            <CreatePartition wcm:action="add"><Order>1</Order><Type>EFI</Type><Size>260</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>2</Order><Type>MSR</Type><Size>16</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>3</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>
          </CreatePartitions>
          <ModifyPartitions>
            <ModifyPartition wcm:action="add"><Order>1</Order><PartitionID>1</PartitionID><Format>FAT32</Format><Label>System</Label></ModifyPartition>
            <ModifyPartition wcm:action="add"><Order>2</Order><PartitionID>3</PartitionID><Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter></ModifyPartition>
          </ModifyPartitions>
        </Disk>
        <WillShowUI>OnError</WillShowUI>
      </DiskConfiguration>
      <ImageInstall>
        <OSImage>
          <InstallTo><DiskID>0</DiskID><PartitionID>3</PartitionID></InstallTo>
          <InstallToAvailablePartition>false</InstallToAvailablePartition>
          <WillShowUI>OnError</WillShowUI>
        </OSImage>
      </ImageInstall>
      <UseConfigurationSet>true</UseConfigurationSet>
      <UserData>
        <AcceptEula>true</AcceptEula>
        <FullName>kiln-vm</FullName>
        <Organization>kiln-vm</Organization>
        <ProductKey><Key>{{PRODUCT_KEY}}</Key><WillShowUI>Never</WillShowUI></ProductKey>
      </UserData>
    </component>
  </settings>
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <ComputerName>{{COMPUTER_NAME}}</ComputerName>
      <TimeZone>GMT Standard Time</TimeZone>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <InputLocale>en-US</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <AutoLogon>
        <Password><Value>{{ADMIN_PASSWORD}}</Value><PlainText>true</PlainText></Password>
        <Username>{{ADMIN_USER}}</Username>
        <Enabled>true</Enabled>
        <LogonCount>1</LogonCount>
      </AutoLogon>
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideLocalAccountScreen>true</HideLocalAccountScreen>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <NetworkLocation>Work</NetworkLocation>
        <ProtectYourPC>3</ProtectYourPC>
        <SkipMachineOOBE>true</SkipMachineOOBE>
        <SkipUserOOBE>true</SkipUserOOBE>
      </OOBE>
      <UserAccounts>
        <LocalAccounts>
          <LocalAccount wcm:action="add">
            <Password><Value>{{ADMIN_PASSWORD}}</Value><PlainText>true</PlainText></Password>
            <Description>kiln-vm Windows VM account</Description>
            <DisplayName>{{ADMIN_USER}}</DisplayName>
            <Group>Administrators</Group>
            <Name>{{ADMIN_USER}}</Name>
          </LocalAccount>
        </LocalAccounts>
      </UserAccounts>
      <FirstLogonCommands>
        <SynchronousCommand wcm:action="add">
          <Order>1</Order>
          <Description>Run kiln-vm Windows provisioning</Description>
          <CommandLine>cmd.exe /c for %D in (A B D E F G H I J K L M N O P Q R S T U V W X Y Z) do if exist %D:\SetupComplete.cmd call %D:\SetupComplete.cmd</CommandLine>
        </SynchronousCommand>
      </FirstLogonCommands>
      <LogonCommands>
        <AsynchronousCommand wcm:action="add">
          <Order>1</Order>
          <Description>Run kiln-vm Windows provisioning fallback</Description>
          <CommandLine>cmd.exe /c for %D in (A B D E F G H I J K L M N O P Q R S T U V W X Y Z) do if exist %D:\SetupComplete.cmd call %D:\SetupComplete.cmd</CommandLine>
        </AsynchronousCommand>
      </LogonCommands>
      <RegisteredOwner>kiln-vm</RegisteredOwner>
      <RegisteredOrganization>kiln-vm</RegisteredOrganization>
    </component>
  </settings>
  <cpi:offlineImage cpi:source="wim://sources/install.wim#Windows 11 Pro" xmlns:cpi="urn:schemas-microsoft-com:cpi" />
</unattend>
`

const setupCompleteTemplate = `@echo off
setlocal EnableExtensions

mkdir C:\kiln-vm 2>nul
icacls.exe C:\kiln-vm /grant "*S-1-5-32-545:(OI)(CI)(RX)" "*S-1-5-11:(OI)(CI)(RX)" >nul 2>&1

set BOOTSTRAP_LOG=C:\kiln-vm\bootstrap.log
set TASK_STARTUP=KilnVMProvisionStartup
set TASK_RETRY=KilnVMProvisionRetry
set PROVISION=C:\kiln-vm\provision.ps1
set AUTHORIZED_KEYS=C:\kiln-vm\authorized_keys
set PROVISION_LOG=C:\kiln-vm\provision.log
set TASK_ACTION=powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\kiln-vm\provision.ps1 -AuthorizedKeysPath C:\kiln-vm\authorized_keys -LogPath C:\kiln-vm\provision.log

echo [%DATE% %TIME%] Bootstrap started > "%BOOTSTRAP_LOG%"
icacls.exe "%BOOTSTRAP_LOG%" /grant "*S-1-5-32-545:R" "*S-1-5-11:R" >nul 2>&1

if exist C:\kiln-vm\provision.complete (
  echo Provisioning sentinel already exists >> "%BOOTSTRAP_LOG%"
  exit /b 0
)

if not exist "%PROVISION%" (
  set PROVISION_DRIVE=
  for %%D in (A B D E F G H I J K L M N O P Q R S T U V W X Y Z) do (
    if exist %%D:\provision.ps1 set PROVISION_DRIVE=%%D:
  )

  if "%PROVISION_DRIVE%"=="" (
    echo Could not find kiln-vm Windows provisioning media >> "%BOOTSTRAP_LOG%"
    exit /b 1
  )

  echo Copying provisioning payload from %PROVISION_DRIVE% >> "%BOOTSTRAP_LOG%"
  copy /Y "%PROVISION_DRIVE%\provision.ps1" "%PROVISION%" >> "%BOOTSTRAP_LOG%" 2>&1
  copy /Y "%PROVISION_DRIVE%\authorized_keys" "%AUTHORIZED_KEYS%" >> "%BOOTSTRAP_LOG%" 2>&1
)

if not exist "%PROVISION%" exit /b 1
if not exist "%AUTHORIZED_KEYS%" exit /b 1

echo Running provisioning synchronously >> "%BOOTSTRAP_LOG%"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%PROVISION%" -AuthorizedKeysPath "%AUTHORIZED_KEYS%" -LogPath "%PROVISION_LOG%" >> "%BOOTSTRAP_LOG%" 2>&1
if exist C:\kiln-vm\provision.complete (
  echo Provisioning completed synchronously >> "%BOOTSTRAP_LOG%"
  exit /b 0
)

echo Synchronous provisioning did not complete; creating retry tasks >> "%BOOTSTRAP_LOG%"
echo Creating provisioning scheduled tasks >> "%BOOTSTRAP_LOG%"
schtasks.exe /Create /TN "%TASK_STARTUP%" /SC ONSTART /RU SYSTEM /RL HIGHEST /TR "%TASK_ACTION%" /F >> "%BOOTSTRAP_LOG%" 2>&1
if errorlevel 1 exit /b 1
schtasks.exe /Create /TN "%TASK_RETRY%" /SC MINUTE /MO 1 /ST 00:00 /RU SYSTEM /RL HIGHEST /TR "%TASK_ACTION%" /F >> "%BOOTSTRAP_LOG%" 2>&1
if errorlevel 1 exit /b 1

echo Starting provisioning task >> "%BOOTSTRAP_LOG%"
schtasks.exe /Run /TN "%TASK_STARTUP%" >> "%BOOTSTRAP_LOG%" 2>&1
if errorlevel 1 exit /b 1

echo Bootstrap complete >> "%BOOTSTRAP_LOG%"
exit /b 0
`

const provisionTemplate = `param(
  [Parameter(Mandatory=$true)][string]$AuthorizedKeysPath,
  [Parameter(Mandatory=$true)][string]$LogPath
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$taskNames = @("KilnVMProvisionStartup", "KilnVMProvisionRetry")
$mutex = New-Object System.Threading.Mutex -ArgumentList $false, "Global\KilnVMProvision"
if (-not $mutex.WaitOne(0)) { exit 0 }
$transcriptStarted = $false

function Remove-ProvisionTasks {
  foreach ($taskName in $taskNames) {
    $previousErrorActionPreference = $ErrorActionPreference
    try {
      $ErrorActionPreference = "Continue"
      schtasks.exe /Delete /TN $taskName /F *> $null
    } catch {
    } finally {
      $ErrorActionPreference = $previousErrorActionPreference
    }
  }
}

function Grant-Readable {
  param([Parameter(Mandatory=$true)][string]$Path)
  if (Test-Path $Path) {
    icacls.exe $Path /grant "*S-1-5-32-545:R" "*S-1-5-11:R" | Out-Null
  }
}

function Append-Log {
  param([Parameter(Mandatory=$true)][string]$Message)
  try {
    Add-Content -Path $LogPath -Value $Message
    Grant-Readable -Path $LogPath
  } catch {}
}

trap {
  Write-Host "Provisioning failed"
  Write-Host ($_ | Out-String)
  if ($transcriptStarted) {
    try { Stop-Transcript | Out-Null } catch {}
  } else {
    Append-Log "Provisioning failed before clean completion"
    Append-Log ($_ | Out-String)
  }
  if ($mutex) {
    try { $mutex.ReleaseMutex() | Out-Null } catch {}
    $mutex.Dispose()
  }
  exit 1
}

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $LogPath) | Out-Null
icacls.exe (Split-Path -Parent $LogPath) /grant "*S-1-5-32-545:(OI)(CI)(RX)" "*S-1-5-11:(OI)(CI)(RX)" | Out-Null
Append-Log "Provisioning invoked $(Get-Date -Format o) as $([Security.Principal.WindowsIdentity]::GetCurrent().Name)"
Start-Transcript -Path $LogPath -Append -Force
$transcriptStarted = $true
Grant-Readable -Path $LogPath

$sentinel = "C:\kiln-vm\provision.complete"
if (Test-Path $sentinel) {
  Write-Host "Provisioning already complete"
  Remove-ProvisionTasks
  Stop-Transcript
  $transcriptStarted = $false
  $mutex.ReleaseMutex() | Out-Null
  $mutex.Dispose()
  exit 0
}

function Add-ToMachinePath {
  param([Parameter(Mandatory=$true)][string]$PathEntry)
  $current = [Environment]::GetEnvironmentVariable("Path", "Machine")
  $entries = $current -split ";" | Where-Object { $_ }
  if ($entries -notcontains $PathEntry) {
    [Environment]::SetEnvironmentVariable("Path", ($entries + $PathEntry) -join ";", "Machine")
  }
  $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
}

function Find-PayloadRoot {
  $driveLetters = @("E", "F", "G", "H", "I", "J", "K", "L", "M", "N", "O", "P", "Q", "R", "S", "T", "U", "V", "W", "X", "Y", "Z", "D", "A", "B")
  foreach ($letter in $driveLetters) {
    $root = "${letter}:\"
    if (-not (Test-Path $root)) { continue }
    $payloadRoot = Join-Path $root "payload"
    if (Test-Path (Join-Path $payloadRoot "manifest.json")) { return $payloadRoot }
  }
  return $null
}

function Assert-FileSha256 {
  param(
    [Parameter(Mandatory=$true)][string]$Path,
    [Parameter(Mandatory=$true)][string]$Expected
  )
  if (-not (Test-Path $Path)) { throw "Missing payload artifact: $Path" }
  $actual = (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLowerInvariant()
  if ($actual -ne $Expected.ToLowerInvariant()) {
    throw "SHA-256 mismatch for ${Path}: expected $Expected, got $actual"
  }
}

function Install-OpenSshFromPayload {
  param([Parameter(Mandatory=$true)][string]$PayloadRoot)

  $openSshZip = Join-Path $PayloadRoot "{{OPENSSH_ZIP}}"
  $openSshRoot = "C:\tools\OpenSSH-Win64"
  Assert-FileSha256 -Path $openSshZip -Expected "{{OPENSSH_SHA256}}"

  if (-not (Test-Path (Join-Path $openSshRoot "sshd.exe"))) {
    Write-Host "Installing Win32-OpenSSH {{OPENSSH_TAG}} from $openSshZip"
    Remove-Item -Path $openSshRoot -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path "C:\tools" | Out-Null
    Expand-Archive -Path $openSshZip -DestinationPath "C:\tools" -Force
  } else {
    Write-Host "Win32-OpenSSH already expanded at $openSshRoot"
  }

  Add-ToMachinePath -PathEntry $openSshRoot

  if (-not (Get-Service -Name sshd -ErrorAction SilentlyContinue)) {
    & (Join-Path $openSshRoot "install-sshd.ps1")
  }

  Set-Service -Name sshd -StartupType Automatic
  New-Item -Path "HKLM:\SOFTWARE\OpenSSH" -Force | Out-Null
  New-ItemProperty -Path "HKLM:\SOFTWARE\OpenSSH" -Name DefaultShell -Value "C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" -PropertyType String -Force | Out-Null

  if (-not (Get-NetFirewallRule -Name "kiln-vm-OpenSSH-Server" -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name "kiln-vm-OpenSSH-Server" -DisplayName "kiln-vm OpenSSH Server" -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
  }

  $programDataSsh = "C:\ProgramData\ssh"
  New-Item -ItemType Directory -Force -Path $programDataSsh | Out-Null
  $adminKeys = Join-Path $programDataSsh "administrators_authorized_keys"
  Copy-Item -Path $AuthorizedKeysPath -Destination $adminKeys -Force
  icacls.exe $adminKeys /inheritance:r /grant "Administrators:F" /grant "SYSTEM:F" | Out-Null

  Restart-Service -Name sshd -ErrorAction SilentlyContinue
  Start-Service -Name sshd
}

function Invoke-Bounded {
  param(
    [Parameter(Mandatory=$true)][string]$FilePath,
    [Parameter(Mandatory=$true)][string[]]$ArgumentList,
    [Parameter(Mandatory=$true)][int]$TimeoutSeconds
  )
  $process = Start-Process -FilePath $FilePath -ArgumentList $ArgumentList -PassThru -WindowStyle Hidden
  $null = $process.Handle
  if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
    try { $process.Kill() } catch {}
    throw "$FilePath did not exit within $TimeoutSeconds seconds"
  }
  return $process.ExitCode
}

function Find-VirtIORoot {
  foreach ($drive in Get-PSDrive -PSProvider FileSystem) {
    if (Test-Path (Join-Path $drive.Root "guest-agent\qemu-ga-x86_64.msi")) { return $drive.Root }
  }
  return $null
}

function Install-VirtIOGuestSupport {
  $virtioRoot = Find-VirtIORoot
  if (-not $virtioRoot) { throw "No virtio-win media found" }

  $exitCode = Invoke-Bounded -FilePath "certutil.exe" -ArgumentList "-f", "-addstore", "TrustedPublisher", ('"{0}"' -f (Join-Path $virtioRoot "cert\Virtio_Win_Red_Hat_CA.cer")) -TimeoutSeconds 60
  Write-Host "certutil TrustedPublisher exited with $exitCode"
  foreach ($driver in @("Balloon", "NetKVM", "pvpanic", "vioinput", "viorng", "vioscsi", "vioserial", "viostor")) {
    $driverDir = Join-Path $virtioRoot "$driver\w11\amd64"
    if (-not (Test-Path $driverDir)) { continue }
    $exitCode = Invoke-Bounded -FilePath "pnputil.exe" -ArgumentList "/add-driver", ('"{0}\*.inf"' -f $driverDir), "/install" -TimeoutSeconds 300
    Write-Host "pnputil $driver exited with $exitCode"
  }

  if (-not (Get-Service -Name QEMU-GA -ErrorAction SilentlyContinue)) {
    $msi = Join-Path $virtioRoot "guest-agent\qemu-ga-x86_64.msi"
    Write-Host "Installing QEMU guest agent from $msi"
    $exitCode = Invoke-Bounded -FilePath "msiexec.exe" -ArgumentList "/i", ('"{0}"' -f $msi), "/qn", "/norestart" -TimeoutSeconds 600
    if ($exitCode -ne 0 -and $exitCode -ne 3010) { throw "QEMU guest agent installer exited with $exitCode" }
  }

  Set-Service -Name QEMU-GA -StartupType Automatic
  Start-Service -Name QEMU-GA
}

New-Item -ItemType Directory -Force -Path "C:\kiln-vm\cache", "C:\kiln-vm\workspaces", "C:\tools" | Out-Null

Write-Host "Configuring Windows VM basics"
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem" -Name LongPathsEnabled -Type DWord -Value 1
New-Item -Path "HKLM:\SOFTWARE\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell" -Force | Out-Null
Set-ItemProperty -Path "HKLM:\SOFTWARE\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell" -Name ExecutionPolicy -Value RemoteSigned
powercfg.exe /change standby-timeout-ac 0 | Out-Null
powercfg.exe /change hibernate-timeout-ac 0 | Out-Null
powercfg.exe /hibernate off | Out-Null

$payloadRoot = Find-PayloadRoot
if (-not $payloadRoot) { throw "No kiln-vm Windows payload media found" }
Write-Host "Using payload media at $payloadRoot"
Install-OpenSshFromPayload -PayloadRoot $payloadRoot
Install-VirtIOGuestSupport

Write-Host "Provisioning complete"
New-Item -ItemType File -Force -Path $sentinel | Out-Null
Grant-Readable -Path $LogPath
Remove-ProvisionTasks
Stop-Transcript
$transcriptStarted = $false
$mutex.ReleaseMutex() | Out-Null
$mutex.Dispose()
exit 0
`
