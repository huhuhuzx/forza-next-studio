package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"forzanext/internal/assets"
	"forzanext/internal/engine"
	"forzanext/internal/hid"
	"forzanext/internal/lighting"
	"forzanext/internal/server"
	"forzanext/internal/telemetry"
	"forzanext/internal/v4bridge"
)

func resolveDataDir(rootFlag string) string {
	if rootFlag != "" {
		return rootFlag
	}
	// If user placed a portable config\ folder right next to ForzaNext.exe, honor it.
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(exeDir, "config", "next_config.json")); err == nil {
			return exeDir
		}
	}
	// Default for single-file distribution: %LOCALAPPDATA%\ForzaNext (keeps user's Desktop 100% clean!)
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return filepath.Join(localAppData, "ForzaNext")
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		return filepath.Join(cfgDir, "ForzaNext")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), ".forzanext_data")
	}
	return "."
}

func isAlreadyRunning(httpPort int) bool {
	client := &http.Client{
		Timeout:   450 * time.Millisecond,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", httpPort))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func main() {
	udpPort := flag.Int("udp-port", 5300, "Forza UDP telemetry listen port")
	httpPort := flag.Int("http-port", 8300, "Web studio HTTP listen port")
	rootFlag := flag.String("root", "", "Optional custom data directory (defaults to %LOCALAPPDATA%\\ForzaNext)")
	noBrowser := flag.Bool("no-browser", false, "Do not auto-open the web studio in browser")
	flag.Parse()

	studioURL := fmt.Sprintf("http://127.0.0.1:%d/", *httpPort)

	// Single-instance guard: if ForzaNext is already running on this port, just open the Studio UI!
	if !*noBrowser && isAlreadyRunning(*httpPort) {
		log.Printf("[INFO] ForzaNext is already running at %s — opening browser...", studioURL)
		openBrowser(studioURL)
		return
	}

	setConsoleTitle("ForzaNext Studio v3 · 关闭窗口将自动恢复手柄原灯效并安全退出")

	dataDir := resolveDataDir(*rootFlag)
	cfgPath := filepath.Join(dataDir, "config", "next_config.json")
	lightCfgPath := filepath.Join(dataDir, "config", "lighting_config.json")
	garagePath := filepath.Join(dataDir, "data", "garage.json")
	v4Dir := filepath.Join(dataDir, "v4_runtime")

	_ = os.MkdirAll(filepath.Dir(cfgPath), 0755)
	_ = os.MkdirAll(filepath.Dir(garagePath), 0755)

	hid.BeginHighResTimer()
	defer hid.EndHighResTimer()

	writer := hid.NewWriter()
	rt := engine.NewRuntime(cfgPath, garagePath, writer)
	lights := lighting.NewController(lighting.LoadLightConfig(lightCfgPath))
	lights.Start()
	v4Mgr := v4bridge.NewManager(v4Dir, assets.PresetsFS(), assets.EnsureV4Runtime)

	// Bind Forza UDP telemetry port (0.0.0.0:5300)
	var udpConn *net.UDPConn
	udpBindErr := ""
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("0.0.0.0:%d", *udpPort))
	if err == nil {
		udpConn, err = net.ListenUDP("udp", addr)
	}
	if err != nil {
		udpBindErr = fmt.Sprintf("UDP 端口 %d 绑定失败（请先退出右下角飞智空间站/其他 Engine）: %v", *udpPort, err)
		log.Printf("[WARN] %s", udpBindErr)
	} else {
		_ = udpConn.SetReadBuffer(1024 * 1024)
		log.Printf("[UDP ] Listening for Forza telemetry on 0.0.0.0:%d", *udpPort)
	}

	// Bind HTTP listener synchronously BEFORE opening browser so first load never fails
	httpAddr := fmt.Sprintf("127.0.0.1:%d", *httpPort)
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatalf("[ERR ] 无法监听工作台端口 %s（可能已有实例运行）: %v", httpAddr, err)
	}

	shutdownReq := make(chan struct{})
	var reqOnce sync.Once
	triggerShutdown := func() {
		reqOnce.Do(func() { close(shutdownReq) })
	}

	app := server.NewApp(assets.WebFS(), dataDir, writer, rt, v4Mgr, lights, lightCfgPath, udpBindErr, triggerShutdown)
	rt.Start()

	// UDP ingest & dual-mode router goroutine
	if udpConn != nil {
		go func() {
			buf := make([]byte, 2048)
			for {
				n, _, err := udpConn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				raw := make([]byte, n)
				copy(raw, buf[:n])

				if app.Mode() == server.ModePro {
					v4Mgr.ForwardUDP(raw)
				}
				if pkt, perr := telemetry.Parse(raw); perr == nil {
					rt.IngestPacket(pkt)
					car := rt.Profiler().Active()
					lights.ObservePacket(pkt, car.RedlineRatio, car.IsElectric)
				}
			}
		}()
	}

	httpSrv := &http.Server{Handler: app.Handler()}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[ERR ] HTTP server stopped: %v", err)
			triggerShutdown()
		}
	}()

	log.Printf("==============================================================")
	log.Printf(" ForzaNext Studio v3 (单文件绿色版 · 双模式触觉 + APEX 6 灯带)")
	log.Printf(" 工作台地址 : %s", studioURL)
	log.Printf(" UDP 监听  : 127.0.0.1:%d  (游戏数据输出填此地址与端口)", *udpPort)
	log.Printf(" 数据目录  : %s", dataDir)
	log.Printf(" 提示：最小化本窗口即可；点网页右上角「安全退出」或直接关闭本窗口")
	log.Printf("       都会自动恢复手柄原灯效并安全释放所有进程。")
	log.Printf("==============================================================")

	if !*noBrowser {
		openBrowser(studioURL)
	}

	// Unified graceful cleanup (runs once whether triggered by UI「安全退出」, Ctrl+C, or Console X button)
	shutdownDone := make(chan struct{})
	var cleanOnce sync.Once
	doCleanup := func() {
		cleanOnce.Do(func() {
			log.Println("[EXIT] 正在恢复手柄原灯效并安全退出...")
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
			if udpConn != nil {
				_ = udpConn.Close()
			}
			rt.Stop()
			writer.Close()
			lights.Shutdown()
			v4Mgr.Stop()
			close(shutdownDone)
		})
	}

	// Catch red X button on console window & Windows logoff/shutdown cleanly
	installConsoleCloseHandler(func() {
		triggerShutdown()
		doCleanup()
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigCh:
	case <-shutdownReq:
	}

	doCleanup()
	<-shutdownDone
}
