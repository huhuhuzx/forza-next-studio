package v4bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	InternalUDPPort  = 15300
	InternalHTTPPort = 18300
)

type PresetMeta struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Author     string   `json:"author"`
	Category   string   `json:"category"`
	Tags       []string `json:"tags"`
	Summary    string   `json:"summary"`
	File       string   `json:"file"`
	ParamCount int      `json:"paramCount"`
}

type Status struct {
	Running        bool   `json:"running"`
	Ready          bool   `json:"ready"`
	ActivePresetID string `json:"activePresetId"`
	LastError      string `json:"lastError,omitempty"`
	HTTPBase       string `json:"httpBase"`
	UDPPort        int    `json:"udpPort"`
}

// Manager supervises the untouched official v4 ForzaTelemetryHarness.exe for Pro Mode.
type Manager struct {
	mu             sync.Mutex
	v4Dir          string
	presetsFS      fs.FS
	ensureV4       func(string) error
	presets        []PresetMeta
	activePresetID string
	cmd            *exec.Cmd
	running        bool
	ready          bool
	lastError      string
	udpConn        *net.UDPConn
	httpClient     *http.Client
	streamClient   *http.Client
	proxy          *httputil.ReverseProxy

	// Cached live V4 SSE state so Pro Mode devices & generation are ALWAYS immediately available
	watchCancel   context.CancelFunc
	lastHello     json.RawMessage
	lastStatus    json.RawMessage
	lastDeviceGen int64
}

func NewManager(v4Dir string, presetsFS fs.FS, ensureV4 func(string) error) *Manager {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", InternalHTTPPort))
	directTransport := &http.Transport{
		Proxy: nil, // Never route 127.0.0.1 loopback through system HTTP_PROXY
		DialContext: (&net.Dialer{
			Timeout:   800 * time.Millisecond,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = directTransport
	rp.FlushInterval = -1 // CRITICAL: flush SSE events immediately with zero buffering!
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host // CRITICAL: V4 Harness enforces Host == 127.0.0.1:18300 (otherwise 403!)
		req.Header.Del("Origin")
		req.Header.Del("Referer")
	}

	m := &Manager{
		v4Dir:          v4Dir,
		presetsFS:      presetsFS,
		ensureV4:       ensureV4,
		presets:        make([]PresetMeta, 0),
		activePresetID: "v4_official",
		httpClient:     &http.Client{Timeout: 2500 * time.Millisecond, Transport: directTransport},
		streamClient:   &http.Client{Timeout: 0, Transport: directTransport},
		proxy:          rp,
	}
	m.loadPresets()
	return m
}

func (m *Manager) RuntimeConfigPath() string {
	return filepath.Join(m.v4Dir, "config", "runtime_config.json")
}

func (m *Manager) loadPresets() {
	if m.presetsFS == nil {
		return
	}
	b, err := fs.ReadFile(m.presetsFS, "manifest.json")
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &m.presets)
}

func (m *Manager) Presets() []PresetMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PresetMeta, len(m.presets))
	copy(out, m.presets)
	return out
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Running:        m.running,
		Ready:          m.ready,
		ActivePresetID: m.activePresetID,
		LastError:      m.lastError,
		HTTPBase:       fmt.Sprintf("http://127.0.0.1:%d", InternalHTTPPort),
		UDPPort:        InternalUDPPort,
	}
}

// CachedHello returns the latest V4 hello snapshot (with devices & deviceGeneration) + status.
func (m *Manager) CachedHello() (json.RawMessage, json.RawMessage, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastHello, m.lastStatus, m.lastDeviceGen
}

func (m *Manager) Start() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}

	if m.ensureV4 != nil {
		if err := m.ensureV4(m.v4Dir); err != nil {
			m.lastError = fmt.Sprintf("解压内置 V4 运行时失败: %v", err)
			m.mu.Unlock()
			return errors.New(m.lastError)
		}
	}

	exePath := filepath.Join(m.v4Dir, "ForzaTelemetryHarness.exe")
	cfgPath := filepath.Join(m.v4Dir, "config", "runtime_config.json")
	webPath := filepath.Join(m.v4Dir, "monitor")
	if _, err := os.Stat(exePath); err != nil {
		m.lastError = "未找到 v4_runtime/ForzaTelemetryHarness.exe"
		m.mu.Unlock()
		return errors.New(m.lastError)
	}

	cmd := exec.Command(
		exePath,
		"--config", cfgPath,
		"--web-root", webPath,
		"--port", fmt.Sprintf("%d", InternalUDPPort),
		"--monitor-port", fmt.Sprintf("%d", InternalHTTPPort),
		"--watch-config",
	)
	cmd.Dir = m.v4Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if err := cmd.Start(); err != nil {
		m.lastError = fmt.Sprintf("启动 V4 引擎失败: %v", err)
		m.mu.Unlock()
		return errors.New(m.lastError)
	}

	dstAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", InternalUDPPort))
	if err == nil {
		if uc, uerr := net.DialUDP("udp", nil, dstAddr); uerr == nil {
			m.udpConn = uc
		}
	}

	m.cmd = cmd
	m.running = true
	m.ready = false
	m.lastError = ""
	m.lastHello = nil
	m.lastStatus = nil
	m.lastDeviceGen = 0
	m.mu.Unlock()

	go func(c *exec.Cmd) {
		_ = c.Wait()
		m.mu.Lock()
		if m.cmd == c {
			m.running = false
			m.ready = false
			m.cmd = nil
			if m.watchCancel != nil {
				m.watchCancel()
				m.watchCancel = nil
			}
		}
		m.mu.Unlock()
	}(cmd)

	// Wait for V4 HTTP server to become ready
	time.Sleep(220 * time.Millisecond)
	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		stillRunning := m.running
		m.mu.Unlock()
		if !stillRunning {
			break
		}
		resp, err := m.httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/api/settings", InternalHTTPPort))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				m.mu.Lock()
				m.ready = true
				ctx, cancel := context.WithCancel(context.Background())
				m.watchCancel = cancel
				m.mu.Unlock()

				// Start background V4 SSE watcher so device list & generation are immediately cached
				go m.watchV4Events(ctx)
				// Wait briefly (up to 600ms) for first hello event to populate
				hDeadline := time.Now().Add(600 * time.Millisecond)
				for time.Now().Before(hDeadline) {
					m.mu.Lock()
					got := len(m.lastHello) > 0
					m.mu.Unlock()
					if got {
						break
					}
					time.Sleep(30 * time.Millisecond)
				}
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil
}

func (m *Manager) watchV4Events(ctx context.Context) {
	urlStr := fmt.Sprintf("http://127.0.0.1:%d/events", InternalHTTPPort)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return
		}
		resp, err := m.streamClient.Do(req)
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 128*1024), 512*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			raw := strings.TrimSpace(line[5:])
			var hdr struct {
				Type             string `json:"type"`
				DeviceGeneration int64  `json:"deviceGeneration"`
			}
			if json.Unmarshal([]byte(raw), &hdr) != nil {
				continue
			}
			if hdr.Type == "hello" {
				cp := make(json.RawMessage, len(raw))
				copy(cp, raw)
				m.mu.Lock()
				m.lastHello = cp
				if hdr.DeviceGeneration > 0 {
					m.lastDeviceGen = hdr.DeviceGeneration
				}
				m.mu.Unlock()
			} else if hdr.Type == "status" {
				cp := make(json.RawMessage, len(raw))
				copy(cp, raw)
				m.mu.Lock()
				m.lastStatus = cp
				m.mu.Unlock()
			}
		}
		_ = resp.Body.Close()
		time.Sleep(200 * time.Millisecond)
	}
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if m.watchCancel != nil {
		m.watchCancel()
		m.watchCancel = nil
	}
	if m.udpConn != nil {
		_ = m.udpConn.Close()
		m.udpConn = nil
	}
	cmd := m.cmd
	gen := m.lastDeviceGen
	m.cmd = nil
	m.running = false
	m.ready = false
	m.lastHello = nil
	m.lastStatus = nil
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		// Gracefully release V4's HID handle using the REAL deviceGeneration before terminating
		payload := fmt.Sprintf(`{"gen":%d,"id":-1}`, gen)
		_, _ = m.httpClient.Post(
			fmt.Sprintf("http://127.0.0.1:%d/api/device", InternalHTTPPort),
			"application/json",
			bytes.NewBufferString(payload),
		)
		time.Sleep(120 * time.Millisecond)
		_ = cmd.Process.Kill()
	}
}

func (m *Manager) ForwardUDP(raw []byte) {
	m.mu.Lock()
	uc := m.udpConn
	run := m.running
	m.mu.Unlock()
	if run && uc != nil {
		_, _ = uc.Write(raw)
	}
}

func (m *Manager) ApplyPreset(presetID string) (PresetMeta, error) {
	m.mu.Lock()
	var chosen *PresetMeta
	for i := range m.presets {
		if m.presets[i].ID == presetID {
			p := m.presets[i]
			chosen = &p
			break
		}
	}
	running := m.running
	m.mu.Unlock()

	if chosen == nil {
		return PresetMeta{}, fmt.Errorf("未找到预设: %s", presetID)
	}
	if m.ensureV4 != nil {
		if err := m.ensureV4(m.v4Dir); err != nil {
			return PresetMeta{}, err
		}
	}
	rawCfg, err := fs.ReadFile(m.presetsFS, chosen.File)
	if err != nil {
		return PresetMeta{}, err
	}

	runtimePath := filepath.Join(m.v4Dir, "config", "runtime_config.json")
	_ = os.MkdirAll(filepath.Dir(runtimePath), 0755)
	if err := os.WriteFile(runtimePath, rawCfg, 0644); err != nil {
		return PresetMeta{}, err
	}

	if running {
		_ = m.importRawConfigToV4(rawCfg)
	}

	m.mu.Lock()
	m.activePresetID = chosen.ID
	m.mu.Unlock()
	return *chosen, nil
}

func (m *Manager) ApplyRawJSON(rawCfg []byte) error {
	var parsed map[string]any
	if err := json.Unmarshal(rawCfg, &parsed); err != nil {
		return fmt.Errorf("JSON 格式错误: %w", err)
	}
	if _, ok := parsed["settings"]; !ok {
		return errors.New("缺少 settings 字段，不是有效的 V4 配置文件")
	}
	if m.ensureV4 != nil {
		if err := m.ensureV4(m.v4Dir); err != nil {
			return err
		}
	}
	runtimePath := filepath.Join(m.v4Dir, "config", "runtime_config.json")
	_ = os.MkdirAll(filepath.Dir(runtimePath), 0755)
	if err := os.WriteFile(runtimePath, rawCfg, 0644); err != nil {
		return err
	}
	m.mu.Lock()
	running := m.running
	m.activePresetID = "custom"
	m.mu.Unlock()
	if running {
		return m.importRawConfigToV4(rawCfg)
	}
	return nil
}

func (m *Manager) importRawConfigToV4(rawCfg []byte) error {
	resp, err := m.httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/api/settings", InternalHTTPPort))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var st struct {
		Revision int `json:"revision"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)

	var cfgObj any
	if err := json.Unmarshal(rawCfg, &cfgObj); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"action":               "import",
		"requestId":            fmt.Sprintf("next-%d", time.Now().UnixNano()),
		"revision":             st.Revision,
		"config":               cfgObj,
		"confirmTractionMerge": true,
	})
	pr, err := m.httpClient.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/settings", InternalHTTPPort),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer pr.Body.Close()
	_, _ = io.Copy(io.Discard, pr.Body)
	if pr.StatusCode >= 300 {
		return fmt.Errorf("V4 拒绝配置导入 (HTTP %d)", pr.StatusCode)
	}
	return nil
}

// DeviceCommand sends a /api/device command to V4, automatically injecting the latest valid deviceGeneration!
func (m *Manager) DeviceCommand(raw []byte) ([]byte, int, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err == nil {
		if _, hasRefresh := body["refresh"]; !hasRefresh {
			m.mu.Lock()
			gen := m.lastDeviceGen
			m.mu.Unlock()
			if gen > 0 {
				body["gen"] = gen
				raw, _ = json.Marshal(body)
			}
		}
	}
	return m.PostV4JSON("/api/device", raw)
}

func (m *Manager) GetV4JSON(path string) ([]byte, int, error) {
	resp, err := m.httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", InternalHTTPPort, path))
	if err != nil {
		return nil, 502, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (m *Manager) PostV4JSON(path string, body []byte) ([]byte, int, error) {
	resp, err := m.httpClient.Post(fmt.Sprintf("http://127.0.0.1:%d%s", InternalHTTPPort, path), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 502, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (m *Manager) ServeProxy(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	if !m.Status().Running {
		http.Error(w, "V4 专业模式未运行", http.StatusServiceUnavailable)
		return
	}
	r2 := r.Clone(r.Context())
	if stripPrefix != "" && len(r2.URL.Path) >= len(stripPrefix) {
		r2.URL.Path = r2.URL.Path[len(stripPrefix):]
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
	}
	m.proxy.ServeHTTP(w, r2)
}
