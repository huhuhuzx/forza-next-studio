package lighting

import (
	"encoding/json"
	"math"
	"os"
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
	return os.WriteFile(path, b, 0644)
}

// Sanitize clamps all LightConfig parameters to safe bounds.
func (c *LightConfig) Sanitize() {
	if c.HardBrakeThreshold <= 0 {
		c.HardBrakeThreshold = 0.35
	}
	c.HardBrakeThreshold = clampf(c.HardBrakeThreshold, 0.02, 0.92)
	if c.ShiftRatioOverride != 0 {
		c.ShiftRatioOverride = clampf(c.ShiftRatioOverride, 0.72, 0.98)
	}
	if c.BrightnessScale <= 0 {
		c.BrightnessScale = 1.0
	}
	c.BrightnessScale = clampf(c.BrightnessScale, 0.20, 1.0)
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

// FlashClock maintains independent phase accumulators matching k6-telemetry-console:
// - standard: 120ms half-period (ABS, shift-up)
// - brake: pressure-adaptive 280ms -> 75ms half-period (faster strobe under harder braking!)
type FlashClock struct {
	lastAt   time.Time
	standard float64
	brake    float64
}

const (
	flashStandardHalfMs = 120.0
	flashBrakeSlowMs    = 280.0
	flashBrakeFastMs    = 75.0
)

func (fc *FlashClock) Advance(now time.Time, brakeNorm float64) (stdOn, brakeOn bool) {
	if fc.lastAt.IsZero() {
		fc.lastAt = now
		return true, true
	}
	elapsedMs := clampf(float64(now.Sub(fc.lastAt).Milliseconds()), 0, 100)
	fc.lastAt = now
	if elapsedMs > 0 {
		fc.standard = math.Mod(fc.standard+elapsedMs/flashStandardHalfMs, 2.0)
		bp := lerp(flashBrakeSlowMs, flashBrakeFastMs, smoothstep(clampf(brakeNorm, 0, 1)))
		fc.brake = math.Mod(fc.brake+elapsedMs/bp, 2.0)
	}
	return fc.standard < 1.0, fc.brake < 1.0
}

// TelemetryInput holds the minimal live FH telemetry state needed to evaluate LED lighting.
type TelemetryInput struct {
	Packet       telemetry.Packet
	PacketFresh  bool
	RedlineRatio float64
	IsElectric   bool
	HasWheelRot  bool
}

// EvaluateTelemetry computes the desired LED color and 12-lamp bar state from live FH telemetry.
// Priority matches k6-telemetry-console + user request:
//   ABS (ice-blue fast strobe) > Hard Brake (pressure-accelerated red strobe) > Redline Shift (red/white strobe) > Continuous RPM Palette
func EvaluateTelemetry(in TelemetryInput, cfg LightConfig, stdOn, brakeOn bool) (target ColorTarget, litLamps int, absActive, hardBrake, shifting bool) {
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
	rpmNorm := clampf(float64(pkt.RpmRatio), 0, 1)
	if in.IsElectric && pkt.EngineMaxRpm < 500 {
		rpmNorm = clampf(spd/260.0, 0, 1)
	}

	// Shift threshold: use override if set, else ForzaNext's learned car redline (clamped 0.82..0.97)
	shiftRatio := in.RedlineRatio
	if shiftRatio < 0.75 || shiftRatio > 0.99 {
		shiftRatio = 0.90
	}
	if cfg.ShiftRatioOverride > 0 {
		shiftRatio = cfg.ShiftRatioOverride
	}

	// Accurate Forza Horizon ABS / wheel lockup detection (matches ForzaNext haptic ABS!)
	maxNegSlip := 0.0
	maxCombSlip := 0.0
	minRot := 9999.0
	for i := 0; i < 4; i++ {
		if ns := -float64(pkt.SlipRatio[i]); ns > maxNegSlip {
			maxNegSlip = ns
		}
		if cs := math.Abs(float64(pkt.CombinedSlip[i])); cs > maxCombSlip {
			maxCombSlip = cs
		}
		if ar := math.Abs(float64(pkt.WheelRotSpeed[i])); ar < minRot {
			minRot = ar
		}
	}
	wheelLocked := pkt.Brake >= 75 && spd > 12 && ((in.HasWheelRot && minRot < 0.85) || maxNegSlip >= 0.75)
	absActive = cfg.EnableABS && pkt.Brake >= 55 && spd > 12 &&
		(wheelLocked || maxNegSlip >= 0.38 || (brakeNorm >= 0.25 && maxCombSlip >= 0.82))

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

	// Priority 3: Redline Shift-Up Flash -> Red / Bright-White strobe (distinct from brake red/off!)
	if shifting {
		if stdOn {
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

	// Quantize to multiples of 4 so tiny 1-level RPM noise doesn't spam 0xA4 RAM transactions
	q := func(v float64) byte {
		rv := math.Round(v/4.0) * 4.0
		return byte(clampf(rv, 0, 255))
	}
	br := scaleBrightness(lerp(lo.brightness, hi.brightness, t), brightnessScale)
	return ColorTarget{
		R:          q(lerp(lo.r, hi.r, t)),
		G:          q(lerp(lo.g, hi.g, t)),
		B:          q(lerp(lo.b, hi.b, t)),
		Brightness: br,
		Cue:        "rpm",
		Label:      "转速指示灯",
	}
}

func scaleBrightness(raw float64, scale float64) byte {
	return byte(clampf(math.Round(raw*clampf(scale, 0.2, 1.0)), 0, 100))
}

func clampf(v, lo, hi float64) float64 {
	if v < lo {
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
