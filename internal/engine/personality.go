package engine

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"forzanext/internal/telemetry"
)

// CarPersonality describes the physical haptic character of the active vehicle.
type CarPersonality struct {
	ArchetypeID   string   `json:"archetypeId"`
	Category      string   `json:"category"`
	Label         string   `json:"label"`
	Tags          []string `json:"tags"`
	EngineFamily  string   `json:"engineFamily"` // ev, small_na, turbo_punch, muscle_v8, flat_gt, v12_silk, rotary_f1
	CarOrdinal    int32    `json:"carOrdinal"`
	CustomName    string   `json:"customName,omitempty"`
	Cylinders     int32    `json:"cylinders"`
	DriveTrain    int32    `json:"driveTrain"` // 0=FWD, 1=RWD, 2=AWD
	CarClass      int32    `json:"carClass"`
	CarPI         int32    `json:"carPi"`
	IdleRpm       float32  `json:"idleRpm"`
	MaxRpm        float32  `json:"maxRpm"`
	IsElectric    bool     `json:"isElectric"`
	IsRotary      bool     `json:"isRotary"`

	// Physical inferred metrics (Stage 2 continuous learning)
	InferredMaxSuspM float64 `json:"inferredMaxSuspM"`
	PeakBoostPsi     float64 `json:"peakBoostPsi"`
	PeakTorqueNm     float64 `json:"peakTorqueNm"`
	PeakPowerKw      float64 `json:"peakPowerKw"`

	// Learned redline & rev-limiter
	PeakPowerRpm      float64 `json:"peakPowerRpm"`
	LearnedLimiterRpm float64 `json:"learnedLimiterRpm"`
	RedlineRatio      float64 `json:"redlineRatio"`
	RedlineLearned    bool    `json:"redlineLearned"`
	RedlineConfidence float64 `json:"redlineConfidence"` // 0..1

	// Core tactile personality axes (wide contrast across engine & suspension families!)
	EngineRoundness     float64 `json:"engineRoundness"`     // 1.0=圆润饱满, 0.0=犀利撕裂
	EngineBurble        float64 `json:"engineBurble"`        // 0..1 怠速与低转突突肌肉感
	EngineFreqBaseHz    float64 `json:"engineFreqBaseHz"`    // 17..85 Hz wide family span
	EngineFreqTopHz     float64 `json:"engineFreqTopHz"`     // 60..148 Hz wide family span
	EngineBedScale      float64 `json:"engineBedScale"`      // 0.36..1.55 wide amplitude span!
	EngineTurboSurge    float64 `json:"engineTurboSurge"`    // 0..0.75 boost expansion punch
	SuspensionStiffness float64 `json:"suspensionStiffness"` // 0.0=绵软长行程越野, 1.0=生硬颠簸赛道
	SuspensionReboundMs float64 `json:"suspensionReboundMs"` // 45..160 ms
	ShiftCrispness      float64 `json:"shiftCrispness"`      // 0.0=无换挡/柔和, 1.0=序列式犀利撞击
	ShiftDurationMs     float64 `json:"shiftDurationMs"`     // 135..240 ms
	DirtSensitivity     float64 `json:"dirtSensitivity"`     // 0.8..1.4
	LandingSensitivity  float64 `json:"landingSensitivity"`  // 0.8..1.4
	SlipExpressiveness  float64 `json:"slipExpressiveness"`  // 0.8..1.35

	UserOverride bool `json:"userOverride"`
}

// EffectiveRedlineRatio returns the single-source-of-truth redline ratio shared by
// the haptic synthesizer and the LED lighting controller.
func (c CarPersonality) EffectiveRedlineRatio(maxRpm float32) float64 {
	if c.UserOverride {
		return clamp(c.RedlineRatio, 0.80, 0.985)
	}
	if c.RedlineLearned && c.LearnedLimiterRpm > 0 && maxRpm > 1000 {
		limRatio := clamp((c.LearnedLimiterRpm*0.982)/float64(maxRpm), 0.865, 0.985)
		if c.RedlineRatio >= 0.865 {
			return math.Min(limRatio, clamp(c.RedlineRatio, 0.865, 0.985))
		}
		return limRatio
	}
	return clamp(c.RedlineRatio, 0.865, 0.985)
}

type GarageEntry struct {
	CarOrdinal          int32   `json:"carOrdinal"`
	CustomName          string  `json:"customName,omitempty"`
	ArchetypeID         string  `json:"archetypeId"`
	Cylinders           int32   `json:"cylinders"`
	DriveTrain          int32   `json:"driveTrain"`
	CarPI               int32   `json:"carPi"`
	MaxRpm              float32 `json:"maxRpm"`
	InferredMaxSuspM    float64 `json:"inferredMaxSuspM"`
	PeakBoostPsi        float64 `json:"peakBoostPsi"`
	PeakTorqueNm        float64 `json:"peakTorqueNm"`
	PeakPowerKw         float64 `json:"peakPowerKw"`
	PeakPowerRpm        float64 `json:"peakPowerRpm"`
	LearnedLimiterRpm   float64 `json:"learnedLimiterRpm"`
	RedlineRatio        float64 `json:"redlineRatio"`
	RedlineLearned      bool    `json:"redlineLearned"`
	UserOverride        bool    `json:"userOverride"`
	EngineRoundness     float64 `json:"engineRoundness"`
	EngineBurble        float64 `json:"engineBurble"`
	SuspensionStiffness float64 `json:"suspensionStiffness"`
	ShiftCrispness      float64 `json:"shiftCrispness"`
	EngineBedScale      float64 `json:"engineBedScale"`
}

type GarageStore struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Cars          map[string]GarageEntry `json:"cars"`
}

type ArchetypeInfo struct {
	ID                  string   `json:"id"`
	Category            string   `json:"category"`
	Label               string   `json:"label"`
	Tags                []string `json:"tags"`
	Description         string   `json:"description"`
	EngineFamily        string   `json:"engineFamily"`
	EngineRoundness     float64  `json:"engineRoundness"`
	EngineBurble        float64  `json:"engineBurble"`
	EngineBedScale      float64  `json:"engineBedScale"`
	EngineFreqBaseHz    float64  `json:"engineFreqBaseHz"`
	EngineFreqTopHz     float64  `json:"engineFreqTopHz"`
	SuspensionStiffness float64  `json:"suspensionStiffness"`
	ShiftCrispness      float64  `json:"shiftCrispness"`
}

var (
	archetypeCatalog = []ArchetypeInfo{
		{ID: "electric_hyper", Category: "新能源电驱", Label: "纯电顶级超跑 / 极速电兽", Tags: []string{"零怠速", "高频丝滑电流", "零换挡"}, Description: "原地完全静谧，加速呈极轻盈丝滑高频电驱声浪（85-148Hz），无换挡顿挫。", EngineFamily: "ev", EngineRoundness: 0.96, EngineBurble: 0.0, EngineBedScale: 0.45, EngineFreqBaseHz: 85, EngineFreqTopHz: 148, SuspensionStiffness: 0.86, ShiftCrispness: 0.0},
		{ID: "electric_street", Category: "新能源电驱", Label: "纯电街车 / 电动越野", Tags: []string{"极静电驱", "突出路感", "柔韧悬架"}, Description: "极轻盈电机嗡鸣（76-132Hz，振幅仅 36%），最大程度突出路面与悬架。", EngineFamily: "ev", EngineRoundness: 0.94, EngineBurble: 0.0, EngineBedScale: 0.36, EngineFreqBaseHz: 76, EngineFreqTopHz: 132, SuspensionStiffness: 0.40, ShiftCrispness: 0.0},
		{ID: "rotary_screamer", Category: "特殊引擎", Label: "万转转子 / 高转三缸轻跑", Tags: []string{"高频蜂鸣撕裂", "万转高亢", "干脆换挡"}, Description: "独特转子高频蜂鸣（52-152Hz），振幅密实紧绷，与大排量截然相反。", EngineFamily: "rotary_f1", EngineRoundness: 0.14, EngineBurble: 0.06, EngineBedScale: 1.08, EngineFreqBaseHz: 52, EngineFreqTopHz: 152, SuspensionStiffness: 0.74, ShiftCrispness: 0.88},
		{ID: "baja_trophy_offroad", Category: "越野与拉力", Label: "Baja 奖杯卡车 / 长行程硬派越野", Tags: []string{"十字曲轴V8", "长行程避震", "重度砂石"}, Description: "紧致浑厚越野 V8（29-70Hz，振幅 130%），长行程避震与扎实落地。", EngineFamily: "muscle_v8", EngineRoundness: 0.72, EngineBurble: 0.75, EngineBedScale: 1.30, EngineFreqBaseHz: 29, EngineFreqTopHz: 70, SuspensionStiffness: 0.14, ShiftCrispness: 0.50},
		{ID: "track_x_prototype", Category: "顶级赛道", Label: "X 级原型赛车 / 方程式", Tags: []string{"万转高频嘶吼", "极硬生脆底盘", "序列式重击"}, Description: "高频尖锐赛用引擎（54-156Hz），极硬短行程底盘与闪电般序列式换挡撞击。", EngineFamily: "rotary_f1", EngineRoundness: 0.10, EngineBurble: 0.04, EngineBedScale: 1.15, EngineFreqBaseHz: 54, EngineFreqTopHz: 156, SuspensionStiffness: 0.97, ShiftCrispness: 0.99},
		{ID: "rally_awd_turbo", Category: "越野与拉力", Label: "WRC / B 组四驱涡轮拉力", Tags: []string{"涡轮起压爆发", "砂石高解析", "韧性长避震"}, Description: "低转轻快、涡轮起压后震感猛然膨胀爆发（25-96Hz），砂石与飞坡解析极高。", EngineFamily: "turbo_punch", EngineRoundness: 0.42, EngineBurble: 0.42, EngineBedScale: 0.96, EngineFreqBaseHz: 25, EngineFreqTopHz: 96, SuspensionStiffness: 0.34, ShiftCrispness: 0.85},
		{ID: "heavy_pickup_suv", Category: "越野与拉力", Label: "重型越野皮卡 / 大排量 SUV", Tags: []string{"大排量V8", "饱满扭矩", "柔韧长避震"}, Description: "28-68Hz 大排量 V8 机械共鸣（振幅 125%），干脆不拖沓的长行程避震。", EngineFamily: "muscle_v8", EngineRoundness: 0.76, EngineBurble: 0.70, EngineBedScale: 1.25, EngineFreqBaseHz: 28, EngineFreqTopHz: 68, SuspensionStiffness: 0.20, ShiftCrispness: 0.38},
		{ID: "v12_na_hypercar", Category: "自吸超跑", Label: "V12 高转自吸旗舰超跑", Tags: []string{"圆润丝绸高转", "126Hz华丽拉转", "精准底盘"}, Description: "毫无杂振的丝绸般圆润十二缸高频歌唱（34-126Hz），高转清亮华丽。", EngineFamily: "v12_silk", EngineRoundness: 0.88, EngineBurble: 0.05, EngineBedScale: 0.88, EngineFreqBaseHz: 34, EngineFreqTopHz: 126, SuspensionStiffness: 0.84, ShiftCrispness: 0.90},
		{ID: "v10_na_exotic", Category: "自吸超跑", Label: "V10 高亢自吸超跑", Tags: []string{"密实高亢", "双离合犀利", "紧致底盘"}, Description: "36-122Hz 密实紧绷十缸声浪震感，中高频爆发力强，双离合干脆利落。", EngineFamily: "flat_gt", EngineRoundness: 0.45, EngineBurble: 0.16, EngineBedScale: 1.04, EngineFreqBaseHz: 36, EngineFreqTopHz: 122, SuspensionStiffness: 0.80, ShiftCrispness: 0.88},
		{ID: "v8_flatplane_gt", Category: "赛道与GT", Label: "高转平面曲轴 V8 / GT 赛车", Tags: []string{"凌厉高频V8", "生硬赛道悬架", "强刹车解析"}, Description: "区别于美式十字曲轴 V8：频率更高（34-116Hz）、脉冲凌厉紧绷、悬架生硬直给。", EngineFamily: "flat_gt", EngineRoundness: 0.24, EngineBurble: 0.28, EngineBedScale: 1.12, EngineFreqBaseHz: 34, EngineFreqTopHz: 116, SuspensionStiffness: 0.88, ShiftCrispness: 0.92},
		{ID: "v8_rwd_muscle", Category: "肌肉与漂移", Label: "美式大排量 V8 后驱肌肉车", Tags: []string{"十字曲轴V8", "30Hz紧致低音", "135%饱满振幅"}, Description: "紧致有力的美式大排量十字曲轴 V8（30-72Hz，振幅 135%），拉转浑厚连贯不拖沓。", EngineFamily: "muscle_v8", EngineRoundness: 0.68, EngineBurble: 0.85, EngineBedScale: 1.35, EngineFreqBaseHz: 30, EngineFreqTopHz: 72, SuspensionStiffness: 0.38, ShiftCrispness: 0.68},
		{ID: "twin_turbo_supercar", Category: "增压超跑", Label: "现代双涡轮增压超跑 / Hypercar", Tags: []string{"涡轮暴涨推背", "紧致底盘", "高转绵密"}, Description: "中段涡轮建压带来强烈震感膨胀（28-108Hz），高速底盘紧绷稳健。", EngineFamily: "turbo_punch", EngineRoundness: 0.56, EngineBurble: 0.24, EngineBedScale: 1.02, EngineFreqBaseHz: 28, EngineFreqTopHz: 108, SuspensionStiffness: 0.78, ShiftCrispness: 0.86},
		{ID: "rwd_drift_jdm", Category: "肌肉与漂移", Label: "后驱 JDM 涡轮街跑 / 漂移特调", Tags: []string{"中后段涡轮爆冲", "清晰起漂", "手刹强化"}, Description: "低转轻快、起压后暴冲饱满（26-102Hz），后轮滑移表达尤为丰富。", EngineFamily: "turbo_punch", EngineRoundness: 0.46, EngineBurble: 0.36, EngineBedScale: 0.92, EngineFreqBaseHz: 26, EngineFreqTopHz: 102, SuspensionStiffness: 0.60, ShiftCrispness: 0.78},
		{ID: "rwd_lightweight_roadster", Category: "街跑与钢炮", Label: "轻量化后驱自吸小跑（MX-5/86/S2000）", Tags: []string{"轻盈细腻", "低振幅不扰手", "绝佳路感"}, Description: "轻快通透的小排量高转自吸（32-94Hz，振幅 62%），把空间让给路面与转向。", EngineFamily: "small_na", EngineRoundness: 0.66, EngineBurble: 0.14, EngineBedScale: 0.62, EngineFreqBaseHz: 32, EngineFreqTopHz: 94, SuspensionStiffness: 0.62, ShiftCrispness: 0.64},
		{ID: "fwd_hot_hatch", Category: "街跑与钢炮", Label: "前驱涡轮性能钢炮（Type R / GTI）", Tags: []string{"涡轮拉扯", "紧凑硬朗", "高转清脆"}, Description: "紧凑清脆的涡轮四缸（29-98Hz，起压爆发），前轮推头与烧胎反馈极为清晰。", EngineFamily: "turbo_punch", EngineRoundness: 0.44, EngineBurble: 0.26, EngineBedScale: 0.84, EngineFreqBaseHz: 29, EngineFreqTopHz: 98, SuspensionStiffness: 0.68, ShiftCrispness: 0.74},
		{ID: "fwd_classic_economy", Category: "经典与街车", Label: "前驱自吸经典小车 / 复古代步", Tags: []string{"极轻柔温和", "小振幅", "日常舒适"}, Description: "非常轻柔克制的小排量引擎（28-78Hz，振幅 50%），柔和换挡与舒适避震。", EngineFamily: "small_na", EngineRoundness: 0.80, EngineBurble: 0.18, EngineBedScale: 0.50, EngineFreqBaseHz: 28, EngineFreqTopHz: 78, SuspensionStiffness: 0.34, ShiftCrispness: 0.40},
		{ID: "vintage_classic_cruiser", Category: "经典与街车", Label: "经典老爷车 / 复古巡航 GT", Tags: []string{"老机械共鸣", "柔韧避震", "干脆换挡"}, Description: "32-76Hz 经典老机械质感（振幅 122%）搭配舒展有韧性的经典减震。", EngineFamily: "muscle_v8", EngineRoundness: 0.78, EngineBurble: 0.60, EngineBedScale: 1.22, EngineFreqBaseHz: 32, EngineFreqTopHz: 76, SuspensionStiffness: 0.24, ShiftCrispness: 0.42},
		{ID: "awd_performance_gt", Category: "全能兜底", Label: "四驱全能性能车 / 通用均衡", Tags: []string{"饱满韧性", "左右独立路感", "全路况均衡"}, Description: "27-94Hz 饱满均衡的四驱性能性格，兼顾引擎机械质感与清晰路感。", EngineFamily: "turbo_punch", EngineRoundness: 0.58, EngineBurble: 0.28, EngineBedScale: 0.95, EngineFreqBaseHz: 27, EngineFreqTopHz: 94, SuspensionStiffness: 0.56, ShiftCrispness: 0.72},
	}
	archetypeByID map[string]ArchetypeInfo
)

func init() {
	archetypeByID = make(map[string]ArchetypeInfo, len(archetypeCatalog))
	for _, info := range archetypeCatalog {
		archetypeByID[info.ID] = info
	}
}

// IsValidArchetypeID reports whether id matches one of the built-in vehicle archetypes.
func IsValidArchetypeID(id string) bool {
	_, ok := archetypeByID[id]
	return ok
}

// AllArchetypes returns all 18 full-spectrum dynamic vehicle archetypes with wide engine & suspension contrast.
func AllArchetypes() []ArchetypeInfo {
	out := make([]ArchetypeInfo, len(archetypeCatalog))
	copy(out, archetypeCatalog)
	return out
}

type Profiler struct {
	mu          sync.Mutex
	garagePath  string
	garage      GarageStore
	active      CarPersonality
	hasActive   bool
	dirtyGarage bool

	suspSamples []float64

	binPowerSum     [20]float64
	binCounts       [20]uint32
	peakPowerKw     float64
	peakPowerRpm    float64
	learnedLimRpm   float64
	limiterHits     int
	upshiftEma      float64
	upshiftN        int
	prevGear        uint8
	prevRpm         float64
	prevPowerKw     float64
	rpmBounceCount  int
	lastPersistTick int
}

func NewProfiler(garagePath string) *Profiler {
	p := &Profiler{
		garagePath: garagePath,
		garage: GarageStore{
			SchemaVersion: 1,
			Cars:          make(map[string]GarageEntry),
		},
		active: defaultPersonality(),
	}
	p.loadGarage()
	return p
}

func defaultPersonality() CarPersonality {
	return CarPersonality{
		ArchetypeID:         "awd_performance_gt",
		Category:            "全能兜底",
		Label:               "等待车辆接入（通用均衡）",
		Tags:                []string{"饱满韧性", "左右独立路感"},
		EngineFamily:        "turbo_punch",
		RedlineRatio:        0.925,
		EngineRoundness:     0.58,
		EngineBurble:        0.28,
		EngineFreqBaseHz:    27,
		EngineFreqTopHz:     94,
		EngineBedScale:      0.95,
		EngineTurboSurge:    0.35,
		SuspensionStiffness: 0.55,
		SuspensionReboundMs: 92,
		ShiftCrispness:      0.72,
		ShiftDurationMs:     70,
		DirtSensitivity:     1.0,
		LandingSensitivity:  1.0,
		SlipExpressiveness:  1.0,
	}
}

func (p *Profiler) loadGarage() {
	b, err := os.ReadFile(p.garagePath)
	if err != nil {
		return
	}
	var g GarageStore
	if err := json.Unmarshal(b, &g); err == nil && g.Cars != nil {
		cleaned := false
		for k, entry := range g.Cars {
			if !entry.UserOverride && entry.RedlineRatio > 0 && entry.RedlineRatio < 0.90 {
				entry.RedlineRatio = 0.925
				entry.RedlineLearned = false
				entry.LearnedLimiterRpm = 0
				g.Cars[k] = entry
				cleaned = true
			}
		}
		p.garage = g
		if cleaned {
			p.saveGarageLocked()
		}
	}
}

func (p *Profiler) saveGarageLocked() {
	if p.garagePath == "" {
		return
	}
	b, err := json.MarshalIndent(p.garage, "", "  ")
	if err == nil {
		_ = atomicWriteFile(p.garagePath, b, 0644)
	}
	p.dirtyGarage = false
}

// FlushGarage persists pending garage updates to disk (safe to call on shutdown).
func (p *Profiler) FlushGarage() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dirtyGarage {
		p.syncCurrentToGarageLocked()
		p.saveGarageLocked()
		p.lastPersistTick = 0
	}
}

func (p *Profiler) Observe(pkt telemetry.Packet, autoMatch, autoRedline bool) CarPersonality {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !pkt.IsRaceOn || pkt.CarOrdinal == 0 {
		return p.active
	}

	carChanged := !p.hasActive || p.active.CarOrdinal != pkt.CarOrdinal
	swapDetected := p.hasActive && p.active.CarOrdinal == pkt.CarOrdinal &&
		pkt.EngineMaxRpm > 1500 && p.active.MaxRpm > 1500 &&
		math.Abs(float64(pkt.EngineMaxRpm-p.active.MaxRpm)) > 150.0

	if carChanged || swapDetected {
		p.initVehicleLocked(pkt, swapDetected)
	}

	p.active.Cylinders = pkt.Cylinders
	p.active.DriveTrain = pkt.DriveTrain
	p.active.CarClass = pkt.CarClass
	p.active.CarPI = pkt.CarPI
	p.active.IdleRpm = pkt.EngineIdleRpm
	if pkt.EngineMaxRpm > 1000 {
		p.active.MaxRpm = pkt.EngineMaxRpm
	}

	for i := 0; i < 4; i++ {
		ns := float64(pkt.NormSusp[i])
		sm := float64(pkt.SuspTravelM[i])
		if ns >= 0.25 && ns <= 0.90 && sm > 0.015 {
			est := sm / ns
			if est >= 0.04 && est <= 0.75 {
				p.suspSamples = append(p.suspSamples, est)
				if len(p.suspSamples) > 65 {
					p.suspSamples = p.suspSamples[len(p.suspSamples)-65:]
				}
			}
		}
	}
	if len(p.suspSamples) >= 8 {
		cp := make([]float64, len(p.suspSamples))
		copy(cp, p.suspSamples)
		sort.Float64s(cp)
		med := cp[len(cp)/2]
		if p.active.InferredMaxSuspM < 0.03 {
			p.active.InferredMaxSuspM = med
		} else if med >= p.active.InferredMaxSuspM*0.85 {
			p.active.InferredMaxSuspM = 0.85*p.active.InferredMaxSuspM + 0.15*med
		} else if len(p.suspSamples) >= 16 {
			// Allow gradual downward tracking when suspension is stiffened/swapped.
			p.active.InferredMaxSuspM = 0.92*p.active.InferredMaxSuspM + 0.08*med
		}
	}

	if float64(pkt.BoostPsi) > p.active.PeakBoostPsi && pkt.BoostPsi < 80 {
		p.active.PeakBoostPsi = float64(pkt.BoostPsi)
	}
	if float64(pkt.TorqueNm) > p.active.PeakTorqueNm && pkt.TorqueNm < 5000 {
		p.active.PeakTorqueNm = float64(pkt.TorqueNm)
	}
	kw := math.Max(0, float64(pkt.PowerW)/1000.0)
	if kw > p.active.PeakPowerKw && kw < 3000 {
		p.active.PeakPowerKw = kw
	}

	if autoMatch && !p.active.UserOverride {
		p.applyArchetypeLocked()
	}

	if autoRedline && !p.active.UserOverride && !p.active.IsElectric && pkt.EngineMaxRpm >= 2500 {
		p.stepRedlineLearnerLocked(pkt, kw)
	}

	p.lastPersistTick++
	if p.dirtyGarage && p.lastPersistTick >= 240 {
		p.syncCurrentToGarageLocked()
		p.saveGarageLocked()
		p.lastPersistTick = 0
	}

	return p.active
}

func (p *Profiler) initVehicleLocked(pkt telemetry.Packet, swapDetected bool) {
	p.hasActive = true
	p.suspSamples = p.suspSamples[:0]
	p.resetRedlineLearnerLocked()

	p.active = defaultPersonality()
	p.active.CarOrdinal = pkt.CarOrdinal
	p.active.Cylinders = pkt.Cylinders
	p.active.DriveTrain = pkt.DriveTrain
	p.active.CarClass = pkt.CarClass
	p.active.CarPI = pkt.CarPI
	p.active.IdleRpm = pkt.EngineIdleRpm
	p.active.MaxRpm = pkt.EngineMaxRpm
	p.active.IsElectric = pkt.Cylinders == 0
	p.active.IsRotary = (pkt.Cylinders == 2 || pkt.Cylinders == 3) && pkt.EngineMaxRpm >= 8500

	key := strconv.Itoa(int(pkt.CarOrdinal))
	if saved, ok := p.garage.Cars[key]; ok && !swapDetected {
		p.active.CustomName = saved.CustomName
		p.active.InferredMaxSuspM = saved.InferredMaxSuspM
		p.active.PeakBoostPsi = saved.PeakBoostPsi
		p.active.PeakTorqueNm = saved.PeakTorqueNm
		p.active.PeakPowerKw = saved.PeakPowerKw
		p.active.PeakPowerRpm = saved.PeakPowerRpm
		if (saved.RedlineRatio >= 0.90 || (saved.UserOverride && saved.RedlineRatio >= 0.80)) && saved.RedlineRatio <= 0.985 {
			p.active.LearnedLimiterRpm = saved.LearnedLimiterRpm
			p.active.RedlineRatio = saved.RedlineRatio
			p.active.RedlineLearned = saved.RedlineLearned
			if saved.RedlineLearned {
				p.active.RedlineConfidence = 1.0
				if saved.LearnedLimiterRpm > 0 {
					p.learnedLimRpm = saved.LearnedLimiterRpm
					p.limiterHits = 3
				}
			}
			if saved.PeakPowerRpm > 0 {
				p.peakPowerRpm = saved.PeakPowerRpm
			}
			if saved.PeakPowerKw > 0 {
				p.peakPowerKw = saved.PeakPowerKw
			}
		} else {
			p.dirtyGarage = true
		}
		p.applyArchetypeLocked()
		if saved.UserOverride {
			p.active.UserOverride = true
			p.active.EngineRoundness = saved.EngineRoundness
			p.active.EngineBurble = saved.EngineBurble
			p.active.SuspensionStiffness = saved.SuspensionStiffness
			p.active.ShiftCrispness = saved.ShiftCrispness
			if saved.EngineBedScale > 0 {
				p.active.EngineBedScale = saved.EngineBedScale
			}
			p.recomputeDerivedFeelLocked()
		}
	} else {
		p.applyArchetypeLocked()
		p.dirtyGarage = true
	}
}

func (p *Profiler) resetRedlineLearnerLocked() {
	p.binPowerSum = [20]float64{}
	p.binCounts = [20]uint32{}
	p.peakPowerKw = 0
	p.peakPowerRpm = 0
	p.learnedLimRpm = 0
	p.limiterHits = 0
	p.upshiftEma = 0
	p.upshiftN = 0
	p.prevGear = 0
	p.prevRpm = 0
	p.prevPowerKw = 0
	p.rpmBounceCount = 0
}

func (p *Profiler) stepRedlineLearnerLocked(pkt telemetry.Packet, kw float64) {
	maxRpm := float64(pkt.EngineMaxRpm)
	curRpm := float64(pkt.CurrentRpm)
	ratio := curRpm / maxRpm

	maxSlip := 0.0
	for i := 0; i < 4; i++ {
		if s := math.Abs(float64(pkt.CombinedSlip[i])); s > maxSlip {
			maxSlip = s
		}
	}
	stablePull := pkt.Accel >= 200 && pkt.Gear >= 1 && pkt.Gear <= 10 && pkt.SpeedKmh > 25 && maxSlip < 1.15

	if stablePull && ratio >= 0.60 && ratio <= 1.02 && kw > 15 {
		bi := int((ratio - 0.60) / 0.02)
		if bi < 0 {
			bi = 0
		} else if bi > 19 {
			bi = 19
		}
		p.binPowerSum[bi] += kw
		p.binCounts[bi]++
		avgKw := p.binPowerSum[bi] / float64(p.binCounts[bi])
		if p.binCounts[bi] >= 4 && avgKw > p.peakPowerKw {
			p.peakPowerKw = avgKw
			p.peakPowerRpm = (0.61 + float64(bi)*0.02) * maxRpm
			p.active.PeakPowerRpm = p.peakPowerRpm
		}
	}

	topRatio := math.Max(curRpm, p.prevRpm) / maxRpm
	if stablePull && pkt.Accel >= 242 && topRatio >= 0.885 && p.peakPowerKw > 35 {
		powerCliff := p.prevPowerKw > p.peakPowerKw*0.78 && kw < p.peakPowerKw*0.72
		rpmBounce := p.prevRpm > curRpm+28 && p.prevRpm >= maxRpm*0.885
		if powerCliff || rpmBounce {
			p.rpmBounceCount++
			cand := math.Max(curRpm, p.prevRpm)
			if p.rpmBounceCount >= 3 {
				if p.limiterHits == 0 {
					p.learnedLimRpm = cand
				} else {
					p.learnedLimRpm = 0.75*p.learnedLimRpm + 0.25*cand
				}
				p.limiterHits++
				p.active.LearnedLimiterRpm = p.learnedLimRpm
			}
		}
	} else if ratio < 0.84 || pkt.Accel < 200 {
		p.rpmBounceCount = 0
	}

	if p.prevGear >= 1 && pkt.Gear == p.prevGear+1 && pkt.Accel >= 200 && p.prevRpm > maxRpm*0.88 && maxSlip < 1.25 {
		if p.upshiftN == 0 {
			p.upshiftEma = p.prevRpm
		} else {
			p.upshiftEma = 0.7*p.upshiftEma + 0.3*p.prevRpm
		}
		p.upshiftN++
	}

	if p.limiterHits >= 1 && p.learnedLimRpm >= maxRpm*0.885 {
		target := p.learnedLimRpm * 0.982
		if p.peakPowerRpm > 0 && p.peakPowerRpm*1.01 > target && p.peakPowerRpm*1.01 >= p.learnedLimRpm*0.975 {
			target = p.peakPowerRpm * 1.01
		}
		p.active.RedlineRatio = clamp(target/maxRpm, 0.90, 0.985)
		p.active.RedlineLearned = true
		p.active.RedlineConfidence = math.Min(1.0, 0.65+0.15*float64(p.limiterHits))
		p.dirtyGarage = true
	} else if p.upshiftN >= 3 && p.upshiftEma > maxRpm*0.895 {
		target := p.upshiftEma * 0.985
		p.active.RedlineRatio = clamp(target/maxRpm, 0.90, 0.985)
		p.active.RedlineLearned = true
		p.active.RedlineConfidence = math.Min(0.85, 0.40+0.15*float64(p.upshiftN))
		p.dirtyGarage = true
	} else if p.peakPowerRpm > 0 {
		p.active.RedlineConfidence = math.Min(0.35, float64(p.upshiftN)*0.15+0.20)
	}

	p.prevGear = pkt.Gear
	p.prevRpm = curRpm
	p.prevPowerKw = kw
}

func (p *Profiler) applyArchetypeLocked() {
	a := &p.active
	a.IsElectric = a.Cylinders == 0
	a.IsRotary = (a.Cylinders == 2 || a.Cylinders == 3) && a.MaxRpm >= 8500

	id := "awd_performance_gt"
	switch {
	case a.Cylinders == 0 && a.CarPI >= 850:
		id = "electric_hyper"
	case a.Cylinders == 0:
		id = "electric_street"
	case (a.Cylinders == 2 || a.Cylinders == 3) && a.MaxRpm >= 8500:
		id = "rotary_screamer"
	case a.InferredMaxSuspM >= 0.24:
		id = "baja_trophy_offroad"
	case a.CarPI >= 950:
		id = "track_x_prototype"
	case a.DriveTrain == 2 && a.PeakBoostPsi >= 6.0 && a.InferredMaxSuspM >= 0.14:
		id = "rally_awd_turbo"
	case a.MaxRpm > 1000 && a.MaxRpm <= 6500 && a.PeakTorqueNm >= 600:
		id = "heavy_pickup_suv"
	case a.Cylinders == 12 && a.PeakBoostPsi <= 1.5:
		id = "v12_na_hypercar"
	case a.Cylinders == 10 && a.PeakBoostPsi <= 1.5:
		id = "v10_na_exotic"
	case a.Cylinders == 8 && a.MaxRpm >= 8200 && a.CarPI >= 800:
		id = "v8_flatplane_gt"
	case a.Cylinders == 8 && a.DriveTrain == 1:
		id = "v8_rwd_muscle"
	case a.CarPI >= 850 && a.PeakBoostPsi >= 8.0:
		id = "twin_turbo_supercar"
	case a.DriveTrain == 1 && (a.Cylinders == 4 || a.Cylinders == 6) && a.PeakBoostPsi >= 5.0:
		id = "rwd_drift_jdm"
	case a.DriveTrain == 1 && (a.Cylinders == 4 || a.Cylinders == 6) && a.PeakBoostPsi <= 1.5:
		id = "rwd_lightweight_roadster"
	case a.DriveTrain == 0 && a.CarPI >= 620:
		id = "fwd_hot_hatch"
	case a.DriveTrain == 0:
		id = "fwd_classic_economy"
	case a.CarPI > 0 && a.CarPI <= 620 && a.MaxRpm > 1000 && a.MaxRpm <= 7200:
		id = "vintage_classic_cruiser"
	default:
		id = "awd_performance_gt"
	}

	if id != a.ArchetypeID {
		p.dirtyGarage = true
	}
	p.applyArchetypeByIDLocked(id)

	if !a.IsElectric {
		if a.PeakTorqueNm > 700 {
			a.EngineBedScale = clamp(a.EngineBedScale*1.08, 0.35, 1.38)
		} else if a.PeakTorqueNm > 50 && a.PeakTorqueNm < 260 {
			a.EngineBedScale = clamp(a.EngineBedScale*0.86, 0.35, 1.38)
		}
		if a.MaxRpm >= 9500 && a.EngineFreqTopHz < 120 {
			a.EngineFreqTopHz += 16
		}
	}
	if a.InferredMaxSuspM >= 0.05 {
		physStiff := clamp(1.0-(a.InferredMaxSuspM-0.06)/0.24, 0.10, 0.98)
		a.SuspensionStiffness = 0.55*a.SuspensionStiffness + 0.45*physStiff
	}
	p.recomputeDerivedFeelLocked()
}

func (p *Profiler) applyArchetypeByIDLocked(id string) {
	a := &p.active
	if info, ok := archetypeByID[id]; ok {
		a.ArchetypeID = info.ID
		a.Category = info.Category
		a.Label = info.Label
		a.Tags = info.Tags
		a.EngineFamily = info.EngineFamily
		a.EngineRoundness = info.EngineRoundness
		a.EngineBurble = info.EngineBurble
		a.EngineBedScale = info.EngineBedScale
		a.EngineFreqBaseHz = info.EngineFreqBaseHz
		a.EngineFreqTopHz = info.EngineFreqTopHz
		a.SuspensionStiffness = info.SuspensionStiffness
		a.ShiftCrispness = info.ShiftCrispness
	}
	a.EngineTurboSurge = 0.20
	if a.EngineFamily == "turbo_punch" {
		a.EngineTurboSurge = 0.68
	} else if a.EngineFamily == "ev" {
		a.EngineTurboSurge = 0.0
	}
	a.DirtSensitivity = 1.0
	a.LandingSensitivity = 1.0
	a.SlipExpressiveness = 1.0
	if !a.RedlineLearned {
		a.RedlineRatio = 0.925
	}
	switch id {
	case "rotary_screamer", "track_x_prototype":
		if !a.RedlineLearned {
			a.RedlineRatio = 0.945
		}
	case "baja_trophy_offroad":
		a.DirtSensitivity = 1.35
		a.LandingSensitivity = 1.32
	case "rally_awd_turbo":
		a.DirtSensitivity = 1.30
		a.LandingSensitivity = 1.24
	case "heavy_pickup_suv":
		a.DirtSensitivity = 1.20
	case "v12_na_hypercar", "v10_na_exotic", "v8_flatplane_gt":
		if !a.RedlineLearned {
			a.RedlineRatio = 0.935
		}
	case "v8_rwd_muscle", "rwd_drift_jdm":
		a.SlipExpressiveness = 1.25
	case "fwd_hot_hatch":
		a.SlipExpressiveness = 1.20
	}
}

func (p *Profiler) recomputeDerivedFeelLocked() {
	a := &p.active
	a.SuspensionReboundMs = 155.0 - 107.0*clamp(a.SuspensionStiffness, 0, 1)
	a.ShiftDurationMs = 95.0 - 35.0*clamp(a.ShiftCrispness, 0, 1)
}

func (p *Profiler) Active() CarPersonality {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *Profiler) ApplyDemoArchetype(id string) CarPersonality {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !IsValidArchetypeID(id) {
		return p.active
	}
	p.active.IsElectric = id == "electric_hyper" || id == "electric_street"
	p.active.IsRotary = id == "rotary_screamer"
	p.applyArchetypeByIDLocked(id)
	p.recomputeDerivedFeelLocked()
	return p.active
}

func (p *Profiler) SaveCurrentOverride(customName string, roundness, burble, stiffness, shiftCrisp, bedScale, redlineRatio float64) CarPersonality {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active.UserOverride = true
	p.active.CustomName = strings.TrimSpace(customName)
	p.active.EngineRoundness = sanitizeFloat(roundness, p.active.EngineRoundness, 0, 1)
	p.active.EngineBurble = sanitizeFloat(burble, p.active.EngineBurble, 0, 1)
	p.active.SuspensionStiffness = sanitizeFloat(stiffness, p.active.SuspensionStiffness, 0, 1)
	p.active.ShiftCrispness = sanitizeFloat(shiftCrisp, p.active.ShiftCrispness, 0, 1)
	p.active.EngineBedScale = sanitizeFloat(bedScale, p.active.EngineBedScale, 0.3, 1.8)
	if !math.IsNaN(redlineRatio) && !math.IsInf(redlineRatio, 0) && redlineRatio >= 0.80 && redlineRatio <= 0.985 {
		p.active.RedlineRatio = redlineRatio
		p.active.RedlineLearned = true
		p.active.RedlineConfidence = 1.0
	}
	p.recomputeDerivedFeelLocked()
	if p.active.CarOrdinal != 0 {
		p.syncCurrentToGarageLocked()
		p.saveGarageLocked()
	}
	return p.active
}

func (p *Profiler) ResetCurrentOverride() CarPersonality {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active.UserOverride = false
	p.active.CustomName = ""
	p.active.RedlineLearned = false
	p.active.LearnedLimiterRpm = 0
	p.active.RedlineConfidence = 0
	p.resetRedlineLearnerLocked()
	p.applyArchetypeLocked()
	if p.active.CarOrdinal != 0 {
		p.syncCurrentToGarageLocked()
		p.saveGarageLocked()
	}
	return p.active
}

func (p *Profiler) syncCurrentToGarageLocked() {
	if p.active.CarOrdinal == 0 {
		return
	}
	key := strconv.Itoa(int(p.active.CarOrdinal))
	p.garage.Cars[key] = GarageEntry{
		CarOrdinal:          p.active.CarOrdinal,
		CustomName:          p.active.CustomName,
		ArchetypeID:         p.active.ArchetypeID,
		Cylinders:           p.active.Cylinders,
		DriveTrain:          p.active.DriveTrain,
		CarPI:               p.active.CarPI,
		MaxRpm:              p.active.MaxRpm,
		InferredMaxSuspM:    p.active.InferredMaxSuspM,
		PeakBoostPsi:        p.active.PeakBoostPsi,
		PeakTorqueNm:        p.active.PeakTorqueNm,
		PeakPowerKw:         p.active.PeakPowerKw,
		PeakPowerRpm:        p.active.PeakPowerRpm,
		LearnedLimiterRpm:   p.active.LearnedLimiterRpm,
		RedlineRatio:        p.active.RedlineRatio,
		RedlineLearned:      p.active.RedlineLearned,
		UserOverride:        p.active.UserOverride,
		EngineRoundness:     p.active.EngineRoundness,
		EngineBurble:        p.active.EngineBurble,
		SuspensionStiffness: p.active.SuspensionStiffness,
		ShiftCrispness:      p.active.ShiftCrispness,
		EngineBedScale:      p.active.EngineBedScale,
	}
}

func (p *Profiler) ListGarage() []GarageEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]GarageEntry, 0, len(p.garage.Cars))
	for _, v := range p.garage.Cars {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CarOrdinal < out[j].CarOrdinal })
	return out
}

func (p *Profiler) DeleteGarageEntry(ordinal int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.garage.Cars, strconv.Itoa(int(ordinal)))
	if p.hasActive && p.active.CarOrdinal == ordinal {
		p.active.UserOverride = false
		p.active.CustomName = ""
		p.active.RedlineLearned = false
		p.active.LearnedLimiterRpm = 0
		p.active.RedlineConfidence = 0
		p.resetRedlineLearnerLocked()
		p.applyArchetypeLocked()
	}
	p.saveGarageLocked()
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
