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
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	InternalUDPPort  = 15300
	InternalHTTPPort = 18300

	jobObjectInfoClassExtendedLimit = 9
	jobObjectLimitKillOnJobClose    = 0x00002000
	processSetQuota                 = 0x0100
	processTerminate                = 0x0001
)

var (
	modKernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = modKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = modKernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = modKernel32.NewProc("AssignProcessToJobObject")
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func createKillOnCloseJob() (syscall.Handle, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateJobObjectW: %w", err)
	}
	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r1, _, err := procSetInformationJobObject.Call(
		h,
		uintptr(jobObjectInfoClassExtendedLimit),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r1 == 0 {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return syscall.Handle(h), nil
}

func assignProcessToJob(job syscall.Handle, pid int) error {
	procH, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(procH)
	r1, _, callErr := procAssignProcessToJobObject.Call(uintptr(job), uintptr(procH))
	if r1 == 0 {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, callErr)
	}
	return nil
}

type v4StripPrefixKey struct{}

// lockedTailBuffer captures the last N bytes written by child stdout/stderr safely.
type lockedTailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newLockedTailBuffer(max int) *lockedTailBuffer {
	return &lockedTailBuffer{max: max}
}

func (b *lockedTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *lockedTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func checkPortAvailable(network, addr string) error {
	if network == "tcp" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		_ = ln.Close()
		return nil
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	_ = pc.Close()
	return nil
}

func atomicWriteFile(dst string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cfg-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpPath, perm)
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

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
	cmdDone        chan struct{}
	jobHandle      syscall.Handle
	exitErr        error
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
		if prefix, _ := req.Context().Value(v4StripPrefixKey{}).(string); prefix != "" {
			// Ensure HTML responses are uncompressed so ModifyResponse can rewrite root-relative paths
			req.Header.Del("Accept-Encoding")
		}
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		prefix, _ := resp.Request.Context().Value(v4StripPrefixKey{}).(string)
		if prefix == "" {
			return nil
		}
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if !strings.Contains(ct, "text/html") {
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		s := string(body)
		s = strings.ReplaceAll(s, `"/events"`, `"`+prefix+`/events"`)
		s = strings.ReplaceAll(s, `'/events'`, `'`+prefix+`/events'`)
		s = strings.ReplaceAll(s, `"/api/`, `"`+prefix+`/api/`)
		s = strings.ReplaceAll(s, `'/api/`, `'`+prefix+`/api/`)
		s = strings.ReplaceAll(s, `href="/"`, `href="`+prefix+`/"`)
		rewritten := []byte(s)
		resp.Body = io.NopCloser(bytes.NewReader(rewritten))
		resp.ContentLength = int64(len(rewritten))
		resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		resp.Header.Del("Content-Encoding")
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		http.Error(w, fmt.Sprintf("V4 代理请求失败: %v", err), http.StatusBadGateway)
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

	// Pre-check ports so an orphaned ForzaTelemetryHarness.exe cannot cause a false-positive 200 OK
	if err := checkPortAvailable("tcp", fmt.Sprintf("127.0.0.1:%d", InternalHTTPPort)); err != nil {
		m.lastError = fmt.Sprintf("V4 监控端口 127.0.0.1:%d 已被占用（可能存在残留 V4 进程）: %v", InternalHTTPPort, err)
		m.mu.Unlock()
		return errors.New(m.lastError)
	}
	if err := checkPortAvailable("udp", fmt.Sprintf("127.0.0.1:%d", InternalUDPPort)); err != nil {
		m.lastError = fmt.Sprintf("V4 UDP 端口 127.0.0.1:%d 已被占用（可能存在残留 V4 进程）: %v", InternalUDPPort, err)
		m.mu.Unlock()
		return errors.New(m.lastError)
	}

	job, jobErr := createKillOnCloseJob()
	if jobErr != nil {
		log.Printf("[WARN] createKillOnCloseJob failed: %v", jobErr)
	}

	stderrBuf := newLockedTailBuffer(4096)
	cmd := exec.Command(
		exePath,
		"--config", cfgPath,
		"--web-root", webPath,
		"--port", fmt.Sprintf("%d", InternalUDPPort),
		"--monitor-port", fmt.Sprintf("%d", InternalHTTPPort),
		"--watch-config",
	)
	cmd.Dir = m.v4Dir
	cmd.Stderr = stderrBuf
	cmd.Stdout = stderrBuf
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if err := cmd.Start(); err != nil {
		if job != 0 {
			_ = syscall.CloseHandle(job)
		}
		m.lastError = fmt.Sprintf("启动 V4 引擎失败: %v", err)
		m.mu.Unlock()
		return errors.New(m.lastError)
	}

	if job != 0 {
		if err := assignProcessToJob(job, cmd.Process.Pid); err != nil {
			log.Printf("[WARN] assignProcessToJob failed for V4 pid %d: %v", cmd.Process.Pid, err)
			_ = syscall.CloseHandle(job)
			job = 0
		}
	}

	dstAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", InternalUDPPort))
	if err == nil {
		if uc, uerr := net.DialUDP("udp", nil, dstAddr); uerr == nil {
			m.udpConn = uc
		}
	}

	cmdDone := make(chan struct{})
	m.cmd = cmd
	m.cmdDone = cmdDone
	m.jobHandle = job
	m.exitErr = nil
	m.running = true
	m.ready = false
	m.lastError = ""
	m.lastHello = nil
	m.lastStatus = nil
	m.lastDeviceGen = 0
	m.mu.Unlock()

	go func(c *exec.Cmd, done chan struct{}) {
		waitErr := c.Wait()
		close(done)
		m.mu.Lock()
		var jobToClose syscall.Handle
		var udpToClose *net.UDPConn
		if m.cmd == c {
			m.running = false
			m.ready = false
			m.cmd = nil
			m.cmdDone = nil
			m.exitErr = waitErr
			if m.watchCancel != nil {
				m.watchCancel()
				m.watchCancel = nil
			}
			udpToClose = m.udpConn
			m.udpConn = nil
			jobToClose = m.jobHandle
			m.jobHandle = 0
		}
		m.mu.Unlock()
		if udpToClose != nil {
			_ = udpToClose.Close()
		}
		if jobToClose != 0 {
			_ = syscall.CloseHandle(jobToClose)
		}
	}(cmd, cmdDone)

	// Wait for V4 HTTP server to become ready
	select {
	case <-cmdDone:
	case <-time.After(220 * time.Millisecond):
	}
	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		sameAndRunning := m.cmd == cmd && m.running
		m.mu.Unlock()
		if !sameAndRunning {
			break
		}
		resp, err := m.httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/api/settings", InternalHTTPPort))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				m.mu.Lock()
				if m.cmd != cmd || !m.running {
					m.mu.Unlock()
					return errors.New("V4 引擎启动期间已停止")
				}
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
					still := m.cmd == cmd && m.running
					m.mu.Unlock()
					if got || !still {
						break
					}
					select {
					case <-cmdDone:
					case <-time.After(30 * time.Millisecond):
					}
				}
				m.mu.Lock()
				still := m.cmd == cmd && m.running
				m.mu.Unlock()
				if !still {
					break
				}
				return nil
			}
		}
		select {
		case <-cmdDone:
		case <-time.After(150 * time.Millisecond):
		}
	}

	// Failed to become ready (either child exited early, Stop() was called, or 3.5s timed out)
	var reason string
	select {
	case <-cmdDone:
		m.mu.Lock()
		exitErr := m.exitErr
		m.mu.Unlock()
		outTxt := strings.TrimSpace(stderrBuf.String())
		if len(outTxt) > 400 {
			outTxt = outTxt[len(outTxt)-400:]
		}
		switch {
		case exitErr != nil && outTxt != "":
			reason = fmt.Sprintf("V4 引擎提前退出 (%v): %s", exitErr, outTxt)
		case outTxt != "":
			reason = fmt.Sprintf("V4 引擎提前退出: %s", outTxt)
		case exitErr != nil:
			reason = fmt.Sprintf("V4 引擎提前退出: %v", exitErr)
		default:
			reason = "V4 引擎在就绪前提前退出"
		}
	default:
		outTxt := strings.TrimSpace(stderrBuf.String())
		if len(outTxt) > 400 {
			outTxt = outTxt[len(outTxt)-400:]
		}
		if outTxt != "" {
			reason = fmt.Sprintf("V4 引擎启动超时 (3.5s 未就绪): %s", outTxt)
		} else {
			reason = "V4 引擎启动超时 (3.5s 未就绪)"
		}
	}

	m.Stop()
	m.mu.Lock()
	m.lastError = reason
	m.mu.Unlock()
	return errors.New(reason)
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
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
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
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
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
	cmdDone := m.cmdDone
	job := m.jobHandle
	gen := m.lastDeviceGen
	m.cmd = nil
	m.cmdDone = nil
	m.jobHandle = 0
	m.running = false
	m.ready = false
	m.lastHello = nil
	m.lastStatus = nil
	m.lastDeviceGen = 0
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		alreadyExited := false
		if cmdDone != nil {
			select {
			case <-cmdDone:
				alreadyExited = true
			default:
			}
		}
		if !alreadyExited {
			// Gracefully release V4's HID handle using the REAL deviceGeneration before terminating
			payload := fmt.Sprintf(`{"gen":%d,"id":-1}`, gen)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			req, reqErr := http.NewRequestWithContext(
				ctx,
				http.MethodPost,
				fmt.Sprintf("http://127.0.0.1:%d/api/device", InternalHTTPPort),
				bytes.NewBufferString(payload),
			)
			if reqErr == nil {
				req.Header.Set("Content-Type", "application/json")
				if resp, err := m.httpClient.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}
			cancel()

			if cmdDone != nil {
				select {
				case <-cmdDone:
					alreadyExited = true
				case <-time.After(120 * time.Millisecond):
				}
			} else {
				time.Sleep(120 * time.Millisecond)
			}
			if !alreadyExited {
				_ = cmd.Process.Kill()
			}
		}
		if cmdDone != nil {
			select {
			case <-cmdDone:
			case <-time.After(2 * time.Second):
			}
		}
	}
	if job != 0 {
		_ = syscall.CloseHandle(job)
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
	if !running && m.ensureV4 != nil {
		if err := m.ensureV4(m.v4Dir); err != nil {
			return PresetMeta{}, err
		}
	}
	rawCfg, err := fs.ReadFile(m.presetsFS, chosen.File)
	if err != nil {
		return PresetMeta{}, err
	}

	runtimePath := filepath.Join(m.v4Dir, "config", "runtime_config.json")
	if err := atomicWriteFile(runtimePath, rawCfg, 0644); err != nil {
		return PresetMeta{}, err
	}

	if running {
		if err := m.importRawConfigToV4(rawCfg); err != nil {
			return PresetMeta{}, err
		}
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
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()

	if !running && m.ensureV4 != nil {
		if err := m.ensureV4(m.v4Dir); err != nil {
			return err
		}
	}
	runtimePath := filepath.Join(m.v4Dir, "config", "runtime_config.json")
	if err := atomicWriteFile(runtimePath, rawCfg, 0644); err != nil {
		return err
	}
	if running {
		if err := m.importRawConfigToV4(rawCfg); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.activePresetID = "custom"
	m.mu.Unlock()
	return nil
}

func (m *Manager) importRawConfigToV4(rawCfg []byte) error {
	var cfgObj any
	if err := json.Unmarshal(rawCfg, &cfgObj); err != nil {
		return err
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := m.httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/api/settings", InternalHTTPPort))
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return fmt.Errorf("读取 V4 配置失败 (HTTP %d)", resp.StatusCode)
		}
		var st struct {
			Revision int `json:"revision"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&st)
		_ = resp.Body.Close()
		if decErr != nil {
			return fmt.Errorf("解析 V4 配置版本失败: %w", decErr)
		}

		body, err := json.Marshal(map[string]any{
			"action":               "import",
			"requestId":            fmt.Sprintf("next-%d", time.Now().UnixNano()),
			"revision":             st.Revision,
			"config":               cfgObj,
			"confirmTractionMerge": true,
		})
		if err != nil {
			return err
		}
		pr, err := m.httpClient.Post(
			fmt.Sprintf("http://127.0.0.1:%d/api/settings", InternalHTTPPort),
			"application/json",
			bytes.NewReader(body),
		)
		if err != nil {
			return err
		}
		respBytes, _ := io.ReadAll(io.LimitReader(pr.Body, 4096))
		_ = pr.Body.Close()
		if pr.StatusCode == http.StatusConflict && attempt == 0 {
			// --watch-config may have bumped revision right after atomic rename; retry once
			time.Sleep(60 * time.Millisecond)
			continue
		}
		if pr.StatusCode >= 300 {
			msg := strings.TrimSpace(string(respBytes))
			if msg != "" {
				return fmt.Errorf("V4 拒绝配置导入 (HTTP %d): %s", pr.StatusCode, msg)
			}
			return fmt.Errorf("V4 拒绝配置导入 (HTTP %d)", pr.StatusCode)
		}
		return nil
	}
	return errors.New("V4 配置导入版本冲突重试失败")
}

// DeviceCommand sends a /api/device command to V4, injecting m.lastDeviceGen only when client gen is missing/0 or stale.
func (m *Manager) DeviceCommand(raw []byte) ([]byte, int, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err == nil {
		if _, hasRefresh := body["refresh"]; !hasRefresh {
			m.mu.Lock()
			cachedGen := m.lastDeviceGen
			m.mu.Unlock()
			if cachedGen > 0 {
				var clientGen int64
				if gv, ok := body["gen"]; ok {
					switch v := gv.(type) {
					case float64:
						clientGen = int64(v)
					case int64:
						clientGen = v
					case int:
						clientGen = int64(v)
					}
				}
				if clientGen <= 0 || clientGen < cachedGen {
					body["gen"] = cachedGen
					if b, merr := json.Marshal(body); merr == nil {
						raw = b
					}
				}
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
	ctx := r.Context()
	if stripPrefix != "" {
		ctx = context.WithValue(ctx, v4StripPrefixKey{}, stripPrefix)
	}
	r2 := r.Clone(ctx)
	if stripPrefix != "" && strings.HasPrefix(r2.URL.Path, stripPrefix) {
		r2.URL.Path = strings.TrimPrefix(r2.URL.Path, stripPrefix)
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		if r2.URL.RawPath != "" && strings.HasPrefix(r2.URL.RawPath, stripPrefix) {
			r2.URL.RawPath = strings.TrimPrefix(r2.URL.RawPath, stripPrefix)
			if r2.URL.RawPath == "" {
				r2.URL.RawPath = "/"
			}
		} else {
			r2.URL.RawPath = ""
		}
	}
	m.proxy.ServeHTTP(w, r2)
}
