//go:build windows

package lighting

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
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
	procWaitForSingleObject = modKernel32.NewProc("WaitForSingleObject")
	procGetOverlappedResult = modKernel32.NewProc("GetOverlappedResult")
	procCancelIo            = modKernel32.NewProc("CancelIo")
)

// Controller manages Flydigi K6 / APEX 6 telemetry lighting over a dedicated overlapped HID handle
// to the 0xFFA0 interface, running completely in parallel with the 1 kHz haptic stream AND Pro Mode V4!
type Controller struct {
	mu sync.Mutex

	cfg     LightConfig
	clock   FlashClock
	lastIn  TelemetryInput
	hasIn   bool
	lastAt  time.Time

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

	// Hardware state (owned by pump goroutine / synchronized by hwMu)
	hwMu         sync.Mutex
	handle       syscall.Handle
	devicePath   string
	product      string
	backupRaw    []byte
	backupFormat string
	backupReady  bool
	lastSig      string
	lastWriteAt  time.Time
	errBackoffTo time.Time
	writesOK     uint64
	writeErrs    uint64
	lastErr      string
	idleRestored bool

	// Pending target color coalescing
	desired    *ColorTarget
	desiredVib bool
	restoreReq bool
	wakeCh     chan struct{}
	stopCh     chan struct{}
	doneCh     chan struct{}
	started    bool
}

func NewController(cfg LightConfig) *Controller {
	cfg.Sanitize()
	return &Controller{
		cfg:    cfg,
		handle: syscall.InvalidHandle,
		wakeCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		curTarget: ColorTarget{
			R: 12, G: 54, B: 128, Brightness: 22, Cue: "idle", Label: "待机 · 等待进入驾驶",
		},
	}
}

func (c *Controller) Start() {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()
	go c.runLoop()
}

// Shutdown stops telemetry lighting, restores the original backed-up APEX 6 LED preset, and closes the HID handle.
func (c *Controller) Shutdown() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	c.started = false
	close(c.stopCh)
	c.mu.Unlock()

	select {
	case <-c.doneCh:
	case <-time.After(3 * time.Second):
	}
}

func (c *Controller) SetConfig(cfg LightConfig) {
	cfg.Sanitize()
	c.mu.Lock()
	wasEnabled := c.cfg.Enabled
	c.cfg = cfg
	c.lastSig = ""
	if wasEnabled && !cfg.Enabled {
		c.restoreReq = true
		c.desired = nil
	}
	c.mu.Unlock()
	c.signalWake()
}

func (c *Controller) Config() LightConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// TriggerTest starts a short visual & hardware LED preview animation (rpmSweep, hardBrake, absFlash, shiftFlash, stop).
func (c *Controller) TriggerTest(mode string) {
	c.mu.Lock()
	if mode == "stop" || mode == "" {
		c.testMode = ""
	} else {
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
	c.signalWake()
}

// RequestRestore asks the hardware pump to immediately restore the backed-up original K6 lighting.
func (c *Controller) RequestRestore() {
	c.mu.Lock()
	c.testMode = ""
	c.restoreReq = true
	c.desired = nil
	c.mu.Unlock()
	c.signalWake()
}

// ObservePacket feeds the latest Forza UDP packet + learned redline into the lighting engine.
// Safe to call at 60+ Hz in both Entertainment Mode and Pro Mode without blocking.
func (c *Controller) ObservePacket(pkt telemetry.Packet, redlineRatio float64, isElectric bool) {
	c.mu.Lock()
	hasRot := c.lastIn.HasWheelRot
	for i := 0; i < 4; i++ {
		if pkt.WheelRotSpeed[i] > 1.0 || pkt.WheelRotSpeed[i] < -1.0 {
			hasRot = true
			break
		}
	}
	c.lastIn = TelemetryInput{
		Packet:       pkt,
		PacketFresh:  true,
		RedlineRatio: redlineRatio,
		IsElectric:   isElectric,
		HasWheelRot:  hasRot,
	}
	c.hasIn = true
	c.lastAt = time.Now()
	c.mu.Unlock()
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

func (c *Controller) signalWake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *Controller) runLoop() {
	defer close(c.doneCh)
	defer c.restoreAndCloseHW()

	// Dedicated evaluation ticker (25ms = 40Hz) keeps flash clocks crisp and responsive
	evalTicker := time.NewTicker(25 * time.Millisecond)
	defer evalTicker.Stop()

	// Separate goroutine pumps queued 0xA4 HID transactions serially so the 25ms UI/clock loop never stalls
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
			<-hwDone
			return
		case <-evalTicker.C:
			c.evaluateStep(time.Now())
		}
	}
}

func (c *Controller) evaluateStep(now time.Time) {
	c.mu.Lock()
	cfg := c.cfg
	in := c.lastIn
	if !c.hasIn || now.Sub(c.lastAt) > 1500*time.Millisecond {
		in.PacketFresh = false
	}
	testMode := c.testMode
	if testMode != "" && now.Sub(c.testStart) >= c.testDur {
		c.testMode = ""
		testMode = ""
	}

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
				stdOn, _ := c.clock.Advance(now, 0)
				_ = stdOn
				target = RPMPaletteColor(rpmNorm, cfg.BrightnessScale)
				target.Label = "试灯 · 转速扫频"
				lit = int(clampf(float64(int(rpmNorm*12+0.99)), 1, 12))
				rpmPct = int(rpmNorm * 100)
			} else {
				stdOn, _ := c.clock.Advance(now, 0)
				sh = true
				rpmPct = 96
				lit = 12
				if stdOn {
					target = ColorTarget{R: 255, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线爆闪", Alert: true, Urgent: true}
				} else {
					target = ColorTarget{R: 255, G: 255, B: 255, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线爆闪", Alert: true, Urgent: true}
				}
			}
		case "hardBrake":
			brakeNorm := clampf(0.45+0.55*u, 0, 1)
			_, brkOn := c.clock.Advance(now, brakeNorm)
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
			stdOn, _ := c.clock.Advance(now, 0.85)
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
			stdOn, _ := c.clock.Advance(now, 0)
			sh = true
			rpmPct = 95
			lit = 12
			if stdOn {
				target = ColorTarget{R: 255, G: 0, B: 0, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线换挡", Alert: true, Urgent: true}
			} else {
				target = ColorTarget{R: 255, G: 255, B: 255, Brightness: scaleBrightness(100, cfg.BrightnessScale), Cue: "shift-up", Label: "试灯 · 红线换挡", Alert: true, Urgent: true}
			}
		}
	} else {
		brakeNorm := 0.0
		if in.PacketFresh && in.Packet.IsRaceOn {
			brakeNorm = clampf(float64(in.Packet.Brake)/255.0, 0, 1)
			rpmPct = int(clampf(float64(in.Packet.RpmRatio)*100, 0, 100))
			brakePct = int(brakeNorm * 100)
		}
		stdOn, brkOn := c.clock.Advance(now, brakeNorm)
		target, lit, absA, hb, sh = EvaluateTelemetry(in, cfg, stdOn, brkOn)
	}

	c.curTarget = target
	c.litLamps = lit
	c.rpmPct = rpmPct
	c.brakePct = brakePct
	c.absActive = absA
	c.hardBrake = hb
	c.shifting = sh

	if cfg.Enabled {
		if target.Cue == "idle" && cfg.AutoRestoreOnIdle && testMode == "" {
			if !c.idleRestored {
				c.restoreReq = true
				c.desired = nil
				c.idleRestored = true
			}
		} else {
			c.idleRestored = false
			cp := target
			c.desired = &cp
			c.desiredVib = cfg.VibrationLink
		}
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
			c.stepHWOnce()
		case <-c.wakeCh:
			c.stepHWOnce()
		}
	}
}

func (c *Controller) stepHWOnce() {
	c.mu.Lock()
	enabled := c.cfg.Enabled
	restoreReq := c.restoreReq
	c.restoreReq = false
	target := c.desired
	vib := c.desiredVib
	c.mu.Unlock()

	c.hwMu.Lock()
	defer c.hwMu.Unlock()

	if restoreReq {
		if c.handle != syscall.InvalidHandle && c.handle != 0 && c.backupReady && len(c.backupRaw) > 0 {
			_ = c.abortWriteLocked()
			if err := c.writeRawRAMLocked(c.backupRaw); err != nil {
				c.writeErrs++
				c.lastErr = "恢复原灯效失败: " + err.Error()
			} else {
				c.lastSig = ""
				c.lastErr = ""
				log.Printf("[LED ] Restored original APEX 6 lighting (%dB, %s)", len(c.backupRaw), c.backupFormat)
			}
		}
		if !enabled {
			c.closeHandleLocked()
		}
		return
	}

	if !enabled {
		if c.handle != syscall.InvalidHandle && c.handle != 0 {
			if c.backupReady && len(c.backupRaw) > 0 {
				_ = c.abortWriteLocked()
				_ = c.writeRawRAMLocked(c.backupRaw)
			}
			c.closeHandleLocked()
		}
		return
	}

	if time.Now().Before(c.errBackoffTo) {
		return
	}

	// Ensure connected + original RAM 0x04 backed up safely
	if c.handle == syscall.InvalidHandle || c.handle == 0 {
		if err := c.connectAndBackupLocked(); err != nil {
			c.lastErr = err.Error()
			c.errBackoffTo = time.Now().Add(2000 * time.Millisecond)
			return
		}
	}
	if !c.backupReady || target == nil {
		return
	}

	sig := fmt.Sprintf("%d,%d,%d,%d,v%t", target.R, target.G, target.B, target.Brightness, vib)
	if sig == c.lastSig {
		return
	}
	// Smooth non-urgent RPM updates are paced to >=55ms so HID channel stays light; urgent cues (ABS/Brake/Shift) fire immediately
	if !target.Urgent && time.Since(c.lastWriteAt) < 55*time.Millisecond {
		return
	}

	wrapped := WrapLightingBin(EncodeSolidLightingBin(target.R, target.G, target.B, target.Brightness, vib))
	if err := c.writeRawRAMLocked(wrapped); err != nil {
		c.writeErrs++
		c.lastErr = err.Error()
		c.errBackoffTo = time.Now().Add(1500 * time.Millisecond)
		// If handle broke (unplugged), close so reconnect picks it back up cleanly
		c.closeHandleLocked()
		return
	}
	c.writesOK++
	c.lastSig = sig
	c.lastWriteAt = time.Now()
	c.lastErr = ""
}

func (c *Controller) restoreAndCloseHW() {
	c.hwMu.Lock()
	defer c.hwMu.Unlock()
	if c.handle != syscall.InvalidHandle && c.handle != 0 {
		if c.backupReady && len(c.backupRaw) > 0 {
			_ = c.abortWriteLocked()
			if err := c.writeRawRAMLocked(c.backupRaw); err == nil {
				log.Printf("[LED ] Restored original APEX 6 lighting on shutdown (%dB)", len(c.backupRaw))
			}
		}
		c.closeHandleLocked()
	}
}

func (c *Controller) connectAndBackupLocked() error {
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
	c.devicePath = chosen.Path
	c.product = chosen.Product

	// Clear any leftover A4 transaction before reading RAM 0x04
	_ = c.abortWriteLocked()
	time.Sleep(25 * time.Millisecond)

	raw, err := c.readLightingRAMLocked()
	if err != nil {
		c.closeHandleLocked()
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
	c.lastSig = ""
	c.lastErr = ""
	log.Printf("[LED ] Connected %s (%s:%s 0x%s) & backed up RAM 0x04 (%dB, %s)",
		chosen.Product, chosen.VID, chosen.PID, chosen.UsagePage, len(raw), fmtKind)
	return nil
}

func (c *Controller) closeHandleLocked() {
	if c.handle != syscall.InvalidHandle && c.handle != 0 {
		procCancelIo.Call(uintptr(c.handle))
		_ = syscall.CloseHandle(c.handle)
	}
	c.handle = syscall.InvalidHandle
	c.devicePath = ""
	c.backupReady = false
	c.lastSig = ""
}

func (c *Controller) readLightingRAMLocked() ([]byte, error) {
	totalLen := 0
	var out []byte
	maxChunks := (MaxLightingRAMSize + RAMReadChunkSize - 1) / RAMReadChunkSize
	for ci := 0; totalLen == 0 || ci*RAMReadChunkSize < totalLen; ci++ {
		if ci > maxChunks {
			return nil, errors.New("灯光 RAM 读取分块过多")
		}
		resp, err := c.requestLocked(CmdReadRAM, []byte{0x00, LEDRAMID, byte(ci), RAMReadChunkSize}, 1200*time.Millisecond)
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

func (c *Controller) abortWriteLocked() error {
	_, err := c.requestLocked(CmdWriteRAM, []byte{0x03}, 500*time.Millisecond)
	return err
}

func (c *Controller) writeRawRAMLocked(wrapped []byte) error {
	if err := c.writeWrappedLocked(wrapped, 16); err == nil {
		return nil
	} else {
		_ = c.abortWriteLocked()
		time.Sleep(100 * time.Millisecond)
		if err2 := c.writeWrappedLocked(wrapped, 8); err2 != nil {
			_ = c.abortWriteLocked()
			return fmt.Errorf("%v (8B重试失败; 首次: %v)", err2, err)
		}
		return nil
	}
}

func (c *Controller) writeWrappedLocked(wrapped []byte, chunkSize int) error {
	c32 := CRC32IEEE(wrapped)
	begin := make([]byte, 8)
	begin[0] = 0x00
	begin[1] = LEDRAMID
	binary.LittleEndian.PutUint16(begin[2:4], uint16(len(wrapped)))
	binary.LittleEndian.PutUint32(begin[4:8], c32)
	if _, err := c.requestA4StatusLocked(begin, "灯光写入开始失败"); err != nil {
		return err
	}
	time.Sleep(10 * time.Millisecond)

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
		if _, err := c.requestA4StatusLocked(pl, fmt.Sprintf("灯光分块写入失败(%d)", off)); err != nil {
			return err
		}
		time.Sleep(4 * time.Millisecond)
	}

	commit := make([]byte, 7)
	commit[0] = 0x02
	binary.LittleEndian.PutUint16(commit[1:3], uint16(len(wrapped)))
	binary.LittleEndian.PutUint32(commit[3:7], c32)
	ack, err := c.requestA4StatusLocked(commit, "灯光提交失败")
	if err != nil {
		return err
	}
	if len(ack) >= 10 {
		finalLen := int(binary.LittleEndian.Uint16(ack[4:6]))
		finalCRC := binary.LittleEndian.Uint32(ack[6:10])
		if finalLen != 0 && finalLen != len(wrapped) {
			return fmt.Errorf("提交长度不一致 %d/%d", finalLen, len(wrapped))
		}
		if finalCRC != 0 && finalCRC != c32 {
			return fmt.Errorf("提交 CRC32 不一致 0x%x/0x%x", finalCRC, c32)
		}
	}
	return nil
}

func (c *Controller) requestA4StatusLocked(payload []byte, stage string) ([]byte, error) {
	var status byte = 255
	for attempt := 0; attempt < 4; attempt++ {
		ack, err := c.requestLocked(CmdWriteRAM, payload, 900*time.Millisecond)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", stage, err)
		}
		if len(ack) >= 2 {
			status = ack[1]
		}
		if status == 0 {
			return ack, nil
		}
		if status != 1 || attempt == 3 {
			break
		}
		time.Sleep(time.Duration(45*(attempt+1)) * time.Millisecond)
	}
	return nil, fmt.Errorf("%s (status=%d)", stage, status)
}

func (c *Controller) requestLocked(cmd byte, payload []byte, timeout time.Duration) ([]byte, error) {
	if c.handle == syscall.InvalidHandle || c.handle == 0 {
		return nil, errors.New("K6 灯效通道未连接")
	}
	tx, err := BuildReport33(0x00, cmd, payload)
	if err != nil {
		return nil, err
	}

	// Arm overlapped read before write so fast firmware ACK is never missed
	inBuf := make([]byte, 33)
	evR, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if evR == 0 {
		return nil, errors.New("CreateEventW(read) failed")
	}
	defer syscall.CloseHandle(syscall.Handle(evR))
	ovR := syscall.Overlapped{HEvent: syscall.Handle(evR)}
	var nR uint32
	rErr := syscall.ReadFile(c.handle, inBuf, &nR, &ovR)
	if rErr != nil && rErr != syscall.ERROR_IO_PENDING {
		return nil, fmt.Errorf("HID ReadFile 失败: %v", rErr)
	}

	evW, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if evW == 0 {
		procCancelIo.Call(uintptr(c.handle))
		return nil, errors.New("CreateEventW(write) failed")
	}
	defer syscall.CloseHandle(syscall.Handle(evW))
	ovW := syscall.Overlapped{HEvent: syscall.Handle(evW)}
	var nW uint32
	wErr := syscall.WriteFile(c.handle, tx[:], &nW, &ovW)
	if wErr == syscall.ERROR_IO_PENDING {
		wRes, _, _ := procWaitForSingleObject.Call(evW, 600)
		if wRes != 0 {
			procCancelIo.Call(uintptr(c.handle))
			return nil, errors.New("HID WriteFile 超时")
		}
		procGetOverlappedResult.Call(uintptr(c.handle), uintptr(unsafe.Pointer(&ovW)), uintptr(unsafe.Pointer(&nW)), 0)
	} else if wErr != nil {
		procCancelIo.Call(uintptr(c.handle))
		return nil, fmt.Errorf("HID WriteFile 失败: %v", wErr)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remMs := uint32(time.Until(deadline).Milliseconds())
		if remMs > 200 {
			remMs = 200
		}
		if remMs < 5 {
			remMs = 5
		}
		if rErr == nil {
			// Completed synchronously
			if resp := ParseResponse(inBuf[:nR], cmd); resp != nil {
				return resp, nil
			}
		} else {
			wRes, _, _ := procWaitForSingleObject.Call(evR, uintptr(remMs))
			if wRes == 0 {
				ok, _, _ := procGetOverlappedResult.Call(uintptr(c.handle), uintptr(unsafe.Pointer(&ovR)), uintptr(unsafe.Pointer(&nR)), 0)
				if ok != 0 && nR > 0 {
					if resp := ParseResponse(inBuf[:nR], cmd); resp != nil {
						return resp, nil
					}
				}
			} else {
				continue
			}
		}
		// Re-arm read for next input report
		ovR = syscall.Overlapped{HEvent: syscall.Handle(evR)}
		rErr = syscall.ReadFile(c.handle, inBuf, &nR, &ovR)
		if rErr != nil && rErr != syscall.ERROR_IO_PENDING {
			break
		}
	}
	procCancelIo.Call(uintptr(c.handle))
	return nil, fmt.Errorf("命令 0x%02x 响应超时", cmd)
}
