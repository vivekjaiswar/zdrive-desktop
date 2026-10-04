; ZDrive Windows installer. Built by CI (see .github/workflows/build.yml):
;   makensis -DWINFSP_MSI=<filename> -DZDRIVE_EXE=<filename> zdrive-installer.nsi
;
; Bundles WinFsp (the Windows driver rclone's mount needs - Windows has no
; built-in equivalent) and silently installs it only if not already present,
; so the user downloads and runs exactly one exe. Legal to bundle/redistribute
; for free under WinFsp's GPLv3 FLOSS exception because this project is
; itself open source (MIT, see ../../LICENSE) - see README's License section.
!include "MUI2.nsh"

!ifndef WINFSP_MSI
  !error "Pass -DWINFSP_MSI=<filename>"
!endif
!ifndef ZDRIVE_EXE
  !error "Pass -DZDRIVE_EXE=<filename>"
!endif

Name "ZDrive"
OutFile "zdrive-windows-setup.exe"
InstallDir "$LOCALAPPDATA\ZDrive"
; Per-user install, deliberately NOT elevated: msiexec self-elevates on its
; own for the one piece that actually needs admin (the WinFsp driver, via
; its own UAC prompt), same as any unprivileged installer bundling an MSI.
; Elevating the whole installer would also elevate the finish page's
; "Run zdrive.exe login" step, and Chrome (plus most modern browsers)
; silently refuses to launch when invoked with an Administrator token -
; no window, no error, sign-in just appears to do nothing.
RequestExecutionLevel user

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_RUN "$INSTDIR\${ZDRIVE_EXE}"
!define MUI_FINISHPAGE_RUN_PARAMETERS "login"
!define MUI_FINISHPAGE_RUN_TEXT "Sign in to ZDrive now"
!define MUI_FINISHPAGE_TEXT "ZDrive is installed. Sign in, then mount your drive from a terminal:$\r$\n$\r$\n$INSTDIR\${ZDRIVE_EXE} mount zdrive: Z: --vfs-cache-mode full"
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_LANGUAGE "English"

Section "Install"
  SetOutPath "$INSTDIR"
  File "${ZDRIVE_EXE}"

  ; WinFsp's own installer records its location here (WOW6432Node, since
  ; we only ship an x64 build) - a more reliable presence check than
  ; guessing an install path, and skips reinstalling every run.
  ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\WinFsp" "InstallDir"
  StrCmp $0 "" 0 WinFspPresent
    DetailPrint "Installing WinFsp (the Windows driver a mounted drive needs)..."
    File "${WINFSP_MSI}"
    ExecWait 'msiexec /i "$INSTDIR\${WINFSP_MSI}" /quiet /norestart' $1
    Delete "$INSTDIR\${WINFSP_MSI}"
    DetailPrint "WinFsp installer exit code: $1"
    Goto WinFspDone
  WinFspPresent:
    DetailPrint "WinFsp already installed, skipping."
  WinFspDone:

  CreateDirectory "$SMPROGRAMS\ZDrive"
  CreateShortCut "$SMPROGRAMS\ZDrive\Sign in to ZDrive.lnk" "$INSTDIR\${ZDRIVE_EXE}" "login"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  ; Registers with Windows' "Apps & Features" so it isn't just a leftover
  ; folder - WinFsp itself is left installed on uninstall (a shared system
  ; driver other apps may also depend on), same as e.g. a VC++ redistributable.
  ; HKCU, not HKLM: this is a per-user, unelevated install (see
  ; RequestExecutionLevel above), and HKCU's own Uninstall key is just as
  ; visible to "Apps & Features" for the installing user.
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\ZDrive" "DisplayName" "ZDrive"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\ZDrive" "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\ZDrive" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\ZDrive" "Publisher" "ZennialHub Technologies"
SectionEnd

Section "Uninstall"
  Delete "$INSTDIR\${ZDRIVE_EXE}"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\ZDrive\Sign in to ZDrive.lnk"
  RMDir "$SMPROGRAMS\ZDrive"
  DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\ZDrive"
SectionEnd
