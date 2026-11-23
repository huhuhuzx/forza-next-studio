package lighting

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"forzanext/internal/telemetry"
)

func TestLightingCodecMatchesRealAPEX6Snapshot(t *testing.T) {
	// Exact 24-byte RAM 0x04 snapshot read live from Flydigi APEX 6 (37d7:2502):
	// solid warm gold (255, 231, 0), brightness 100%, vibrationLink=1
	realHex := "1800d1884c474854020006000000370101ffe70064014c02"
	got := WrapLightingBin(EncodeSolidLightingBin(255, 231, 0, 100, true))
	if hex.EncodeToString(got) != realHex {
		t.Fatalf("codec mismatch:\n got: %s\nwant: %s", hex.EncodeToString(got), realHex)
	}
	fmtKind, err := ValidateRestorableSnapshot(got)
	if err != nil || fmtKind != "wrapped-lght" {
		t.Fatalf("validate snapshot failed: fmt=%q err=%v", fmtKind, err)
	}
	// Direct LGHT without 4B wrapper
	fmtKind2, err := ValidateRestorableSnapshot(got[4:])
	if err != nil || fmtKind2 != "lght" {
		t.Fatalf("validate direct lght failed: fmt=%q err=%v", fmtKind2, err)
	}
	// Reject blank snapshots
	if _, err := ValidateRestorableSnapshot(make([]byte, 24)); err == nil {
		t.Fatal("expected all-zero snapshot to be rejected")
	}

	// Verify real 0xA3 reply parsing (including command-specific length=0 zero-padded fallback!)
	rx, _ := hex.DecodeString("005aa5a300000401180000101800d1884c474854020006000000370100000000b0")
	resp := ParseResponse(rx, CmdReadRAM)
	if resp == nil {
		t.Fatal("ParseResponse failed on real APEX 6 0xA3 reply")
	}
	ch, err := ParseReadRAMChunk(resp, 0)
	if err != nil || ch.TotalLength != 24 || len(ch.Data) != 16 {
		t.Fatalf("ParseReadRAMChunk failed: ch=%+v err=%v", ch, err)
	}
}

func TestForzaLightingPriorityAndPalette(t *testing.T) {
	cfg := DefaultLightConfig()

	// 1. Continuous RPM strip: low -> mid -> high
	low := RPMPaletteColor(0.05, 1.0)
	mid := RPMPaletteColor(0.58, 1.0)
	hi := RPMPaletteColor(0.98, 1.0)
	if low.B <= low.R || mid.G < 180 || hi.R < 240 {
		t.Fatalf("RPM palette wrong: low=%+v mid=%+v hi=%+v", low, mid, hi)
	}

	baseIn := TelemetryInput{
		Packet: telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 120, Gear: 4, CurrentRpm: 7500, EngineMaxRpm: 8000, RpmRatio: 0.94,
			WheelRotSpeed: [4]float32{65, 65, 65, 65},
			NormSusp:      [4]float32{0.45, 0.45, 0.40, 0.40},
		},
		PacketFresh:  true,
		RedlineRatio: 0.90,
		HasWheelRot:  true,
		HasSusp:      true,
	}

	// 2. Redline shift flash uses dedicated fast shiftOn phase (red on / white off)
	cShiftOn, lit, _, _, sh := EvaluateTelemetry(baseIn, cfg, false, true, true)
	cShiftOff, _, _, _, _ := EvaluateTelemetry(baseIn, cfg, true, true, false)
	if !sh || cShiftOn.Cue != "shift-up" || cShiftOn.R != 255 || cShiftOn.G != 0 || cShiftOff.G != 255 || lit != 12 {
		t.Fatalf("shift-up failed: on=%+v off=%+v lit=%d", cShiftOn, cShiftOff, lit)
	}

	// 3. Hard brake (within tire grip, no lockup) overrides shift-up -> Pure Red / Off strobe
	brkIn := baseIn
	brkIn.Packet.Brake = 160 // 62% > 35% threshold
	brkIn.Packet.SlipRatio = [4]float32{-0.45, -0.42, -0.25, -0.25}
	brkIn.Packet.CombinedSlip = [4]float32{0.50, 0.48, 0.30, 0.30}
	cBrkOn, _, absA, hb, _ := EvaluateTelemetry(brkIn, cfg, true, true, true)
	cBrkOff, _, _, _, _ := EvaluateTelemetry(brkIn, cfg, true, false, true)
	if absA || !hb || cBrkOn.Cue != "brake" || cBrkOn.R != 255 || cBrkOn.G != 0 || cBrkOff.R != 0 {
		t.Fatalf("hard brake failed: abs=%v hb=%v on=%+v off=%+v", absA, hb, cBrkOn, cBrkOff)
	}

	// 4. Genuine ABS (longitudinal braking slip crossing Forza's ~1.0 grip limit) overrides hard brake -> Ice-Blue strobe
	absIn := brkIn
	absIn.Packet.SlipRatio = [4]float32{-1.25, -0.98, -0.45, -0.45}
	absIn.Packet.CombinedSlip = [4]float32{1.26, 0.99, 0.48, 0.48}
	cAbsOn, _, absA2, _, _ := EvaluateTelemetry(absIn, cfg, true, true, true)
	cAbsOff, _, _, _, _ := EvaluateTelemetry(absIn, cfg, false, true, true)
	if !absA2 || cAbsOn.Cue != "abs" || cAbsOn.B != 255 || cAbsOn.R != 80 || cAbsOff.B != 0 {
		t.Fatalf("ABS lighting failed: abs=%v on=%+v off=%+v", absA2, cAbsOn, cAbsOff)
	}

	// 5. Light brake below hardBrakeThreshold (e.g. 15% < 35%) stays on RPM strip (no nuisance flashing!)
	lightBrk := baseIn
	lightBrk.Packet.RpmRatio = 0.55
	lightBrk.Packet.Brake = 38 // ~15%
	cLight, litL, absL, hbL, shL := EvaluateTelemetry(lightBrk, cfg, true, true, true)
	if absL || hbL || shL || cLight.Cue != "rpm" || litL != 7 {
		t.Fatalf("light brake should remain RPM strip: got=%+v lit=%d", cLight, litL)
	}
}

func TestRedlineShiftHarmonicCadenceAndResetShift(t *testing.T) {
	// Shift blink half-period must be 81ms (162ms full cycle = 6.17 Hz = exact 1/3 sub-harmonic
	// of grip redline's ~18.5 Hz fuel-cut), safely above 30-50ms APEX 6 0xA4 HID write latency,
	// and faster than the 120ms standard ABS strobe.
	if flashShiftHalfMs != 81.0 {
		t.Fatalf("expected flashShiftHalfMs=81.0ms (6.17Hz 1/3 sub-harmonic), got %.1fms", flashShiftHalfMs)
	}
	if flashShiftHalfMs >= flashStandardHalfMs {
		t.Fatalf("expected shift blink (%.1fms) faster than standard (%.1fms)", flashShiftHalfMs, flashStandardHalfMs)
	}
	if evalTickInterval != 15*time.Millisecond {
		t.Fatalf("expected evalTickInterval=15ms, got %v", evalTickInterval)
	}
	if stalePacketTimeout != 800*time.Millisecond {
		t.Fatalf("expected stalePacketTimeout=800ms, got %v", stalePacketTimeout)
	}
	if rpmWriteCooldown != 150*time.Millisecond {
		t.Fatalf("expected rpmWriteCooldown=150ms, got %v", rpmWriteCooldown)
	}

	var fc FlashClock
	t0 := time.Unix(1700000000, 0)
	stdPrev, _, shiftPrev := fc.Advance(t0, 0)
	if !shiftPrev {
		t.Fatal("expected FlashClock to start with shiftOn=true")
	}

	// Step at exact 15ms controller evaluation cadence for 720ms (48 ticks):
	// - standard (120ms half) toggles at 120, 240, 360, 480, 600, 720 -> 6 toggles
	// - shift (81ms half) toggles at 90, 165, 255, 330, 405, 495, 570, 660 -> 8 toggles (no 2:1 wobble)
	stdToggles := 0
	shiftToggles := 0
	for tick := 1; tick <= 48; tick++ {
		now := t0.Add(time.Duration(tick) * 15 * time.Millisecond)
		stdOn, _, shiftOn := fc.Advance(now, 0)
		if stdOn != stdPrev {
			stdToggles++
			stdPrev = stdOn
		}
		if shiftOn != shiftPrev {
			shiftToggles++
			shiftPrev = shiftOn
		}
	}
	if stdToggles != 6 {
		t.Fatalf("expected 6 standard toggles in 720ms, got %d", stdToggles)
	}
	if shiftToggles != 8 {
		t.Fatalf("expected 8 harmonic shift toggles in 720ms, got %d", shiftToggles)
	}

	// Verify ResetShift() immediately locks phase back to 0ms (Red ON) from mid-White phase
	var fc2 FlashClock
	fc2.Advance(t0, 0)
	_, _, sWhite := fc2.Advance(t0.Add(100*time.Millisecond), 0)
	if sWhite {
		t.Fatal("expected shiftOn=false (White) at 100ms (>81ms)")
	}
	fc2.ResetShift()
	_, _, sAfterReset := fc2.Advance(t0.Add(115*time.Millisecond), 0)
	if !sAfterReset {
		t.Fatal("expected shiftOn=true (Red ON) immediately after ResetShift() (+15ms < 81ms)")
	}
	_, _, sFlip := fc2.Advance(t0.Add(185*time.Millisecond), 0)
	if sFlip {
		t.Fatal("expected shiftOn=false (White) 85ms after ResetShift() (>=81ms)")
	}

	// End-to-end Controller onset test: entering redline while clock is in White half must output Red ON immediately
	ctrl := NewController(DefaultLightConfig())
	ctrl.clock.Advance(t0, 0)
	// Advance clock to 100ms (White half) while NOT shifting
	idlePkt := telemetry.Packet{
		IsRaceOn: true, CarOrdinal: 3173, SpeedKmh: 120, Gear: 4,
		CurrentRpm: 5000, EngineMaxRpm: 7500, RpmRatio: 5000.0 / 7500.0,
	}
	ctrl.ObservePacket(idlePkt, 0.8963, false)
	ctrl.mu.Lock()
	ctrl.lastAt = t0.Add(100 * time.Millisecond)
	ctrl.mu.Unlock()
	ctrl.evaluateStep(t0.Add(100 * time.Millisecond))
	if ctrl.Snapshot().Shifting {
		t.Fatal("expected not shifting at 5000 RPM")
	}

	// Now cross EffectiveRedlineRatio (0.8963 on Car 3173 = 6722 RPM) at +15ms (still in old White half at 115ms!)
	redPkt := idlePkt
	redPkt.CurrentRpm = 6750
	redPkt.RpmRatio = 6750.0 / 7500.0
	ctrl.ObservePacket(redPkt, 0.8963, false)
	ctrl.mu.Lock()
	ctrl.lastAt = t0.Add(115 * time.Millisecond)
	ctrl.mu.Unlock()
	ctrl.evaluateStep(t0.Add(115 * time.Millisecond))
	snap := ctrl.Snapshot()
	if !snap.Shifting || snap.Cue != "shift-up" || snap.R != 255 || snap.G != 0 || snap.B != 0 {
		t.Fatalf("expected immediate Red ON on redline onset, got %+v", snap)
	}
	// +90ms later (>81ms) must flip to bright White
	ctrl.mu.Lock()
	ctrl.lastAt = t0.Add(205 * time.Millisecond)
	ctrl.mu.Unlock()
	ctrl.evaluateStep(t0.Add(205 * time.Millisecond))
	snap2 := ctrl.Snapshot()
	if !snap2.Shifting || snap2.Cue != "shift-up" || snap2.R != 255 || snap2.G != 255 || snap2.B != 255 {
		t.Fatalf("expected White flash 90ms after redline onset, got %+v", snap2)
	}
}

func TestParseResponseDescendingScanZeroTail(t *testing.T) {
	// Construct a 33-byte 0xA3 reply (declaredLen=0 zero-padded format) whose last 4 chunk bytes are 0x00
	// AND whose overall frame checksum at byte 32 is 0x00.
	// An ascending scan would match early before the trailing 0x00 data bytes, truncating the 16B chunk!
	rx := make([]byte, 33)
	rx[0] = 0x00 // HID Report ID
	rx[1] = 0x5A
	rx[2] = 0xA5
	rx[3] = CmdReadRAM // 0xA3
	rx[4] = 0x00       // declaredLen = 0 (triggers fallback scan)
	rx[5] = 0x00       // status = OK
	rx[6] = LEDRAMID   // ramID = 0x04
	rx[7] = 0x01       // activeSlot
	rx[8] = 0x18       // totalLen lo = 24
	rx[9] = 0x00       // totalLen hi
	rx[10] = 0x00      // chunkIndex = 0
	rx[11] = 0x10      // chunkSize = 16
	// Fill first 11 data bytes with sample values, choose 12th byte so sum(rx[3..23]) == 0x00,
	// leaving last 4 data bytes (rx[24..27]) = 0x00, padding (rx[28..31]) = 0x00, checksum (rx[32]) = 0x00!
	sample := []byte{0x18, 0x00, 0xD1, 0x88, 0x4C, 0x47, 0x48, 0x54, 0x02, 0x00, 0x06}
	copy(rx[12:23], sample)
	var sum byte
	for i := 3; i < 23; i++ {
		sum += rx[i]
	}
	rx[23] = byte(-sum) // makes sum(rx[3..23]) == 0x00, and sum(rx[3..31]) == 0x00 == rx[32]

	resp := ParseResponse(rx, CmdReadRAM)
	if resp == nil {
		t.Fatal("ParseResponse returned nil on zero-checksum zero-tail 0xA3 reply")
	}
	ch, err := ParseReadRAMChunk(resp, 0)
	if err != nil {
		t.Fatalf("ParseReadRAMChunk failed (likely truncated by ascending scan): %v", err)
	}
	if len(ch.Data) != 16 || ch.TotalLength != 24 {
		t.Fatalf("unexpected chunk: total=%d len=%d", ch.TotalLength, len(ch.Data))
	}
	// Wrong cmd must strictly return nil
	if wrong := ParseResponse(rx, CmdWriteRAM); wrong != nil {
		t.Fatalf("expected nil for mismatched cmd, got %x", wrong)
	}
}

func TestIsValidTestMode(t *testing.T) {
	valid := []string{"", "stop", "rpmSweep", "hardBrake", "absFlash", "shiftFlash"}
	for _, m := range valid {
		if !IsValidTestMode(m) {
			t.Fatalf("expected IsValidTestMode(%q) == true", m)
		}
	}
	invalid := []string{"redline", "invalid", "STOP", "abs", "rpmSweep ", "shift"}
	for _, m := range invalid {
		if IsValidTestMode(m) {
			t.Fatalf("expected IsValidTestMode(%q) == false", m)
		}
	}
}

func TestRPMPaletteColorQuantization(t *testing.T) {
	scales := []float64{0.2, 0.5, 0.75, 1.0}
	for _, sc := range scales {
		for i := 0; i <= 200; i++ {
			n := float64(i) / 200.0
			c := RPMPaletteColor(n, sc)
			for name, ch := range map[string]byte{"R": c.R, "G": c.G, "B": c.B} {
				if ch != 255 && ch%8 != 0 {
					t.Fatalf("n=%.3f scale=%.2f %s=%d is not quantized to step 8 (or 255)", n, sc, name, ch)
				}
			}
			if c.Brightness > 100 || c.Brightness%4 != 0 {
				t.Fatalf("n=%.3f scale=%.2f Brightness=%d is not quantized to step 4 in [0,100]", n, sc, c.Brightness)
			}
			if c.Urgent {
				t.Fatalf("expected RPM palette target to be non-urgent")
			}
		}
	}
}

func TestControllerStateSafetyAndLatches(t *testing.T) {
	// 1. LightConfig.Sanitize handles NaN/Inf cleanly
	bad := LightConfig{
		Enabled:            true,
		HardBrakeThreshold: math.NaN(),
		ShiftRatioOverride: math.Inf(1),
		BrightnessScale:    math.NaN(),
	}
	bad.Sanitize()
	if bad.HardBrakeThreshold != 0.35 || bad.ShiftRatioOverride != 0 || bad.BrightnessScale != 1.0 {
		t.Fatalf("Sanitize failed on NaN/Inf: %+v", bad)
	}

	// 2. Start() after Shutdown() must not panic; double Shutdown() must not panic
	c0 := NewController(DefaultLightConfig())
	c0.Shutdown()
	c0.Start()
	c0.Shutdown()

	// 3. Manual RequestRestore() latch prevents evaluateStep() from overwriting desired until SetConfig or TriggerTest
	c := NewController(DefaultLightConfig())
	now := time.Now()
	racePkt := telemetry.Packet{
		IsRaceOn: true, CarOrdinal: 262, SpeedKmh: 160, Gear: 4,
		CurrentRpm: 7000, EngineMaxRpm: 9200, RpmRatio: 7000.0 / 9200.0,
		WheelRotSpeed: [4]float32{80, 80, 80, 80},
		NormSusp:      [4]float32{0.5, 0.5, 0.5, 0.5},
	}
	c.ObservePacket(racePkt, 0.8856, false)
	c.evaluateStep(now)
	c.mu.Lock()
	if c.desired == nil {
		c.mu.Unlock()
		t.Fatal("expected desired to be set while racing")
	}
	c.mu.Unlock()

	c.RequestRestore()
	c.evaluateStep(now.Add(20 * time.Millisecond))
	c.mu.Lock()
	if !c.manualRestored || c.desired != nil {
		c.mu.Unlock()
		t.Fatalf("expected manualRestored latch to keep desired=nil after evaluateStep, got manual=%v desired=%+v", c.manualRestored, c.desired)
	}
	c.mu.Unlock()

	// Re-enabling via SetConfig clears manualRestored and resumes desired
	c.SetConfig(DefaultLightConfig())
	c.evaluateStep(now.Add(40 * time.Millisecond))
	c.mu.Lock()
	if c.manualRestored || c.desired == nil {
		c.mu.Unlock()
		t.Fatalf("expected SetConfig(Enabled=true) to clear manualRestored and resume desired, got manual=%v desired=%+v", c.manualRestored, c.desired)
	}
	c.mu.Unlock()

	// TriggerTest also clears manualRestored
	c.RequestRestore()
	c.TriggerTest("shiftFlash")
	c.evaluateStep(now.Add(60 * time.Millisecond))
	c.mu.Lock()
	if c.manualRestored || c.desired == nil || c.desired.Cue != "shift-up" {
		c.mu.Unlock()
		t.Fatalf("expected TriggerTest to clear manualRestored, got manual=%v desired=%+v", c.manualRestored, c.desired)
	}
	c.mu.Unlock()
	c.TriggerTest("stop")

	// 4. CarOrdinal change resets HasWheelRot / HasSusp (even across CarOrdinal==0 pause frame)
	c.ObservePacket(telemetry.Packet{IsRaceOn: false, CarOrdinal: 0}, 0.90, false)
	c.ObservePacket(telemetry.Packet{
		IsRaceOn: true, CarOrdinal: 4223, SpeedKmh: 90, Gear: 2,
		CurrentRpm: 5000, EngineMaxRpm: 10000, RpmRatio: 0.50,
	}, 0.8767, false)
	c.mu.Lock()
	if c.lastIn.HasWheelRot || c.lastIn.HasSusp {
		c.mu.Unlock()
		t.Fatalf("expected HasWheelRot/HasSusp reset on CarOrdinal change, got rot=%v susp=%v", c.lastIn.HasWheelRot, c.lastIn.HasSusp)
	}
	c.mu.Unlock()

	// 5. EV rpmPct uses speed proxy when EngineMaxRpm < 500
	evPkt := telemetry.Packet{
		IsRaceOn: true, CarOrdinal: 9999, SpeedKmh: 130, Gear: 1,
		CurrentRpm: 0, EngineMaxRpm: 0, RpmRatio: 0,
	}
	c.ObservePacket(evPkt, 0.90, true)
	c.evaluateStep(time.Now())
	if got := c.Snapshot().RpmPct; got != 50 {
		t.Fatalf("expected EV rpmPct=50 at 130km/h (130/260), got %d", got)
	}

	// 6. Stale packet timeout is 800ms (active at 500ms, idle at 850ms)
	tFresh := time.Now()
	c.mu.Lock()
	c.lastAt = tFresh
	c.mu.Unlock()
	c.evaluateStep(tFresh.Add(500 * time.Millisecond))
	if c.Snapshot().Cue == "idle" {
		t.Fatal("expected packet to still be fresh at 500ms (<800ms)")
	}
	c.evaluateStep(tFresh.Add(850 * time.Millisecond))
	if c.Snapshot().Cue != "idle" {
		t.Fatalf("expected packet to go stale/idle at 850ms (>800ms), got cue=%q", c.Snapshot().Cue)
	}

	// 7. SetBackupPath persists & recovers valid backup on disk and cleans up on remove
	bkFile := filepath.Join(t.TempDir(), "data", "lighting_backup.bin")
	c.SetBackupPath(bkFile)
	orig := WrapLightingBin(EncodeSolidLightingBin(255, 231, 0, 100, true))
	c.hwMu.Lock()
	c.backupRaw = orig
	c.persistBackupLocked()
	gotRaw, gotFmt, ok := c.loadDiskBackupLocked()
	c.removeBackupFileLocked()
	_, _, okAfterRemove := c.loadDiskBackupLocked()
	c.hwMu.Unlock()
	if !ok || gotFmt != "wrapped-lght" || hex.EncodeToString(gotRaw) != hex.EncodeToString(orig) {
		t.Fatalf("disk backup roundtrip failed: ok=%v fmt=%q len=%d", ok, gotFmt, len(gotRaw))
	}
	if okAfterRemove {
		t.Fatal("expected disk backup file to be removed after clean restore")
	}
}

func TestABSDetectionAlignmentWithForzaHUD(t *testing.T) {
	mkIn := func(mod func(*TelemetryInput)) TelemetryInput {
		in := TelemetryInput{
			Packet: telemetry.Packet{
				IsRaceOn:      true,
				SpeedKmh:      150,
				Gear:          4,
				Brake:         200,
				WheelRotSpeed: [4]float32{95, 95, 95, 95},
				NormSusp:      [4]float32{0.45, 0.45, 0.40, 0.40},
				SlipRatio:     [4]float32{-0.30, -0.30, -0.18, -0.18},
				CombinedSlip:  [4]float32{0.32, 0.32, 0.20, 0.20},
			},
			PacketFresh: true,
			HasWheelRot: true,
			HasSusp:     true,
		}
		if mod != nil {
			mod(&in)
		}
		return in
	}

	cases := []struct {
		name    string
		in      TelemetryInput
		wantABS bool
	}{
		{
			name: "normal firm braking within grip (old 0.38 bug)",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 255
				in.Packet.SlipRatio = [4]float32{-0.55, -0.52, -0.30, -0.28}
				in.Packet.CombinedSlip = [4]float32{0.58, 0.55, 0.32, 0.30}
			}),
			wantABS: false,
		},
		{
			name: "threshold braking just below lockup limit",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 240
				in.Packet.SlipRatio = [4]float32{-0.82, -0.78, -0.35, -0.35}
				in.Packet.CombinedSlip = [4]float32{0.84, 0.80, 0.38, 0.38}
			}),
			wantABS: false,
		},
		{
			name: "trail-braking lateral cornering slip only (CombinedSlip pollution)",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 110
				in.Packet.SlipRatio = [4]float32{-0.22, -0.18, -0.12, -0.10}
				in.Packet.SlipAngle = [4]float32{0.96, 0.94, 0.65, 0.60}
				in.Packet.CombinedSlip = [4]float32{0.98, 0.96, 0.66, 0.61}
			}),
			wantABS: false,
		},
		{
			name: "positive acceleration wheelspin while lightly touching brake",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 60
				in.Packet.Accel = 240
				in.Packet.SlipRatio = [4]float32{0.05, 0.05, 2.60, 2.55}
				in.Packet.CombinedSlip = [4]float32{0.10, 0.10, 2.60, 2.55}
			}),
			wantABS: false,
		},
		{
			name: "handbrake drift rear lockup while touching service brake",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 120
				in.Packet.Handbrake = 255
				in.Packet.SlipRatio = [4]float32{-0.25, -0.20, -6.50, -6.20}
				in.Packet.WheelRotSpeed = [4]float32{85, 85, 0.0, 0.0}
			}),
			wantABS: false,
		},
		{
			name: "low-speed crawl stop below Forza ABS cutoff",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.SpeedKmh = 9.5
				in.Packet.Brake = 220
				in.Packet.SlipRatio = [4]float32{-1.80, -1.70, -1.40, -1.40}
				in.Packet.WheelRotSpeed = [4]float32{0.1, 0.1, 0.2, 0.2}
			}),
			wantABS: false,
		},
		{
			name: "very light brake pedal brush",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 28
				in.Packet.SlipRatio = [4]float32{-1.30, -0.10, -0.10, -0.10}
			}),
			wantABS: false,
		},
		{
			name: "all wheels airborne mid-jump while holding brake",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.NormSusp = [4]float32{0, 0, 0, 0}
				in.Packet.Brake = 255
				in.Packet.SlipRatio = [4]float32{-2.5, -2.5, -2.5, -2.5}
				in.Packet.WheelRotSpeed = [4]float32{0, 0, 0, 0}
			}),
			wantABS: false,
		},
		{
			name: "single unloaded wheel over curb while grounded wheels keep grip",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.NormSusp = [4]float32{0.0, 0.48, 0.42, 0.45}
				in.Packet.SlipRatio = [4]float32{-2.10, -0.35, -0.22, -0.20}
				in.Packet.WheelRotSpeed = [4]float32{0.0, 92, 93, 93}
			}),
			wantABS: false,
		},
		{
			name: "real Forza ABS onset on grounded front wheel (60Hz peak shoulder -0.97)",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 220
				in.Packet.SlipRatio = [4]float32{-0.97, -0.65, -0.28, -0.25}
			}),
			wantABS: true,
		},
		{
			name: "real Forza ABS longitudinal braking slip > 1.0",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.Brake = 255
				in.Packet.SlipRatio = [4]float32{-1.45, -1.12, -0.40, -0.38}
			}),
			wantABS: true,
		},
		{
			name: "low-grip dirt ABS at moderate pedal (~25% brake)",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.SpeedKmh = 52
				in.Packet.Brake = 64
				in.Packet.SlipRatio = [4]float32{-0.15, -1.55, -0.35, -0.10}
			}),
			wantABS: true,
		},
		{
			name: "dead wheel rotation lockup under heavy service brake",
			in: mkIn(func(in *TelemetryInput) {
				in.Packet.SpeedKmh = 88
				in.Packet.Brake = 180
				in.Packet.SlipRatio = [4]float32{-0.80, -0.70, -0.40, -0.40}
				in.Packet.WheelRotSpeed = [4]float32{0.3, 60, 62, 62}
			}),
			wantABS: true,
		},
	}

	cfg := DefaultLightConfig()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotABS, _ := DetectABSInstant(tc.in, false)
			if gotABS != tc.wantABS {
				t.Fatalf("DetectABSInstant() = %v, want %v (brk=%d hb=%d spd=%.1f sr=%v cs=%v wr=%v susp=%v)",
					gotABS, tc.wantABS, tc.in.Packet.Brake, tc.in.Packet.Handbrake, tc.in.Packet.SpeedKmh,
					tc.in.Packet.SlipRatio, tc.in.Packet.CombinedSlip, tc.in.Packet.WheelRotSpeed, tc.in.Packet.NormSusp)
			}
			_, _, evalABS, _, _ := EvaluateTelemetry(tc.in, cfg, true, true, true)
			if evalABS != tc.wantABS {
				t.Fatalf("EvaluateTelemetry() absActive = %v, want %v", evalABS, tc.wantABS)
			}
		})
	}
}

func TestABSTrackerHysteresisAndCooldown(t *testing.T) {
	var tr ABSTracker
	t0 := time.Unix(1700000000, 0)

	mk := func(brk, hb uint8, spd float32, sr [4]float32) TelemetryInput {
		return TelemetryInput{
			Packet: telemetry.Packet{
				IsRaceOn:      true,
				SpeedKmh:      spd,
				Brake:         brk,
				Handbrake:     hb,
				SlipRatio:     sr,
				WheelRotSpeed: [4]float32{80, 80, 80, 80},
				NormSusp:      [4]float32{0.45, 0.45, 0.40, 0.40},
			},
			PacketFresh: true,
			HasWheelRot: true,
			HasSusp:     true,
		}
	}

	// 1. Engage ABS on real longitudinal slip peak
	inPeak := mk(240, 0, 180, [4]float32{-1.35, -0.90, -0.35, -0.35})
	if !tr.Update(inPeak, t0) || !tr.IsActive(inPeak, t0) {
		t.Fatal("expected ABS to engage on -1.35 braking slip")
	}

	// 2. ABS valve-release valley (SR rebounds to -0.35 for 120ms while brake stays held): must hold cleanly
	inValley := mk(235, 0, 170, [4]float32{-0.35, -0.32, -0.25, -0.25})
	if !tr.Update(inValley, t0.Add(60*time.Millisecond)) {
		t.Fatal("expected ABS hold to bridge 60ms release valley")
	}
	if !tr.Update(inValley, t0.Add(120*time.Millisecond)) {
		t.Fatal("expected ABS hold to bridge 120ms release valley")
	}

	// 3. After 150ms hold expires without a new >=0.95 peak, ABS releases cleanly
	if tr.Update(inValley, t0.Add(170*time.Millisecond)) || tr.IsActive(inValley, t0.Add(170*time.Millisecond)) {
		t.Fatal("expected ABS to release after 150ms valley hold expires")
	}

	// 4. Re-engage ABS, then lift off brake pedal: must release IMMEDIATELY (no linger!)
	t1 := t0.Add(300 * time.Millisecond)
	if !tr.Update(inPeak, t1) {
		t.Fatal("expected ABS re-engage")
	}
	inLift := mk(15, 0, 160, [4]float32{-0.45, -0.40, -0.20, -0.20})
	if tr.Update(inLift, t1.Add(20*time.Millisecond)) || tr.IsActive(inLift, t1.Add(20*time.Millisecond)) {
		t.Fatal("expected ABS to clear immediately upon lifting brake pedal")
	}

	// 5. Handbrake pull clears ABS and starts 250ms rear-wheel spin-up cooldown
	t2 := t0.Add(600 * time.Millisecond)
	if !tr.Update(inPeak, t2) {
		t.Fatal("expected ABS engage before handbrake")
	}
	inHB := mk(150, 255, 110, [4]float32{-0.25, -0.20, -6.80, -6.50})
	if tr.Update(inHB, t2.Add(20*time.Millisecond)) {
		t.Fatal("handbrake pull must immediately suppress ABS")
	}
	// Right after releasing handbrake (80ms later), rear wheels are still recovering at -5.0 -> NO false ABS
	inPostHBRear := mk(80, 0, 100, [4]float32{-0.25, -0.20, -5.20, -4.80})
	if tr.Update(inPostHBRear, t2.Add(100*time.Millisecond)) {
		t.Fatal("post-handbrake rear wheel recovery must not falsely trigger ABS during cooldown")
	}
	// But if a FRONT wheel genuinely locks under heavy brake during cooldown, ABS still triggers!
	inPostHBFront := mk(240, 0, 100, [4]float32{-1.30, -0.80, -3.00, -2.80})
	if !tr.Update(inPostHBFront, t2.Add(120*time.Millisecond)) {
		t.Fatal("genuine front-wheel lockup under heavy brake should still trigger ABS during rear HB cooldown")
	}
}

func TestABSOnRealForzaRecordings(t *testing.T) {
	recDir := os.Getenv("FORZA_RECORDINGS_DIR")
	if recDir == "" {
		t.Skip("FORZA_RECORDINGS_DIR not set")
	}
	pfile := filepath.Join(recDir, "遥测录制传奇岛路肩", "packets.jsonl")
	f, err := os.Open(pfile)
	if err != nil {
		t.Skipf("recording not found: %v", err)
	}
	defer f.Close()

	type recLine struct {
		Hex string `json:"hex"`
	}
	var tr ABSTracker
	hasRot, hasSusp := false, false
	brkFrames := 0
	absFrames := 0
	nonAbsBrkFalseHits := 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	tBase := time.Unix(1700000000, 0)
	for scanner.Scan() {
		var rl recLine
		if err := json.Unmarshal(scanner.Bytes(), &rl); err != nil {
			continue
		}
		raw, err := hex.DecodeString(rl.Hex)
		if err != nil {
			continue
		}
		pkt, err := telemetry.Parse(raw)
		if err != nil || !pkt.IsRaceOn {
			continue
		}
		for i := 0; i < 4; i++ {
			if pkt.WheelRotSpeed[i] > 1.0 || pkt.WheelRotSpeed[i] < -1.0 {
				hasRot = true
			}
			if pkt.NormSusp[i] > 0.01 {
				hasSusp = true
			}
		}
		in := TelemetryInput{
			Packet:      pkt,
			PacketFresh: true,
			HasWheelRot: hasRot,
			HasSusp:     hasSusp,
		}
		now := tBase.Add(time.Duration(pkt.TimestampMs) * time.Millisecond)
		active := tr.Update(in, now)
		if pkt.Brake >= 45 && pkt.SpeedKmh > 12 {
			brkFrames++
			if active {
				absFrames++
			}
		}
		// Instant detector must NEVER fire when max grounded neg slip < 0.90 and all wheels are spinning
		maxG := 0.0
		minW := 9999.0
		for i := 0; i < 4; i++ {
			if pkt.NormSusp[i] > 0.02 {
				if ns := -float64(pkt.SlipRatio[i]); ns > maxG {
					maxG = ns
				}
				wr := float64(pkt.WheelRotSpeed[i])
				if wr < 0 {
					wr = -wr
				}
				if wr < minW {
					minW = wr
				}
			}
		}
		if maxG < 0.90 && minW > 5.0 {
			if inst, _ := DetectABSInstant(in, false); inst {
				nonAbsBrkFalseHits++
			}
		}
	}
	if brkFrames == 0 {
		t.Fatal("expected braking frames in recording")
	}
	if nonAbsBrkFalseHits != 0 {
		t.Fatalf("got %d false instant ABS triggers on non-locking braking frames", nonAbsBrkFalseHits)
	}
	absPct := 100.0 * float64(absFrames) / float64(brkFrames)
	t.Logf("Legend Island recording: %d braking frames, %d ABS frames (%.1f%%; old buggy 0.38 logic was 58.6%%)", brkFrames, absFrames, absPct)
	if absPct < 15.0 || absPct > 50.0 {
		t.Fatalf("unexpected ABS duty ratio %.1f%% on Legend Island recording (want 15%%..50%%)", absPct)
	}
}
