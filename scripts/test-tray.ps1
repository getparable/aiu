[CmdletBinding()]
param(
    [string] $BinaryPath = (Join-Path $PSScriptRoot '..\target\debug\aiu.exe'),
    [int] $TimeoutSeconds = 20,
    [switch] $OccludedQuit
)

$ErrorActionPreference = 'Stop'

if ($env:OS -ne 'Windows_NT') { throw 'This smoke test requires Windows.' }
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$fixture = Join-Path $repo 'fixtures\demo.json'
$binary = (Resolve-Path $BinaryPath -ErrorAction Stop).Path
if (-not (Test-Path $fixture -PathType Leaf)) { throw "Fixture not found: $fixture" }

if (-not ('AiuTraySmoke.Native' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

namespace AiuTraySmoke {
  public sealed class WindowInfo {
    public IntPtr Hwnd;
    public int Pid;
    public bool Visible;
    public string ClassName = "";
    public string Title = "";
  }
  public static class Native {
    private delegate bool EnumWindowsProc(IntPtr hwnd, IntPtr lParam);
    [DllImport("user32.dll")] static extern bool EnumWindows(EnumWindowsProc cb, IntPtr data);
    [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr hwnd, out uint pid);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetClassName(IntPtr hwnd, StringBuilder text, int max);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetWindowText(IntPtr hwnd, StringBuilder text, int max);
    [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr hwnd);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern bool GetWindowRect(IntPtr hwnd, out Rect rect);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern IntPtr CreateWindowEx(uint exStyle, string className, string title, uint style, int x, int y, int width, int height, IntPtr parent, IntPtr menu, IntPtr instance, IntPtr param);
    [DllImport("user32.dll")] public static extern bool UpdateWindow(IntPtr hwnd);
    [DllImport("user32.dll")] public static extern bool DestroyWindow(IntPtr hwnd);
    [DllImport("user32.dll")] public static extern bool PostMessage(IntPtr hwnd, uint msg, IntPtr wParam, IntPtr lParam);
    [DllImport("user32.dll")] public static extern IntPtr SendMessage(IntPtr hwnd, uint msg, IntPtr wParam, IntPtr lParam);
    [DllImport("user32.dll")] public static extern IntPtr GetMenu(IntPtr hwnd);
    [DllImport("user32.dll")] public static extern int GetMenuItemCount(IntPtr menu);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetMenuString(IntPtr menu, uint item, StringBuilder text, int max, uint flags);
    [DllImport("user32.dll")] public static extern uint GetMenuItemID(IntPtr menu, int position);
    [StructLayout(LayoutKind.Sequential)] struct NotifyIconIdentifier {
      public int cbSize; public IntPtr hWnd; public uint uID; public Guid guidItem;
    }
    [StructLayout(LayoutKind.Sequential)] public struct Rect { public int left, top, right, bottom; }
    [DllImport("shell32.dll")] static extern int Shell_NotifyIconGetRect(ref NotifyIconIdentifier id, out Rect rect);
    public static WindowInfo[] ForProcess(int wantedPid) {
      var result = new List<WindowInfo>();
      EnumWindows((hwnd, data) => {
        uint pid; GetWindowThreadProcessId(hwnd, out pid);
        if (pid != (uint)wantedPid) return true;
        var cls = new StringBuilder(256); GetClassName(hwnd, cls, cls.Capacity);
        var title = new StringBuilder(512); GetWindowText(hwnd, title, title.Capacity);
        result.Add(new WindowInfo { Hwnd=hwnd, Pid=(int)pid, Visible=IsWindowVisible(hwnd), ClassName=cls.ToString(), Title=title.ToString() });
        return true;
      }, IntPtr.Zero);
      return result.ToArray();
    }
    public static bool HasTrayRect(IntPtr hwnd, uint id) {
      var identifier = new NotifyIconIdentifier { cbSize=Marshal.SizeOf<NotifyIconIdentifier>(), hWnd=hwnd, uID=id };
      Rect rect; return Shell_NotifyIconGetRect(ref identifier, out rect) == 0;
    }
    public static IntPtr CreateCover(IntPtr panel) {
      Rect rect;
      if (!GetWindowRect(panel, out rect)) return IntPtr.Zero;
      const uint WS_EX_TOOLWINDOW = 0x00000080;
      const uint WS_EX_TOPMOST = 0x00000008;
      const uint WS_POPUP = 0x80000000;
      const uint WS_VISIBLE = 0x10000000;
      int margin = 2;
      var cover = CreateWindowEx(WS_EX_TOOLWINDOW | WS_EX_TOPMOST, "STATIC", "AIU smoke cover",
        WS_POPUP | WS_VISIBLE, rect.left - margin, rect.top - margin,
        (rect.right - rect.left) + (margin * 2), (rect.bottom - rect.top) + (margin * 2),
        IntPtr.Zero, IntPtr.Zero, IntPtr.Zero, IntPtr.Zero);
      if (cover != IntPtr.Zero) UpdateWindow(cover);
      return cover;
    }
  }
}
'@
}

$outLog = Join-Path ([IO.Path]::GetTempPath()) "aiu-tray-$([guid]::NewGuid()).out.log"
$errLog = Join-Path ([IO.Path]::GetTempPath()) "aiu-tray-$([guid]::NewGuid()).err.log"
$process = $null
$ownedPid = $null
$cover = [IntPtr]::Zero
$failed = $true

function Get-AiuWindows { [AiuTraySmoke.Native]::ForProcess($ownedPid) }
function Wait-Aiu([scriptblock] $Condition, [string] $Failure) {
    $waitDeadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $waitDeadline) {
        if (& $Condition) { return }
        Start-Sleep -Milliseconds 100
    }
    throw $Failure
}
function Assert-Aiu([bool] $Value, [string] $Message) { if (-not $Value) { throw $Message } }

try {
    $process = Start-Process -FilePath $binary -ArgumentList @('gui', '--fixture', "`"$fixture`"") `
        -WindowStyle Hidden -RedirectStandardOutput $outLog -RedirectStandardError $errLog -PassThru
    $ownedPid = $process.Id

    Wait-Aiu { $null -ne (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'AIU panel did not become visible.'
    $windows = Get-AiuWindows
    $panel = $windows | Where-Object { $_.Visible -and $_.Title -match '^AIU' } | Select-Object -First 1
    $tray = $windows | Where-Object { $_.ClassName -eq 'tray_icon_app' } | Select-Object -First 1
    Assert-Aiu ($null -ne $tray) 'tray_icon_app native window was not created.'

    # Closing the panel must hide it while the tray process remains alive.
    Assert-Aiu ([AiuTraySmoke.Native]::PostMessage($panel.Hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)) 'WM_CLOSE could not be posted.'
    Wait-Aiu { -not (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'WM_CLOSE did not hide the AIU panel.'
    Assert-Aiu (-not $process.HasExited) 'AIU exited when its panel was closed.'

    # tray-icon dispatches WM_USER_TRAYICON (6002); WM_LBUTTONUP restores the panel.
    # tray-icon drops callback events when Shell_NotifyIconGetRect cannot find
    # the icon (for example when Explorer has placed it in the overflow area).
    $hasTrayRect = @(1..8 | Where-Object { [AiuTraySmoke.Native]::HasTrayRect($tray.Hwnd, $_) }).Count -gt 0
    Assert-Aiu $hasTrayRect 'Shell_NotifyIconGetRect could not locate AIU tray icon.'
    Assert-Aiu ([AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 6002, [IntPtr]::Zero, [IntPtr]::new(0x0202))) 'Tray left-click message could not be posted.'
    Wait-Aiu { $null -ne (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'Tray left-click did not restore AIU.'

    # Right-click must create a native popup menu. GetMenu is attempted on both the
    # tray owner and popup; muda owns the HMENU and TrackPopupMenu owns the modal loop.
    $tray = (Get-AiuWindows | Where-Object { $_.ClassName -eq 'tray_icon_app' } | Select-Object -First 1)
    [AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 6002, [IntPtr]::Zero, [IntPtr]::new(0x0205)) | Out-Null
    Wait-Aiu { $null -ne (Get-AiuWindows | Where-Object { $_.ClassName -eq '#32768' -and $_.Visible }) } 'Tray right-click did not create a native popup menu.'
    $popup = Get-AiuWindows | Where-Object { $_.ClassName -eq '#32768' -and $_.Visible } | Select-Object -First 1
    # TrackPopupMenu owns the popup HWND; MN_GETHMENU (rather than GetMenu)
    # retrieves muda's HMENU so labels and the actual command IDs can be read.
    $menu = [AiuTraySmoke.Native]::SendMessage($popup.Hwnd, 0x01e1, [IntPtr]::Zero, [IntPtr]::Zero)
    Assert-Aiu ($menu -ne [IntPtr]::Zero) 'MN_GETHMENU did not return the tray HMENU.'
    $count = [AiuTraySmoke.Native]::GetMenuItemCount($menu)
    Assert-Aiu ($count -ge 3) "Tray menu has only $count native items; expected Show, Refresh and Quit."
    $items = for ($i = 0; $i -lt $count; $i++) {
        $text = New-Object Text.StringBuilder 256
        [AiuTraySmoke.Native]::GetMenuString($menu, [uint32]$i, $text, $text.Capacity, 0x400) | Out-Null
        [pscustomobject]@{ Text=$text.ToString(); Id=[AiuTraySmoke.Native]::GetMenuItemID($menu, $i) }
    }
    $show = $items | Where-Object { $_.Text -match '(?i)show' } | Select-Object -First 1
    $refresh = $items | Where-Object { $_.Text -match '(?i)refresh' } | Select-Object -First 1
    $quit = $items | Where-Object { $_.Text -match '(?i)quit|exit' } | Select-Object -First 1
    Assert-Aiu ($null -ne $show -and $null -ne $refresh -and $null -ne $quit) ("Tray menu labels were: " + (($items | ForEach-Object Text) -join ', '))
    [AiuTraySmoke.Native]::PostMessage($popup.Hwnd, 0x0100, [IntPtr]0x1b, [IntPtr]::Zero) | Out-Null # VK_ESCAPE
    [AiuTraySmoke.Native]::PostMessage($popup.Hwnd, 0x0101, [IntPtr]0x1b, [IntPtr]::Zero) | Out-Null
    [AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 0x001f, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null # WM_CANCELMODE
    Wait-Aiu { -not (Get-AiuWindows | Where-Object { $_.ClassName -eq '#32768' -and $_.Visible }) } 'Tray popup did not dismiss.'

    # WM_COMMAND is the same native dispatch path used by TrackPopupMenu.
    # Verify Show independently by hiding the panel, then invoking its command.
    $panel = Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' } | Select-Object -First 1
    Assert-Aiu ($null -ne $panel) 'AIU panel disappeared before Show command test.'
    [AiuTraySmoke.Native]::PostMessage($panel.Hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null
    Wait-Aiu { -not (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'Panel did not hide for Show command test.'
    [AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 0x0111, [IntPtr]$show.Id, [IntPtr]::Zero) | Out-Null
    Wait-Aiu { $null -ne (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'Tray Show command did not restore AIU.'

    # Hide again; Refresh must leave the panel hidden, and Quit must terminate
    # from the same native command route.
    $panel = Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' } | Select-Object -First 1
    [AiuTraySmoke.Native]::PostMessage($panel.Hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null
    Wait-Aiu { -not (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'Panel did not hide before Refresh command test.'
    [AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 0x0111, [IntPtr]$refresh.Id, [IntPtr]::Zero) | Out-Null
    Start-Sleep -Milliseconds 500
    Assert-Aiu (-not (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' })) 'Tray Refresh unexpectedly showed the panel.'
    if ($OccludedQuit) {
        [AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 0x0111, [IntPtr]$show.Id, [IntPtr]::Zero) | Out-Null
        Wait-Aiu { $null -ne (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' }) } 'Tray Show command did not restore AIU for occlusion test.'
        $panel = Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' } | Select-Object -First 1
        $cover = [AiuTraySmoke.Native]::CreateCover($panel.Hwnd)
        Assert-Aiu ($cover -ne [IntPtr]::Zero) 'Could not create native occlusion cover.'
        Assert-Aiu ([AiuTraySmoke.Native]::IsWindowVisible($cover)) 'Native occlusion cover is not visible.'
        Assert-Aiu ($null -ne (Get-AiuWindows | Where-Object { $_.Visible -and $_.Title -match '^AIU' })) 'AIU panel disappeared under occlusion cover.'
    }
    [AiuTraySmoke.Native]::PostMessage($tray.Hwnd, 0x0111, [IntPtr]$quit.Id, [IntPtr]::Zero) | Out-Null
    Wait-Aiu { $process.HasExited } 'Tray menu Quit did not exit AIU.'
    $failed = $false
    Write-Output "PASS: panel, tray window, hide/restore, native popup menu, and Quit ($ownedPid)."
} catch {
    Write-Host "SMOKE TEST FAILED: $($_.Exception.Message)"
    if ($outLog -and (Test-Path $outLog)) { Write-Host (Get-Content $outLog -Raw) }
    if ($errLog -and (Test-Path $errLog)) { Write-Host (Get-Content $errLog -Raw) }
    throw
} finally {
    if ($cover -ne [IntPtr]::Zero) { [AiuTraySmoke.Native]::DestroyWindow($cover) | Out-Null }
    if ($failed -and $process -and -not $process.HasExited -and $ownedPid -and (Get-Process -Id $ownedPid -ErrorAction SilentlyContinue)) {
        Stop-Process -Id $ownedPid -Force -ErrorAction SilentlyContinue
    }
    Remove-Item $outLog, $errLog -Force -ErrorAction SilentlyContinue
}
