//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const linuxGUIHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>MCP Remote Access - User Client</title>
<style>
  body { font-family: system-ui, sans-serif; background: #f4f5f7; margin: 0; padding: 24px; color: #1f2937; }
  .card { max-width: 400px; margin: 0 auto; background: #fff; padding: 20px; border-radius: 8px; box-shadow: 0 2px 8px rgba(0,0,0,0.08); }
  h3 { margin-top: 0; margin-bottom: 16px; }
  label { display: block; font-size: 13px; margin-bottom: 4px; font-weight: 600; }
  input { width: 100%; box-sizing: border-box; padding: 8px; margin-bottom: 12px; border: 1px solid #d1d5db; border-radius: 4px; }
  button { background: #2563eb; color: #fff; border: none; padding: 9px 16px; border-radius: 4px; cursor: pointer; font-weight: 600; }
  button:hover { background: #1d4ed8; }
  .status { margin-top: 16px; padding: 10px; background: #f3f4f6; border-radius: 4px; font-size: 13px; word-break: break-all; }
</style>
</head>
<body>
<div class="card">
  <h3>MCP Remote Access Client</h3>
  <label>Connection Key</label>
  <input id="key" type="text" />
  <button onclick="connectNow()">Connect / Save</button>
  <div class="status" id="status">Status: Loading...</div>
</div>
<script>
async function loadState() {
  const r = await fetch('/api/state');
  const d = await r.json();
  if (!document.getElementById('key').value) document.getElementById('key').value = d.key || '';
  document.getElementById('status').innerText = 'Status: ' + d.status;
}
async function connectNow() {
  const key = document.getElementById('key').value;
  await fetch('/api/connect', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({key})
  });
  loadState();
}
setInterval(loadState, 1000);
loadState();
</script>
</body>
</html>`

func RunGUI(opts ClientOptions) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		if strings.TrimSpace(opts.UserKey) == "" {
			return fmt.Errorf("no GUI display detected and no --key provided; run with --cli --key <key>")
		}
		client := NewClient(opts)
		return client.Run(context.Background())
	}

	var mu sync.Mutex
	curKey := opts.UserKey
	curStatus := "Idle - Enter Connection Key and click Connect."
	var cancelFn context.CancelFunc

	startAgent := func(key string) {
		mu.Lock()
		if cancelFn != nil {
			cancelFn()
			cancelFn = nil
		}
		curKey = strings.TrimSpace(key)
		if curKey == "" {
			curStatus = "Please provide Connection Key."
			mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelFn = cancel
		mu.Unlock()

		pCfg := LoadPersistentConfig()
		pCfg.UserKey = curKey
		if opts.MachineID != "" {
			pCfg.MachineID = opts.MachineID
		}
		_ = SavePersistentConfig(pCfg)

		runOpts := opts
		runOpts.UserKey = curKey
		if runOpts.MachineID == "" {
			runOpts.MachineID = pCfg.MachineID
		}
		runOpts.OnStatus = func(st string) {
			mu.Lock()
			curStatus = st
			mu.Unlock()
		}
		client := NewClient(runOpts)
		go func() {
			_ = client.Run(ctx)
		}()
	}

	if strings.TrimSpace(curKey) != "" {
		startAgent(curKey)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	uiURL := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Fprintf(os.Stderr, "[user-client] GUI control panel available at %s\n", uiURL)

	go openLinuxWindow(uiURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(linuxGUIHTML))
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key":    curKey,
			"status": curStatus,
		})
	})
	mux.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		startAgent(body.Key)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	return http.Serve(ln, mux)
}

func openLinuxWindow(url string) {
	for _, bin := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(bin); err == nil {
			_ = exec.Command(p, "--app="+url).Start()
			return
		}
	}
	if p, err := exec.LookPath("xdg-open"); err == nil {
		_ = exec.Command(p, url).Start()
	}
}

func EnsureCLIConsole() {}

