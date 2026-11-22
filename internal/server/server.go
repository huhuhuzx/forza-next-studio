package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
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
)

// App coordinates UDP routing, Entertainment Next Engine, Pro V4 Manager, HID devices, K6 Telemetry Lighting, and embedded Web UI.
type App struct {
	mu              sync.RWMutex
	mode            Mode
	webFS           fs.FS
	dataDir         string
	writer          *hid.Writer
	runtime         *engine.Runtime
	v4              *v4bridge.Manager
	lights          *lighting.Controller
	lightCfgPath    string
	requestShutdown func()
	devices         []hid.DeviceInfo
	includeAll      bool
	deviceMsg       string
	udpBindErr      string
	selectedPath    string
}

func NewApp(webFS fs.FS, dataDir string, writer *hid.Writer, rt *engine.Runtime, v4 *v4bridge.Manager, lights *lighting.Controller, lightCfgPath string, udpBindErr string, requestShutdown func()) *App {
	a := &App{
		mode:            ModeEntertainment,
		webFS:           webFS,
		dataDir:         dataDir,
		writer:          writer,
		runtime:         rt,
		v4:              v4,
		lights:          lights,
		lightCfgPath:    lightCfgPath,
		udpBindErr:      udpBindErr,
		requestShutdown: requestShutdown,
	}
	a.RefreshDevices(false)
	return a
}

func (a *App) Mode() Mode {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mode
}

func (a *App) RefreshDevices(includeAll bool) []hid.DeviceInfo {
	devs := hid.Enumerate33ByteDevices(includeAll)
	activePath := a.writer.ActivePath()
	for i := range devs {
		if activePath != "" && devs[i].Path == activePath {
			devs[i].Writing = true
		}
	}
	a.mu.Lock()
	a.includeAll = includeAll
	a.devices = devs
	a.mu.Unlock()
	return devs
}

func (a *App) SwitchMode(target Mode) error {
	a.mu.Lock()
	cur := a.mode
	a.mu.Unlock()
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
	a.RefreshDevices(a.includeAll)
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

	return mux
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
	if a.requestShutdown != nil {
		go func() {
			time.Sleep(120 * time.Millisecond)
			a.requestShutdown()
		}()
	}
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	mode := a.mode
	devs := a.devices
	devMsg := a.deviceMsg
	udpErr := a.udpBindErr
	includeAll := a.includeAll
	a.mu.RUnlock()

	written, fail, lastErr, open, activeDev := a.writer.Stats()
	for i := range devs {
		devs[i].Writing = open && activeDev != nil && devs[i].Path == activeDev.Path
	}

	proHello, proStat, _ := a.v4.CachedHello()
	writeJSON(w, 200, map[string]any{
		"mode":           mode,
		"dataDir":        a.dataDir,
		"udpPort":        5300,
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
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
		devs := a.RefreshDevices(r.URL.Query().Get("all") == "1")
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}

	switch req.Action {
	case "refresh":
		devs := a.RefreshDevices(req.IncludeAll)
		writeJSON(w, 200, map[string]any{"ok": true, "devices": devs})
	case "close":
		a.writer.Close()
		a.mu.Lock()
		a.deviceMsg = "已停止手柄硬件输出"
		a.mu.Unlock()
		devs := a.RefreshDevices(a.includeAll)
		writeJSON(w, 200, map[string]any{"ok": true, "writing": false, "devices": devs})
	case "open":
		if a.Mode() != ModeEntertainment {
			writeJSON(w, 400, map[string]any{"error": "当前为专业模式，请通过 V4 设备栏开启"})
			return
		}
		a.mu.RLock()
		devs := a.devices
		a.mu.RUnlock()
		if req.ID < 0 || req.ID >= len(devs) {
			writeJSON(w, 400, map[string]any{"error": "无效的设备编号，请刷新手柄列表"})
			return
		}
		if err := a.writer.Open(devs[req.ID]); err != nil {
			a.mu.Lock()
			a.deviceMsg = err.Error()
			a.mu.Unlock()
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		a.mu.Lock()
		a.selectedPath = devs[req.ID].Path
		a.deviceMsg = fmt.Sprintf("已连接并开启触觉输出：%s (%s:%s)", devs[req.ID].Product, devs[req.ID].VID, devs[req.ID].PID)
		a.mu.Unlock()
		devs = a.RefreshDevices(a.includeAll)
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
	var cfg engine.NextConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	cfg, ok := engine.StylePreset(req.Name)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "未找到风格配方"})
		return
	}
	_ = a.runtime.UpdateConfig(cfg)
	writeJSON(w, 200, map[string]any{"ok": true, "config": cfg})
}

func (a *App) handleNextTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		EventID string `json:"eventId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	var car engine.CarPersonality
	switch req.Action {
	case "override":
		car = a.runtime.Profiler().SaveCurrentOverride(
			req.CustomName, req.EngineRoundness, req.EngineBurble,
			req.SuspensionStiffness, req.ShiftCrispness, req.EngineBedScale, req.RedlineRatio,
		)
	case "reset":
		car = a.runtime.Profiler().ResetCurrentOverride()
	case "previewArchetype":
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		a.runtime.Profiler().DeleteGarageEntry(req.CarOrdinal)
		writeJSON(w, 200, map[string]any{"ok": true, "garage": a.runtime.Profiler().ListGarage()})
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
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
	raw, err := io.ReadAll(io.LimitReader(r.Body, 128*1024))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
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
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 128*1024))
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
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 8*1024))
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
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4*1024))
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
	var cfg lighting.LightConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	cfg.Sanitize()
	a.lights.SetConfig(cfg)
	_ = lighting.SaveLightConfig(a.lightCfgPath, cfg)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
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
			if err == nil {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
