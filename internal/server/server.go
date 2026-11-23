package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"forzanext/internal/engine"
	"forzanext/internal/hid"
	"forzanext/internal/lighting"
	"forzanext/internal/v4bridge"
)

type Mode string

const (
	ModeEntertainment Mode = "entertainment"
	ModePro           Mode = "pro"

	maxJSONBodyBytes = 1 << 20 // 1 MiB
)

// App coordinates UDP routing, Entertainment Next Engine, Pro V4 Manager, HID devices, K6 Telemetry Lighting, and embedded Web UI.
type App struct {
	mu           sync.RWMutex
	switchMu     sync.Mutex
	mode         Mode
	webFS        fs.FS
	dataDir      string
	writer       *hid.Writer
	runtime      *engine.Runtime
	v4           *v4bridge.Manager
	lights       *lighting.Controller
	lightCfgPath string
	udpBindErr   string
	udpPort      int
	shutdownCh   <-chan struct{}
	onShutdown   func()
	devices      []hid.DeviceInfo
	includeAll   bool
	deviceMsg    string
}

func NewApp(
	webFS fs.FS,
	dataDir string,
	writer *hid.Writer,
	rt *engine.Runtime,
	v4 *v4bridge.Manager,
	lights *lighting.Controller,
	lightCfgPath string,
	udpBindErr string,
	udpPort int,
	shutdownCh <-chan struct{},
	onShutdown func(),
) *App {
	if udpPort <= 0 {
		udpPort = 5300
	}
	a := &App{
		mode:         ModeEntertainment,
		webFS:        webFS,
		dataDir:      dataDir,
		writer:       writer,
		runtime:      rt,
		v4:           v4,
		lights:       lights,
		lightCfgPath: lightCfgPath,
		udpBindErr:   udpBindErr,
		udpPort:      udpPort,
		shutdownCh:   shutdownCh,
		onShutdown:   onShutdown,
	}
	a.RefreshDevices(false)
	return a
}

func (a *App) Mode() Mode {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mode
}

func (a *App) includeAllState() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.includeAll
}

func cloneDevices(src []hid.DeviceInfo) []hid.DeviceInfo {
	out := make([]hid.DeviceInfo, len(src))
	copy(out, src)
	return out
}

func (a *App) enumerateDevicesSnapshot(includeAll bool) []hid.DeviceInfo {
	devs := hid.Enumerate33ByteDevices(includeAll)
	activePath := a.writer.ActivePath()
	for i := range devs {
		if activePath != "" && devs[i].Path == activePath {
			devs[i].Writing = true
		}
	}
	return devs
}

func (a *App) RefreshDevices(includeAll bool) []hid.DeviceInfo {
	devs := a.enumerateDevicesSnapshot(includeAll)
	a.mu.Lock()
	a.includeAll = includeAll
	a.devices = devs
	out := cloneDevices(devs)
	a.mu.Unlock()
	return out
}

func (a *App) RefreshDevicesCurrent() []hid.DeviceInfo {
	return a.RefreshDevices(a.includeAllState())
}

func (a *App) SwitchMode(target Mode) error {
	a.switchMu.Lock()
	defer a.switchMu.Unlock()

	a.mu.RLock()
	cur := a.mode
	a.mu.RUnlock()
	if target == cur {
		return nil
	}

	if target == ModePro {
		// Release Next Engine HID cleanly & wait 220ms for Windows HID stack release before launching V4
		a.runtime.SetActive(false)
		a.writer.Close()
		time.Sleep(220 * time.Millisecond)
		if err := a.v4.Start(); err != nil {
			a.runtime.SetActive(true)
			return err
		}
		a.mu.Lock()
		a.mode = ModePro
		a.deviceMsg = "已切换至专业模式（V4 原版引擎）"
		a.mu.Unlock()
		return nil
	}

	// Switch back to Entertainment (Next) Mode
	a.v4.Stop()
	time.Sleep(220 * time.Millisecond)
	a.mu.Lock()
	a.mode = ModeEntertainment
	a.deviceMsg = "已切换至娱乐模式（Next 状态引擎）"
	a.mu.Unlock()
	a.runtime.SetActive(true)
	a.RefreshDevicesCurrent()
	return nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unified state, mode & safe shutdown API
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/mode", a.handleMode)
	mux.HandleFunc("/api/devices", a.handleDevices)
	mux.HandleFunc("/api/stream", a.handleSSE)
	mux.HandleFunc("/api/shutdown", a.handleShutdown)

	// Entertainment (Next) Mode APIs
	mux.HandleFunc("/api/next/config", a.handleNextConfig)
	mux.HandleFunc("/api/next/preset", a.handleNextPreset)
	mux.HandleFunc("/api/next/test", a.handleNextTest)
	mux.HandleFunc("/api/next/car", a.handleNextCar)
	mux.HandleFunc("/api/next/garage", a.handleGarage)

	// Flydigi K6 / APEX 6 Telemetry Lighting APIs (active across BOTH Entertainment & Pro modes!)
	mux.HandleFunc("/api/lighting/config", a.handleLightingConfig)
	mux.HandleFunc("/api/lighting/test", a.handleLightingTest)
	mux.HandleFunc("/api/lighting/restore", a.handleLightingRestore)

	// Pro (V4) Mode APIs
	mux.HandleFunc("/api/pro/hello", a.handleProHello)
	mux.HandleFunc("/api/pro/presets", a.handleProPresets)
	mux.HandleFunc("/api/pro/preset/apply", a.handleProApplyPreset)
	mux.HandleFunc("/api/pro/import", a.handleProImport)
	mux.HandleFunc("/api/pro/settings", a.handleProSettings)
	mux.HandleFunc("/api/pro/device", a.handleProDevice)
	mux.HandleFunc("/api/pro/recording", a.handleProRecording)
	mux.HandleFunc("/api/pro/recording/download", a.handleProRecordingDownload)
	mux.HandleFunc("/api/pro/events", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/events"
		a.v4.ServeProxy(w, r2, "")
	})
	mux.HandleFunc("/v4/", func(w http.ResponseWriter, r *http.Request) {
		a.v4.ServeProxy(w, r, "/v4")
	})

	// Embedded single-EXE static Web UI (served straight from memory!)
	staticSrv := http.FileServer(http.FS(a.webFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		staticSrv.ServeHTTP(w, r)
	})

	return loopbackGuardMiddleware(mux)
}

func isLoopbackHost(hostport string) bool {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return false
	}
	host := h
	if parsedHost, _, err := net.SplitHostPort(h); err == nil {
		host = parsedHost
	} else {
		host = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return isLoopbackHost(u.Host)
}

func loopbackGuardMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch:
			if !isLoopbackHost(r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅允许本机回环 Host 访问变更接口"})
				return
			}
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
				if !isLoopbackURL(origin) {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": "拒绝跨源变更请求 (Origin)"})
					return
				}
			} else if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
				if !isLoopbackURL(referer) {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": "拒绝跨源变更请求 (Referer)"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}

func readBodyLimited(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return io.ReadAll(r.Body)
}

func writeBodyError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "请求体超过大小限制"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func (a *App) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "shutting down cleanly"})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if a.onShutdown != nil {
		go func() {
			time.Sleep(50 * time.Millisecond)
			a.onShutdown()
		}()
	}
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	mode := a.mode
	devs := cloneDevices(a.devices)
	devMsg := a.deviceMsg
	udpErr := a.udpBindErr
	includeAll := a.includeAll
	udpPort := a.udpPort
	a.mu.RUnlock()

	written, fail, lastErr, open, activeDev := a.writer.Stats()
	for i := range devs {
		devs[i].Writing = open && activeDev != nil && devs[i].Path == activeDev.Path
	}

	proHello, proStat, _ := a.v4.CachedHello()
	writeJSON(w, 200, map[string]any{
		"mode":           mode,
		"dataDir":        a.dataDir,
		"udpPort":        udpPort,
		"udpBindError":   udpErr,
		"deviceMessage":  devMsg,
		"includeAllHid":  includeAll,
		"devices":        devs,
		"writing":        open,
		"written":        written,
		"writeFailures":  fail,
		"lastWriteError": lastErr,
		"activeDevice":   activeDev,
		"nextConfig":     a.runtime.Config(),
		"activeCar":      a.runtime.Profiler().Active(),
		"archetypes":     engine.AllArchetypes(),
		"garage":         a.runtime.Profiler().ListGarage(),
		"proStatus":      a.v4.Status(),
		"proHello":       proHello,
		"proLiveStatus":  proStat,
		"proPresets":     a.v4.Presets(),
		"lighting":       a.lights.Snapshot(),
	})
}

func (a *App) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode Mode `json:"mode"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	if req.Mode != ModeEntertainment && req.Mode != ModePro {
		writeJSON(w, 400, map[string]any{"error": "无效模式"})
		return
	}
	if err := a.SwitchMode(req.Mode); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mode": a.Mode(), "proStatus": a.v4.Status()})
}

func (a *App) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		includeAll := a.includeAllState()
		switch r.URL.Query().Get("all") {
		case "1", "true":
			includeAll = true
		case "0", "false":
			includeAll = false
		}
		devs := a.enumerateDevicesSnapshot(includeAll)
		writeJSON(w, 200, map[string]any{"devices": devs})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action     string `json:"action"` // refresh, open, close
		ID         int    `json:"id"`
		IncludeAll bool   `json:"includeAll"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}

	switch req.Action {
	case "refresh":
		devs := a.RefreshDevices(req.IncludeAll)
		writeJSON(w, 200, map[string]any{"ok": true, "devices": devs})
	case "close":
		a.switchMu.Lock()
		a.writer.Close()
		a.mu.Lock()
		a.deviceMsg = "已停止手柄硬件输出"
		a.mu.Unlock()
		a.switchMu.Unlock()
		devs := a.RefreshDevicesCurrent()
		writeJSON(w, 200, map[string]any{"ok": true, "writing": false, "devices": devs})
	case "open":
		a.switchMu.Lock()
		defer a.switchMu.Unlock()

		if a.Mode() != ModeEntertainment {
			writeJSON(w, 400, map[string]any{"error": "当前为专业模式，请通过 V4 设备栏开启"})
			return
		}
		a.mu.RLock()
		var chosen hid.DeviceInfo
		validID := req.ID >= 0 && req.ID < len(a.devices)
		if validID {
			chosen = a.devices[req.ID]
		}
		a.mu.RUnlock()
		if !validID {
			writeJSON(w, 400, map[string]any{"error": "无效的设备编号，请刷新手柄列表"})
			return
		}
		if err := a.writer.Open(chosen); err != nil {
			a.mu.Lock()
			a.deviceMsg = err.Error()
			a.mu.Unlock()
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		a.mu.Lock()
		a.deviceMsg = fmt.Sprintf("已连接并开启触觉输出：%s (%s:%s)", chosen.Product, chosen.VID, chosen.PID)
		a.mu.Unlock()
		devs := a.RefreshDevicesCurrent()
		writeJSON(w, 200, map[string]any{"ok": true, "writing": true, "devices": devs})
	default:
		writeJSON(w, 400, map[string]any{"error": "未知操作"})
	}
}

func (a *App) handleNextConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, a.runtime.Config())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := a.runtime.Config()
	if err := decodeJSON(w, r, maxJSONBodyBytes, &cfg); err != nil {
		writeBodyError(w, err)
		return
	}
	cfg.Sanitize()
	if err := a.runtime.UpdateConfig(cfg); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "config": a.runtime.Config()})
}

func (a *App) handleNextPreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	cfg, ok := engine.StylePreset(req.Name)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "未找到风格配方"})
		return
	}
	cfg.Sanitize()
	if err := a.runtime.UpdateConfig(cfg); err != nil {
		writeJSON(w, 500, map[string]any{"error": "保存风格配方失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "config": a.runtime.Config()})
}

func (a *App) handleNextTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		EventID string `json:"eventId"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	if !engine.IsValidTestEventID(req.EventID) {
		writeJSON(w, 400, map[string]any{"error": "无效的试震事件 ID"})
		return
	}
	a.runtime.TriggerTest(req.EventID)
	writeJSON(w, 200, map[string]any{"ok": true, "eventId": req.EventID})
}

func (a *App) handleNextCar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action              string  `json:"action"` // override, reset, previewArchetype
		ArchetypeID         string  `json:"archetypeId"`
		CustomName          string  `json:"customName"`
		EngineRoundness     float64 `json:"engineRoundness"`
		EngineBurble        float64 `json:"engineBurble"`
		SuspensionStiffness float64 `json:"suspensionStiffness"`
		ShiftCrispness      float64 `json:"shiftCrispness"`
		EngineBedScale      float64 `json:"engineBedScale"`
		RedlineRatio        float64 `json:"redlineRatio"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	var car engine.CarPersonality
	switch req.Action {
	case "override":
		if a.runtime.Profiler().Active().CarOrdinal == 0 {
			writeJSON(w, 400, map[string]any{"error": "当前尚未接入游戏车辆，请进入游戏驾驶后再保存专属调校"})
			return
		}
		car = a.runtime.Profiler().SaveCurrentOverride(
			req.CustomName, req.EngineRoundness, req.EngineBurble,
			req.SuspensionStiffness, req.ShiftCrispness, req.EngineBedScale, req.RedlineRatio,
		)
	case "reset":
		if a.runtime.Profiler().Active().CarOrdinal == 0 {
			writeJSON(w, 400, map[string]any{"error": "当前尚未接入游戏车辆，无法恢复自动匹配"})
			return
		}
		car = a.runtime.Profiler().ResetCurrentOverride()
	case "previewArchetype":
		if !engine.IsValidArchetypeID(req.ArchetypeID) {
			writeJSON(w, 400, map[string]any{"error": "无效的车型性格 ID"})
			return
		}
		car = a.runtime.Profiler().ApplyDemoArchetype(req.ArchetypeID)
	default:
		writeJSON(w, 400, map[string]any{"error": "未知车辆操作"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "car": car, "garage": a.runtime.Profiler().ListGarage()})
}

func (a *App) handleGarage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"garage": a.runtime.Profiler().ListGarage()})
		return
	}
	if r.Method == http.MethodDelete {
		var req struct {
			CarOrdinal int32 `json:"carOrdinal"`
		}
		if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
			writeBodyError(w, err)
			return
		}
		if req.CarOrdinal == 0 {
			writeJSON(w, 400, map[string]any{"error": "无效的车辆编号 (carOrdinal=0)"})
			return
		}
		a.runtime.Profiler().DeleteGarageEntry(req.CarOrdinal)
		writeJSON(w, 200, map[string]any{
			"ok":     true,
			"garage": a.runtime.Profiler().ListGarage(),
			"car":    a.runtime.Profiler().Active(),
		})
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (a *App) handleProPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status":  a.v4.Status(),
		"presets": a.v4.Presets(),
	})
}

func (a *App) handleProApplyPreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	p, err := a.v4.ApplyPreset(req.ID)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "preset": p, "status": a.v4.Status()})
}

func (a *App) handleProImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	raw, err := readBodyLimited(w, r, maxJSONBodyBytes)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	if err := a.v4.ApplyRawJSON(raw); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": a.v4.Status()})
}

func (a *App) handleProSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if !a.v4.Status().Running {
			b, err := os.ReadFile(a.v4.RuntimeConfigPath())
			if err != nil {
				writeJSON(w, 404, map[string]any{"error": "无法读取 V4 配置"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}
		b, code, err := a.v4.GetV4JSON("/api/settings")
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(b)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := readBodyLimited(w, r, maxJSONBodyBytes)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	b, code, err := a.v4.PostV4JSON("/api/settings", raw)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (a *App) handleProHello(w http.ResponseWriter, r *http.Request) {
	hello, st, _ := a.v4.CachedHello()
	writeJSON(w, 200, map[string]any{
		"status": a.v4.Status(),
		"hello":  hello,
		"live":   st,
	})
}

func (a *App) handleProDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	raw, err := readBodyLimited(w, r, 8*1024)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	b, code, err := a.v4.DeviceCommand(raw)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (a *App) handleProRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		b, code, err := a.v4.GetV4JSON("/api/recording")
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(b)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := readBodyLimited(w, r, 4*1024)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	b, code, err := a.v4.PostV4JSON("/api/recording", raw)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (a *App) handleProRecordingDownload(w http.ResponseWriter, r *http.Request) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/api/recording/download"
	a.v4.ServeProxy(w, r2, "")
}

func (a *App) handleLightingConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, a.lights.Snapshot())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := a.lights.Config()
	if err := decodeJSON(w, r, maxJSONBodyBytes, &cfg); err != nil {
		writeBodyError(w, err)
		return
	}
	cfg.Sanitize()
	a.lights.SetConfig(cfg)
	if err := lighting.SaveLightConfig(a.lightCfgPath, cfg); err != nil {
		writeJSON(w, 500, map[string]any{"error": "保存灯效配置失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "lighting": a.lights.Snapshot()})
}

func (a *App) handleLightingTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := decodeJSON(w, r, maxJSONBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	if !lighting.IsValidTestMode(req.Mode) {
		writeJSON(w, 400, map[string]any{"error": "无效的灯效测试模式"})
		return
	}
	a.lights.TriggerTest(req.Mode)
	writeJSON(w, 200, map[string]any{"ok": true, "mode": req.Mode, "lighting": a.lights.Snapshot()})
}

func (a *App) handleLightingRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	a.lights.RequestRestore()
	writeJSON(w, 200, map[string]any{"ok": true, "lighting": a.lights.Snapshot()})
}

func (a *App) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(40 * time.Millisecond) // 25 Hz smooth UI stream
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.shutdownCh:
			return
		case <-ticker.C:
			written, fail, lastErr, open, _ := a.writer.Stats()
			payload := map[string]any{
				"mode":          a.Mode(),
				"writing":       open,
				"written":       written,
				"writeFailures": fail,
				"lastError":     lastErr,
				"snap":          a.runtime.Snapshot(),
				"proStatus":     a.v4.Status(),
				"lighting":      a.lights.Snapshot(),
			}
			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
