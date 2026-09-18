// Command selftunnel-server-gui runs the relay server behind a Fyne
// desktop UI for non-technical operators (spec §3.8): server lifecycle
// with graceful stop, optional ngrok exposure, one-click client-config
// generation, live client-session management and a filterable log view.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mgwn/selftunnel/internal/client"
	"github.com/mgwn/selftunnel/internal/ngrok"
	"github.com/mgwn/selftunnel/internal/server"
)

// settingsPath is the operator-settings file next to the binary; the
// ngrok authtoken is stored here (never logged).
const settingsPath = "selftunnel-server-gui.json"

// settings holds every persisted operator preference (spec §3.8.2).
type settings struct {
	ListenAddr  string `json:"listenAddr"`
	DataDir     string `json:"dataDir"`
	CertFile    string `json:"certFile"`
	KeyFile     string `json:"keyFile"`
	NgrokToken  string `json:"ngrokToken"`
	NgrokBinary string `json:"ngrokBinary"`
}

func defaultSettings() settings {
	return settings{
		ListenAddr: "127.0.0.1:8080",
		DataDir:    "./data",
	}
}

func loadSettings() settings {
	s := defaultSettings()
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if s.ListenAddr == "" {
		s.ListenAddr = "127.0.0.1:8080"
	}
	if s.DataDir == "" {
		s.DataDir = "./data"
	}
	return s
}

func saveSettings(s settings) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(settingsPath, data, 0600)
}

// --- log capture -----------------------------------------------------------

// logRing is a bounded, thread-safe buffer of formatted log lines. It is
// installed as the standard log output, which the default slog handler
// writes through — capturing every server log line for the GUI pane
// (spec §3.8.5).
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogRing(max int) *logRing { return &logRing{max: max} }

// Write splits p into lines and appends them, dropping the oldest.
func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		r.lines = append(r.lines, line)
		if len(r.lines) > r.max {
			r.lines = r.lines[len(r.lines)-r.max:]
		}
	}
	return len(p), nil
}

// filtered returns the lines matching the level token and text filter.
func (r *logRing) filtered(level, contains string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.lines))
	for _, line := range r.lines {
		if level != "All" && !strings.Contains(line, " "+level+" ") {
			continue
		}
		if contains != "" && !strings.Contains(line, contains) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// --- session table model ---------------------------------------------------

// sessionRow is one display row of the session table (spec §3.8.4).
type sessionRow struct {
	id, remote, since, target, status string
	reqs                              uint64
}

func (r sessionRow) cell(col int) string {
	switch col {
	case 0:
		return r.id
	case 1:
		return r.remote
	case 2:
		return r.since
	case 3:
		return r.target
	case 4:
		return fmt.Sprintf("%d", r.reqs)
	default:
		return r.status
	}
}

var tableHeaders = []string{"Tunnel ID", "Remote", "Since", "Target", "Reqs", "Status"}

// --- application state -----------------------------------------------------

type guiState struct {
	mu  sync.Mutex
	app *server.App
	reg *server.Registry
	ng  *ngrok.Manager

	rows      []sessionRow
	selected  int
	publicURL string
}

func (g *guiState) running() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.app != nil && g.app.Running()
}

// refreshSessions rebuilds the table model from the registry.
func (g *guiState) refreshSessions() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reg == nil {
		g.rows = nil
		return
	}
	rows := make([]sessionRow, 0, 16)
	for _, t := range g.reg.List() {
		row := sessionRow{id: t.ID, target: t.Target(), reqs: t.RequestCount(), status: "offline"}
		if sess := t.Session(); sess != nil {
			row.status = "online"
			row.remote = sess.RemoteAddr()
			row.since = sess.StartedAt().Format("15:04:05")
		}
		rows = append(rows, row)
	}
	g.rows = rows
	if g.selected >= len(rows) {
		g.selected = -1
	}
}

// clientServerURL derives the client's server URL from the current
// exposure (spec §3.8.3): the ngrok public URL when active, otherwise the
// listen address with ws/wss according to TLS.
func (g *guiState) clientServerURL(listenAddr string, useTLS bool) string {
	g.mu.Lock()
	public := g.publicURL
	g.mu.Unlock()
	if public != "" {
		return client.NormalizeServer(public)
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8080"
	}
	return client.NormalizeServer(scheme + "://" + listenAddr)
}

// entryTemplate returns the copyable public entry-point template.
func (g *guiState) entryTemplate(listenAddr string, useTLS bool) string {
	g.mu.Lock()
	public := g.publicURL
	g.mu.Unlock()
	base := public
	if base == "" {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		base = scheme + "://" + listenAddr
	}
	return strings.TrimSuffix(base, "/") + "/t/{tunnelID}"
}

// --- main ------------------------------------------------------------------

func main() {
	a := app.New()
	win := a.NewWindow("selftunnel server")
	win.Resize(fyne.NewSize(900, 600))

	set := loadSettings()
	ring := newLogRing(1000)
	log.SetOutput(ring)

	state := &guiState{selected: -1}
	state.ng = &ngrok.Manager{}

	// --- settings form widgets ---

	addrEntry := widget.NewEntry()
	addrEntry.SetText(set.ListenAddr)
	addrEntry.Validator = func(s string) error {
		if _, _, err := net.SplitHostPort(s); err != nil {
			return fmt.Errorf("host:port expected")
		}
		return nil
	}

	dataEntry := widget.NewEntry()
	dataEntry.SetText(set.DataDir)

	certEntry := widget.NewEntry()
	certEntry.SetText(set.CertFile)
	certEntry.SetPlaceHolder("TLS certificate PEM (optional)")

	keyEntry := widget.NewEntry()
	keyEntry.SetText(set.KeyFile)
	keyEntry.SetPlaceHolder("TLS private key PEM (optional)")

	debugCheck := widget.NewCheck("Debug mode (log every request)", nil)

	// ngrok inputs are declared early: the lifecycle handlers persist them
	// with the settings (spec §3.8.2).
	tokenEntry := widget.NewPasswordEntry()
	tokenEntry.SetText(set.NgrokToken)
	tokenEntry.SetPlaceHolder("ngrok authtoken (from https://dashboard.ngrok.com)")

	binaryEntry := widget.NewEntry()
	binaryEntry.SetText(set.NgrokBinary)
	binaryEntry.SetPlaceHolder("ngrok binary path (empty = auto-download)")

	// --- lifecycle ---

	statusLabel := widget.NewLabel("Stopped")
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}
	startBtn := widget.NewButton("Start", nil)
	stopBtn := widget.NewButton("Stop", nil)
	stopBtn.Disable()

	useTLS := func() bool { return certEntry.Text != "" && keyEntry.Text != "" }

	startServer := func() {
		addr := strings.TrimSpace(addrEntry.Text)
		if _, _, err := net.SplitHostPort(addr); err != nil {
			dialog.ShowError(fmt.Errorf("listen address must be host:port, e.g. 127.0.0.1:8080"), win)
			return
		}
		dataDir := strings.TrimSpace(dataEntry.Text)
		if dataDir == "" {
			dataDir = "./data"
		}
		if debugCheck.Checked {
			slog.SetLogLoggerLevel(slog.LevelDebug)
		} else {
			slog.SetLogLoggerLevel(slog.LevelInfo)
		}
		set = settings{
			ListenAddr: addr, DataDir: dataDir,
			CertFile: strings.TrimSpace(certEntry.Text), KeyFile: strings.TrimSpace(keyEntry.Text),
			NgrokToken: strings.TrimSpace(tokenEntry.Text), NgrokBinary: strings.TrimSpace(binaryEntry.Text),
		}
		saveSettings(set)

		if err := os.MkdirAll(dataDir, 0755); err != nil {
			dialog.ShowError(fmt.Errorf("cannot create data directory: %w", err), win)
			return
		}
		reg := server.NewRegistry(dataDir)
		if err := reg.Load(); err != nil {
			dialog.ShowError(fmt.Errorf("cannot load registry: %w", err), win)
			return
		}
		srv := server.New(reg, addr, []string{"native-client://"}, debugCheck.Checked)
		app_ := server.NewApp(srv, addr, set.CertFile, set.KeyFile)
		if err := app_.Start(); err != nil {
			dialog.ShowError(fmt.Errorf("cannot start server: %w", err), win)
			return
		}

		state.mu.Lock()
		state.app, state.reg = app_, reg
		state.mu.Unlock()

		slog.Info("server started", "addr", app_.Addr(), "data", dataDir, "tls", useTLS())
		fyne.Do(func() {
			statusLabel.SetText("Running on " + app_.Addr())
			startBtn.Disable()
			stopBtn.Enable()
		})
	}

	stopServer := func() {
		state.mu.Lock()
		app_ := state.app
		state.mu.Unlock()
		if app_ == nil || !app_.Running() {
			return
		}
		state.ng.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app_.Stop(ctx); err != nil {
			slog.Warn("server shutdown error", "err", err)
		}
		slog.Info("server stopped")
		state.mu.Lock()
		state.publicURL = ""
		state.app, state.reg = nil, nil
		state.mu.Unlock()
		fyne.Do(func() {
			statusLabel.SetText("Stopped")
			startBtn.Enable()
			stopBtn.Disable()
		})
	}

	startBtn.OnTapped = func() { go startServer() }
	stopBtn.OnTapped = func() { go stopServer() }

	// --- ngrok exposure (spec §3.8.2) ---

	exposeBtn := widget.NewButton("Expose via ngrok", nil)
	stopExposeBtn := widget.NewButton("Stop ngrok", nil)
	stopExposeBtn.Disable()
	publicLabel := widget.NewLabel("")
	publicLabel.Truncation = fyne.TextTruncateClip

	copyPublicBtn := widget.NewButton("Copy entry URL", func() {
		a.Clipboard().SetContent(state.entryTemplate(strings.TrimSpace(addrEntry.Text), useTLS()))
	})

	expose := func() {
		token := strings.TrimSpace(tokenEntry.Text)
		if token == "" {
			dialog.ShowError(fmt.Errorf("an ngrok authtoken is required (create one at dashboard.ngrok.com)"), win)
			return
		}
		if !state.running() {
			dialog.ShowError(fmt.Errorf("start the server first"), win)
			return
		}
		state.mu.Lock()
		app_ := state.app
		state.mu.Unlock()

		binary := strings.TrimSpace(binaryEntry.Text)
		exposeBtn.Disable()
		publicLabel.SetText("starting ngrok…")
		go func() {
			var err error
			if binary == "" {
				cacheDir := filepath.Join(os.TempDir(), "selftunnel-ngrok")
				if ucd, uerr := os.UserCacheDir(); uerr == nil {
					cacheDir = filepath.Join(ucd, "selftunnel", "ngrok")
				}
				binary, err = ngrok.EnsureBinary(cacheDir)
				if err != nil {
					fyne.Do(func() {
						dialog.ShowError(err, win)
						exposeBtn.Enable()
						publicLabel.SetText("")
					})
					return
				}
			}
			url, err := state.ng.Start(context.Background(), binary, token, portOf(app_.Addr()))
			fyne.Do(func() {
				exposeBtn.Enable()
				if err != nil {
					dialog.ShowError(err, win)
					publicLabel.SetText("")
					return
				}
				state.mu.Lock()
				state.publicURL = url
				state.mu.Unlock()
				publicLabel.SetText(url)
				stopExposeBtn.Enable()
				slog.Info("ngrok exposure active", "public", url)
			})
		}()
	}
	exposeBtn.OnTapped = expose
	stopExposeBtn.OnTapped = func() {
		state.ng.Stop()
		state.mu.Lock()
		state.publicURL = ""
		state.mu.Unlock()
		publicLabel.SetText("")
		stopExposeBtn.Disable()
		slog.Info("ngrok exposure stopped")
	}

	// --- session table (spec §3.8.4) ---

	table := widget.NewTable(
		func() (int, int) {
			state.mu.Lock()
			defer state.mu.Unlock()
			return len(state.rows), len(tableHeaders)
		},
		func() fyne.CanvasObject { return widget.NewLabel("template") },
		func(id widget.TableCellID, o fyne.CanvasObject) {
			state.mu.Lock()
			defer state.mu.Unlock()
			label := o.(*widget.Label)
			if id.Row < len(state.rows) && id.Col < len(tableHeaders) {
				label.SetText(state.rows[id.Row].cell(id.Col))
			} else {
				label.SetText("")
			}
		},
	)
	table.OnSelected = func(id widget.TableCellID) {
		state.mu.Lock()
		state.selected = id.Row
		state.mu.Unlock()
	}
	for i, w := range []float32{130, 150, 90, 220, 80, 80} {
		table.SetColumnWidth(i, w)
	}
	headerRow := container.NewHBox()
	for _, h := range tableHeaders {
		l := widget.NewLabel(h)
		l.TextStyle = fyne.TextStyle{Bold: true}
		headerRow.Add(l)
	}

	disconnectBtn := widget.NewButton("Disconnect selected", func() {
		state.mu.Lock()
		idx, reg := state.selected, state.reg
		state.mu.Unlock()
		if reg == nil || idx < 0 {
			return
		}
		state.mu.Lock()
		rows := state.rows
		state.mu.Unlock()
		if idx >= len(rows) {
			return
		}
		if tun := reg.Get(rows[idx].id); tun != nil {
			if sess := tun.Session(); sess != nil {
				sess.Close()
				slog.Info("session disconnected by operator", "tunnel", rows[idx].id)
			}
		}
		table.UnselectAll()
	})

	generateBtn := widget.NewButton("Generate client config…", func() {
		if !state.running() {
			dialog.ShowError(fmt.Errorf("start the server first"), win)
			return
		}
		url := state.clientServerURL(strings.TrimSpace(addrEntry.Text), useTLS())
		dialog.ShowFileSave(func(wc fyne.URIWriteCloser, err error) {
			if err != nil || wc == nil {
				return
			}
			path := wc.URI().Path()
			wc.Close()
			if serr := client.SaveConfig(path, &client.Config{Server: url}); serr != nil {
				dialog.ShowError(fmt.Errorf("failed to save config: %w", serr), win)
				return
			}
			slog.Info("client config generated", "path", path, "server", url)
			dialog.ShowInformation("Client config saved",
				"Copy the file to the client machine, fill in the target address and start selftunnel-client.", win)
		}, win)
	})

	// --- log view (spec §3.8.5) ---

	logEntry := widget.NewMultiLineEntry()
	logEntry.Disable()
	logEntry.Wrapping = fyne.TextWrapOff

	levelSelect := widget.NewSelect([]string{"All", "INFO", "WARN", "DEBUG"}, nil)
	levelSelect.SetSelected("All")
	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("filter text")

	refreshLogs := func() {
		lines := ring.filtered(levelSelect.Selected, filterEntry.Text)
		const display = 500
		if len(lines) > display {
			lines = lines[len(lines)-display:]
		}
		fyne.Do(func() { logEntry.SetText(strings.Join(lines, "\n")) })
	}

	// --- periodic refresh ---

	go func() {
		tick := time.NewTicker(1 * time.Second)
		stats := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		defer stats.Stop()
		for range tick.C {
			state.refreshSessions()
			fyne.Do(func() { table.Refresh() })
			refreshLogs()
			select {
			case <-stats.C:
				state.mu.Lock()
				app_ := state.app
				state.mu.Unlock()
				if app_ != nil && app_.Running() {
					app_.Srv().PollStats()
				}
			default:
			}
		}
	}()

	win.SetOnClosed(func() {
		state.ng.Stop()
		stopServer()
	})

	form := widget.NewForm(
		widget.NewFormItem("Listen", addrEntry),
		widget.NewFormItem("Data dir", dataEntry),
		widget.NewFormItem("TLS cert", certEntry),
		widget.NewFormItem("TLS key", keyEntry),
		widget.NewFormItem("", debugCheck),
		widget.NewFormItem("", container.NewHBox(startBtn, stopBtn, statusLabel)),
		widget.NewFormItem("ngrok token", tokenEntry),
		widget.NewFormItem("ngrok binary", binaryEntry),
		widget.NewFormItem("", container.NewHBox(exposeBtn, stopExposeBtn, copyPublicBtn)),
		widget.NewFormItem("Public URL", publicLabel),
	)

	top := container.NewVBox(
		form,
		widget.NewSeparator(),
		headerRow,
	)
	sessions := container.NewBorder(nil, container.NewHBox(disconnectBtn, generateBtn), nil, nil, table)
	logFilter := container.NewHBox(widget.NewLabel("Logs:"), levelSelect, filterEntry)
	logArea := container.NewBorder(logFilter, nil, nil, nil, logEntry)

	win.SetContent(container.NewBorder(top, container.NewVBox(widget.NewSeparator(), logArea), nil, nil, sessions))
	win.ShowAndRun()
}

// portOf extracts the port from a "host:port" address.
func portOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 8080
	}
	n := 0
	for _, c := range port {
		if c < '0' || c > '9' {
			return 8080
		}
		n = n*10 + int(c-'0')
	}
	return n
}
