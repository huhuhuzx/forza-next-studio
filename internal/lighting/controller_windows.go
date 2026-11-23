//go:build windows

package lighting

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"forzanext/internal/hid"
	"forzanext/internal/telemetry"
)

var (
	modKernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW        = modKernel32.NewProc("CreateEventW")
	procResetEvent          = modKernel32.NewProc("ResetEvent")
	procWaitForSingleObject = modKernel32.NewProc("WaitForSingleObject")
	procGetOverlappedResult = modKernel32.NewProc("GetOverlappedResult")
	procCancelIoEx          = modKernel32.NewProc("CancelIoEx")

	errAborted = errors.New("lighting I/O aborted")
)

const (
	evalTickInterval   = 15 * time.Millisecond
	stalePacketTimeout = 800 * time.Millisecond
	rpmWriteCooldown   = 150 * time.Millisecond
)

// Controller manages Flydigi K6 / APEX 6 telemetry lighting over a dedicated overlapped HID handle
// to the 0xFFA0 interface, running completely in parallel with the 1 kHz haptic stream AND Pro Mode V4!
type Controller struct {
	mu sync.Mutex

	cfg            LightConfig
	clock          FlashClock
	absTracker     ABSTracker
	lastIn         TelemetryInput
	lastCarOrdinal int32
	hasIn          bool
	lastAt         time.Time

	// Edge tracking for zero-latency ObservePacket -> runLoop wakeup
	obsABS       bool
	obsHardBrake bool
	obsShifting  bool

	// Test preview animation state
	testMode  string
	testStart time.Time
	testDur   time.Duration

	// Computed live display state (always available for UI 12-lamp bar even if K6 not plugged in)
	curTarget ColorTarget
	litLamps  int
	rpmPct    int
	brakePct  int
	absActive bool
	hardBrake bool
	shifting  bool

	// Pending target color coalescing & latches (protected by mu)
	desired        *ColorTarget
	desiredVib     bool
	restoreReq     bool
	manualRestored bool
	idleRestored   bool
	forceSync      bool
	evalCh         chan struct{}
	wakeCh         chan struct{}
	stopCh         chan struct{}
	doneCh         chan struct{}
	started        bool
	stopped        bool

	// Hardware state (owned by pump goroutine / synchronized by hwMu)
	hwMu             sync.Mutex
	handle           syscall.Handle
	evR              syscall.Handle
	evW              syscall.Handle
	devicePath       string
	product          string
	backupPath       string
	backupDevicePath string
	backupRaw        []byte
	backupFormat     string
	backupReady      bool
	hwModified       bool
	lastSig          string
	lastWriteAt      time.Time
	errBackoffTo     time.Time
	writesOK         uint64
	writeErrs        uint64
	lastErr          string

	// Fine-grained handle mirror allowing safe cross-goroutine CancelIoEx without blocking on hwMu
	handleCancelMu sync.Mutex
	activeHandle   syscall.Handle
}

func NewController(cfg LightConfig) *Controller {
	cfg.Sanitize()
	return &Controller{
		cfg:          cfg,
		handle:       syscall.InvalidHandle,
		activeHandle: syscall.InvalidHandle,
		evR:          syscall.InvalidHandle,
		evW:          syscall.InvalidHandle,
		evalCh:       make(chan struct{}, 1),
		wakeCh:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		curTarget: ColorTarget{
			R: 12, G: 54, B: 128, Brightness: 22, Cue: "idle", Label: "待机 · 等待进入驾驶",
		},
	}
}

// SetBackupPath sets the on-disk crash-recovery backup path for the controller's original RAM 0x04 lighting.
// Call before Start().
func (c *Controller) SetBackupPath(path string) {
	c.hwMu.Lock()
	defer c.hwMu.Unlock()
	c.backupPath = path
}

func (c *Controller) Start() {
	c.mu.Lock()
	if c.started || c.stopped {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()
	go c.runLoop()
}

// Shutdown stops telemetry lighting, cancels in-flight HID I/O via CancelIoEx,
// restores the original backed-up APEX 6 LED preset, and closes the HID handle.
func (c *Controller) Shutdown() {
	c.mu.Lock()
	if !c.started {
		c.stopped = true
		c.mu.Unlock()
		return
	}
	if !c.stopped {
		c.stopped = true
		close(c.stopCh)
	}
	doneCh := c.doneCh
	c.mu.Unlock()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
	}
}

func (c *Controller) SetConfig(cfg LightConfig) {
	cfg.Sanitize()
	c.mu.Lock()
	wasEnabled := c.cfg.Enabled
	c.cfg = cfg
	c.forceSync = true
	if cfg.Enabled {
		c.manualRestored = false
		c.idleRestored = false
	} else if wasEnabled {
		c.restoreReq = true
		c.desired = nil
	}
	c.mu.Unlock()
	c.signalEval()
	c.signalWake()
}

func (c *Controller) Config() LightConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// TriggerTest starts a short visual & hardware LED preview animation (rpmSweep, hardBrake, absFlash, shiftFlash, stop).
func (c *Controller) TriggerTest(mode string) {
	if !IsValidTestMode(mode) {
		return
	}
	c.mu.Lock()
	if mode == "stop" || mode == "" {
		c.testMode = ""
	} else {
		c.manualRestored = false
		c.idleRestored = false
		c.testMode = mode
		c.testStart = time.Now()
		switch mode {
		case "rpmSweep":
			c.testDur = 3600 * time.Millisecond
		default:
			c.testDur = 2400 * time.Millisecond
		}
	}
	c.mu.Unlock()
	c.signalEval()
	c.signalWake()
}

// RequestRestore asks the hardware pump to immediately restore the backed-up original K6 lighting
// and pauses live telemetry LED overwrites until config re-enable or test trigger.
func (c *Controller) RequestRestore() {
	c.mu.Lock()
	c.testMode = ""
	c.manualRestored = true
	c.restoreReq = true
	c.desired = nil
	c.mu.Unlock()
	c.signalWake()
}

// ObservePacket feeds the latest Forza UDP packet + learned redline into the lighting engine.
// Safe to call at 60+ Hz in both Entertainment Mode and Pro Mode without blocking.
// Immediately wakes runLoop on cue-critical state transitions (race enter/exit, braking, ABS, redline).
func (c *Controller) ObservePacket(pkt telemetry.Packet, redlineRatio float64, isElectric bool) {
	c.mu.Lock()
	prevRace := c.hasIn && c.lastIn.PacketFresh && c.lastIn.Packet.IsRaceOn
	prevABS := c.obsABS
	prevHardBrake := c.obsHardBrake
	prevShifting := c.obsShifting

	hasRot := c.lastIn.HasWheelRot
	hasSusp := c.lastIn.HasSusp
	if pkt.CarOrdinal != 0 {
		if c.lastCarOrdinal != 0 && pkt.CarOrdinal != c.lastCarOrdinal {
			hasRot = false
			hasSusp = false
			c.absTracker.Reset()
		}
		c.lastCarOrdinal = pkt.CarOrdinal
	}
	for i := 0; i < 4; i++ {
		if pkt.WheelRotSpeed[i] > 1.0 || pkt.WheelRotSpeed[i] < -1.0 {
			hasRot = true
		}
		if pkt.NormSusp[i] > 0.01 {
			hasSusp = true
		}
	}
	now := time.Now()
	c.lastIn = TelemetryInput{
		Packet:       pkt,
		PacketFresh:  true,
		RedlineRatio: redlineRatio,
		IsElectric:   isElectric,
		HasWheelRot:  hasRot,
		HasSusp:      hasSusp,
	}
	c.hasIn = true
	c.lastAt = now
	absHit := c.absTracker.Update(c.lastIn, now)

	cfg := c.cfg
	newRace := pkt.IsRaceOn
	newABS := newRace && cfg.EnableABS && absHit
	brakeNorm := clampf(float64(pkt.Brake)/255.0, 0, 1)
	spd := float64(pkt.SpeedKmh)
	newHardBrake := newRace && cfg.EnableHardBrake && brakeNorm >= cfg.HardBrakeThreshold && spd > 3.0
	rpmNorm := effectiveRpmNorm(pkt, isElectric)
	shiftRatio := effectiveShiftRatio(redlineRatio, cfg.ShiftRatioOverride)
	newShifting := newRace && cfg.EnableShiftFlash && !isElectric && pkt.Gear >= 1 && spd > 10.0 && rpmNorm >= shiftRatio

	c.obsABS = newABS
	c.obsHardBrake = newHardBrake
	c.obsShifting = newShifting

	urgentEdge := (newRace != prevRace) || (newABS != prevABS) || (newHardBrake != prevHardBrake) || (newShifting != prevShifting)
	c.mu.Unlock()

	if urgentEdge {
		c.signalEval()
	}
}

// Snapshot returns the current live LED state for the Web Studio UI.
func (c *Controller) Snapshot() LightSnapshot {
	c.mu.Lock()
	cfg := c.cfg
	t := c.curTarget
	lit := c.litLamps
	rpmP := c.rpmPct
	brkP := c.brakePct
	absA := c.absActive
	hb := c.hardBrake
	sh := c.shifting
	tm := c.testMode
	c.mu.Unlock()

	c.hwMu.Lock()
	conn := c.handle != syscall.InvalidHandle && c.handle != 0
	prod := c.product
	bkReady := c.backupReady
	bkLen := len(c.backupRaw)
	bkFmt := c.backupFormat
	wOK := c.writesOK
	wErr := c.writeErrs
	lErr := c.lastErr
	c.hwMu.Unlock()

	return LightSnapshot{
		Config:       cfg,
		Connected:    conn,
		Product:      prod,
		BackupReady:  bkReady,
		BackupBytes:  bkLen,
		BackupFormat: bkFmt,
		R:            t.R,
		G:            t.G,
		B:            t.B,
		Brightness:   t.Brightness,
		Cue:          t.Cue,
		Label:        t.Label,
		Alert:        t.Alert,
		LitLamps:     lit,
		RpmPct:       rpmP,
		BrakePct:     brkP,
		AbsActive:    absA,
		HardBrake:    hb,
		Shifting:     sh,
		TestMode:     tm,
		WritesOK:     wOK,
		WriteErrs:    wErr,
		LastError:    lErr,
	}
}

func (c *Controller) signalEval() {
	select {
	case c.evalCh <- struct{}{}:
	default:
	}
}

func (c *Controller) signalWake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *Controller) cancelInFlightIO() {
	c.handleCancelMu.Lock()
	h := c.activeHandle
	if h != syscall.InvalidHandle && h != 0 {
		procCancelIoEx.Call(uintptr(h), 0)
	}
	c.handleCancelMu.Unlock()
}

func (c *Controller) runLoop() {
	defer close(c.doneCh)
	defer c.restoreAndCloseHW()

	// 15ms evaluation ticker (~66.7 Hz) paired with edge-triggered evalCh wakeup for near-zero cue onset lag.
	evalTicker := time.NewTicker(evalTickInterval)
	defer evalTicker.Stop()

	// Separate goroutine pumps queued 0xA4 HID transactions serially so the evaluation/clock loop never stalls.
	hwStop := make(chan struct{})
	hwDone := make(chan struct{})
	go func() {
		defer close(hwDone)
		c.hwPumpLoop(hwStop)
	}()

	for {
		select {
		case <-c.stopCh:
			close(hwStop)
			// Cancel any blocking overlapped HID ReadFile/WriteFile in hwPumpLoop BEFORE waiting for hwDone,
			// so restoreAndCloseHW() always runs promptly and never gets cancelled itself.
			c.cancelInFlightIO()
			<-hwDone
			return
		case <-evalTicker.C:
			c.evaluateStep(time.Now())
		case <-c.evalCh:
			c.evaluateStep(time.Now())
		}
	}
}

func (c *Controller) evaluateStep(now time.Time) {
	c.mu.Lock()
	cfg := c.cfg
	in := c.lastIn
	if !c.hasIn || now.Sub(c.lastAt) > stalePacketTimeout {
		in.PacketFresh = false
		c.absTracker.Reset()
		c.obsABS = false
		c.obsHardBrake = false
		c.obsShifting = false
	}
	testMode := c.testMode
	if testMode != "" && now.Sub(c.testStart) >= c.testDur {
		c.testMode = ""
		testMode = ""
	}

	prevShifting := c.shifting
	prevShiftCue := c.curTarget.Cue == "shift-up"

	var target ColorTarget
	var lit int
	var absA, hb, sh bool
	var rpmPct, brakePct int

	if testMode != "" {
		u := clampf(float64(now.Sub(c.testStart))/float64(c.testDur), 0, 1)
		switch testMode {
		case "rpmSweep":
			if u < 0.80 {
				rpmNorm := u / 0.80
				_, _, _ = c.clock.Advance(now, 0)
				target = RPMPaletteColor(rpmNorm, cfg.BrightnessScale)
				target.Label = "试灯 · 转速扫频"
				lit = int(clampf(float64(int(rpmNorm*12+0.99)), 1, 12))
				rpmPct = int(rpmNorm * 100)
			} else {
				_, _, shiftOn := c.clock.Advance(now, 0)
				if !prevShifting {
					c.clock.ResetShift()
					shiftOn = true
				}
				sh = true
				rpmPct = 96
				lit = 12
				if shiftOn {
					target = ColorTarget{R: 255, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线爆闪", Alert: true, Urgent: true}
				} else {
					target = ColorTarget{R: 255, G: 255, B: 255, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线爆闪", Alert: true, Urgent: true}
				}
			}
		case "hardBrake":
			brakeNorm := clampf(0.45+0.55*u, 0, 1)
			_, brkOn, _ := c.clock.Advance(now, brakeNorm)
			hb = true
			brakePct = int(brakeNorm * 100)
			if brkOn {
				target = ColorTarget{R: 255, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "brake", Label: "试灯 · 急刹变频闪红", Alert: true, Urgent: true}
				lit = 12
			} else {
				target = ColorTarget{R: 0, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "brake", Label: "试灯 · 急刹闪烁", Alert: true, Urgent: true}
				lit = 0
			}
		case "absFlash":
			stdOn, _, _ := c.clock.Advance(now, 0.85)
			absA = true
			brakePct = 85
			if stdOn {
				target = ColorTarget{R: 80, G: 170, B: 255, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "abs", Label: "试灯 · ABS 冰蓝爆闪", Alert: true, Urgent: true}
				lit = 12
			} else {
				target = ColorTarget{R: 0, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "abs", Label: "试灯 · ABS 闪烁", Alert: true, Urgent: true}
				lit = 0
			}
		case "shiftFlash":
			_, _, shiftOn := c.clock.Advance(now, 0)
			if !prevShifting {
				c.clock.ResetShift()
				shiftOn = true
			}
			sh = true
			rpmPct = 95
			lit = 12
			if shiftOn {
				target = ColorTarget{R: 255, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线换挡", Alert: true, Urgent: true}
			} else {
				target = ColorTarget{R: 255, G: 255, B: 255, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线换挡", Alert: true, Urgent: true}
			}
		}
	} else {
		brakeNorm := 0.0
		if in.PacketFresh && in.Packet.IsRaceOn {
			brakeNorm = clampf(float64(in.Packet.Brake)/255.0, 0, 1)
			rpmPct = int(clampf(effectiveRpmNorm(in.Packet, in.IsElectric)*100.0, 0, 100))
			brakePct = int(brakeNorm * 100)
		}
		stdOn, brkOn, shiftOn := c.clock.Advance(now, brakeNorm)
		absDetected := c.absTracker.IsActive(in, now)
		target, lit, absA, hb, sh = EvaluateTelemetryWithABS(in, cfg, absDetected, stdOn, brkOn, shiftOn)
		if (sh && !prevShifting) || (target.Cue == "shift-up" && !prevShiftCue) {
			c.clock.ResetShift()
			if target.Cue == "shift-up" {
				target, lit, absA, hb, sh = EvaluateTelemetryWithABS(in, cfg, absDetected, stdOn, brkOn, true)
			}
		}
	}

	c.curTarget = target
	c.litLamps = lit
	c.rpmPct = rpmPct
	c.brakePct = brakePct
	c.absActive = absA
	c.hardBrake = hb
	c.shifting = sh

	if cfg.Enabled && !c.manualRestored {
		if target.Cue == "idle" && cfg.AutoRestoreOnIdle && testMode == "" {
			c.desired = nil
			if !c.idleRestored {
				c.restoreReq = true
			}
		} else {
			c.idleRestored = false
			cp := target
			c.desired = &cp
			c.desiredVib = cfg.VibrationLink
		}
	} else if c.manualRestored {
		c.desired = nil
	}
	c.mu.Unlock()
	c.signalWake()
}

func (c *Controller) hwPumpLoop(hwStop <-chan struct{}) {
	reconnectTicker := time.NewTicker(1500 * time.Millisecond)
	defer reconnectTicker.Stop()

	for {
		select {
		case <-hwStop:
			return
		case <-reconnectTicker.C:
			c.stepHWOnce(hwStop)
		case <-c.wakeCh:
			c.stepHWOnce(hwStop)
		}
	}
}

func (c *Controller) stepHWOnce(cancelCh <-chan struct{}) {
	if isStopped(cancelCh) {
		return
	}

	c.mu.Lock()
	enabled := c.cfg.Enabled
	restoreReq := c.restoreReq
	c.restoreReq = false
	manualRestored := c.manualRestored
	forceSync := c.forceSync
	c.forceSync = false
	target := c.desired
	vib := c.desiredVib
	c.mu.Unlock()

	markIdleRestored := false

	c.hwMu.Lock()
	if forceSync {
		c.lastSig = ""
	}

	if !enabled {
		if c.handle != syscall.InvalidHandle && c.handle != 0 {
			if c.backupReady && len(c.backupRaw) > 0 && c.hwModified {
				_ = c.abortWriteLocked(cancelCh)
				if err := c.writeRawRAMLocked(c.backupRaw, cancelCh); err == nil {
					c.hwModified = false
					c.removeBackupFileLocked()
				}
			} else if !c.hwModified {
				c.removeBackupFileLocked()
			}
			c.closeHandleLocked()
			if !c.hwModified {
				c.backupRaw = nil
				c.backupFormat = ""
				c.backupReady = false
				c.backupDevicePath = ""
			}
		}
		c.hwMu.Unlock()
		return
	}

	if time.Now().Before(c.errBackoffTo) {
		c.hwMu.Unlock()
		return
	}

	// Ensure connected + original RAM 0x04 backed up safely
	if c.handle == syscall.InvalidHandle || c.handle == 0 {
		if err := c.connectAndBackupLocked(cancelCh); err != nil {
			if !errors.Is(err, errAborted) {
				c.lastErr = err.Error()
				c.errBackoffTo = time.Now().Add(2000 * time.Millisecond)
			}
			c.hwMu.Unlock()
			return
		}
	}

	if restoreReq {
		if c.backupReady && len(c.backupRaw) > 0 {
			if !c.hwModified && !manualRestored {
				// Untouched original preset on fresh boot idle: no redundant RAM write needed
				c.lastSig = ""
				c.lastErr = ""
				markIdleRestored = true
			} else {
				_ = c.abortWriteLocked(cancelCh)
				if err := c.writeRawRAMLocked(c.backupRaw, cancelCh); err != nil {
					if !errors.Is(err, errAborted) {
						c.writeErrs++
						c.lastErr = "恢复原灯效失败: " + err.Error()
						c.errBackoffTo = time.Now().Add(1500 * time.Millisecond)
						c.closeHandleLocked()
					}
				} else {
					c.hwModified = false
					c.lastSig = ""
					c.lastErr = ""
					c.removeBackupFileLocked()
					log.Printf("[LED ] Restored original APEX 6 lighting (%dB, %s)", len(c.backupRaw), c.backupFormat)
					if !manualRestored {
						markIdleRestored = true
					}
				}
			}
		}
		c.hwMu.Unlock()
		if markIdleRestored {
			c.mu.Lock()
			if c.desired == nil {
				c.idleRestored = true
			}
			c.mu.Unlock()
		}
		return
	}

	if !c.backupReady || target == nil || manualRestored {
		c.hwMu.Unlock()
		return
	}

	sig := fmt.Sprintf("%d,%d,%d,%d,v%t", target.R, target.G, target.B, target.Brightness, vib)
	if sig == c.lastSig {
		c.hwMu.Unlock()
		return
	}
	// Throttle non-urgent continuous RPM updates to >=150ms (~5-6 Hz max) to protect 0x57 1kHz haptics
	// on the shared 0xFFA0 bus; Urgent cues (ABS / Hard Brake / Redline Shift) fire with 0ms cooldown.
	if !target.Urgent && time.Since(c.lastWriteAt) < rpmWriteCooldown {
		c.hwMu.Unlock()
		return
	}

	if !c.hwModified {
		c.persistBackupLocked()
		c.hwModified = true
	}

	wrapped := WrapLightingBin(EncodeSolidLightingBin(target.R, target.G, target.B, target.Brightness, vib))
	if err := c.writeRawRAMLocked(wrapped, cancelCh); err != nil {
		if !errors.Is(err, errAborted) {
			c.writeErrs++
			c.lastErr = err.Error()
			c.errBackoffTo = time.Now().Add(1500 * time.Millisecond)
			// Close broken handle while preserving valid c.backupRaw for safe reconnect
			c.closeHandleLocked()
		}
		c.hwMu.Unlock()
		return
	}
	c.writesOK++
	c.lastSig = sig
	c.lastWriteAt = time.Now()
	c.lastErr = ""
	c.hwMu.Unlock()
}

func (c *Controller) restoreAndCloseHW() {
	c.hwMu.Lock()
	defer c.hwMu.Unlock()
	if c.handle != syscall.InvalidHandle && c.handle != 0 {
		if c.backupReady && len(c.backupRaw) > 0 {
			if c.hwModified {
				_ = c.abortWriteLocked(nil)
				if err := c.writeRawRAMLocked(c.backupRaw, nil); err == nil {
					c.hwModified = false
					c.removeBackupFileLocked()
					log.Printf("[LED ] Restored original APEX 6 lighting on shutdown (%dB)", len(c.backupRaw))
				} else {
					log.Printf("[LED ] Failed to restore original APEX 6 lighting on shutdown (backup preserved on disk): %v", err)
				}
			} else {
				c.removeBackupFileLocked()
			}
		}
		c.closeHandleLocked()
	} else if !c.hwModified {
		c.removeBackupFileLocked()
	}
	c.closeEventsLocked()
}

func (c *Controller) persistBackupLocked() {
	if c.backupPath == "" || len(c.backupRaw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.backupPath), 0755); err != nil {
		return
	}
	tmp := c.backupPath + ".tmp"
	if err := os.WriteFile(tmp, c.backupRaw, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.backupPath)
}

func (c *Controller) removeBackupFileLocked() {
	if c.backupPath == "" {
		return
	}
	_ = os.Remove(c.backupPath)
}

func (c *Controller) loadDiskBackupLocked() ([]byte, string, bool) {
	if c.backupPath == "" {
		return nil, "", false
	}
	raw, err := os.ReadFile(c.backupPath)
	if err != nil || len(raw) == 0 {
		return nil, "", false
	}
	fmtKind, err := ValidateRestorableSnapshot(raw)
	if err != nil {
		return nil, "", false
	}
	return raw, fmtKind, true
}

func (c *Controller) connectAndBackupLocked(cancelCh <-chan struct{}) error {
	if isStopped(cancelCh) {
		return errAborted
	}
	devs := hid.Enumerate33ByteDevices(false)
	var chosen *hid.DeviceInfo
	for i := range devs {
		if devs[i].VIDNum == FlydigiVID && devs[i].PIDNum == FlydigiK6PID && devs[i].UsagePageN == FlydigiConfigPage {
			chosen = &devs[i]
			break
		}
	}
	if chosen == nil {
		for i := range devs {
			if devs[i].VIDNum == FlydigiVID && devs[i].UsagePageN == FlydigiConfigPage {
				chosen = &devs[i]
				break
			}
		}
	}
	if chosen == nil {
		return errors.New("未检测到飞智 APEX 6 / K6 灯效接口 (0xFFA0)")
	}

	p16, err := syscall.UTF16PtrFromString(chosen.Path)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(
		p16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil || h == syscall.InvalidHandle {
		return fmt.Errorf("无法打开 K6 灯效通道: %v", err)
	}
	c.handle = h
	c.handleCancelMu.Lock()
	c.activeHandle = h
	c.handleCancelMu.Unlock()
	c.devicePath = chosen.Path
	c.product = chosen.Product

	// Clear any leftover A4 transaction before reading/writing RAM 0x04
	_ = c.abortWriteLocked(cancelCh)
	if !sleepInterruptible(25*time.Millisecond, cancelCh) {
		c.closeHandleLocked()
		return errAborted
	}

	// Case 1: Reconnecting to the same device after a transient write error — NEVER overwrite valid original backup!
	if c.backupReady && len(c.backupRaw) > 0 && c.backupDevicePath == chosen.Path {
		c.lastSig = ""
		c.lastErr = ""
		log.Printf("[LED ] Reconnected %s (%s:%s 0x%s) — preserved original RAM 0x04 backup (%dB, %s)",
			chosen.Product, chosen.VID, chosen.PID, chosen.UsagePage, len(c.backupRaw), c.backupFormat)
		return nil
	}

	// Case 2: Crash recovery — if an unrestored disk backup exists from a prior crashed session, recover it!
	if !c.backupReady {
		if diskRaw, diskFmt, ok := c.loadDiskBackupLocked(); ok {
			c.backupRaw = diskRaw
			c.backupFormat = diskFmt
			c.backupReady = true
			c.backupDevicePath = chosen.Path
			c.hwModified = true
			c.lastSig = ""
			c.lastErr = ""
			log.Printf("[LED ] Connected %s (%s:%s 0x%s) — recovered original RAM 0x04 backup from disk (%dB, %s)",
				chosen.Product, chosen.VID, chosen.PID, chosen.UsagePage, len(diskRaw), diskFmt)
			return nil
		}
	}

	// Case 3: Fresh connection — read & validate live RAM 0x04 and persist backup to disk
	raw, err := c.readLightingRAMLocked(cancelCh)
	if err != nil {
		c.closeHandleLocked()
		if errors.Is(err, errAborted) {
			return errAborted
		}
		return fmt.Errorf("保护模式：备份原灯效失败 (%v)", err)
	}
	fmtKind, err := ValidateRestorableSnapshot(raw)
	if err != nil {
		c.closeHandleLocked()
		return fmt.Errorf("保护模式：原灯效快照校验失败 (%v)", err)
	}
	c.backupRaw = raw
	c.backupFormat = fmtKind
	c.backupReady = true
	c.backupDevicePath = chosen.Path
	c.hwModified = false
	c.lastSig = ""
	c.lastErr = ""
	c.persistBackupLocked()
	log.Printf("[LED ] Connected %s (%s:%s 0x%s) & backed up RAM 0x04 (%dB, %s)",
		chosen.Product, chosen.VID, chosen.PID, chosen.UsagePage, len(raw), fmtKind)
	return nil
}

func (c *Controller) closeHandleLocked() {
	c.handleCancelMu.Lock()
	h := c.handle
	c.activeHandle = syscall.InvalidHandle
	c.handleCancelMu.Unlock()

	if h != syscall.InvalidHandle && h != 0 {
		procCancelIoEx.Call(uintptr(h), 0)
		_ = syscall.CloseHandle(h)
	}
	c.handle = syscall.InvalidHandle
	c.devicePath = ""
	c.lastSig = ""
}

func (c *Controller) ensureEventsLocked() error {
	if c.evR == syscall.InvalidHandle || c.evR == 0 {
		ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
		if ev == 0 {
			return fmt.Errorf("CreateEventW(read) failed: %v", err)
		}
		c.evR = syscall.Handle(ev)
	}
	if c.evW == syscall.InvalidHandle || c.evW == 0 {
		ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
		if ev == 0 {
			return fmt.Errorf("CreateEventW(write) failed: %v", err)
		}
		c.evW = syscall.Handle(ev)
	}
	return nil
}

func (c *Controller) closeEventsLocked() {
	if c.evR != syscall.InvalidHandle && c.evR != 0 {
		_ = syscall.CloseHandle(c.evR)
		c.evR = syscall.InvalidHandle
	}
	if c.evW != syscall.InvalidHandle && c.evW != 0 {
		_ = syscall.CloseHandle(c.evW)
		c.evW = syscall.InvalidHandle
	}
}

func cancelAndWaitOverlapped(h syscall.Handle, ov *syscall.Overlapped, n *uint32) {
	if h == syscall.InvalidHandle || h == 0 {
		return
	}
	procCancelIoEx.Call(uintptr(h), uintptr(unsafe.Pointer(ov)))
	// Mandatory blocking GetOverlappedResult(..., TRUE) guarantees kernel has finished touching ov/buf/n
	procGetOverlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(n)), 1)
}

// drainInputLocked clears stale queued HID input reports before starting a new 0xA3/0xA4 transaction.
func (c *Controller) drainInputLocked(maxReports int) {
	if c.handle == syscall.InvalidHandle || c.handle == 0 {
		return
	}
	if err := c.ensureEventsLocked(); err != nil {
		return
	}
	buf := make([]byte, 33)
	for i := 0; i < maxReports; i++ {
		procResetEvent.Call(uintptr(c.evR))
		ov := syscall.Overlapped{HEvent: c.evR}
		var n uint32
		rErr := syscall.ReadFile(c.handle, buf, &n, &ov)
		if rErr == nil {
			continue
		}
		if rErr != syscall.ERROR_IO_PENDING {
			return
		}
		wRes, _, _ := procWaitForSingleObject.Call(uintptr(c.evR), 0)
		if wRes == 0 {
			var got uint32
			ok, _, _ := procGetOverlappedResult.Call(uintptr(c.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&got)), 1)
			runtime.KeepAlive(buf)
			runtime.KeepAlive(&ov)
			if ok != 0 && got > 0 {
				continue
			}
			return
		}
		cancelAndWaitOverlapped(c.handle, &ov, &n)
		runtime.KeepAlive(buf)
		runtime.KeepAlive(&ov)
		return
	}
}

func (c *Controller) readLightingRAMLocked(cancelCh <-chan struct{}) ([]byte, error) {
	c.drainInputLocked(32)
	totalLen := 0
	var out []byte
	maxChunks := (MaxLightingRAMSize + RAMReadChunkSize - 1) / RAMReadChunkSize
	for ci := 0; totalLen == 0 || ci*RAMReadChunkSize < totalLen; ci++ {
		if ci > maxChunks {
			return nil, errors.New("灯光 RAM 读取分块过多")
		}
		resp, err := c.requestLocked(CmdReadRAM, []byte{0x00, LEDRAMID, byte(ci), RAMReadChunkSize}, 1200*time.Millisecond, cancelCh)
		if err != nil {
			return nil, err
		}
		ch, err := ParseReadRAMChunk(resp, byte(ci))
		if err != nil {
			return nil, err
		}
		if totalLen == 0 {
			totalLen = ch.TotalLength
			out = make([]byte, totalLen)
		} else if ch.TotalLength != totalLen {
			return nil, fmt.Errorf("灯光 RAM 长度变化: %d/%d", ch.TotalLength, totalLen)
		}
		off := ci * RAMReadChunkSize
		expLen := RAMReadChunkSize
		if totalLen-off < expLen {
			expLen = totalLen - off
		}
		if len(ch.Data) != expLen {
			return nil, fmt.Errorf("分块 %d 长度不一致: %d/%d", ci, len(ch.Data), expLen)
		}
		copy(out[off:], ch.Data)
	}
	return out, nil
}

func (c *Controller) abortWriteLocked(cancelCh <-chan struct{}) error {
	_, err := c.requestLocked(CmdWriteRAM, []byte{0x03}, 500*time.Millisecond, cancelCh)
	return err
}

func (c *Controller) writeRawRAMLocked(wrapped []byte, cancelCh <-chan struct{}) error {
	if err := c.writeWrappedLocked(wrapped, 16, cancelCh); err == nil {
		return nil
	} else {
		if errors.Is(err, errAborted) || isStopped(cancelCh) {
			return errAborted
		}
		_ = c.abortWriteLocked(cancelCh)
		if !sleepInterruptible(100*time.Millisecond, cancelCh) {
			return errAborted
		}
		if err2 := c.writeWrappedLocked(wrapped, 8, cancelCh); err2 != nil {
			if errors.Is(err2, errAborted) || isStopped(cancelCh) {
				return errAborted
			}
			_ = c.abortWriteLocked(cancelCh)
			return fmt.Errorf("%v (8B重试失败; 首次: %v)", err2, err)
		}
		return nil
	}
}

func (c *Controller) writeWrappedLocked(wrapped []byte, chunkSize int, cancelCh <-chan struct{}) error {
	// Drain any stale queued input reports before BEGIN to prevent ACK desync
	c.drainInputLocked(32)

	c32 := CRC32IEEE(wrapped)
	begin := make([]byte, 8)
	begin[0] = 0x00
	begin[1] = LEDRAMID
	binary.LittleEndian.PutUint16(begin[2:4], uint16(len(wrapped)))
	binary.LittleEndian.PutUint32(begin[4:8], c32)
	if _, err := c.requestA4StatusLocked(begin, "灯光写入开始失败", cancelCh); err != nil {
		return err
	}
	if !sleepInterruptible(10*time.Millisecond, cancelCh) {
		return errAborted
	}

	for off := 0; off < len(wrapped); off += chunkSize {
		end := off + chunkSize
		if end > len(wrapped) {
			end = len(wrapped)
		}
		ch := wrapped[off:end]
		pl := make([]byte, 4+len(ch))
		pl[0] = 0x01
		binary.LittleEndian.PutUint16(pl[1:3], uint16(off))
		pl[3] = byte(len(ch))
		copy(pl[4:], ch)
		if _, err := c.requestA4StatusLocked(pl, fmt.Sprintf("灯光分块写入失败(%d)", off), cancelCh); err != nil {
			return err
		}
		if !sleepInterruptible(4*time.Millisecond, cancelCh) {
			return errAborted
		}
	}

	commit := make([]byte, 7)
	commit[0] = 0x02
	binary.LittleEndian.PutUint16(commit[1:3], uint16(len(wrapped)))
	binary.LittleEndian.PutUint32(commit[3:7], c32)
	ack, err := c.requestA4StatusLocked(commit, "灯光提交失败", cancelCh)
	if err != nil {
		return err
	}
	// Strictly require >= 10B COMMIT ACK so shorter stale BEGIN/CHUNK ACKs are rejected
	if len(ack) < 10 {
		return fmt.Errorf("灯光提交回包过短 (%dB)", len(ack))
	}
	finalLen := int(binary.LittleEndian.Uint16(ack[4:6]))
	finalCRC := binary.LittleEndian.Uint32(ack[6:10])
	if finalLen != 0 && finalLen != len(wrapped) {
		return fmt.Errorf("提交长度不一致 %d/%d", finalLen, len(wrapped))
	}
	if finalCRC != 0 && finalCRC != c32 {
		return fmt.Errorf("提交 CRC32 不一致 0x%x/0x%x", finalCRC, c32)
	}
	return nil
}

func (c *Controller) requestA4StatusLocked(payload []byte, stage string, cancelCh <-chan struct{}) ([]byte, error) {
	var status byte = 255
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			c.drainInputLocked(16)
		}
		ack, err := c.requestLocked(CmdWriteRAM, payload, 900*time.Millisecond, cancelCh)
		if err != nil {
			if errors.Is(err, errAborted) {
				return nil, errAborted
			}
			return nil, fmt.Errorf("%s: %v", stage, err)
		}
		if len(ack) < 2 {
			return nil, fmt.Errorf("%s: 回包过短 (%dB)", stage, len(ack))
		}
		status = ack[1]
		if status == 0 {
			return ack, nil
		}
		if status != 1 || attempt == 3 {
			break
		}
		if !sleepInterruptible(time.Duration(45*(attempt+1))*time.Millisecond, cancelCh) {
			return nil, errAborted
		}
	}
	return nil, fmt.Errorf("%s (status=%d)", stage, status)
}

func (c *Controller) requestLocked(cmd byte, payload []byte, timeout time.Duration, cancelCh <-chan struct{}) ([]byte, error) {
	if isStopped(cancelCh) {
		return nil, errAborted
	}
	if c.handle == syscall.InvalidHandle || c.handle == 0 {
		return nil, errors.New("K6 灯效通道未连接")
	}
	if err := c.ensureEventsLocked(); err != nil {
		return nil, err
	}
	tx, err := BuildReport33(0x00, cmd, payload)
	if err != nil {
		return nil, err
	}

	// Arm overlapped read before write so fast firmware ACK is never missed.
	inBuf := make([]byte, 33)
	procResetEvent.Call(uintptr(c.evR))
	ovR := syscall.Overlapped{HEvent: c.evR}
	var nR uint32
	rErr := syscall.ReadFile(c.handle, inBuf, &nR, &ovR)
	readPending := false
	if rErr == syscall.ERROR_IO_PENDING {
		readPending = true
	} else if rErr != nil {
		return nil, fmt.Errorf("HID ReadFile 失败: %v", rErr)
	}

	// Always cancel + wait via CancelIoEx + GetOverlappedResult(..., TRUE) if ReadFile is still pending on exit.
	defer func() {
		if readPending {
			cancelAndWaitOverlapped(c.handle, &ovR, &nR)
			readPending = false
		}
		runtime.KeepAlive(inBuf)
		runtime.KeepAlive(&ovR)
		runtime.KeepAlive(&nR)
	}()

	procResetEvent.Call(uintptr(c.evW))
	ovW := syscall.Overlapped{HEvent: c.evW}
	var nW uint32
	wErr := syscall.WriteFile(c.handle, tx[:], &nW, &ovW)
	if wErr == syscall.ERROR_IO_PENDING {
		writeDeadline := time.Now().Add(600 * time.Millisecond)
		writeDone := false
		for time.Now().Before(writeDeadline) {
			if isStopped(cancelCh) {
				cancelAndWaitOverlapped(c.handle, &ovW, &nW)
				runtime.KeepAlive(&tx)
				runtime.KeepAlive(&ovW)
				runtime.KeepAlive(&nW)
				return nil, errAborted
			}
			remMs := uint32(time.Until(writeDeadline).Milliseconds())
			if remMs > 50 {
				remMs = 50
			}
			if remMs < 5 {
				remMs = 5
			}
			wRes, _, _ := procWaitForSingleObject.Call(uintptr(c.evW), uintptr(remMs))
			if wRes == 0 {
				ok, _, gErr := procGetOverlappedResult.Call(uintptr(c.handle), uintptr(unsafe.Pointer(&ovW)), uintptr(unsafe.Pointer(&nW)), 1)
				runtime.KeepAlive(&tx)
				runtime.KeepAlive(&ovW)
				runtime.KeepAlive(&nW)
				if ok == 0 {
					if isStopped(cancelCh) {
						return nil, errAborted
					}
					return nil, fmt.Errorf("HID WriteFile 失败: %v", gErr)
				}
				writeDone = true
				break
			}
		}
		if !writeDone {
			cancelAndWaitOverlapped(c.handle, &ovW, &nW)
			runtime.KeepAlive(&tx)
			runtime.KeepAlive(&ovW)
			runtime.KeepAlive(&nW)
			if isStopped(cancelCh) {
				return nil, errAborted
			}
			return nil, errors.New("HID WriteFile 超时")
		}
	} else if wErr != nil {
		runtime.KeepAlive(&tx)
		return nil, fmt.Errorf("HID WriteFile 失败: %v", wErr)
	}

	deadline := time.Now().Add(timeout)
	for {
		if isStopped(cancelCh) {
			return nil, errAborted
		}
		if !readPending {
			if nR > 0 {
				if resp := ParseResponse(inBuf[:nR], cmd); resp != nil {
					return resp, nil
				}
			}
		} else {
			if !time.Now().Before(deadline) {
				break
			}
			remMs := uint32(time.Until(deadline).Milliseconds())
			if remMs > 50 {
				remMs = 50
			}
			if remMs < 5 {
				remMs = 5
			}
			wRes, _, _ := procWaitForSingleObject.Call(uintptr(c.evR), uintptr(remMs))
			if wRes != 0 {
				continue
			}
			ok, _, gErr := procGetOverlappedResult.Call(uintptr(c.handle), uintptr(unsafe.Pointer(&ovR)), uintptr(unsafe.Pointer(&nR)), 1)
			readPending = false
			if ok == 0 {
				if isStopped(cancelCh) {
					return nil, errAborted
				}
				return nil, fmt.Errorf("HID ReadFile 失败: %v", gErr)
			}
			if nR > 0 {
				if resp := ParseResponse(inBuf[:nR], cmd); resp != nil {
					return resp, nil
				}
			}
		}

		if !time.Now().Before(deadline) {
			break
		}
		procResetEvent.Call(uintptr(c.evR))
		ovR = syscall.Overlapped{HEvent: c.evR}
		nR = 0
		rErr = syscall.ReadFile(c.handle, inBuf, &nR, &ovR)
		if rErr == syscall.ERROR_IO_PENDING {
			readPending = true
		} else if rErr == nil {
			readPending = false
		} else {
			if isStopped(cancelCh) {
				return nil, errAborted
			}
			return nil, fmt.Errorf("HID ReadFile 失败: %v", rErr)
		}
	}
	if isStopped(cancelCh) {
		return nil, errAborted
	}
	return nil, fmt.Errorf("命令 0x%02x 响应超时", cmd)
}

func isStopped(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func sleepInterruptible(d time.Duration, cancelCh <-chan struct{}) bool {
	if d <= 0 {
		return !isStopped(cancelCh)
	}
	if cancelCh == nil {
		time.Sleep(d)
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-cancelCh:
		return false
	case <-t.C:
		return true
	}
}
