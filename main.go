// GP2040 Remap — a beginner-friendly button remapper for GP2040-CE controllers.
//
// It serves a small local UI and forwards /api/* calls to the controller's
// config-mode web API (http://192.168.7.1 by default). Run with -demo to try
// it without a controller.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed ui
var uiFiles embed.FS

const appName = "GP2040 Remap"

// version is set at build time (-ldflags "-X main.version=1.2.3").
var version = "dev"

var lastPing atomic.Int64

var (
	flagDevice  = flag.String("device", "http://192.168.7.1", "address of the controller in config mode")
	flagDemo    = flag.Bool("demo", false, "use a pretend controller (no hardware needed)")
	flagBrowser = flag.Bool("browser", false, "open in your web browser instead of an app window")
	flagPort    = flag.Int("port", 0, "local port for the UI (0 = pick a free one)")
)

func main() {
	flag.Parse()
	// Double-clicking "GP2040 Remap (demo).exe" also starts demo mode.
	if strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "demo") {
		*flagDemo = true
	}

	mux := http.NewServeMux()

	sub, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("/", noCache(http.FileServer(http.FS(sub))))

	if *flagDemo {
		m := newMock()
		mux.Handle("/api/", m)
		mux.HandleFunc("/mock/press", m.press)
	} else {
		target, err := url.Parse(*flagDevice)
		if err != nil {
			log.Fatalf("bad -device address: %v", err)
		}
		mux.Handle("/api/", deviceProxy(target))
	}

	mux.HandleFunc("/app/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"version": version, "demo": *flagDemo, "device": *flagDevice, "backupDir": backupDir()})
	})
	mux.HandleFunc("/app/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().Unix())
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/app/layouts", storeHandler("layouts.json"))
	mux.HandleFunc("/app/pads", storeHandler("pads.json"))
	mux.HandleFunc("/app/export", exportPad)
	mux.HandleFunc("/app/backups", listBackups)
	mux.HandleFunc("/app/backup", saveOrGetBackup)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *flagPort))
	if err != nil {
		log.Fatal(err)
	}
	addr := "http://" + ln.Addr().String() + "/"
	log.Printf("%s running at %s", appName, addr)
	go func() { log.Fatal(http.Serve(ln, mux)) }()

	var closed <-chan struct{}
	if !*flagBrowser {
		closed = openWindow(addr)
	}
	if closed == nil {
		openBrowser(addr)
	}
	waitUntilClosed(closed)
}

// waitUntilClosed returns once the app window has been closed. The window's
// process ending is the main signal; the page also pings every 3 s, which
// covers a normal browser tab (or a window handed to an already-open browser).
func waitUntilClosed(closed <-chan struct{}) {
	start := time.Now()
	for {
		select {
		case <-closed:
			if time.Since(start) > 8*time.Second {
				return
			}
			closed = nil // ended at once: the window went to another browser process, so rely on pings
		case <-time.After(3 * time.Second):
		}
		if lp := lastPing.Load(); lp > 0 && time.Now().Unix()-lp > 30 {
			return
		}
	}
}

// deviceProxy forwards UI calls to the controller. getHeldPins can block for
// ~5 seconds on the device while it waits for a press, so allow for that.
func deviceProxy(target *url.URL) http.Handler {
	p := httputil.NewSingleHostReverseProxy(target)
	p.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		ResponseHeaderTimeout: 9 * time.Second,
		DisableKeepAlives:     true, // the controller's tiny web server prefers fresh connections
	}
	orig := p.Director
	p.Director = func(r *http.Request) {
		orig(r)
		r.Host = target.Host
		r.Header.Del("Origin")
		r.Header.Del("Referer")
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"offline": true, "error": err.Error()})
	}
	return p
}

// ---- Backups -------------------------------------------------------------

func backupDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	docs := filepath.Join(home, "Documents")
	if _, err := os.Stat(docs); err != nil {
		docs = home
	}
	return filepath.Join(docs, "GP2040 Remap backups")
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9 _.\-()]+\.json$`)

func listBackups(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(backupDir())
	type item struct {
		Name string    `json:"name"`
		Time time.Time `json:"time"`
	}
	out := []item{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, item{e.Name(), info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	writeJSON(w, out)
}

func saveOrGetBackup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !json.Valid(body) {
			http.Error(w, "backup must be JSON", http.StatusBadRequest)
			return
		}
		label := regexp.MustCompile(`[^A-Za-z0-9 _\-]`).ReplaceAllString(r.URL.Query().Get("label"), "")
		if label == "" {
			label = "backup"
		}
		name := fmt.Sprintf("%s %s.json", time.Now().Format("2006-01-02 1504-05"), label)
		if err := os.MkdirAll(backupDir(), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(filepath.Join(backupDir(), name), body, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"name": name})
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		if !safeName.MatchString(name) {
			http.Error(w, "bad name", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filepath.Join(backupDir(), name))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- picture layouts (where each button sits on the drawing) -------------

func storeFile(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = backupDir()
	}
	return filepath.Join(dir, "GP2040 Remap", name)
}

// storeHandler keeps one JSON file in the user's settings folder.
// Before overwriting, the previous version is kept as <name>.bak.
func storeHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file := storeFile(name)
		switch r.Method {
		case http.MethodGet:
			data, err := os.ReadFile(file)
			if err != nil || !json.Valid(data) {
				data = []byte("null")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(data)
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil || !json.Valid(body) {
				http.Error(w, "must be JSON", http.StatusBadRequest)
				return
			}
			os.MkdirAll(filepath.Dir(file), 0o755)
			if old, err := os.ReadFile(file); err == nil {
				os.WriteFile(file+".bak", old, 0o644)
			}
			tmp := file + ".tmp"
			if err := os.WriteFile(tmp, body, 0o644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.Rename(tmp, file); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// ---- sharing pads ----------------------------------------------------------

func exportDir() string { return filepath.Join(filepath.Dir(backupDir()), "GP2040 Remap pads") }

// exportPad writes a .gp2040pad file the user can send to a friend, then
// shows it in Explorer.
func exportPad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !json.Valid(body) {
		http.Error(w, "must be JSON", http.StatusBadRequest)
		return
	}
	base := strings.TrimSpace(regexp.MustCompile(`[^A-Za-z0-9 _\-()]`).ReplaceAllString(r.URL.Query().Get("name"), ""))
	if base == "" {
		base = "My pad"
	}
	if err := os.MkdirAll(exportDir(), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path := filepath.Join(exportDir(), base+".gp2040pad")
	for i := 2; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		path = filepath.Join(exportDir(), fmt.Sprintf("%s (%d).gp2040pad", base, i))
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	showInFolder(path)
	writeJSON(w, map[string]string{"path": path, "file": filepath.Base(path)})
}

// ---- helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}
