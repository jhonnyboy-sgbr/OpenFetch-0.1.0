#define AppName "OpenFetch"
#define AppVersion "0.1.0"
#define RootDir ".."
[Setup]
AppId={{56DB4F55-8A4B-4CC2-AE8E-925BCDB75021}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
DefaultDirName={autopf}\OpenFetch
DefaultGroupName=OpenFetch
DisableProgramGroupPage=yes
OutputDir=..\release
OutputBaseFilename=OpenFetch-Setup-0.1.0
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayName=OpenFetch
VersionInfoVersion=0.1.0

[Tasks]
Name: "desktopicon"; Description: "Criar atalho na área de trabalho"; GroupDescription: "Atalhos:"; Flags: unchecked

[Files]
Source: "..\build\bin\OpenFetch.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\build\bin\tools\yt-dlp.exe"; DestDir: "{app}\tools"; Flags: ignoreversion
Source: "..\build\bin\tools\ffmpeg.exe"; DestDir: "{app}\tools"; Flags: ignoreversion
Source: "..\build\bin\tools\deno.exe"; DestDir: "{app}\tools"; Flags: ignoreversion
Source: "..\build\bin\tools\ffprobe.exe"; DestDir: "{app}\tools"; Flags: ignoreversion skipifsourcedoesntexist
Source: "..\docs\LICENCAS-E-CREDITOS.txt"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\OpenFetch"; Filename: "{app}\OpenFetch.exe"
Name: "{autodesktop}\OpenFetch"; Filename: "{app}\OpenFetch.exe"; Tasks: desktopicon

[Run]
Filename: "{app}\OpenFetch.exe"; Description: "Abrir OpenFetch"; Flags: nowait postinstall skipifsilent
