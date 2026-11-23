package lighting

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"

	"forzanext/internal/telemetry"
)

// LightConfig stores user preferences for Forza Horizon telemetry LED lighting on Flydigi K6 / APEX 6.
type LightConfig struct {
	Enabled             bool    `json:"enabled"`
	EnableRPM           bool    `json:"enableRpm"`
	EnableShiftFlash    bool    `json:"enableShiftFlash"`
	EnableHardBrake     bool    `json:"enableHardBrake"`
	EnableABS           bool    `json:"enableAbs"`
	HardBrakeThreshold  float64 `json:"hardBrakeThreshold"`  // 0.05..0.90 (default 0.35 = 35% pedal)
	ShiftRatioOverride  float64 `json:"shiftRatioOverride"`  // 0 = auto car redline, else 0.75..0.98
	BrightnessScale     float64 `json:"brightnessScale"`     // 0.20..1.00 (default 1.00)
	VibrationLink       bool    `json:"vibrationLink"`       // firmware vibration-zone modulation
	AutoRestoreOnIdle   bool    `json:"autoRestoreOnIdle"`   // restore original LED when leaving race/paused
}

// DefaultLightConfig returns sensible Forza Horizon defaults (RPM strip + hard brake + ABS + shift flash).
func DefaultLightConfig() LightConfig {
	return LightConfig{
		Enabled:            true,
		EnableRPM:          true,
		EnableShiftFlash:   true,
		EnableHardBrake:    true,
		EnableABS:          true,
		HardBrakeThreshold: 0.35,
		ShiftRatioOverride: 0.0, // follow ForzaNext auto-learned car redline!
		BrightnessScale:    1.0,
		VibrationLink:      false,
		AutoRestoreOnIdle:  true, // keep original APEX 6 LED on desktop/menus; switch live when racing or testing!
	}
}

func LoadLightConfig(path string) LightConfig {
	cfg := DefaultLightConfig()
	if path == "" {
		return cfg
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return DefaultLightConfig()
	}
	cfg.Sanitize()
	return cfg
}

func SaveLightConfig(path string, cfg LightConfig) error {
	if path == "" {
		return nil
	}
	cfg.Sanitize()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Sanitize clamps all LightConfig parameters to safe bounds and replaces NaN/Inf with defaults.
func (c *LightConfig) Sanitize() {
	if math.IsNaN(c.HardBrakeThreshold) || math.IsInf(c.HardBrakeThreshold, 0) || c.HardBrakeThreshold <= 0 {
		c.HardBrakeThreshold = 0.35
	}
	c.HardBrakeThreshold = clampf(c.HardBrakeThreshold, 0.02, 0.92)
	if math.IsNaN(c.ShiftRatioOverride) || math.IsInf(c.ShiftRatioOverride, 0) || c.ShiftRatioOverride < 0 {
		c.ShiftRatioOverride = 0
	}
	if c.ShiftRatioOverride != 0 {
		c.ShiftRatioOverride = clampf(c.ShiftRatioOverride, 0.72, 0.98)
	}
	if math.IsNaN(c.BrightnessScale) || math.IsInf(c.BrightnessScale, 0) || c.BrightnessScale <= 0 {
		c.BrightnessScale = 1.0
	}
	c.BrightnessScale = clampf(c.BrightnessScale, 0.20, 1.0)
}

// IsValidTestMode validates supported LED preview test modes (including empty/"stop" to halt).
func IsValidTestMode(mode string) bool {
	switch mode {
	case "", "stop", "rpmSweep", "hardBrake", "absFlash", "shiftFlash":
		return true
	default:
		return false
	}
}

// ColorTarget represents a single desired K6 light strip state.
type ColorTarget struct {
	R          byte   `json:"r"`
	G          byte   `json:"g"`
	B          byte   `json:"b"`
	Brightness byte   `json:"brightness"`
	Cue        string `json:"cue"`   // idle, rpm, shift-up, brake, abs, test
	Label      string `json:"label"`
	Alert      bool   `json:"alert"`
	Urgent     bool   `json:"-"` // bypass smooth RPM throttle
}

// LightSnapshot exposes live LED strip state & hardware telemetry to the Web Studio UI.
type LightSnapshot struct {
	Config       LightConfig `json:"config"`
	Connected    bool        `json:"connected"`
	Product      string      `json:"product"`
	BackupReady  bool        `json:"backupReady"`
	BackupBytes  int         `json:"backupBytes"`
	BackupFormat string      `json:"backupFormat"`
	R            byte        `json:"r"`
	G            byte        `json:"g"`
	B            byte        `json:"b"`
	Brightness   byte        `json:"brightness"`
	Cue          string      `json:"cue"`
	Label        string      `json:"label"`
	Alert        bool        `json:"alert"`
	LitLamps     int         `json:"litLamps"` // 0..12 lamps for UI shift-light bar
	RpmPct       int         `json:"rpmPct"`
	BrakePct     int         `json:"brakePct"`
	AbsActive    bool        `json:"absActive"`
	HardBrake    bool        `json:"hardBrake"`
	Shifting     bool        `json:"shifting"`
	TestMode     string      `json:"testMode"`
	WritesOK     uint64      `json:"writesOk"`
	WriteErrs    uint64      `json:"writeErrs"`
	LastError    string      `json:"lastError"`
}

// FlashClock maintains independent phase accumulators for LED alert strobes:
// - shift:    81ms half-period (162ms full cycle = 6.17 Hz = exact 1/3 sub-harmonic of grip redline's
//             18.5 Hz fuel-cut stutter, safely above 30-50ms APEX 6 0xA4 HID write latency so zero
//             dropped cycles or asymmetric wobble occur)
// - standard: 120ms half-period (240ms full cycle = 4.17 Hz; ABS ice-blue strobe)
// - brake:    pressure-adaptive 280ms -> 75ms half-period (faster strobe under harder braking)
type FlashClock struct {
	lastAt     time.Time
	standardMs float64
	shiftMs    float64
	brake      float64
}

const (
	flashStandardHalfMs = 120.0
	flashShiftHalfMs    = 81.0
	flashBrakeSlowMs    = 280.0
	flashBrakeFastMs    = 75.0
)

// ResetShift resets the shift-strobe phase to 0ms (immediate bright Red ON) on redline onset.
func (fc *FlashClock) ResetShift() {
	fc.shiftMs = 0
}

func (fc *FlashClock) Advance(now time.Time, brakeNorm float64) (stdOn, brakeOn, shiftOn bool) {
	if fc.lastAt.IsZero() {
		fc.lastAt = now
		return true, true, true
	}
	elapsedMs := clampf(float64(now.Sub(fc.lastAt))/float64(time.Millisecond), 0, 100)
	fc.lastAt = now
	if elapsedMs > 0 {
		fc.standardMs = math.Mod(fc.standardMs+elapsedMs, 2.0*flashStandardHalfMs)
		fc.shiftMs = math.Mod(fc.shiftMs+elapsedMs, 2.0*flashShiftHalfMs)
		bp := lerp(flashBrakeSlowMs, flashBrakeFastMs, smoothstep(clampf(brakeNorm, 0, 1)))
		fc.brake = math.Mod(fc.brake+elapsedMs/bp, 2.0)
	}
	return fc.standardMs < flashStandardHalfMs, fc.brake < 1.0-1e-9, fc.shiftMs < flashShiftHalfMs
}

// TelemetryInput holds the minimal live FH telemetry state needed to evaluate LED lighting.
type TelemetryInput struct {
	Packet       telemetry.Packet
	PacketFresh  bool
	RedlineRatio float64
	IsElectric   bool
	HasWheelRot  bool
	HasSusp      bool
}

const (
	absBrakeEnterMin    uint8   = 45   // ~18% pedal: catches low-grip dirt/wet ABS, ignores light pedal brush
	absBrakeHoldMin     uint8   = 40   // ~16% pedal release floor during ABS hysteresis hold
	absBrakeLockMin     uint8   = 75   // ~29% pedal for dead-stop wheel rotation lockup check
	absHandbrakeMax     uint8   = 25   // exclude handbrake pulls / drift e-brake rear lockup
	absSpeedEnterKmh    float64 = 12.0 // Forza Horizon ABS disengages at low crawl speed
	absSpeedHoldKmh     float64 = 10.0
	absNegSlipEnter     float64 = 0.95 // Forza normalized longitudinal grip limit (~1.0; 0.95 catches 60Hz UDP peaks)
	absNegSlipHoldMin   float64 = 0.20 // wheel still actively braking during ABS pressure-dump valley
	absWheelLockRotRadS float64 = 0.85
	absSuspGroundedMin  float64 = 0.02
	absHoldDuration             = 150 * time.Millisecond
	absHBCooldown               = 250 * time.Millisecond
)

// DetectABSInstant evaluates single-frame Forza Horizon longitudinal braking ABS/lockup conditions.
// Unlike CombinedSlip (which includes lateral cornering SlipAngle), this inspects only negative
// longitudinal SlipRatio (and dead wheel rotation drop) on grounded wheels while service braking
// without handbrake engaged — aligning with Forza Horizon's in-game HUD ABS indicator.
func DetectABSInstant(in TelemetryInput, ignoreRear bool) (triggered bool, effMaxNegSlip float64) {
	if !in.PacketFresh || !in.Packet.IsRaceOn {
		return false, 0
	}
	pkt := in.Packet
	if pkt.Handbrake > absHandbrakeMax {
		return false, 0
	}

	useSusp := in.HasSusp
	if !useSusp {
		for i := 0; i < 4; i++ {
			if pkt.NormSusp[i] > 0.01 {
				useSusp = true
				break
			}
		}
	}

	maxNegSlip := 0.0
	minRot := 9999.0
	grounded := 0
	for i := 0; i < 4; i++ {
		if ignoreRear && i >= 2 {
			continue
		}
		if useSusp && float64(pkt.NormSusp[i]) <= absSuspGroundedMin {
			continue
		}
		grounded++
		if ns := -float64(pkt.SlipRatio[i]); ns > maxNegSlip {
			maxNegSlip = ns
		}
		if ar := math.Abs(float64(pkt.WheelRotSpeed[i])); ar < minRot {
			minRot = ar
		}
	}
	if grounded == 0 {
		return false, 0
	}

	spd := float64(pkt.SpeedKmh)
	wheelLocked := !ignoreRear && in.HasWheelRot && pkt.Brake >= absBrakeLockMin && spd > absSpeedEnterKmh && minRot < absWheelLockRotRadS
	triggered = pkt.Brake >= absBrakeEnterMin && spd > absSpeedEnterKmh && (maxNegSlip >= absNegSlipEnter || wheelLocked)
	return triggered, maxNegSlip
}

// ABSTracker adds stateful Forza Horizon HUD-aligned hysteresis to DetectABSInstant:
// - Bridges Forza's 30-150ms ABS pressure-release valleys while brake stays pressed.
// - Suppresses rear-wheel false positives during a 250ms spin-up cooldown after handbrake release.
// - Releases immediately on brake lift, handbrake pull, low crawl speed, or race pause.
type ABSTracker struct {
	holdUntil       time.Time
	hbCooldownUntil time.Time
	lastActive      bool
}

func (t *ABSTracker) Reset() {
	t.holdUntil = time.Time{}
	t.hbCooldownUntil = time.Time{}
	t.lastActive = false
}

// Update ingests one new telemetry packet at timestamp now and returns whether ABS is active.
func (t *ABSTracker) Update(in TelemetryInput, now time.Time) bool {
	if !in.PacketFresh || !in.Packet.IsRaceOn {
		t.Reset()
		return false
	}
	pkt := in.Packet
	if pkt.Handbrake > absHandbrakeMax {
		t.hbCooldownUntil = now.Add(absHBCooldown)
		t.holdUntil = time.Time{}
		t.lastActive = false
		return false
	}
	inHBCool := !t.hbCooldownUntil.IsZero() && !now.After(t.hbCooldownUntil)
	rawHit, effNeg := DetectABSInstant(in, inHBCool)
	if rawHit {
		t.holdUntil = now.Add(absHoldDuration)
		t.lastActive = true
		return true
	}
	if pkt.Brake < absBrakeHoldMin || float64(pkt.SpeedKmh) <= absSpeedHoldKmh {
		t.holdUntil = time.Time{}
		t.lastActive = false
		return false
	}
	if !t.holdUntil.IsZero() && !now.After(t.holdUntil) && effNeg >= absNegSlipHoldMin {
		t.lastActive = true
		return true
	}
	t.lastActive = false
	return false
}

// IsActive queries whether the tracked ABS state is still valid at timestamp now (without extending hold).
func (t *ABSTracker) IsActive(in TelemetryInput, now time.Time) bool {
	if !t.lastActive || !in.PacketFresh || !in.Packet.IsRaceOn {
		return false
	}
	pkt := in.Packet
	if pkt.Handbrake > absHandbrakeMax || pkt.Brake < absBrakeHoldMin || float64(pkt.SpeedKmh) <= absSpeedHoldKmh {
		return false
	}
	return !t.holdUntil.IsZero() && !now.After(t.holdUntil)
}

// EvaluateTelemetry computes the desired LED color and 12-lamp bar state from live FH telemetry
// using instantaneous ABS detection.
// Priority: ABS (ice-blue strobe) > Hard Brake (pressure-accelerated red strobe) > Redline Shift (fast red/white strobe) > Continuous RPM Palette
func EvaluateTelemetry(in TelemetryInput, cfg LightConfig, stdOn, brakeOn, shiftOn bool) (target ColorTarget, litLamps int, absActive, hardBrake, shifting bool) {
	rawABS, _ := DetectABSInstant(in, false)
	return EvaluateTelemetryWithABS(in, cfg, rawABS, stdOn, brakeOn, shiftOn)
}

// EvaluateTelemetryWithABS computes the desired LED color and 12-lamp bar state using a caller-supplied
// stateful ABS detection result (e.g., from ABSTracker).
func EvaluateTelemetryWithABS(in TelemetryInput, cfg LightConfig, absDetected bool, stdOn, brakeOn, shiftOn bool) (target ColorTarget, litLamps int, absActive, hardBrake, shifting bool) {
	if !in.PacketFresh || !in.Packet.IsRaceOn {
		// Standby dim icy-blue breathing indicator (very soft, non-intrusive)
		target = ColorTarget{
			R: 12, G: 54, B: 128,
			Brightness: scaleBrightness(22, cfg.BrightnessScale),
			Cue:        "idle",
			Label:      "待机 · 等待进入驾驶",
		}
		return target, 0, false, false, false
	}

	pkt := in.Packet
	brakeNorm := clampf(float64(pkt.Brake)/255.0, 0, 1)
	spd := float64(pkt.SpeedKmh)

	// Normalized RPM (or speed proxy for EVs if Forza sends 0 RPM)
	rpmNorm := effectiveRpmNorm(pkt, in.IsElectric)

	// Shift threshold: use override if set, else ForzaNext's learned car EffectiveRedlineRatio
	shiftRatio := effectiveShiftRatio(in.RedlineRatio, cfg.ShiftRatioOverride)

	absActive = cfg.EnableABS && absDetected
	hardBrake = cfg.EnableHardBrake && brakeNorm >= cfg.HardBrakeThreshold && spd > 3.0
	shifting = cfg.EnableShiftFlash && !in.IsElectric && pkt.Gear >= 1 && spd > 10.0 && rpmNorm >= shiftRatio

	// Priority 1: ABS Intervention / Lockup -> Ice-Blue high-speed strobe
	if absActive {
		if stdOn {
			target = ColorTarget{
				R: 80, G: 170, B: 255,
				Brightness: scaleBrightness(100, cfg.BrightnessScale),
				Cue:        "abs",
				Label:      "ABS 介入",
				Alert:      true,
				Urgent:     true,
			}
			return target, 12, true, hardBrake, shifting
		}
		target = ColorTarget{
			R: 0, G: 0, B: 0,
			Brightness: scaleBrightness(100, cfg.BrightnessScale),
			Cue:        "abs",
			Label:      "ABS 闪烁",
			Alert:      true,
			Urgent:     true,
		}
		return target, 0, true, hardBrake, shifting
	}

	// Priority 2: Hard Braking -> Pure Red strobe accelerating with brake pressure
	if hardBrake {
		if brakeOn {
			target = ColorTarget{
				R: 255, G: 0, B: 0,
				Brightness: scaleBrightness(100, cfg.BrightnessScale),
				Cue:        "brake",
				Label:      "急刹警示",
				Alert:      true,
				Urgent:     true,
			}
			return target, 12, false, true, shifting
		}
		target = ColorTarget{
			R: 0, G: 0, B: 0,
			Brightness: scaleBrightness(100, cfg.BrightnessScale),
			Cue:        "brake",
			Label:      "急刹闪烁",
			Alert:      true,
			Urgent:     true,
		}
		return target, 0, false, true, shifting
	}

	// Priority 3: Redline Shift-Up Flash -> Fast Red / Bright-White strobe (distinct from brake red/off!)
	if shifting {
		if shiftOn {
			target = ColorTarget{
				R: 255, G: 0, B: 0,
				Brightness: scaleBrightness(100, cfg.BrightnessScale),
				Cue:        "shift-up",
				Label:      "红线换挡 · 红",
				Alert:      true,
				Urgent:     true,
			}
		} else {
			target = ColorTarget{
				R: 255, G: 255, B: 255,
				Brightness: scaleBrightness(100, cfg.BrightnessScale),
				Cue:        "shift-up",
				Label:      "红线换挡 · 白",
				Alert:      true,
				Urgent:     true,
			}
		}
		return target, 12, false, false, true
	}

	// Priority 4: Continuous RPM Shift-Light Palette (exact 5-stop smoothstep curve from k6-telemetry-console)
	if cfg.EnableRPM {
		c := RPMPaletteColor(rpmNorm, cfg.BrightnessScale)
		lamps := int(math.Ceil(rpmNorm * 12.0))
		if lamps < 0 {
			lamps = 0
		} else if lamps > 12 {
			lamps = 12
		}
		return c, lamps, false, false, false
	}

	// All cues disabled -> dark
	return ColorTarget{R: 0, G: 0, B: 0, Brightness: 0, Cue: "off", Label: "灯效已静音"}, 0, false, false, false
}

// RPMPaletteColor implements the exact 5-stop smoothstep continuous RPM gradient from k6-telemetry-console.
func RPMPaletteColor(normalized float64, brightnessScale float64) ColorTarget {
	n := clampf(normalized, 0, 1)
	type stop struct {
		at         float64
		r, g, b    float64
		brightness float64
	}
	stops := []stop{
		{0.00, 15, 90, 255, 60},   // Idle / Low RPM: Deep Blue
		{0.28, 15, 225, 112, 76},  // Eco / Mid-Low: Emerald Green
		{0.58, 255, 205, 16, 84},  // Powerband: Vivid Yellow
		{0.82, 255, 92, 8, 94},    // High RPM: Fierce Orange
		{1.00, 255, 0, 0, 100},    // Redline: Pure Red
	}
	upperIdx := len(stops) - 1
	for i := 1; i < len(stops); i++ {
		if n <= stops[i].at {
			upperIdx = i
			break
		}
	}
	lo := stops[upperIdx-1]
	hi := stops[upperIdx]
	t := smoothstep(clampf((n-lo.at)/(hi.at-lo.at), 0, 1))

	// Quantize RGB to step 8 and Brightness to step 4 so smooth RPM ramps don't saturate 0xFFA0
	// while 0x57 drives 1kHz triggers and grips on the shared HID endpoint.
	qRGB := func(v float64) byte {
		rv := math.Round(v/8.0) * 8.0
		return byte(clampf(rv, 0, 255))
	}
	rawBr := lerp(lo.brightness, hi.brightness, t) * clampf(brightnessScale, 0.2, 1.0)
	br := byte(clampf(math.Round(rawBr/4.0)*4.0, 0, 100))
	return ColorTarget{
		R:          qRGB(lerp(lo.r, hi.r, t)),
		G:          qRGB(lerp(lo.g, hi.g, t)),
		B:          qRGB(lerp(lo.b, hi.b, t)),
		Brightness: br,
		Cue:        "rpm",
		Label:      "转速指示灯",
	}
}

func effectiveRpmNorm(pkt telemetry.Packet, isElectric bool) float64 {
	rpmNorm := clampf(float64(pkt.RpmRatio), 0, 1)
	if isElectric && pkt.EngineMaxRpm < 500 {
		rpmNorm = clampf(float64(pkt.SpeedKmh)/260.0, 0, 1)
	}
	return rpmNorm
}

func effectiveShiftRatio(redlineRatio, override float64) float64 {
	shiftRatio := redlineRatio
	if math.IsNaN(shiftRatio) || math.IsInf(shiftRatio, 0) || shiftRatio < 0.75 || shiftRatio > 0.99 {
		shiftRatio = 0.90
	}
	if override > 0 && !math.IsNaN(override) && !math.IsInf(override, 0) {
		shiftRatio = override
	}
	return shiftRatio
}

func scaleBrightness(raw float64, scale float64) byte {
	return byte(clampf(math.Round(raw*clampf(scale, 0.2, 1.0)), 0, 100))
}

func clampf(v, lo, hi float64) float64 {
	if math.IsNaN(v) || v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func lerp(a, b, t float64) float64 {
	return a + (b-a)*t
}

func smoothstep(t float64) float64 {
	return t * t * (3.0 - 2.0*t)
}
