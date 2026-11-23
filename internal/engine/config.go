package engine

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
)

const CurrentSchemaVersion = 2

// EventFeel controls how a single deduced driving event feels on the controller.
type EventFeel struct {
	Enabled   bool    `json:"enabled"`
	Intensity float64 `json:"intensity"` // 0.0 .. 1.6 (1.0 = APEX 6 hardware-calibrated full feel)
	Sharpness float64 `json:"sharpness"` // 0.0 = 圆润饱满 (round/plush), 1.0 = 生硬犀利 (crisp/harsh)
	FreqScale float64 `json:"freqScale"` // 0.65 .. 1.45 frequency multiplier
}

// NextConfig holds the complete user-tunable configuration for Entertainment (Next) mode.
type NextConfig struct {
	SchemaVersion int    `json:"schemaVersion"`
	ProfileName   string `json:"profileName"`

	// Global Output Controls
	MasterGripGain    float64 `json:"masterGripGain"`    // 0.0 .. 1.6
	MasterTriggerGain float64 `json:"masterTriggerGain"` // 0.0 .. 1.6
	AutoCarMatching   bool    `json:"autoCarMatching"`
	AutoRedlineLearn  bool    `json:"autoRedlineLearn"`
	DuckingStrength   float64 `json:"duckingStrength"`  // 0.0 .. 0.85
	StereoSeparation  float64 `json:"stereoSeparation"` // 0.0 .. 1.0
	AntiFatigueClamp  float64 `json:"antiFatigueClamp"` // 0.4 .. 1.2 sustained bed power

	// Event-Driven Feel Controls (14 Independent State-Deduced Events)
	Events struct {
		EngineIdle     EventFeel `json:"engineIdle"`     // 怠速呼吸与突突质感
		EngineRev      EventFeel `json:"engineRev"`      // 引擎拉转与涡轮负载
		GearShift      EventFeel `json:"gearShift"`      // 升降挡机械顿挫
		Redline        EventFeel `json:"redline"`        // 自学习红线与断油提醒
		RoadUndulation EventFeel `json:"roadUndulation"` // 全时路面起伏与连续悬架直驱
		RoadSurface    EventFeel `json:"roadSurface"`    // 柏油/土路/砂石/草雪多材质纹理
		CurbRumble     EventFeel `json:"curbRumble"`     // 路肩与马路牙子（随速锯齿+纯几何识别）
		JumpLanding    EventFeel `json:"jumpLanding"`    // 大小飞坡两段式落地冲击
		CollisionWall  EventFeel `json:"collisionWall"`  // 360° 全向碰撞（正撞/追尾/侧撞/斜角/障碍物）
		Guardrail      EventFeel `json:"guardrail"`      // 护栏侧向连续刮擦（16/8ms 定向脉冲）
		WaterSplash    EventFeel `json:"waterSplash"`    // 入水飞溅冲击与涉水波浪阻力
		Understeer     EventFeel `json:"understeer"`     // 推头：前轮外侧搓地抖动抵抗感
		OversteerDrift EventFeel `json:"oversteerDrift"` // 甩尾/漂移：车尾沉厚横扫滑移感+起步烧胎
		BrakingABS     EventFeel `json:"brakingAbs"`     // 重刹 ABS 脉动与轮胎抱死
	} `json:"events"`

	// Trigger Feel Controls
	Trigger struct {
		EnableLeftBrake   bool    `json:"enableLeftBrake"`
		EnableRightAccel  bool    `json:"enableRightAccel"`
		SmartArbitration  bool    `json:"smartArbitration"`
		BrakeResistance   float64 `json:"brakeResistance"`
		ThrottleMicroFeel float64 `json:"throttleMicroFeel"`
	} `json:"trigger"`
}

// DefaultNextConfig returns hardware-calibrated v2 defaults matching Flydigi APEX 6 actuator energy.
func DefaultNextConfig() NextConfig {
	var c NextConfig
	c.SchemaVersion = CurrentSchemaVersion
	c.ProfileName = "地平线全景沉浸 v2（饱满标准）"
	c.MasterGripGain = 1.05
	c.MasterTriggerGain = 1.05
	c.AutoCarMatching = true
	c.AutoRedlineLearn = true
	c.DuckingStrength = 0.58
	c.StereoSeparation = 0.94
	c.AntiFatigueClamp = 0.92

	c.Events.EngineIdle = EventFeel{Enabled: true, Intensity: 1.00, Sharpness: 0.35, FreqScale: 1.0}
	c.Events.EngineRev = EventFeel{Enabled: true, Intensity: 0.96, Sharpness: 0.45, FreqScale: 1.0}
	c.Events.GearShift = EventFeel{Enabled: true, Intensity: 1.12, Sharpness: 0.65, FreqScale: 1.0}
	c.Events.Redline = EventFeel{Enabled: true, Intensity: 1.08, Sharpness: 0.80, FreqScale: 1.0}
	c.Events.RoadUndulation = EventFeel{Enabled: true, Intensity: 1.08, Sharpness: 0.48, FreqScale: 1.0}
	c.Events.RoadSurface = EventFeel{Enabled: true, Intensity: 1.12, Sharpness: 0.52, FreqScale: 1.0}
	c.Events.CurbRumble = EventFeel{Enabled: true, Intensity: 1.15, Sharpness: 0.78, FreqScale: 1.0}
	c.Events.JumpLanding = EventFeel{Enabled: true, Intensity: 1.18, Sharpness: 0.58, FreqScale: 1.0}
	c.Events.CollisionWall = EventFeel{Enabled: true, Intensity: 1.20, Sharpness: 0.72, FreqScale: 1.0}
	c.Events.Guardrail = EventFeel{Enabled: true, Intensity: 1.15, Sharpness: 0.86, FreqScale: 1.0}
	c.Events.WaterSplash = EventFeel{Enabled: true, Intensity: 1.12, Sharpness: 0.25, FreqScale: 1.0}
	c.Events.Understeer = EventFeel{Enabled: true, Intensity: 1.08, Sharpness: 0.78, FreqScale: 1.0}
	c.Events.OversteerDrift = EventFeel{Enabled: true, Intensity: 1.05, Sharpness: 0.32, FreqScale: 1.0}
	c.Events.BrakingABS = EventFeel{Enabled: true, Intensity: 1.08, Sharpness: 0.72, FreqScale: 1.0}

	c.Trigger.EnableLeftBrake = true
	c.Trigger.EnableRightAccel = true
	c.Trigger.SmartArbitration = true
	c.Trigger.BrakeResistance = 0.48
	c.Trigger.ThrottleMicroFeel = 0.45
	return c
}

// StylePreset returns built-in entertainment style presets for quick switching.
func StylePreset(name string) (NextConfig, bool) {
	c := DefaultNextConfig()
	switch name {
	case "immersive_default":
		return c, true
	case "heavy_punch":
		c.ProfileName = "强劲震撼 · 满血反馈"
		c.MasterGripGain = 1.32
		c.MasterTriggerGain = 1.25
		c.AntiFatigueClamp = 1.12
		c.Events.EngineIdle.Intensity = 1.20
		c.Events.EngineRev.Intensity = 1.15
		c.Events.RoadSurface.Intensity = 1.30
		c.Events.CollisionWall.Intensity = 1.35
		c.Events.JumpLanding.Intensity = 1.35
		return c, true
	case "plush_cruiser":
		c.ProfileName = "圆润巡航 · 久玩不手麻"
		c.MasterGripGain = 0.85
		c.AntiFatigueClamp = 0.72
		c.Events.EngineIdle = EventFeel{Enabled: true, Intensity: 0.85, Sharpness: 0.15, FreqScale: 0.88}
		c.Events.EngineRev = EventFeel{Enabled: true, Intensity: 0.75, Sharpness: 0.20, FreqScale: 0.90}
		c.Events.RoadUndulation = EventFeel{Enabled: true, Intensity: 0.95, Sharpness: 0.22, FreqScale: 0.88}
		c.Events.RoadSurface = EventFeel{Enabled: true, Intensity: 0.95, Sharpness: 0.26, FreqScale: 0.92}
		c.Events.JumpLanding = EventFeel{Enabled: true, Intensity: 1.05, Sharpness: 0.28, FreqScale: 0.88}
		return c, true
	case "crisp_track":
		c.ProfileName = "生硬赛道 · 犀利路感"
		c.MasterGripGain = 1.12
		c.StereoSeparation = 1.0
		c.Events.EngineRev = EventFeel{Enabled: true, Intensity: 0.88, Sharpness: 0.80, FreqScale: 1.06}
		c.Events.GearShift = EventFeel{Enabled: true, Intensity: 1.28, Sharpness: 0.90, FreqScale: 1.08}
		c.Events.RoadUndulation = EventFeel{Enabled: true, Intensity: 1.20, Sharpness: 0.84, FreqScale: 1.10}
		c.Events.CurbRumble = EventFeel{Enabled: true, Intensity: 1.32, Sharpness: 0.92, FreqScale: 1.12}
		c.Events.Understeer = EventFeel{Enabled: true, Intensity: 1.25, Sharpness: 0.88, FreqScale: 1.05}
		c.Events.OversteerDrift = EventFeel{Enabled: true, Intensity: 1.15, Sharpness: 0.42, FreqScale: 1.00}
		c.Events.BrakingABS = EventFeel{Enabled: true, Intensity: 1.22, Sharpness: 0.86, FreqScale: 1.05}
		return c, true
	case "rally_offroad":
		c.ProfileName = "拉力越野 · 飞坡砂石"
		c.MasterGripGain = 1.18
		c.Events.RoadUndulation = EventFeel{Enabled: true, Intensity: 1.30, Sharpness: 0.38, FreqScale: 0.92}
		c.Events.RoadSurface = EventFeel{Enabled: true, Intensity: 1.38, Sharpness: 0.65, FreqScale: 1.00}
		c.Events.JumpLanding = EventFeel{Enabled: true, Intensity: 1.40, Sharpness: 0.45, FreqScale: 0.92}
		c.Events.WaterSplash = EventFeel{Enabled: true, Intensity: 1.32, Sharpness: 0.28, FreqScale: 1.00}
		c.Events.OversteerDrift = EventFeel{Enabled: true, Intensity: 1.18, Sharpness: 0.35, FreqScale: 0.95}
		return c, true
	case "drift_smoke":
		c.ProfileName = "漂移拉烟 · 清晰推头甩尾"
		c.AntiFatigueClamp = 0.82
		c.Events.EngineRev = EventFeel{Enabled: true, Intensity: 0.95, Sharpness: 0.58, FreqScale: 1.00}
		c.Events.Understeer = EventFeel{Enabled: true, Intensity: 1.15, Sharpness: 0.82, FreqScale: 1.05}
		c.Events.OversteerDrift = EventFeel{Enabled: true, Intensity: 1.28, Sharpness: 0.30, FreqScale: 0.95}
		c.Events.RoadSurface = EventFeel{Enabled: true, Intensity: 0.85, Sharpness: 0.40, FreqScale: 1.00}
		c.Events.GearShift = EventFeel{Enabled: true, Intensity: 1.18, Sharpness: 0.72, FreqScale: 1.05}
		return c, true
	default:
		return c, false
	}
}

func sanitizeFloat(v, def, lo, hi float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sanitizeEventFeel(e *EventFeel, def EventFeel) {
	e.Intensity = sanitizeFloat(e.Intensity, def.Intensity, 0.0, 1.6)
	e.Sharpness = sanitizeFloat(e.Sharpness, def.Sharpness, 0.0, 1.0)
	e.FreqScale = sanitizeFloat(e.FreqScale, def.FreqScale, 0.65, 1.45)
}

// Sanitize replaces NaN/Inf values with safe defaults and clamps all config fields to valid ranges.
func (c *NextConfig) Sanitize() {
	def := DefaultNextConfig()
	if c.SchemaVersion < CurrentSchemaVersion {
		c.SchemaVersion = CurrentSchemaVersion
	}
	c.MasterGripGain = sanitizeFloat(c.MasterGripGain, def.MasterGripGain, 0.0, 1.6)
	c.MasterTriggerGain = sanitizeFloat(c.MasterTriggerGain, def.MasterTriggerGain, 0.0, 1.6)
	c.DuckingStrength = sanitizeFloat(c.DuckingStrength, def.DuckingStrength, 0.0, 0.85)
	c.StereoSeparation = sanitizeFloat(c.StereoSeparation, def.StereoSeparation, 0.0, 1.0)
	c.AntiFatigueClamp = sanitizeFloat(c.AntiFatigueClamp, def.AntiFatigueClamp, 0.4, 1.2)

	sanitizeEventFeel(&c.Events.EngineIdle, def.Events.EngineIdle)
	sanitizeEventFeel(&c.Events.EngineRev, def.Events.EngineRev)
	sanitizeEventFeel(&c.Events.GearShift, def.Events.GearShift)
	sanitizeEventFeel(&c.Events.Redline, def.Events.Redline)
	sanitizeEventFeel(&c.Events.RoadUndulation, def.Events.RoadUndulation)
	sanitizeEventFeel(&c.Events.RoadSurface, def.Events.RoadSurface)
	sanitizeEventFeel(&c.Events.CurbRumble, def.Events.CurbRumble)
	sanitizeEventFeel(&c.Events.JumpLanding, def.Events.JumpLanding)
	sanitizeEventFeel(&c.Events.CollisionWall, def.Events.CollisionWall)
	sanitizeEventFeel(&c.Events.Guardrail, def.Events.Guardrail)
	sanitizeEventFeel(&c.Events.WaterSplash, def.Events.WaterSplash)
	sanitizeEventFeel(&c.Events.Understeer, def.Events.Understeer)
	sanitizeEventFeel(&c.Events.OversteerDrift, def.Events.OversteerDrift)
	sanitizeEventFeel(&c.Events.BrakingABS, def.Events.BrakingABS)

	c.Trigger.BrakeResistance = sanitizeFloat(c.Trigger.BrakeResistance, def.Trigger.BrakeResistance, 0.0, 1.0)
	c.Trigger.ThrottleMicroFeel = sanitizeFloat(c.Trigger.ThrottleMicroFeel, def.Trigger.ThrottleMicroFeel, 0.0, 1.0)
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("empty file path")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
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
	_ = tmp.Sync()
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpPath, perm)
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func LoadNextConfig(path string) NextConfig {
	def := DefaultNextConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	var peek struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(b, &peek); err != nil || peek.SchemaVersion < CurrentSchemaVersion {
		// Automatically upgrade v1 weak defaults to v2 hardware-calibrated defaults!
		_ = SaveNextConfig(path, def)
		return def
	}
	if err := json.Unmarshal(b, &def); err != nil {
		return DefaultNextConfig()
	}
	def.Sanitize()
	return def
}

func SaveNextConfig(path string, cfg NextConfig) error {
	cfg.Sanitize()
	cfg.SchemaVersion = CurrentSchemaVersion
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, b, 0644)
}
