//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// psEscape escapes a string for safe embedding inside a PowerShell
// single-quoted string literal by doubling any single-quote characters.
// The result is safe for use in: Set-ItemProperty -Name 'result'
func psEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ─────────────────────────────────────────────────────────────────────────────
// screenshot
// ─────────────────────────────────────────────────────────────────────────────

func executeScreenshot(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		MaxWidth int `json:"max_width"`
	}
	_ = json.Unmarshal(input, &args)
	if args.MaxWidth <= 0 {
		args.MaxWidth = 1280
	}

	ps := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bounds = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
$bmp = New-Object System.Drawing.Bitmap($bounds.Width, $bounds.Height)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
$g.Dispose()
$maxW = %d
if ($bmp.Width -gt $maxW) {
    $ratio = $maxW / $bmp.Width
    $newH = [int]($bmp.Height * $ratio)
    $resized = New-Object System.Drawing.Bitmap($maxW, $newH)
    $g2 = [System.Drawing.Graphics]::FromImage($resized)
    $g2.DrawImage($bmp, 0, 0, $maxW, $newH)
    $g2.Dispose(); $bmp.Dispose()
    $bmp = $resized
}
$ms = New-Object System.IO.MemoryStream
$bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
"IMAGE_BASE64:" + [Convert]::ToBase64String($ms.ToArray())
`, args.MaxWidth)

	return executePowerShell(ctx, ps, 30)
}

// ─────────────────────────────────────────────────────────────────────────────
// process_list
// ─────────────────────────────────────────────────────────────────────────────

func executeProcessList(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Filter string `json:"filter"`
	}
	_ = json.Unmarshal(input, &args)

	ps := `Get-Process | Sort-Object CPU -Descending | Select-Object -First 50 Name, Id, @{N='CPU_s';E={[math]::Round($_.CPU,1)}}, @{N='Mem_MB';E={[math]::Round($_.WorkingSet/1MB,1)}} | ConvertTo-Json -Compress`
	if args.Filter != "" {
		ps = fmt.Sprintf(`Get-Process -Name '*%s*' -ErrorAction SilentlyContinue | Select-Object Name, Id, @{N='CPU_s';E={[math]::Round($_.CPU,1)}}, @{N='Mem_MB';E={[math]::Round($_.WorkingSet/1MB,1)}} | ConvertTo-Json -Compress`, psEscape(args.Filter))
	}
	return executePowerShell(ctx, ps, 15)
}

// ─────────────────────────────────────────────────────────────────────────────
// kill_process
// ─────────────────────────────────────────────────────────────────────────────

func executeKillProcess(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		PID  int    `json:"pid"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("kill_process: invalid input: %w", err)
	}

	var ps string
	if args.PID > 0 {
		ps = fmt.Sprintf(`Stop-Process -Id %d -Force -ErrorAction Stop; "Process %d terminated."`, args.PID, args.PID)
	} else if args.Name != "" {
		safe := psEscape(args.Name)
		ps = fmt.Sprintf(`Stop-Process -Name '%s' -Force -ErrorAction Stop; "Process '%s' terminated."`, safe, safe)
	} else {
		return "", fmt.Errorf("kill_process: provide pid or name")
	}
	return executePowerShell(ctx, ps, 15)
}

// ─────────────────────────────────────────────────────────────────────────────
// toast_notify
// ─────────────────────────────────────────────────────────────────────────────

func executeToastNotify(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("toast_notify: invalid input: %w", err)
	}
	if args.Message == "" {
		return "", fmt.Errorf("toast_notify: message is required")
	}
	if args.Title == "" {
		args.Title = "WinClaw"
	}

	ps := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::Information
$n.Visible = $true
$n.BalloonTipTitle = '%s'
$n.BalloonTipText = '%s'
$n.ShowBalloonTip(5000)
Start-Sleep -Milliseconds 300
$n.Dispose()
"Notification sent."
`, psEscape(args.Title), psEscape(args.Message))

	return executePowerShell(ctx, ps, 10)
}

// ─────────────────────────────────────────────────────────────────────────────
// run_elevated
// ─────────────────────────────────────────────────────────────────────────────

func executeRunElevated(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("run_elevated: invalid input: %w", err)
	}
	if args.Command == "" {
		return "", fmt.Errorf("run_elevated: command is required")
	}

	// Escape single quotes in the command.
	escaped := ""
	for _, c := range args.Command {
		if c == '\'' {
			escaped += "''"
		} else {
			escaped += string(c)
		}
	}

	ps := fmt.Sprintf(`
$outFile = [System.IO.Path]::GetTempFileName()
try {
    $proc = Start-Process powershell -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList '-NoProfile','-NonInteractive','-Command','%s | Out-File -FilePath $outFile -Encoding UTF8'
    $output = Get-Content $outFile -Raw -ErrorAction SilentlyContinue
    if ($output) { $output.Trim() } else { "(no output)" }
} finally {
    Remove-Item $outFile -ErrorAction SilentlyContinue
}
`, escaped)

	return executePowerShell(ctx, ps, 60)
}

// ─────────────────────────────────────────────────────────────────────────────
// registry_read
// ─────────────────────────────────────────────────────────────────────────────

func executeRegistryRead(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("registry_read: invalid input: %w", err)
	}
	if args.Path == "" {
		return "", fmt.Errorf("registry_read: path is required (e.g. HKCU:\\Software\\MyApp)")
	}

	safePath := psEscape(args.Path)
	var ps string
	if args.Name != "" {
		safeName := psEscape(args.Name)
		ps = fmt.Sprintf(`(Get-ItemProperty -Path '%s' -Name '%s' -ErrorAction Stop).'%s'`, safePath, safeName, safeName)
	} else {
		ps = fmt.Sprintf(`Get-ItemProperty -Path '%s' -ErrorAction Stop | ConvertTo-Json -Depth 2`, safePath)
	}
	return executePowerShell(ctx, ps, 10)
}

// ─────────────────────────────────────────────────────────────────────────────
// registry_write
// ─────────────────────────────────────────────────────────────────────────────

func executeRegistryWrite(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		Value string `json:"value"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("registry_write: invalid input: %w", err)
	}
	if args.Path == "" || args.Name == "" {
		return "", fmt.Errorf("registry_write: path and name are required")
	}
	// Whitelist registry value types to prevent command injection via the
	// -Type argument, which is inserted into the PowerShell script unquoted.
	switch args.Type {
	case "String", "DWord", "QWord", "Binary", "ExpandString", "MultiString":
		// valid
	case "":
		args.Type = "String"
	default:
		return fmt.Sprintf("unsupported registry type %q; use: String, DWord, QWord, Binary, ExpandString, MultiString", args.Type), nil
	}

	safePath := psEscape(args.Path)
	safeName := psEscape(args.Name)
	safeValue := psEscape(args.Value)
	ps := fmt.Sprintf(`
if (-not (Test-Path '%s')) { New-Item -Path '%s' -Force | Out-Null }
Set-ItemProperty -Path '%s' -Name '%s' -Value '%s' -Type %s -ErrorAction Stop
"Registry value '%s' written."
`, safePath, safePath, safePath, safeName, safeValue, args.Type, safeName)

	return executePowerShell(ctx, ps, 10)
}
