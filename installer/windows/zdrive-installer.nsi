; ZDrive Windows installer. Built by CI (see .github/workflows/build.yml):
;   makensis -DWINFSP_MSI=<path> -DWINFSP_MSI_NAME=<filename>
;            -DZDRIVE_EXE=<path> -DZDRIVE_EXE_NAME=<filename>
;            zdrive-installer.nsi
;
; Two defines per file, not one: File instructions below need the real
; build-time path to read the file off disk (CI passes an absolute path
; specifically so this works regardless of invocation cwd - NSIS resolves
; a relative File path against the .nsi script's own directory, not the
; caller's). Every *runtime* reference ($INSTDIR\... - the finish page's
; Run target, the Start Menu shortcut, msiexec's own install command, the
; uninstaller) needs just the bare filename instead: $INSTDIR already
; supplies the real runtime directory, and concatenating a CI build
; path onto it produces a path that doesn't exist on the installing
; machine at all. Confirmed as a real bug 2026-10-21 (a live customer's
; first-ever Windows install: the finish page's "Sign in to ZDrive now"
; silently tried to run a path like
; "...\AppData\Local\ZDrive\/home/runner/work/zdrive-desktop/.../
; zdrive-windows-amd64.exe" - using the single WINFSP_MSI/ZDRIVE_EXE
; define for both purposes instead of two. The same bug independently
; broke the WinFsp driver's own msiexec install too (same pattern,
; $INSTDIR\${WINFSP_MSI}), meaning no Windows install before this fix
; could have ever produced a working mount, sign-in or not.
;
; Bundles WinFsp (the Windows driver rclone's mount needs - Windows has no
; built-in equivalent) and silently installs it only if not already present,
; so the user downloads and runs exactly one exe. Legal to bundle/redistribute
; for free under WinFsp's GPLv3 FLOSS exception because this project is
; itself open source (MIT, see ../../LICENSE) - see README's License section.
!include "MUI2.nsh"

!ifndef WINFSP_MSI
  !error "Pass -DWINFSP_MSI=<path>"
!endif
!ifndef WINFSP_MSI_NAME
  !error "Pass -DWINFSP_MSI_NAME=<filename>"
!endif
!ifndef ZDRIVE_EXE
  !error "Pass -DZDRIVE_EXE=<path>"
!endif
!ifndef ZDRIVE_EXE_NAME
  !error "Pass -DZDRIVE_EXE_NAME=<filename>"
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

!define MUI_FINISHPAGE_RUN "$INSTDIR\${ZDRIVE_EXE_NAME}"
!define MUI_FINISHPAGE_RUN_PARAMETERS "login"
!define MUI_FINISHPAGE_RUN_TEXT "Sign in to ZDrive now"
!define MUI_FINISHPAGE_TEXT "ZDrive is installed. Sign in, then mount your drive from a terminal:$\r$\n$\r$\n$INSTDIR\${ZDRIVE_EXE_NAME} mount zdrive: Z: --vfs-cache-mode full"
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
    ExecWait 'msiexec /i "$INSTDIR\${WINFSP_MSI_NAME}" /quiet /norestart' $1
    Delete "$INSTDIR\${WINFSP_MSI_NAME}"
    DetailPrint "WinFsp installer exit code: $1"
    Goto WinFspDone
  WinFspPresent:
    DetailPrint "WinFsp already installed, skipping."
  WinFspDone:

  CreateDirectory "$SMPROGRAMS\ZDrive"
  CreateShortCut "$SMPROGRAMS\ZDrive\Sign in to ZDrive.lnk" "$INSTDIR\${ZDRIVE_EXE_NAME}" "login"
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
  ; Tears down the autostart Run-key registration (cmd/autostart) before
  ; deleting the exe it points at - run while the binary still exists,
  ; not after, since this shells out to it.
  ExecWait '"$INSTDIR\${ZDRIVE_EXE_NAME}" autostart uninstall'

  Delete "$INSTDIR\${ZDRIVE_EXE_NAME}"
  ; A leftover from the auto-update swap (cmd/tray/update_windows.go),
  ; if the user never relaunched after an update before uninstalling -
  ; that cleanup normally happens on next run, which never came.
  Delete "$INSTDIR\${ZDRIVE_EXE_NAME}.old"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\ZDrive\Sign in to ZDrive.lnk"
  RMDir "$SMPROGRAMS\ZDrive"
  DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\ZDrive"
SectionEnd
