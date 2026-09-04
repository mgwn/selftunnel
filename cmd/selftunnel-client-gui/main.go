// Command selftunnel-client-gui is the Fyne desktop client (spec §3.6): a
// single window with the connection form (server, target, custom ID,
// TLS/debug checkboxes, optional mTLS key pair), a status bar with the
// current tunnelID, a scrolling log pane and one Connect/Disconnect
// button. The client core runs in a background goroutine; all UI updates
// are marshalled onto the Fyne main thread via fyne.Do.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mgwn/selftunnel/internal/client"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// configPath is the config file next to the binary, persisted on every
// connect (spec §6.5).
const configPath = "config.json"

// main builds the window and runs the Fyne event loop. Layout top-to-
// bottom: settings form, status bar (state + tunnelID + copy button),
// log pane, connect button. Closing the window stops a running client.
func main() {
	a := app.New()
	w := a.NewWindow("selftunnel client")
	w.Resize(fyne.NewSize(720, 540))

	cfg, err := client.LoadConfig(configPath)
	if err != nil {
		cfg = &client.Config{}
	}

	var c *client.Client
	var ctxCancel context.CancelFunc

	// --- settings form widgets (pre-filled from the config file) ---

	serverEntry := widget.NewEntry()
	serverEntry.SetText(cfg.Server)
	serverEntry.SetPlaceHolder("wss://relay.example.com or https://relay.example.com")

	targetEntry := widget.NewEntry()
	targetEntry.SetText(cfg.Target)
	targetEntry.SetPlaceHolder("http://192.168.1.10:8080")

	customEntry := widget.NewEntry()
	customEntry.SetText(cfg.CustomID)
	customEntry.SetPlaceHolder("8-char alphanumeric, empty for random")

	serverInsecure := widget.NewCheck("Skip relay server TLS verification", nil)
	targetInsecure := widget.NewCheck("Skip target HTTPS self-signed certificate verification", nil)
	debugCheck := widget.NewCheck("Debug mode (log every network request)", nil)

	clientCertEntry := widget.NewEntry()
	clientCertEntry.SetPlaceHolder("Target client certificate PEM path (optional)")
	clientKeyEntry := widget.NewEntry()
	clientKeyEntry.SetPlaceHolder("Target client key PEM path (optional)")

	// --- status bar widgets ---

	statusLabel := widget.NewLabel("Disconnected")
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}
	tunIDLabel := widget.NewLabel("-")
	copyBtn := widget.NewButton("Copy ID", func() {
		if c == nil {
			return
		}
		id := c.TunnelID()
		if id != "" {
			a.Clipboard().SetContent(id)
		}
	})
	copyBtn.Disable()

	// --- log pane: keeps roughly the last 500 lines responsive ---

	logEntry := widget.NewMultiLineEntry()
	logEntry.Disable()
	logEntry.Wrapping = fyne.TextWrapWord
	logEntry.SetText("")
	logScroll := container.NewScroll(logEntry)
	logScroll.SetMinSize(fyne.NewSize(0, 200))

	appendLog := func(line string) {
		fyne.Do(func() {
			text := logEntry.Text
			if text != "" {
				text += "\n"
			}
			text += line
			// Keep the last ~500 lines so the widget stays responsive.
			lines := strings.Split(text, "\n")
			if len(lines) > 500 {
				lines = lines[len(lines)-500:]
				text = strings.Join(lines, "\n")
			}
			logEntry.SetText(text)
			logScroll.ScrollToBottom()
		})
	}

	// updateUI maps client status transitions onto the status bar widgets;
	// always invoked on the Fyne main thread.
	updateUI := func(status client.Status) {
		fyne.Do(func() {
			switch status {
			case client.StatusOnline:
				statusLabel.SetText("Online")
				if c != nil {
					tunIDLabel.SetText(c.TunnelID())
				}
				connectBtn.SetText("Disconnect")
				copyBtn.Enable()
			case client.StatusConnecting:
				statusLabel.SetText("Connecting...")
				connectBtn.SetText("Connecting")
				copyBtn.Disable()
			case client.StatusBackoff:
				statusLabel.SetText("Reconnecting...")
				connectBtn.SetText("Reconnecting")
				copyBtn.Disable()
			default:
				statusLabel.SetText("Disconnected")
				tunIDLabel.SetText("-")
				connectBtn.SetText("Connect")
				copyBtn.Disable()
			}
		})
	}

	// onLog renders one client log line (slog-style attrs) into the pane.
	onLog := func(level, msg string, attrs ...any) {
		line := fmt.Sprintf("[%s] %s", strings.ToUpper(level), msg)
		for i := 0; i+1 < len(attrs); i += 2 {
			line += fmt.Sprintf(" %v=%v", attrs[i], attrs[i+1])
		}
		appendLog(line)
	}

	// stopClient disconnects a running client and cancels its context.
	stopClient := func() {
		if c != nil {
			c.Stop()
			c = nil
		}
		if ctxCancel != nil {
			ctxCancel()
			ctxCancel = nil
		}
		updateUI(client.StatusDisconnected)
	}

	// connectBtn doubles as Connect (validate + save config + start the
	// client goroutine) and Disconnect while online.
	connectBtn = widget.NewButton("Connect", func() {
		if c != nil && c.Status() == client.StatusOnline {
			stopClient()
			return
		}

		server := client.NormalizeServer(serverEntry.Text)
		target := strings.TrimSpace(targetEntry.Text)
		customID := strings.ToLower(strings.TrimSpace(customEntry.Text))

		if server == "" {
			dialog.ShowError(fmt.Errorf("please enter the relay server address"), w)
			return
		}
		if target != "" && !client.IsValidTarget(target) {
			dialog.ShowError(fmt.Errorf("target address must start with http:// or https://"), w)
			return
		}
		if customID != "" && !client.IsValidCustomID(customID) {
			dialog.ShowError(fmt.Errorf("custom ID must be 8 lowercase letters or digits"), w)
			return
		}

		cfg.Server = server
		cfg.Target = target
		cfg.CustomID = customID
		if err := client.SaveConfig(configPath, cfg); err != nil {
			dialog.ShowError(fmt.Errorf("failed to save config: %w", err), w)
			return
		}

		c = client.New(cfg, client.Options{
			ServerInsecure: serverInsecure.Checked,
			TargetInsecure: targetInsecure.Checked,
			ClientCert:     strings.TrimSpace(clientCertEntry.Text),
			ClientKey:      strings.TrimSpace(clientKeyEntry.Text),
			Debug:          debugCheck.Checked,
			OnStatusChange: updateUI,
			OnLog:          onLog,
		})
		c.SetConfigPath(configPath)

		var ctx context.Context
		ctx, ctxCancel = context.WithCancel(context.Background())
		go func() {
			_ = c.Run(ctx)
		}()
	})

	form := widget.NewForm(
		widget.NewFormItem("Server", serverEntry),
		widget.NewFormItem("Target", targetEntry),
		widget.NewFormItem("Custom ID", customEntry),
		widget.NewFormItem("", container.NewHBox(serverInsecure, targetInsecure, debugCheck)),
		widget.NewFormItem("Client cert", clientCertEntry),
		widget.NewFormItem("Client key", clientKeyEntry),
	)

	top := container.NewVBox(
		form,
		container.NewHBox(widget.NewLabel("Status:"), statusLabel, widget.NewLabel("TunnelID:"), tunIDLabel, copyBtn),
	)

	w.SetContent(container.NewBorder(
		top,
		container.NewVBox(widget.NewSeparator(), connectBtn),
		nil, nil,
		container.NewBorder(widget.NewLabel("Logs:"), nil, nil, nil, logScroll),
	))

	w.SetOnClosed(func() {
		stopClient()
	})

	w.ShowAndRun()
}

// connectBtn is the shared Connect/Disconnect button, assigned inside main
// because updateUI references it before the button constructor runs.
var connectBtn *widget.Button
