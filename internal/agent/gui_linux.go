//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
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
  input[type="text"] { width: 100%; box-sizing: border-box; padding: 8px; margin-bottom: 12px; border: 1px solid #d1d5db; border-radius: 4px; }
  button { background: #2563eb; color: #fff; border: none; padding: 9px 16px; border-radius: 4px; cursor: pointer; font-weight: 600; }
  button:hover { background: #1d4ed8; }
  .btn-exit { background: #dc2626; }
  .btn-exit:hover { background: #b91c1c; }
  .status { margin-top: 16px; padding: 10px; background: #f3f4f6; border-radius: 4px; font-size: 13px; word-break: break-all; }
</style>
</head>
<body>
<div class="card">
  <h3>MCP Remote Access Client</h3>
  <label for="key">Connection Key</label>
  <input id="key" type="text" />
  <div style="margin-bottom: 14px;">
    <label style="display: inline-flex; align-items: center; font-size: 13px; font-weight: normal; cursor: pointer; user-select: none;">
      <input id="saveFile" type="checkbox" style="width: auto; margin: 0 8px 0 0; cursor: pointer;" />
      Save File
    </label>
  </div>
  <div style="display: flex; gap: 10px;">
    <button onclick="connectNow()" style="flex: 1;">Connect</button>
    <button class="btn-exit" onclick="exitNow()">Exit</button>
  </div>
  <div class="status" id="status">Status: Loading...</div>
</div>
<script>
let userChangedSaveFile = false;
let isExiting = false;

document.getElementById('saveFile').addEventListener('change', function() {
  userChangedSaveFile = true;
});

async function loadState() {
  if (isExiting) return;
  try {
    const r = await fetch('/api/state');
    const d = await r.json();
    if (!document.getElementById('key').value && d.key) {
      document.getElementById('key').value = d.key;
    }
    if (!userChangedSaveFile && typeof d.saveFile === 'boolean') {
      document.getElementById('saveFile').checked = d.saveFile;
    }
    document.getElementById('status').innerText = 'Status: ' + d.status;
  } catch(e) {
    if (!isExiting) {
      document.getElementById('status').innerText = 'Status: Client stopped or disconnected';
    }
  }
}

async function sendHeartbeat() {
  if (isExiting) return;
  try {
    await fetch('/api/heartbeat', { method: 'POST' });
  } catch(e) {}
}

async function connectNow() {
  if (isExiting) return;
  const key = document.getElementById('key').value;
  const saveFile = document.getElementById('saveFile').checked;
  await fetch('/api/connect', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({key, saveFile})
  });
  loadState();
}

async function exitNow() {
  if (isExiting) return;
  if (!confirm("Are you sure you want to disconnect and exit?")) return;
  isExiting = true;
  document.getElementById('status').innerText = 'Status: Exiting...';
  try {
    await fetch('/api/exit', { method: 'POST' });
  } catch(e) {}
  setTimeout(() => {
    window.close();
    document.body.innerHTML = '<div class="card" style="text-align: center;"><h3>Client Exited</h3><p style="color:#6b7280; font-size:14px;">The daemon process has been stopped. You can close this window now.</p></div>';
  }, 200);
}

window.addEventListener('pagehide', () => {
  if (isExiting) return;
  if (navigator.sendBeacon) {
    navigator.sendBeacon('/api/exit');
  } else {
    fetch('/api/exit', { method: 'POST', keepalive: true });
  }
});

setInterval(sendHeartbeat, 1000);
setInterval(loadState, 1000);
sendHeartbeat();
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
	var sharedClient *Client

	startAgent := func(key string, saveFile bool) {
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

		pCfg := LoadPersistentConfig()
		if saveFile {
			pCfg.UserKey = curKey
			if opts.MachineID != "" {
				pCfg.MachineID = opts.MachineID
			}
			_ = SavePersistentConfig(pCfg)
		} else {
			_ = os.Remove(ConfigFilePath())
		}

		if sharedClient == nil {
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
			sharedClient = NewClient(runOpts)
		} else {
			sharedClient.SetUserKey(curKey)
		}
		client := sharedClient
		mu.Unlock()

		go func() {
			_ = client.Run(ctx)
		}()
	}

	if strings.TrimSpace(curKey) != "" {
		startAgent(curKey, false)
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
	srv := &http.Server{Handler: mux}

	// 架构妥协与生命周期管理说明：
	// 这是不得已的妥协办法：
	// 在 Linux 上为了严格坚持纯 Go (CGO_ENABLED=0) 跨平台编译，避免引入庞大的 C 编译工具链与复杂的 GTK/Qt CGo 动态库依赖，
	// 采用了“本地回环 HTTP + 浏览器打开控制面板”提供图形界面的方案。
	// 但外部浏览器是独立的外部进程，关闭标签页或窗口时，操作系统并不会向 Go 守护进程发送销毁信号。
	// 如果不加任何处理，会导致用户叉掉网页后，底层的 Go 进程、WebSocket 连接以及 systemd-inhibit 唤醒锁依然在后台常驻（沦为幽灵孤儿进程）。
	//
	// 为了让用户有一个确切的“叉掉即退出”的实体交互体验，这里通过组合手段解决：
	// 1. 网页前端每秒向 /api/heartbeat 发送心跳；
	// 2. 页面卸载（叉掉窗口/标签页）时通过 pagehide + navigator.sendBeacon 极速发送 /api/exit；
	// 3. 后端心跳看门狗（Watchdog）在启动时给予 25 秒首屏宽限期；一旦建立心跳后若连续 4 秒未收到心跳，判定网页已关闭并自动优雅退出；
	// 4. 网页界面上提供醒目的红色 Exit 退出按钮供主动安全退出。
	var exitOnce sync.Once
	exitDaemon := func(reason string) {
		exitOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "[user-client] Exiting GUI (%s)...\n", reason)
			mu.Lock()
			if cancelFn != nil {
				cancelFn()
				cancelFn = nil
			}
			if sharedClient != nil {
				sharedClient.Close()
				sharedClient = nil
			}
			mu.Unlock()
			go func() {
				time.Sleep(100 * time.Millisecond)
				_ = srv.Close()
			}()
		})
	}

	var lastHeartbeat time.Time
	var receivedFirstHeartbeat bool

	touchHeartbeat := func() {
		mu.Lock()
		lastHeartbeat = time.Now()
		receivedFirstHeartbeat = true
		mu.Unlock()
	}

	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		touchHeartbeat()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/exit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		exitDaemon("exit requested from web GUI")
	})

	// 心跳看门狗监控协程
	go func() {
		startTime := time.Now()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			mu.Lock()
			seen := receivedFirstHeartbeat
			lh := lastHeartbeat
			mu.Unlock()

			if seen {
				// 已经收到过心跳，如果连续 4 秒未收到新心跳（页面已被叉掉或浏览器退出），自动退出守护进程
				if time.Since(lh) > 4*time.Second {
					exitDaemon("heartbeat timeout (web page closed)")
					return
				}
			} else {
				// 启动后给 25 秒宽限期等待浏览器完成拉起并加载首屏
				if time.Since(startTime) > 25*time.Second {
					exitDaemon("initial web page failed to connect within 25s")
					return
				}
			}
		}
	}()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(linuxGUIHTML))
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		touchHeartbeat()
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"key":      curKey,
			"status":   curStatus,
			"saveFile": false,
		})
	})
	mux.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		touchHeartbeat()
		var body struct {
			Key      string `json:"key"`
			SaveFile *bool  `json:"saveFile"`
			SaveKey  *bool  `json:"saveKey"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		saveFile := false
		if body.SaveFile != nil {
			saveFile = *body.SaveFile
		} else if body.SaveKey != nil {
			saveFile = *body.SaveKey
		}
		startAgent(body.Key, saveFile)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	err = srv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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

