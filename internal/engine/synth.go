package engine

import (
	"math"

	"forzanext/internal/hid"
	"forzanext/internal/telemetry"
)

// ActiveEvent describes a deduced live vehicle state/event for UI visualization.
type ActiveEvent struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Category  string  `json:"category"` // engine, road, chassis, impact, water, tire
	Intensity float64 `json:"intensity"`
	Channel   string  `json:"channel"` // grip, grip_l, grip_r, trig_l, trig_r, both
}

// RenderResult holds one 8 ms synthesized output packet + rich diagnostic metadata.
type RenderResult struct {
	Frame          [33]byte
	Flags          byte
	Trig           [8]byte
	GripL          [8]byte
	GripR          [8]byte
	GripLRms       float64
	GripRRms       float64
	TrigRoute      int // -1=none/collision, 0=left, 1=right, 2=both
	RightTrigNeed  bool
	SurfaceLabel   string
	ImpactActive   bool
	ImpactAngleDeg float64 // 0=front, +90=left hit, -90=right hit, +-180=rear
	ImpactLabel    string
	ActiveEvents   []ActiveEvent
	Clipped        bool
	ConflictSolved bool
}

type trigPriority uint8

const (
	tpOff              trigPriority = 0
	tpReleaseTail      trigPriority = 1
	tpStaticResistance trigPriority = 2
	tpEdgeWarning      trigPriority = 3
	tpDynamicEvent     trigPriority = 4
)

type pebbleGrain struct {
	activeS float64
	durS    float64
	freqHz  float64
	amp     float64
	sharp   float64
}

// Synthesizer v4:
// - Anti-Clatter Trigger Engine: bipolar 32ms 110Hz damped AC shift, pure sine 85-140Hz sweet spot, ±64 cap [60,196], 10ms soft ramp, 70ms route hysteresis
// - 4-Bus Anti-Mush Grip Engine: bed / road / tire / imp with intra-road ducking + two-level sidechain ducking + frequency slotting
// - 4-Tire physical understeer/oversteer routing to L/R Grips AND L/R Triggers
// - Two-Tier 360° Directional Collisions (strong low-speed bumps + razor-sharp 100% vs 0% L/R isolation)
// - High-sensitivity Directional Guardrail Scrape (opposing-steer + micro-jerk, 100% vs 0% L/R isolation)
// - 7 Distinct Engine Families with massive frequency (17..148Hz), amplitude (0.36x..1.55x) & rhythm contrast
type Synthesizer struct {
	phEngBase    float64
	phEngSub     float64
	phEngLope    float64
	phTarmacL    float64
	phTarmacR    float64
	phRoadL      float64
	phRoadR      float64
	phHeave      float64
	phCurbGateL  float64
	phCurbGateR  float64
	phCurbToneL  float64
	phCurbToneR  float64
	phGrassL     float64
	phGrassR     float64
	phGrassMod   float64
	phWater      float64
	phWaterSwell float64
	phUnderL     float64
	phUnderR     float64
	phUnderHop   float64
	phOverL      float64
	phOverR      float64
	phOverSweep  float64
	phRedline    float64
	phRedSub     float64
	phRedGate    float64
	phTrigL      float64
	phTrigR      float64
	phAbsPulse   float64
	phAbsGrip    float64
	phWallA      float64
	phWallB      float64
	phGuard      float64
	phSmash      float64

	rngL       uint32
	rngR       uint32
	pebL       pebbleGrain
	pebR       pebbleGrain
	pebNextMsL float64
	pebNextMsR float64

	suspInit    bool
	prevSuspM   [4]float64
	prevSuspVel [4]float64
	hpSuspVel   [4]float64
	lpSuspMean  float64
	heaveEnv    float64
	roadEnvL    float64
	roadEnvR    float64

	curbEnvL  float64
	curbEnvR  float64
	curbHoldL float64
	curbHoldR float64

	airborne      bool
	airTimeS      float64
	landLastAirS  float64
	landWinS      float64
	landActiveS   float64
	landDurS      float64
	landPeak      float64
	landLeftW     float64
	landRightW    float64
	landReboundMs float64

	prevGear       uint8
	shiftActiveS   float64
	shiftDurS      float64
	shiftStrength  float64
	shiftIsUpshift bool

	// Two-Tier 360° Directional Collision + Guardrail Engine
	emaAx            float64
	emaAz            float64
	prevAx           float64
	prevAz           float64
	spdWin           [6]float64
	spdIdx           int
	spdCount         int
	latWin           [10]float64
	latIdx           int
	latCount         int
	impactActiveS    float64
	impactDurS       float64
	impactPeak       float64
	impactEntrySpd   float64
	impactAngleDeg   float64
	impactLabel      string
	impactKind       string
	impactLeftW      float64
	impactRightW     float64
	impactMuteL      bool
	impactMuteR      bool
	impactDelayL     float64
	impactDelayR     float64
	impactYawKick    float64
	impactCoolS      float64
	impactLowSpeed   bool
	lastImpactSide   int     // -1=left, +1=right, 0=center
	lastImpactSinceS float64 // seconds since last collision

	guardActiveS  float64
	guardElapsedS float64
	guardPeak     float64
	guardSide     int // -1=left, +1=right, 0=both
	smashActiveS  float64
	smashDurS     float64
	smashPeak     float64
	prevSmashDiff float32

	prevMaxPuddle float64
	splashActiveS float64
	splashDurS    float64
	splashPeak    float64
	splashLeftW   float64
	splashRightW  float64

	// Two-level grip sidechain ducking + collision isolation ramps
	engineDuckGain float64
	midDuckGain    float64
	collisionRampL float64
	collisionRampR float64

	// Anti-clatter trigger soft attack/release ramps + 70ms route hysteresis
	trigEnvL       float64
	trigEnvR       float64
	trigLastFreqL  float64
	trigLastAmpL   float64
	trigLastFreqR  float64
	trigLastAmpR   float64
	lastTrigRoute  int
	lastTrigPrio   trigPriority
	trigRouteHoldS float64

	hasPrevPkt  bool
	hasWheelRot bool

	testEventID  string
	testRemainS  float64
	testElapsedS float64
	testTotalS   float64
}

func NewSynthesizer() *Synthesizer {
	return &Synthesizer{
		rngL:             0x13579BDF,
		rngR:             0x2468ACE1,
		engineDuckGain:   1.0,
		midDuckGain:      1.0,
		collisionRampL:   1.0,
		collisionRampR:   1.0,
		lastImpactSinceS: 10.0,
		lastTrigRoute:    -1,
	}
}

func (s *Synthesizer) nextRand01(state *uint32) float64 {
	x := *state
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	*state = x
	return float64(x&0xFFFF) / 65535.0
}

func (s *Synthesizer) nextNoise(state *uint32) float64 {
	return s.nextRand01(state)*2.0 - 1.0
}

// IsValidTestEventID reports whether id is a supported Test Bench haptic event.
func IsValidTestEventID(id string) bool {
	switch id {
	case "engineIdle", "engineRev", "gearShift", "redline",
		"roadUndulation", "surfTarmac", "surfDirt", "roadSurface", "surfGravel", "surfGrass",
		"curbLeft", "curbRumble", "curbRight", "jumpLanding",
		"impactLowSpeed", "collisionWall", "impactFront", "impactRear", "impactLeft", "impactRight",
		"guardrail", "guardrailLeft", "guardrailRight", "waterSplash",
		"understeer", "understeerLeft", "understeerRight",
		"oversteerDrift", "oversteerLeft", "tireSlipDrift", "oversteerRight",
		"brakingAbs":
		return true
	default:
		return false
	}
}

func (s *Synthesizer) TriggerTest(eventID string) {
	if !IsValidTestEventID(eventID) {
		return
	}
	s.testEventID = eventID
	s.testElapsedS = 0
	switch eventID {
	case "gearShift":
		s.testTotalS = 0.12
	case "jumpLanding":
		s.testTotalS = 0.16
	case "impactLowSpeed":
		s.testTotalS = 0.095
	case "collisionWall", "impactFront", "impactRear", "impactLeft", "impactRight":
		s.testTotalS = 0.135
	case "guardrail", "guardrailLeft", "guardrailRight":
		s.testTotalS = 0.55
	case "waterSplash":
		s.testTotalS = 0.78
	case "curbLeft", "curbRight", "curbRumble":
		s.testTotalS = 0.72
	default:
		s.testTotalS = 0.90
	}
	s.testRemainS = s.testTotalS
}

func (s *Synthesizer) resetDynamicState() {
	s.suspInit = false
	s.prevSuspVel = [4]float64{}
	s.hpSuspVel = [4]float64{}
	s.heaveEnv = 0
	s.roadEnvL = 0
	s.roadEnvR = 0
	s.pebL = pebbleGrain{}
	s.pebR = pebbleGrain{}
	s.pebNextMsL = 0
	s.pebNextMsR = 0
	s.curbEnvL = 0
	s.curbEnvR = 0
	s.curbHoldL = 0
	s.curbHoldR = 0
	s.airborne = false
	s.airTimeS = 0
	s.landLastAirS = 0
	s.landWinS = 0
	s.landActiveS = 0
	s.shiftActiveS = 0
	s.spdCount = 0
	s.spdIdx = 0
	s.latCount = 0
	s.latIdx = 0
	s.impactActiveS = 0
	s.impactEntrySpd = 0
	s.impactCoolS = 0
	s.lastImpactSinceS = 10.0
	s.lastImpactSide = 0
	s.guardActiveS = 0
	s.guardElapsedS = 0
	s.guardPeak = 0
	s.guardSide = 0
	s.smashActiveS = 0
	s.splashActiveS = 0
	s.engineDuckGain = 1.0
	s.midDuckGain = 1.0
	s.collisionRampL = 1.0
	s.collisionRampR = 1.0
	s.trigEnvL = 0
	s.trigEnvR = 0
	s.trigLastFreqL = 0
	s.trigLastAmpL = 0
	s.trigLastFreqR = 0
	s.trigLastAmpR = 0
	s.lastTrigRoute = -1
	s.lastTrigPrio = tpOff
	s.trigRouteHoldS = 0
	s.hasPrevPkt = false
	s.hasWheelRot = false
}

// Reset clears all live synthesis and test-bench state (e.g. when switching out of Entertainment Mode).
func (s *Synthesizer) Reset() {
	s.resetDynamicState()
	s.testEventID = ""
	s.testRemainS = 0
	s.testElapsedS = 0
	s.testTotalS = 0
}

func (s *Synthesizer) StepPacket(pkt telemetry.Packet, cfg NextConfig, car CarPersonality, dtS float64) {
	if !pkt.IsRaceOn {
		s.resetDynamicState()
		return
	}

	dt := clamp(dtS, 0.004, 0.050)
	if !s.hasPrevPkt || dtS > 0.15 {
		s.hasPrevPkt = true
		s.prevGear = pkt.Gear
		s.emaAx = float64(pkt.AccelX)
		s.emaAz = float64(pkt.AccelZ)
		s.prevAx = float64(pkt.AccelX)
		s.prevAz = float64(pkt.AccelZ)
		s.spdWin[0] = float64(pkt.SpeedKmh)
		s.spdIdx = 1
		s.spdCount = 1
		s.latCount = 0
		s.latIdx = 0
		s.roadEnvL = 0
		s.roadEnvR = 0
		s.heaveEnv = 0
		s.curbEnvL = 0
		s.curbEnvR = 0
		s.curbHoldL = 0
		s.curbHoldR = 0
		s.airborne = false
		s.airTimeS = 0
		s.landWinS = 0
		s.pebL = pebbleGrain{}
		s.pebR = pebbleGrain{}
		s.guardActiveS = 0
		s.guardElapsedS = 0
		s.guardPeak = 0
		s.prevMaxPuddle = max4f(pkt.PuddleDepth)
		s.prevSmashDiff = pkt.SmashableVelDiff
		for i := 0; i < 4; i++ {
			s.prevSuspM[i] = float64(pkt.SuspTravelM[i])
			s.prevSuspVel[i] = 0
			s.hpSuspVel[i] = 0
		}
		s.lpSuspMean = avg4f(pkt.NormSusp)
		s.suspInit = true
		return
	}

	// 1. True High-Pass Suspension Velocity + Body Heave (Crisper 0.60x Rebound Decay)
	fc := 7.0 + 7.0*car.SuspensionStiffness
	rc := 1.0 / (2.0 * math.Pi * fc)
	alpha := rc / (rc + dt)

	var velHP [4]float64
	var stepJump [4]float64
	for i := 0; i < 4; i++ {
		curM := float64(pkt.SuspTravelM[i])
		deltaM := curM - s.prevSuspM[i]
		stepJump[i] = math.Abs(deltaM)
		vel := deltaM / dt
		s.hpSuspVel[i] = alpha * (s.hpSuspVel[i] + vel - s.prevSuspVel[i])
		velHP[i] = math.Tanh(s.hpSuspVel[i] * (0.65 + 0.45*car.SuspensionStiffness))
		s.prevSuspVel[i] = vel
		s.prevSuspM[i] = curM
	}

	speedGate := clamp((float64(pkt.SpeedKmh)-3.5)/15.0, 0, 1)
	rawL := (0.60*math.Abs(velHP[0]) + 0.40*math.Abs(velHP[2])) * speedGate
	rawR := (0.60*math.Abs(velHP[1]) + 0.40*math.Abs(velHP[3])) * speedGate
	latBias := clamp(float64(pkt.AccelX)/16.0, -0.32, 0.32)
	rawL *= (1.0 + latBias)
	rawR *= (1.0 - latBias)

	reboundMs := math.Max(car.SuspensionReboundMs, 30.0)
	decay := math.Exp(-dt / (reboundMs * 0.00060))
	if rawL > s.roadEnvL {
		s.roadEnvL = rawL
	} else {
		s.roadEnvL *= decay
	}
	if rawR > s.roadEnvR {
		s.roadEnvR = rawR
	} else {
		s.roadEnvR *= decay
	}

	meanNorm := avg4f(pkt.NormSusp)
	s.lpSuspMean = 0.86*s.lpSuspMean + 0.14*meanNorm
	heaveDelta := math.Abs(meanNorm-s.lpSuspMean)*4.0 + math.Abs(float64(pkt.AccelY))/15.0
	heaveTarget := clamp(heaveDelta*speedGate, 0, 1.0)
	if heaveTarget > s.heaveEnv {
		s.heaveEnv = 0.50*s.heaveEnv + 0.50*heaveTarget
	} else {
		s.heaveEnv *= math.Exp(-dt / 0.078)
	}

	// 2. Curb / Rumble Strip Detector (Telem + Asymmetric SurfaceRumble + Geometric Step, 28ms Tooth Hold + Fast 55ms Exit Release!)
	leftNorm := 0.5 * float64(pkt.NormSusp[0]+pkt.NormSusp[2])
	rightNorm := 0.5 * float64(pkt.NormSusp[1]+pkt.NormSusp[3])
	asym := leftNorm - rightNorm
	leftStep := math.Max(stepJump[0], stepJump[2])
	rightStep := math.Max(stepJump[1], stepJump[3])
	leftRumble := math.Max(float64(pkt.SurfaceRumble[0]), float64(pkt.SurfaceRumble[2]))
	rightRumble := math.Max(float64(pkt.SurfaceRumble[1]), float64(pkt.SurfaceRumble[3]))
	leftTelemCurb := pkt.RumbleStrip[0] != 0 || pkt.RumbleStrip[2] != 0
	rightTelemCurb := pkt.RumbleStrip[1] != 0 || pkt.RumbleStrip[3] != 0
	leftAsymRumble := leftRumble >= 0.065 && leftRumble > rightRumble*1.85+0.045 && (leftStep > 0.004 || math.Abs(asym) > 0.045)
	rightAsymRumble := rightRumble >= 0.065 && rightRumble > leftRumble*1.85+0.045 && (rightStep > 0.004 || math.Abs(asym) > 0.045)

	curbHitL, curbHitR := 0.0, 0.0
	if speedGate > 0.15 {
		if leftTelemCurb {
			curbHitL = 0.98
		} else if leftAsymRumble || (leftStep > 0.0085 && math.Abs(asym) > 0.075) || (asym > 0.12 && rawL > 0.20) {
			rumbleBoost := math.Max(0, leftRumble-rightRumble) * 1.8
			curbHitL = clamp((leftStep/0.018)*0.65+math.Abs(asym)*2.1+rumbleBoost, 0.76, 1.0)
		}
		if rightTelemCurb {
			curbHitR = 0.98
		} else if rightAsymRumble || (rightStep > 0.0085 && math.Abs(asym) > 0.075) || (asym < -0.12 && rawR > 0.20) {
			rumbleBoost := math.Max(0, rightRumble-leftRumble) * 1.8
			curbHitR = clamp((rightStep/0.018)*0.65+math.Abs(asym)*2.1+rumbleBoost, 0.76, 1.0)
		}
	}
	if curbHitL > 0 {
		s.curbHoldL = 0.036
		if curbHitL > s.curbEnvL {
			s.curbEnvL = curbHitL
		}
	} else if s.curbHoldL > 0 {
		s.curbHoldL = math.Max(0, s.curbHoldL-dt)
		s.curbEnvL *= math.Exp(-dt / 0.18)
	} else {
		s.curbEnvL *= math.Exp(-dt / 0.048)
	}
	if curbHitR > 0 {
		s.curbHoldR = 0.036
		if curbHitR > s.curbEnvR {
			s.curbEnvR = curbHitR
		}
	} else if s.curbHoldR > 0 {
		s.curbHoldR = math.Max(0, s.curbHoldR-dt)
		s.curbEnvR *= math.Exp(-dt / 0.18)
	} else {
		s.curbEnvR *= math.Exp(-dt / 0.048)
	}

	// 3. Two-Stage Jump Landing Engine (Crisp Tightened Duration + 48ms Post-Touchdown Compression Upgrade Window!)
	maxNorm := max4f(pkt.NormSusp)
	if maxNorm < 0.06 && pkt.SpeedKmh > 14 {
		s.airborne = true
		s.airTimeS = math.Min(s.airTimeS+dt, 2.5)
		s.landWinS = 0
	} else {
		if s.airborne {
			if s.airTimeS >= 0.035 {
				s.landLastAirS = s.airTimeS
				s.landWinS = 0.048
			}
			s.airborne = false
			s.airTimeS = 0
		}
		if s.landWinS > 0 {
			s.landWinS = math.Max(0, s.landWinS-dt)
			maxStep := max4f64(stepJump)
			absAy := math.Abs(float64(pkt.AccelY))
			if maxNorm >= 0.24 || absAy >= 3.0 || maxStep >= 0.016 {
				airFac := clamp((s.landLastAirS-0.030)/0.42, 0.45, 1.0)
				compFac := clamp((maxNorm-0.20)/0.56, 0.42, 1.0)
				vertFac := clamp(absAy/14.5+maxStep/0.042, 0.38, 1.0)
				peak := clamp((0.40*airFac+0.40*compFac+0.20*vertFac)*car.LandingSensitivity, 0.58, 1.0)
				dur := 0.098 + 0.065*peak + 0.025*(1.0-car.SuspensionStiffness)
				lComp := 0.5 * float64(pkt.NormSusp[0]+pkt.NormSusp[2])
				rComp := 0.5 * float64(pkt.NormSusp[1]+pkt.NormSusp[3])
				mComp := math.Max(math.Max(lComp, rComp), 0.10)
				if s.landActiveS <= 0 {
					s.landPeak = peak
					s.landReboundMs = car.SuspensionReboundMs
					s.landDurS = dur
					s.landActiveS = 0.0001
					s.landLeftW = clamp(lComp/mComp, 0.55, 1.0)
					s.landRightW = clamp(rComp/mComp, 0.55, 1.0)
				} else if peak > s.landPeak {
					s.landPeak = peak
					s.landDurS = math.Max(s.landDurS, dur)
					s.landLeftW = clamp(lComp/mComp, 0.55, 1.0)
					s.landRightW = clamp(rComp/mComp, 0.55, 1.0)
				}
			}
		}
	}

	// 4. Gear Shift Detection
	if !car.IsElectric && car.ShiftCrispness > 0.05 && pkt.Gear != s.prevGear && pkt.Gear >= 1 && s.prevGear >= 1 && pkt.SpeedKmh > 5 {
		s.shiftIsUpshift = pkt.Gear > s.prevGear
		load := clamp(float64(pkt.Accel)/255.0*0.6+float64(pkt.RpmRatio)*0.4, 0.45, 1.0)
		s.shiftStrength = clamp(load*(0.62+0.48*car.ShiftCrispness), 0.45, 1.0)
		if !s.shiftIsUpshift {
			s.shiftStrength *= 0.85
		}
		s.shiftDurS = car.ShiftDurationMs * 0.001
		s.shiftActiveS = 0.0001
	}
	s.prevGear = pkt.Gear

	// 5. TWO-TIER 360° DIRECTIONAL COLLISION DETECTOR (Onset-Only: ZERO Post-Crash Re-Triggering!)
	for i := 0; i < 4; i++ {
		if math.Abs(float64(pkt.WheelRotSpeed[i])) > 1.0 {
			s.hasWheelRot = true
		}
	}
	if s.impactCoolS > 0 {
		s.impactCoolS = math.Max(0, s.impactCoolS-dt)
	}
	s.lastImpactSinceS = math.Min(10.0, s.lastImpactSinceS+dt)
	curSpd := float64(pkt.SpeedKmh)
	maxRecentSpd := curSpd
	for i := 0; i < s.spdCount; i++ {
		if s.spdWin[i] > maxRecentSpd {
			maxRecentSpd = s.spdWin[i]
		}
	}
	dvWin := maxRecentSpd - curSpd

	axNow := float64(pkt.AccelX)
	azNow := float64(pkt.AccelZ)
	prevAx := s.prevAx
	prevAz := s.prevAz

	shockX := axNow - s.emaAx
	shockZ := azNow - s.emaAz
	dAxSigned := axNow - prevAx
	dAzSigned := azNow - prevAz
	dAx := math.Abs(dAxSigned)
	dAz := math.Abs(dAzSigned)
	shockMag := math.Hypot(shockX, shockZ)

	// Onset-only invariants: true crashes spike acceleration AWAY from zero, NEVER returning toward zero!
	frontOnset := azNow <= -5.5 && dAzSigned <= -3.2
	rearOnset := azNow >= 7.0 && dAzSigned >= 4.0
	sideOnset := math.Abs(axNow) >= 6.5 && (math.Abs(axNow) >= math.Abs(prevAx)+4.5 || axNow*prevAx < -12.0)

	s.prevAx = axNow
	s.prevAz = azNow
	// Clamp EMA tracking input to normal driving G-range so giant crash spikes NEVER poison EMA and cause snapback false triggers!
	emaAlpha := clamp(dt/0.090, 0.08, 0.35)
	s.emaAx += emaAlpha * (clamp(axNow, -18.0, 18.0) - s.emaAx)
	s.emaAz += emaAlpha * (clamp(azNow, -18.0, 18.0) - s.emaAz)
	s.spdWin[s.spdIdx] = curSpd
	s.spdIdx = (s.spdIdx + 1) % len(s.spdWin)
	if s.spdCount < len(s.spdWin) {
		s.spdCount++
	}

	hardBraking := pkt.Brake >= 180
	inUpgradeWin := s.impactActiveS > 0 && s.impactActiveS < 0.048
	effRecentSpd := maxRecentSpd
	if inUpgradeWin && s.impactEntrySpd > effRecentSpd {
		effRecentSpd = s.impactEntrySpd
	}
	effDvWin := math.Max(dvWin, effRecentSpd-curSpd)

	isHeavySide := sideOnset && math.Abs(shockX) >= 13.0 && (dAx >= 5.2 || math.Abs(axNow) >= 22.0) && (effRecentSpd >= 6.0 || dAx >= 11.0)
	isHeavyFront := false
	if frontOnset {
		if hardBraking {
			isHeavyFront = (shockZ <= -13.5 && dAz >= 4.8 && effRecentSpd >= 10.0) ||
				(effDvWin >= 10.5 && shockZ <= -9.5 && dAz >= 4.2 && effRecentSpd >= 10.0) ||
				(azNow <= -22.0 && effDvWin >= 4.0 && dAz >= 4.5)
		} else {
			isHeavyFront = (shockZ <= -11.5 && effRecentSpd >= 8.0) ||
				(effDvWin >= 8.5 && shockZ <= -7.5 && effRecentSpd >= 9.0) ||
				(azNow <= -20.0 && effDvWin >= 3.2)
		}
	}
	isHeavyRear := rearOnset && azNow >= 11.0 && shockZ >= 11.0 && pkt.Accel < 190
	isHeavyVec := (frontOnset || rearOnset || sideOnset) && shockMag >= 17.0 && (dAx >= 5.2 || dAz >= 5.2) && effRecentSpd >= 8.0 && (!hardBraking || math.Abs(shockX) >= 10.5 || shockMag >= 19.5)

	// Tier A: LOW-SPEED BUMP / FENDER BENDER (3..85 km/h, including impacts while braking when dAz shock spikes!)
	isLowFront := frontOnset && effRecentSpd >= 3.0 && curSpd <= 85.0 &&
		((effDvWin >= 2.8 && shockZ <= -4.5 && pkt.Brake < 140) ||
			(effDvWin >= 3.2 && shockZ <= -6.0 && dAz >= 4.2) ||
			(effDvWin >= 3.0 && curSpd < 4.0 && shockZ <= -4.2))
	isLowSide := sideOnset && ((effRecentSpd >= 3.0 && curSpd <= 85.0 && math.Abs(shockX) >= 6.2 && dAx >= 5.0) ||
		(curSpd <= 25.0 && math.Abs(shockX) >= 8.0 && dAx >= 6.5))
	isLowRear := rearOnset && azNow >= 7.2 && shockZ >= 6.2 && pkt.Accel < 160 && curSpd <= 80.0

	isHeavy := isHeavySide || isHeavyFront || isHeavyRear || isHeavyVec
	isLow := !isHeavy && (isLowFront || isLowSide || isLowRear)

	if (s.impactCoolS <= 0 || inUpgradeWin) && (isHeavy || isLow) {
		vx := shockX
		vz := shockZ
		if math.Abs(azNow) > 16 && math.Abs(vz) < 9 {
			vz = azNow
		}
		if math.Abs(axNow) > 14 && math.Abs(vx) < 9 {
			vx = axNow
		}
		angleDeg := math.Atan2(vx, -vz) * 180.0 / math.Pi

		var sev float64
		if isHeavy {
			sevShock := clamp((shockMag-8.0)/24.0, 0.0, 1.0)
			sevDv := clamp((effDvWin-3.5)/26.0, 0.0, 1.0)
			sev = clamp(0.78+0.22*math.Max(sevShock, 0.60*sevShock+0.55*sevDv), 0.78, 1.0)
		} else {
			sevLow := clamp((shockMag-3.8)/9.5*0.65+(effDvWin-2.2)/8.0*0.35, 0.0, 1.0)
			sev = clamp(0.74+0.22*sevLow, 0.74, 0.96)
		}
		if s.impactCoolS <= 0 {
			s.impactEntrySpd = maxRecentSpd
			// Flush speed window to current speed so the same crash deceleration never re-triggers after cooldown!
			for i := range s.spdWin {
				s.spdWin[i] = curSpd
			}
			s.emaAx = clamp(axNow, -12.0, 12.0)
			s.emaAz = clamp(azNow, -12.0, 12.0)
			s.triggerDirectionalImpact(angleDeg, sev, math.Abs(float64(pkt.AngVelY)), isLow)
		} else if (s.impactLowSpeed && isHeavy) || sev > s.impactPeak+0.02 {
			// Peak-upgrade within the 75ms onset window without restarting impactActiveS (zero aftershock loop!)
			savedActiveS := s.impactActiveS
			savedPhA, savedPhB := s.phWallA, s.phWallB
			for i := range s.spdWin {
				s.spdWin[i] = curSpd
			}
			s.emaAx = clamp(axNow, -12.0, 12.0)
			s.emaAz = clamp(azNow, -12.0, 12.0)
			s.triggerDirectionalImpact(angleDeg, math.Max(s.impactPeak, sev), math.Abs(float64(pkt.AngVelY)), s.impactLowSpeed && !isHeavy)
			s.impactActiveS = savedActiveS
			s.phWallA, s.phWallB = savedPhA, savedPhB
		}
	}

	// 6. PHYSICS-VERIFIED 3-SIGNATURE DIRECTIONAL GUARDRAIL SCRAPE (Continuous Metallic Grind, Zero Post-Crash Tail!)
	s.latWin[s.latIdx] = axNow
	s.latIdx = (s.latIdx + 1) % len(s.latWin)
	if s.latCount < len(s.latWin) {
		s.latCount++
	}
	if s.latCount >= 4 && curSpd >= 14.0 && maxNorm > 0.08 && dvWin < 14.0 {
		meanLat, jerkE := 0.0, 0.0
		edges2, edges45 := 0, 0
		for i := 0; i < s.latCount; i++ {
			meanLat += s.latWin[i]
		}
		meanLat /= float64(s.latCount)
		for i := 1; i < s.latCount; i++ {
			a := (s.latIdx - i + len(s.latWin)) % len(s.latWin)
			b := (s.latIdx - i - 1 + len(s.latWin)) % len(s.latWin)
			d := math.Abs(s.latWin[a] - s.latWin[b])
			jerkE += d
			if d > 2.0 {
				edges2++
			}
			if d > 4.5 {
				edges45++
			}
		}
		steerF := float64(pkt.Steer) / 127.0
		avyNow := float64(pkt.AngVelY)

		isPureFrontRear := (s.impactActiveS > 0 || s.lastImpactSinceS < 0.35) && s.lastImpactSide == 0
		hasWallLat := math.Abs(axNow) >= 3.5 || math.Abs(shockX) >= 3.0
		sigWallFriction := !isPureFrontRear && pkt.Brake < 35 && s.shiftActiveS <= 0 && hasWallLat &&
			((pkt.Accel >= 140 && azNow <= -2.8) ||
				(pkt.Accel < 140 && azNow <= -(5.2+0.018*curSpd)))
		sigWallNormal := !isPureFrontRear && curSpd >= 16.0 && math.Abs(axNow) > (24.5+0.025*curSpd) && azNow < 0.8 && pkt.Brake < 60
		steerIntoWall := pkt.Brake < 40 && ((meanLat > 2.2 && steerF < -0.15) || (meanLat < -2.2 && steerF > 0.15)) && azNow < 1.5
		straightWallRub := pkt.Brake < 40 && curSpd <= 110.0 && math.Abs(steerF) <= 0.12 && math.Abs(avyNow) < 0.12 && math.Abs(meanLat) > 3.5 && azNow < 1.2 && edges2 >= 2 && jerkE >= 5.5
		sigChatter := !isPureFrontRear && ((steerIntoWall && edges2 >= 2 && jerkE >= 4.0) ||
			straightWallRub ||
			(edges45 >= 3 && jerkE >= 28.0))

		if sigWallFriction || sigWallNormal || sigChatter {
			fricE := clamp((-azNow-1.5)/14.0, 0, 1.0)
			normE := clamp((math.Abs(axNow)-16.0)/30.0, 0, 1.0)
			chatE := clamp((jerkE-3.5)/18.0, 0, 1.0)
			rawPeak := clamp(math.Max(math.Max(fricE, normE), chatE)*0.45+0.68, 0.70, 1.0)
			s.guardPeak = math.Max(s.guardPeak*0.85, rawPeak)
			s.guardActiveS = 0.065 // stops crisply within 65ms when leaving the guardrail!
			if math.Abs(shockX) > 4.0 {
				if shockX > 0 {
					s.guardSide = -1 // pushed right => LEFT guardrail
				} else {
					s.guardSide = 1 // pushed left => RIGHT guardrail
				}
			} else if s.lastImpactSinceS < 0.85 && s.lastImpactSide != 0 {
				s.guardSide = s.lastImpactSide
			} else if meanLat >= 0 {
				s.guardSide = -1
			} else {
				s.guardSide = 1
			}
		}
	}

	// 7. Smashable Debris
	if pkt.SmashableVelDiff > 1.0 && pkt.SmashableVelDiff != s.prevSmashDiff && curSpd > 5 {
		massFac := clamp(math.Log1p(math.Max(0, float64(pkt.SmashableMass)))/4.8, 0.35, 1.0)
		velFac := clamp(float64(pkt.SmashableVelDiff)/16.0, 0.35, 1.0)
		s.smashPeak = clamp(0.55*velFac+0.45*massFac, 0.48, 1.0)
		s.smashDurS = 0.055 + 0.035*s.smashPeak
		s.smashActiveS = 0.0001
	}
	s.prevSmashDiff = pkt.SmashableVelDiff

	// 8. Water Entry Splash (with Dynamic Depth Ramp Upgrade!)
	curMaxPuddle := max4f(pkt.PuddleDepth)
	if curMaxPuddle >= 0.030 && curSpd >= 10 {
		spdFac := clamp(curSpd/95.0, 0.68, 1.0)
		depFac := clamp(curMaxPuddle/0.25, 0.68, 1.0)
		candPeak := clamp(0.50*spdFac+0.50*depFac, 0.68, 1.0)
		lp := math.Max(float64(pkt.PuddleDepth[0]), float64(pkt.PuddleDepth[2]))
		rp := math.Max(float64(pkt.PuddleDepth[1]), float64(pkt.PuddleDepth[3]))
		mp := math.Max(math.Max(lp, rp), 0.03)
		if s.splashActiveS <= 0 && (s.prevMaxPuddle < 0.028 || curMaxPuddle-s.prevMaxPuddle >= 0.025) {
			s.splashPeak = candPeak
			s.splashDurS = 0.14 + 0.09*s.splashPeak
			s.splashActiveS = 0.0001
			s.splashLeftW = clamp(lp/mp, 0.45, 1.0)
			s.splashRightW = clamp(rp/mp, 0.45, 1.0)
		} else if s.splashActiveS > 0 && candPeak > s.splashPeak {
			s.splashPeak = candPeak
			s.splashDurS = math.Max(s.splashDurS, 0.14+0.09*s.splashPeak)
			s.splashLeftW = clamp(lp/mp, 0.45, 1.0)
			s.splashRightW = clamp(rp/mp, 0.45, 1.0)
		}
	}
	s.prevMaxPuddle = curMaxPuddle
}

func (s *Synthesizer) triggerDirectionalImpact(angleDeg, sev, yawKick float64, lowSpeed bool) {
	s.impactAngleDeg = angleDeg
	s.impactPeak = sev
	s.impactLowSpeed = lowSpeed
	if lowSpeed {
		// Crisp, dry 70..95ms bumper thud (zero dragging tail!)
		s.impactDurS = 0.070 + 0.025*sev
		s.impactCoolS = 0.30
	} else {
		// Crisp, authoritative 95..140ms dry mechanical crash slam + 380ms refractory guard
		s.impactDurS = 0.095 + 0.045*sev
		s.impactCoolS = 0.38
	}
	s.impactActiveS = 0.0001
	s.phWallA = 0
	s.phWallB = 0
	s.impactYawKick = clamp(yawKick/2.5, 0, 1.0)
	s.impactMuteL, s.impactMuteR = false, false
	s.lastImpactSinceS = 0
	s.lastImpactSide = 0

	prefix := ""
	if lowSpeed {
		prefix = "低速"
	}
	absA := math.Abs(angleDeg)
	switch {
	case absA <= 28:
		s.impactKind = "front"
		s.impactLabel = prefix + "正面撞击"
		s.impactLeftW, s.impactRightW = 1.0, 1.0
		s.impactDelayL, s.impactDelayR = 0.0, 0.0
	case absA >= 148:
		s.impactKind = "rear"
		s.impactLabel = prefix + "后方追尾"
		s.impactLeftW, s.impactRightW = 0.96, 0.96
		s.impactDelayL, s.impactDelayR = 0.0, 0.0
	case angleDeg > 28 && angleDeg < 55:
		s.impactKind = "front_left"
		s.impactLabel = prefix + "左前斜角撞击"
		s.impactLeftW, s.impactRightW = 1.0, 0.18
		s.impactMuteR = true
		s.impactDelayL, s.impactDelayR = 0.0, 0.040
		s.lastImpactSide = -1
	case angleDeg < -28 && angleDeg > -55:
		s.impactKind = "front_right"
		s.impactLabel = prefix + "右前斜角撞击"
		s.impactLeftW, s.impactRightW = 0.18, 1.0
		s.impactMuteL = true
		s.impactDelayL, s.impactDelayR = 0.040, 0.0
		s.lastImpactSide = 1
	case angleDeg >= 55 && angleDeg <= 125:
		s.impactKind = "left"
		s.impactLabel = prefix + "左侧撞击"
		s.impactLeftW, s.impactRightW = 1.0, 0.0
		s.impactMuteR = true
		s.impactDelayL, s.impactDelayR = 0.0, 0.125
		s.lastImpactSide = -1
	case angleDeg <= -55 && angleDeg >= -125:
		s.impactKind = "right"
		s.impactLabel = prefix + "右侧撞击"
		s.impactLeftW, s.impactRightW = 0.0, 1.0
		s.impactMuteL = true
		s.impactDelayL, s.impactDelayR = 0.125, 0.0
		s.lastImpactSide = 1
	case angleDeg > 125:
		s.impactKind = "rear_left"
		s.impactLabel = prefix + "左后斜角撞击"
		s.impactLeftW, s.impactRightW = 1.0, 0.16
		s.impactMuteR = true
		s.impactDelayL, s.impactDelayR = 0.0, 0.040
		s.lastImpactSide = -1
	default:
		s.impactKind = "rear_right"
		s.impactLabel = prefix + "右后斜角撞击"
		s.impactLeftW, s.impactRightW = 0.16, 1.0
		s.impactMuteL = true
		s.impactDelayL, s.impactDelayR = 0.040, 0.0
		s.lastImpactSide = 1
	}
}

func (s *Synthesizer) Render8ms(pkt telemetry.Packet, cfg NextConfig, car CarPersonality, pktFresh bool) RenderResult {
	var res RenderResult
	for i := 0; i < 8; i++ {
		res.Trig[i] = 128
		res.GripL[i] = 128
		res.GripR[i] = 128
	}
	res.TrigRoute = -1
	res.SurfaceLabel = "静止"
	res.ActiveEvents = make([]ActiveEvent, 0, 8)

	if s.testRemainS > 0 {
		return s.renderTest8ms(cfg, car)
	}

	if !pktFresh || !pkt.IsRaceOn {
		s.resetDynamicState()
		res.Flags = 0x84
		res.Frame = hid.BuildNeutral0x57()
		return res
	}

	spd := float64(pkt.SpeedKmh)
	rpmRatio := clamp(float64(pkt.RpmRatio), 0, 1.15)
	accelF := float64(pkt.Accel) / 255.0
	brakeF := float64(pkt.Brake) / 255.0
	steerF := float64(pkt.Steer) / 127.0
	hbActive := pkt.Handbrake > 40

	maxRumble := max4f(pkt.SurfaceRumble)
	leftRumble := math.Max(float64(pkt.SurfaceRumble[0]), float64(pkt.SurfaceRumble[2]))
	rightRumble := math.Max(float64(pkt.SurfaceRumble[1]), float64(pkt.SurfaceRumble[3]))
	maxPuddle := max4f(pkt.PuddleDepth)
	leftPuddle := math.Max(float64(pkt.PuddleDepth[0]), float64(pkt.PuddleDepth[2]))
	rightPuddle := math.Max(float64(pkt.PuddleDepth[1]), float64(pkt.PuddleDepth[3]))

	// 4-TIRE LOAD-WEIGHTED SLIP (FL=0, FR=1, RL=2, RR=3)
	loadW := func(i int) float64 { return 0.45 + 0.55*clamp(float64(pkt.NormSusp[i]), 0, 1) }
	slipFL := math.Abs(float64(pkt.CombinedSlip[0])) * loadW(0)
	slipFR := math.Abs(float64(pkt.CombinedSlip[1])) * loadW(1)
	slipRL := math.Abs(float64(pkt.CombinedSlip[2])) * loadW(2)
	slipRR := math.Abs(float64(pkt.CombinedSlip[3])) * loadW(3)

	rawFrontSlip := 0.5 * (math.Abs(float64(pkt.CombinedSlip[0])) + math.Abs(float64(pkt.CombinedSlip[1])))
	rawRearSlip := 0.5 * (math.Abs(float64(pkt.CombinedSlip[2])) + math.Abs(float64(pkt.CombinedSlip[3])))
	rearLatSlip := 0.5 * (math.Abs(float64(pkt.SlipAngle[2])) + math.Abs(float64(pkt.SlipAngle[3])))
	maxSlip := math.Max(rawFrontSlip, rawRearSlip)
	drivenSlip := rawRearSlip
	if car.DriveTrain == 0 {
		drivenSlip = rawFrontSlip
	} else if car.DriveTrain == 2 {
		drivenSlip = 0.5 * (rawFrontSlip + rawRearSlip)
	}

	// Contrast-expanded L/R weights directly from the 4 individual tires!
	underL, underR := contrastLR(slipFL, slipFR, 2.4)
	if math.Abs(slipFL-slipFR) < 0.12 {
		if steerF > 0.15 {
			underL, underR = math.Max(underL, 0.94), math.Min(underR, 0.20)
		} else if steerF < -0.15 {
			underL, underR = math.Min(underL, 0.20), math.Max(underR, 0.94)
		}
	}
	overL, overR := contrastLR(slipRL, slipRR, 2.0)
	if math.Abs(slipRL-slipRR) < 0.12 {
		if pkt.AngVelY > 0.15 {
			overL, overR = math.Max(overL, 0.96), math.Min(overR, 0.35)
		} else if pkt.AngVelY < -0.15 {
			overL, overR = math.Min(overL, 0.35), math.Max(overR, 0.96)
		}
	}
	absL, absR := contrastLR(slipFL+slipRL, slipFR+slipRR, 0.85)

	surfaceKind := "tarmac"
	res.SurfaceLabel = "铺装柏油"
	if maxPuddle >= 0.03 {
		surfaceKind = "water"
		res.SurfaceLabel = "涉水路面"
	} else if s.curbEnvL > 0.22 || s.curbEnvR > 0.22 {
		surfaceKind = "curb"
		res.SurfaceLabel = "压路肩 / 马路牙子"
	} else if maxRumble >= 0.22 {
		surfaceKind = "gravel"
		res.SurfaceLabel = "重度砂石 / 碎石"
	} else if maxRumble >= 0.07 {
		if maxSlip > 0.85 && math.Abs(float64(pkt.AccelX)) < 6.5 {
			surfaceKind = "grass_snow"
			res.SurfaceLabel = "草地 / 雪地"
		} else {
			surfaceKind = "dirt"
			res.SurfaceLabel = "非铺装土路"
		}
	}

	events := make([]ActiveEvent, 0, 8)
	isIdle := !car.IsElectric && spd < 5.5 && pkt.Accel < 60 && pkt.CurrentRpm > 200
	effRedlineRatio := car.EffectiveRedlineRatio(pkt.EngineMaxRpm)
	isRedline := !car.IsElectric && cfg.Events.Redline.Enabled && rpmRatio >= effRedlineRatio && pkt.Gear >= 1 && spd > 10.0

	maxNorm := max4f(pkt.NormSusp)
	grounded := maxNorm > 0.06

	isLaunchSpin := grounded && cfg.Events.OversteerDrift.Enabled && spd < 46 && pkt.Accel >= 180 && drivenSlip >= 0.62
	isOversteer := grounded && cfg.Events.OversteerDrift.Enabled && !isLaunchSpin && spd >= 18 &&
		((rawRearSlip >= 0.58 && rawRearSlip > rawFrontSlip*1.06+0.10) || rearLatSlip >= 0.38 || (hbActive && spd > 15))
	isUndersteer := grounded && cfg.Events.Understeer.Enabled && !isOversteer && spd >= 20 &&
		(math.Abs(steerF) > 0.14 || math.Abs(float64(pkt.AccelX)) > 3.2 || math.Max(slipFL, slipFR) > 0.72) &&
		rawFrontSlip >= 0.55 && rawFrontSlip > rawRearSlip*1.05+0.08
	isFourWheelDrift := grounded && cfg.Events.OversteerDrift.Enabled && !isLaunchSpin && !isOversteer && !isUndersteer &&
		spd >= 22 && rawFrontSlip >= 0.75 && rawRearSlip >= 0.75

	minRot := math.Min(math.Min(math.Abs(float64(pkt.WheelRotSpeed[0])), math.Abs(float64(pkt.WheelRotSpeed[1]))),
		math.Min(math.Abs(float64(pkt.WheelRotSpeed[2])), math.Abs(float64(pkt.WheelRotSpeed[3]))))
	maxNegSlip := math.Max(
		math.Max(math.Max(0, -float64(pkt.SlipRatio[0])), math.Max(0, -float64(pkt.SlipRatio[1]))),
		math.Max(math.Max(0, -float64(pkt.SlipRatio[2])), math.Max(0, -float64(pkt.SlipRatio[3]))),
	)
	// V4 开心散仙竞速2 (kaixin_race2_clear_lock): abs_brake_threshold=70, abs_min_speed_kmh=12, wheel_lock_rotation_threshold=0.70, traction_warning_point=0.29
	isWheelLock := grounded && !hbActive && cfg.Events.BrakingABS.Enabled && pkt.Brake >= 70 && spd > 12 &&
		((s.hasWheelRot && minRot < 0.85) || (maxNegSlip >= 0.70 && pkt.Brake >= 130) || (rawFrontSlip > 1.40 && pkt.Brake >= 170 && !isUndersteer))
	isABS := grounded && !hbActive && cfg.Events.BrakingABS.Enabled && !isWheelLock && pkt.Brake >= 65 && spd > 12 &&
		(maxNegSlip >= 0.42 || (!isUndersteer && maxSlip >= 0.80) || (pkt.Brake >= 150 && maxSlip >= 0.86))
	brakeTireLoad := clamp((brakeF-0.196)/0.75*0.55+math.Max(maxNegSlip, maxSlip*0.55)*0.65, 0, 1.0)
	isBrakeWarning := grounded && !hbActive && cfg.Events.BrakingABS.Enabled && !isWheelLock && !isABS && !isUndersteer && !isOversteer && !isFourWheelDrift &&
		pkt.Brake >= 55 && spd > 14 && brakeTireLoad >= 0.29

	// 7-FAMILY HIGH-CONTRAST ENGINE FREQUENCY & AMPLITUDE
	engSharp := clamp((1.0-car.EngineRoundness)*0.70+cfg.Events.EngineRev.Sharpness*0.30, 0, 1)
	idleSharp := clamp((1.0-car.EngineRoundness)*0.60+cfg.Events.EngineIdle.Sharpness*0.40, 0, 1)
	suspSharp := clamp(car.SuspensionStiffness*0.65+cfg.Events.RoadUndulation.Sharpness*0.35, 0, 1)

	engFreq := (car.EngineFreqBaseHz + (car.EngineFreqTopHz-car.EngineFreqBaseHz)*math.Pow(clamp(rpmRatio, 0, 1), 1.12)) * cfg.Events.EngineRev.FreqScale
	if isIdle {
		engFreq = car.EngineFreqBaseHz * cfg.Events.EngineIdle.FreqScale
	}
	if car.IsElectric {
		engFreq = (car.EngineFreqBaseHz + (car.EngineFreqTopHz-car.EngineFreqBaseHz)*clamp(spd/250.0*0.65+accelF*0.35, 0, 1)) * cfg.Events.EngineRev.FreqScale
	}

	boostExp := 1.0 + car.EngineTurboSurge*clamp(float64(pkt.BoostPsi)/16.0, 0, 1.0)
	engAmp := 0.0
	if isIdle && cfg.Events.EngineIdle.Enabled {
		engAmp = 0.45 * cfg.Events.EngineIdle.Intensity * car.EngineBedScale
		events = append(events, ActiveEvent{ID: "engineIdle", Label: "怠速呼吸", Category: "engine", Intensity: clamp(engAmp*1.2, 0, 1), Channel: "grip"})
	} else if cfg.Events.EngineRev.Enabled && (pkt.CurrentRpm > 200 || car.IsElectric) {
		rpmCurve := 0.25 + 0.52*math.Pow(clamp(rpmRatio, 0, 1), 1.20) + 0.25*accelF
		if car.IsElectric {
			rpmCurve = 0.14 + 0.56*accelF + 0.26*clamp(spd/240.0, 0, 1)
		}
		engAmp = 0.42 * rpmCurve * boostExp * cfg.Events.EngineRev.Intensity * car.EngineBedScale
		if engAmp > 0.04 {
			events = append(events, ActiveEvent{ID: "engineRev", Label: "引擎拉转", Category: "engine", Intensity: clamp(engAmp*1.5, 0, 1), Channel: "grip"})
		}
	}

	if (s.roadEnvL > 0.10 || s.roadEnvR > 0.10 || s.heaveEnv > 0.15) && cfg.Events.RoadUndulation.Enabled {
		events = append(events, ActiveEvent{ID: "roadUndulation", Label: "路面起伏/悬架", Category: "road", Intensity: clamp(math.Max(math.Max(s.roadEnvL, s.roadEnvR), s.heaveEnv), 0, 1), Channel: "grip"})
	}
	if surfaceKind != "tarmac" && surfaceKind != "water" && surfaceKind != "curb" && cfg.Events.RoadSurface.Enabled && spd > 4.5 {
		events = append(events, ActiveEvent{ID: "roadSurface", Label: res.SurfaceLabel, Category: "road", Intensity: clamp(maxRumble*2.8, 0.35, 1), Channel: "grip"})
	}
	if (s.curbEnvL > 0.14 || s.curbEnvR > 0.14) && cfg.Events.CurbRumble.Enabled {
		ch := "grip"
		if s.curbEnvL > s.curbEnvR*1.4 {
			ch = "grip_l"
		} else if s.curbEnvR > s.curbEnvL*1.4 {
			ch = "grip_r"
		}
		events = append(events, ActiveEvent{ID: "curbRumble", Label: "压路肩/马路牙子", Category: "road", Intensity: math.Max(s.curbEnvL, s.curbEnvR), Channel: ch})
	}
	if s.landActiveS > 0 && cfg.Events.JumpLanding.Enabled {
		events = append(events, ActiveEvent{ID: "jumpLanding", Label: "飞坡落地冲击", Category: "chassis", Intensity: s.landPeak, Channel: "grip"})
	}
	if s.shiftActiveS > 0 && cfg.Events.GearShift.Enabled {
		lbl := "升挡顿挫"
		if !s.shiftIsUpshift {
			lbl = "降挡补油"
		}
		events = append(events, ActiveEvent{ID: "gearShift", Label: lbl, Category: "engine", Intensity: s.shiftStrength, Channel: "both"})
	}
	if isRedline {
		redCh := "both"
		if !cfg.Trigger.EnableRightAccel || cfg.Trigger.ThrottleMicroFeel <= 0.001 {
			redCh = "grip"
		}
		events = append(events, ActiveEvent{ID: "redline", Label: "红线爆转断油", Category: "engine", Intensity: clamp((rpmRatio-effRedlineRatio)/0.06+0.72, 0.72, 1), Channel: redCh})
	}
	if s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled {
		res.ImpactActive = true
		res.ImpactAngleDeg = s.impactAngleDeg
		res.ImpactLabel = s.impactLabel
		events = append(events, ActiveEvent{ID: "collisionWall", Label: s.impactLabel, Category: "impact", Intensity: s.impactPeak, Channel: "grip"})
	}
	if s.guardActiveS > 0 && cfg.Events.Guardrail.Enabled {
		ch, lbl := "grip", "护栏连续刮擦"
		if s.guardSide < 0 {
			ch, lbl = "grip_l", "左侧护栏刮擦"
			if !res.ImpactActive {
				res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 90, lbl
			}
		} else if s.guardSide > 0 {
			ch, lbl = "grip_r", "右侧护栏刮擦"
			if !res.ImpactActive {
				res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, -90, lbl
			}
		}
		events = append(events, ActiveEvent{ID: "guardrail", Label: lbl, Category: "impact", Intensity: s.guardPeak, Channel: ch})
	}
	if s.smashActiveS > 0 && cfg.Events.CollisionWall.Enabled {
		events = append(events, ActiveEvent{ID: "smashable", Label: "撞碎障碍物", Category: "impact", Intensity: s.smashPeak, Channel: "grip"})
	}
	if (s.splashActiveS > 0 || maxPuddle >= 0.03) && cfg.Events.WaterSplash.Enabled && spd > 5 {
		lbl := "涉水波浪阻力"
		inten := clamp(maxPuddle*2.4*clamp(spd/75.0, 0.30, 1), 0.30, 1)
		if s.splashActiveS > 0 {
			lbl = "入水飞溅冲击"
			inten = s.splashPeak
		}
		events = append(events, ActiveEvent{ID: "waterSplash", Label: lbl, Category: "water", Intensity: inten, Channel: "grip"})
	}
	if isLaunchSpin {
		events = append(events, ActiveEvent{ID: "oversteerDrift", Label: "起步烧胎", Category: "tire", Intensity: clamp(drivenSlip/1.4, 0.45, 1), Channel: "both"})
	} else if isOversteer || isFourWheelDrift {
		lbl := "后轮甩尾 / 漂移"
		if isFourWheelDrift {
			lbl = "四轮大角度漂移"
		} else if overL > overR*1.3 {
			lbl = "左后轮甩尾漂移"
		} else if overR > overL*1.3 {
			lbl = "右后轮甩尾漂移"
		}
		events = append(events, ActiveEvent{ID: "oversteerDrift", Label: lbl, Category: "tire", Intensity: clamp((rawRearSlip-0.42)/1.2, 0.42, 1), Channel: "both"})
	} else if isUndersteer {
		lbl := "前轮推头搓地"
		if underL > underR*1.4 {
			lbl = "左前轮推头搓地"
		} else if underR > underL*1.4 {
			lbl = "右前轮推头搓地"
		}
		events = append(events, ActiveEvent{ID: "understeer", Label: lbl, Category: "tire", Intensity: clamp((rawFrontSlip-0.45)/1.1, 0.42, 1), Channel: "both"})
	}
	if isWheelLock {
		events = append(events, ActiveEvent{ID: "brakingAbs", Label: "重刹抱死 (V4散仙竞速2)", Category: "tire", Intensity: 0.95, Channel: "both"})
	} else if isABS {
		events = append(events, ActiveEvent{ID: "brakingAbs", Label: "ABS 脉动 (V4散仙竞速2)", Category: "tire", Intensity: 0.85, Channel: "both"})
	} else if isBrakeWarning {
		events = append(events, ActiveEvent{ID: "brakingAbs", Label: "刹车抓地临界预警", Category: "tire", Intensity: clamp(brakeTireLoad, 0.35, 0.78), Channel: "trig_l"})
	}
	res.ActiveEvents = events

	var trigLBuf, trigRBuf [8]byte
	var pL, pR trigPriority
	var conflictSeen bool
	var shiftTrigActiveFrame bool
	var sumSqL, sumSqR float64

	collisionExclusive := s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled && !s.impactLowSpeed && s.impactPeak >= 0.65

	for ms := 0; ms < 8; ms++ {
		const dt1 = 0.001
		bedL, bedR := 0.0, 0.0
		roadL, roadR := 0.0, 0.0
		tireL, tireR := 0.0, 0.0
		impL, impR := 0.0, 0.0
		engDuckEnv := 0.0
		impDuckEnv := 0.0
		tireActivityEnv := 0.0

		shiftT := s.shiftActiveS
		impactLive := s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled
		guardLive := s.guardActiveS > 0 && cfg.Events.Guardrail.Enabled
		isCenterImpact := impactLive && (s.impactKind == "front" || s.impactKind == "rear")
		muteFarL := (impactLive && s.impactMuteL && s.impactActiveS < s.impactDelayL) || (guardLive && !isCenterImpact && s.guardSide > 0)
		muteFarR := (impactLive && s.impactMuteR && s.impactActiveS < s.impactDelayR) || (guardLive && !isCenterImpact && s.guardSide < 0)

		// Intra-road ducking so suspension rumble never fills curb rib valleys or buries discrete gravel/dirt pebbles
		roadDuck := 1.0
		if s.curbEnvL > 0.15 || s.curbEnvR > 0.15 {
			roadDuck = 0.14
		} else if surfaceKind == "dirt" || surfaceKind == "gravel" {
			roadDuck = 0.30
		}

		// 1) BUS 1 (BED): 7-FAMILY ENGINE SYNTHESIS (Crisp 28-76Hz V8 Cross-Plane Growl, Zero Low-Mid RPM Sub-Octave "通通通" Pulsing!)
		if engAmp > 0.005 {
			s.phEngBase += 2 * math.Pi * engFreq * dt1
			s.phEngSub += 2 * math.Pi * (engFreq * 0.5) * dt1
			wrapPhase(&s.phEngBase)
			wrapPhase(&s.phEngSub)

			lopeHz := 14.5 + 3.5*(1.0-car.EngineRoundness)
			s.phEngLope += 2 * math.Pi * lopeHz * dt1
			wrapPhase(&s.phEngLope)

			sharp := engSharp
			if isIdle {
				sharp = idleSharp
			}

			var w float64
			switch car.EngineFamily {
			case "ev":
				w = math.Sin(s.phEngBase) * 0.92
			case "muscle_v8":
				w = driveWave(s.phEngBase, 0.48, 1.65)*0.84 + 0.24*math.Sin(2*s.phEngBase)
				if isIdle && spd < 4.0 {
					w *= (0.78 + 0.22*math.Sin(s.phEngLope))
				}
			case "v12_silk":
				w = math.Sin(s.phEngBase)*0.84 + math.Sin(2*s.phEngBase)*0.20
			case "rotary_f1":
				w = driveWave(s.phEngBase, 0.85, 1.55)*0.78 + math.Sin(2*s.phEngBase)*0.24
			case "flat_gt":
				w = driveWave(s.phEngBase, clamp(sharp+0.20, 0, 1), 1.55)
			case "small_na":
				w = math.Sin(s.phEngBase)*0.90 + 0.15*math.Sin(2*s.phEngBase)
			default: // turbo_punch
				w = driveWave(s.phEngBase, sharp, 1.45)
				if isIdle && spd < 4.0 && car.EngineBurble > 0.15 {
					w *= (0.82 + 0.18*car.EngineBurble*math.Sin(s.phEngLope))
				}
			}

			engOut := w * engAmp * cfg.AntiFatigueClamp
			bedL += engOut
			bedR += engOut
		}

		// 2) BUS 2 (ROAD): Crisp Tight 28-44Hz Road Undulation & Suspension Direct-Drive
		if cfg.Events.RoadUndulation.Enabled && (s.roadEnvL > 0.015 || s.roadEnvR > 0.015 || s.heaveEnv > 0.025) {
			uFreq := (28.0 + 16.0*suspSharp) * cfg.Events.RoadUndulation.FreqScale
			s.phRoadL += 2 * math.Pi * uFreq * dt1
			s.phRoadR += 2 * math.Pi * (uFreq * 1.07) * dt1
			s.phHeave += 2 * math.Pi * ((22.0 + 10.0*suspSharp) * cfg.Events.RoadUndulation.FreqScale) * dt1
			wrapPhase(&s.phRoadL)
			wrapPhase(&s.phRoadR)
			wrapPhase(&s.phHeave)

			eL, eR := mixStereo(s.roadEnvL, s.roadEnvR, cfg.StereoSeparation)
			gain := 0.56 * cfg.Events.RoadUndulation.Intensity * roadDuck
			uWaveL := math.Sin(s.phRoadL) + 0.22*suspSharp*math.Sin(2*s.phRoadL)
			uWaveR := math.Sin(s.phRoadR) + 0.22*suspSharp*math.Sin(2*s.phRoadR)
			roadL += uWaveL * eL * gain
			roadR += uWaveR * eR * gain
			heaveW := math.Sin(s.phHeave) * s.heaveEnv * 0.28 * cfg.Events.RoadUndulation.Intensity * roadDuck
			roadL += heaveW
			roadR += heaveW
		}

		// 3) BUS 2 (ROAD) / BUS 1 (SUBTLE TARMAC): 6 Distinct Road Surfaces (Dry Crisp Pebble Hits, Low Heave Pad!)
		if cfg.Events.RoadSurface.Enabled && spd > 4.0 && surfaceKind != "water" {
			sg := clamp((spd-4.0)/22.0, 0, 1)
			surfGain := cfg.Events.RoadSurface.Intensity * car.DirtSensitivity
			surfSharp := clamp(cfg.Events.RoadSurface.Sharpness*0.65+car.SuspensionStiffness*0.35, 0, 1)

			switch surfaceKind {
			case "dirt":
				lInt, rInt := mixStereo(clamp(leftRumble*4.2, 0.38, 0.95)*sg, clamp(rightRumble*4.2, 0.38, 0.95)*sg, cfg.StereoSeparation)
				// Crisp 66-104Hz dry 8ms pebble impacts + minimal 0.10 heave pad
				pebRateMs := clamp(54.0-spd*0.20, 28.0, 54.0)
				roadL += s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, pebRateMs, 66.0, 104.0, 0.0085, clamp(surfSharp*0.75+0.20, 0, 1)) * lInt * 1.06 * surfGain
				roadR += s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, pebRateMs, 66.0, 104.0, 0.0085, clamp(surfSharp*0.75+0.20, 0, 1)) * rInt * 1.06 * surfGain
				roadL += math.Sin(s.phHeave*1.25) * lInt * 0.10 * surfGain
				roadR += math.Sin(s.phHeave*1.25+0.9) * rInt * 0.10 * surfGain
			case "gravel":
				lInt, rInt := mixStereo(clamp(leftRumble*2.6, 0.55, 1.0)*sg, clamp(rightRumble*2.6, 0.55, 1.0)*sg, cfg.StereoSeparation)
				stoneRateMs := clamp(46.0-spd*0.18, 26.0, 46.0)
				kL := s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, stoneRateMs, 68.0, 108.0, 0.010, clamp(surfSharp+0.25, 0, 1)) * lInt
				kR := s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, stoneRateMs, 68.0, 108.0, 0.010, clamp(surfSharp+0.25, 0, 1)) * rInt
				roadL += (kL*0.96 + s.nextNoise(&s.rngL)*0.10*lInt) * surfGain
				roadR += (kR*0.96 + s.nextNoise(&s.rngR)*0.10*rInt) * surfGain
				roadL += math.Sin(s.phHeave*1.15) * lInt * 0.10 * surfGain
				roadR += math.Sin(s.phHeave*1.15+1.3) * rInt * 0.10 * surfGain
			case "grass_snow":
				s.phGrassL += 2 * math.Pi * (24.5 * cfg.Events.RoadSurface.FreqScale) * dt1
				s.phGrassR += 2 * math.Pi * (27.5 * cfg.Events.RoadSurface.FreqScale) * dt1
				s.phGrassMod += 2 * math.Pi * (7.0 + 3.5*sg) * dt1
				wrapPhase(&s.phGrassL)
				wrapPhase(&s.phGrassR)
				wrapPhase(&s.phGrassMod)
				swell := 0.55 + 0.45*math.Sin(s.phGrassMod)
				lInt, rInt := mixStereo(clamp(leftRumble*3.5, 0.35, 0.85)*sg, clamp(rightRumble*3.5, 0.35, 0.85)*sg, cfg.StereoSeparation)
				roadL += math.Sin(s.phGrassL) * swell * lInt * 0.48 * surfGain
				roadR += math.Sin(s.phGrassR) * (1.1 - swell*0.5) * rInt * 0.48 * surfGain
			default:
				if spd > 35 && surfaceKind == "tarmac" {
					s.phTarmacL += 2 * math.Pi * 48.0 * dt1
					s.phTarmacR += 2 * math.Pi * 51.5 * dt1
					wrapPhase(&s.phTarmacL)
					wrapPhase(&s.phTarmacR)
					tarm := math.Min(0.055, clamp((spd-35)/190.0, 0, 1)*0.048*surfGain*cfg.AntiFatigueClamp)
					bedL += math.Sin(s.phTarmacL) * tarm
					bedR += math.Sin(s.phTarmacR) * tarm
				}
			}
		}

		// 4) BUS 3 (TIRE): Speed-Synced Periodic Curb Ribs (Crisp 2.35 exponent + 70-96Hz & 56Hz dry tooth punch + silent valleys!)
		if cfg.Events.CurbRumble.Enabled && (s.curbEnvL > 0.05 || s.curbEnvR > 0.05) {
			ribHz := clamp(spd/8.2, 9.5, 31.0) * cfg.Events.CurbRumble.FreqScale
			toneHz := (70.0 + 26.0*cfg.Events.CurbRumble.Sharpness) * cfg.Events.CurbRumble.FreqScale
			s.phCurbGateL += 2 * math.Pi * ribHz * dt1
			s.phCurbGateR += 2 * math.Pi * (ribHz * 1.02) * dt1
			s.phCurbToneL += 2 * math.Pi * toneHz * dt1
			s.phCurbToneR += 2 * math.Pi * (toneHz * 1.03) * dt1
			wrapPhase(&s.phCurbGateL)
			wrapPhase(&s.phCurbGateR)
			wrapPhase(&s.phCurbToneL)
			wrapPhase(&s.phCurbToneR)

			ribEnvL := math.Pow(math.Max(0, math.Sin(s.phCurbGateL)), 2.35)
			ribEnvR := math.Pow(math.Max(0, math.Sin(s.phCurbGateR)), 2.35)
			cL, cR := mixStereo(s.curbEnvL, s.curbEnvR, cfg.StereoSeparation)
			curbWaveL := 0.76*driveWave(s.phCurbToneL, cfg.Events.CurbRumble.Sharpness*0.85, 1.72) + 0.36*driveWave(s.phCurbToneL*0.76, 0.50, 1.55)
			curbWaveR := 0.76*driveWave(s.phCurbToneR, cfg.Events.CurbRumble.Sharpness*0.85, 1.72) + 0.36*driveWave(s.phCurbToneR*0.76, 0.50, 1.55)
			tireL += curbWaveL * ribEnvL * cL * 1.28 * cfg.Events.CurbRumble.Intensity
			tireR += curbWaveR * ribEnvR * cR * 1.28 * cfg.Events.CurbRumble.Intensity
			curbMax := math.Max(cL, cR)
			engDuckEnv = math.Max(engDuckEnv, curbMax*0.96)
			tireActivityEnv = math.Max(tireActivityEnv, curbMax*0.90)
		}

		// 5) BUS 4 (IMP): Crisp Two-Stage Jump Landing (20ms 108-128Hz tire slap + 76->48Hz fast exp(-4.2*u) thump + 55..90ms rebound!)
		if s.landActiveS > 0 && cfg.Events.JumpLanding.Enabled {
			if s.landActiveS >= s.landDurS {
				s.landActiveS = 0
			} else {
				t := s.landActiveS
				u := t / s.landDurS
				inten := s.landPeak * cfg.Events.JumpLanding.Intensity
				if t < 0.020 {
					f := (108.0 + 20.0*cfg.Events.JumpLanding.Sharpness) * cfg.Events.JumpLanding.FreqScale
					slap := driveWave(2*math.Pi*f*t, cfg.Events.JumpLanding.Sharpness, 1.88) * (1.0 - t/0.020) * inten * 1.32
					impL += slap * s.landLeftW
					impR += slap * s.landRightW
				}
				phLow := 2 * math.Pi * (76.0 - 14.0*u) * cfg.Events.JumpLanding.FreqScale * t
				tail := clamp((1.0-u)/0.20, 0, 1)
				thump := driveWave(phLow, 0.42, 1.82) * math.Exp(-4.2*u) * tail * inten * 1.44
				rebound := 0.0
				if t >= 0.055 && t <= 0.090 {
					ru := (t - 0.055) / 0.035
					rebound = driveWave(phLow*0.86, 0.42, 1.72) * math.Sin(math.Pi*ru) * tail * inten * 0.76
				}
				impL += (thump + rebound) * s.landLeftW
				impR += (thump + rebound) * s.landRightW
				engDuckEnv = math.Max(engDuckEnv, math.Exp(-2.8*u)*clamp(s.landPeak*1.1, 0.78, 1.0))
				impDuckEnv = math.Max(impDuckEnv, math.Exp(-3.0*u)*s.landPeak)
				s.landActiveS += dt1
			}
		}

		// 6) BUS 4 (IMP): Crisp Dry Gear Shift Kick (60-95ms total: 18ms 108-132Hz dog-clutch click + 64->46Hz punch!)
		if s.shiftActiveS > 0 && cfg.Events.GearShift.Enabled {
			if s.shiftActiveS >= s.shiftDurS {
				s.shiftActiveS = 0
			} else {
				t := s.shiftActiveS
				u := t / s.shiftDurS
				inten := s.shiftStrength * cfg.Events.GearShift.Intensity
				sharp := clamp(car.ShiftCrispness*0.6+cfg.Events.GearShift.Sharpness*0.4, 0, 1)
				if t < 0.018 {
					click := driveWave(2*math.Pi*(108.0+24.0*sharp)*cfg.Events.GearShift.FreqScale*t, sharp, 1.88) * (1.0 - t/0.018) * inten * 1.38
					impL += click
					impR += click
				}
				tail := clamp((1.0-u)/0.22, 0, 1)
				phThud := 2 * math.Pi * (64.0 - 9.0*u) * cfg.Events.GearShift.FreqScale * t
				thud := driveWave(phThud, 0.46, 1.78) * math.Pow(1.0-u, 0.85) * math.Exp(-2.6*u) * tail * inten * 1.46
				impL += thud
				impR += thud
				engDuckEnv = math.Max(engDuckEnv, math.Exp(-2.8*u)*0.94)
				impDuckEnv = math.Max(impDuckEnv, math.Exp(-3.0*u)*0.85)
				s.shiftActiveS += dt1
			}
		}

		// 7) BUS 4 (IMP): Crisp Staccato Multi-Layer Redline Eruption (46-56Hz punch + 78-94Hz roar + 18.5Hz cyc<0.46 fuel-cut pops!)
		if isRedline {
			sharp := cfg.Events.Redline.Sharpness
			fScale := cfg.Events.Redline.FreqScale
			subHz := (46.0 + 10.0*sharp) * fScale  // 46..56 Hz tight cylinder-cut punch
			roarHz := (78.0 + 16.0*sharp) * fScale // 78..94 Hz crisp high-RPM roar harmonic
			cutHz := (17.0 + 2.0*sharp) * clamp(fScale, 0.85, 1.15)
			s.phRedSub += 2 * math.Pi * subHz * dt1
			s.phRedline += 2 * math.Pi * roarHz * dt1
			s.phRedGate += 2 * math.Pi * cutHz * dt1
			wrapPhase(&s.phRedSub)
			wrapPhase(&s.phRedline)
			wrapPhase(&s.phRedGate)

			cyc := s.phRedGate / (2 * math.Pi) // [0, 1) within each ~18.5 Hz ignition-cut cycle
			burstEnv := 0.0
			popEnv := 0.0
			if cyc < 0.46 {
				uCut := cyc / 0.46
				// Tight 46% duty cycle staccato fuel-cut pop ("啪！啪！啪！") with 54% crisp cut valley
				burstEnv = math.Pow(1.0-uCut*0.50, 1.40)
				if cyc < 0.18 {
					popEnv = 1.0 - cyc/0.18
				}
			}
			excess := clamp((rpmRatio-effRedlineRatio)/0.05+0.84, 0.84, 1.0)
			thump := driveWave(s.phRedSub, 0.60+0.35*sharp, 1.95)
			roar := driveWave(s.phRedline, 0.76+0.24*sharp, 1.95)
			popL := (0.72*driveWave(s.phRedline*1.42, 0.95, 2.0) + 0.28*s.nextNoise(&s.rngL)) * popEnv * 0.60
			popR := (0.72*driveWave(s.phRedline*1.42+0.4, 0.95, 2.0) + 0.28*s.nextNoise(&s.rngR)) * popEnv * 0.60
			rwL := ((0.62*thump+0.54*roar)*burstEnv + popL) * excess * 1.36 * cfg.Events.Redline.Intensity
			rwR := ((0.62*thump+0.54*roar)*burstEnv + popR) * excess * 1.36 * cfg.Events.Redline.Intensity
			impL += rwL
			impR += rwR
			// Gate engine bed dynamically with the fuel-cut valley so peaks combine to ±110..122 while valleys drop cleanly!
			bedCutGain := 0.12 + 0.88*burstEnv
			bedL *= bedCutGain
			bedR *= bedCutGain
			engDuckEnv = math.Max(engDuckEnv, 0.30*(1.0-burstEnv))
			impDuckEnv = math.Max(impDuckEnv, burstEnv*0.72)
		}

		// 8) BUS 4 (IMP) & BUS 2 (ROAD): Water Entry Hydro-Impact Splash + Liquid Wave Swells
		if cfg.Events.WaterSplash.Enabled && (s.splashActiveS > 0 || (maxPuddle >= 0.03 && spd > 5)) {
			if s.splashActiveS > 0 {
				if s.splashActiveS >= s.splashDurS {
					s.splashActiveS = 0
				} else {
					t := s.splashActiveS
					u := t / s.splashDurS
					phSp := 2 * math.Pi * (96.0 - 22.0*u) * cfg.Events.WaterSplash.FreqScale * t
					envSp := math.Exp(-3.2*u) * clamp((1.0-u)/0.20, 0, 1)
					sharpSp := clamp(0.50+0.35*cfg.Events.WaterSplash.Sharpness, 0, 1)
					spL := (0.78*driveWave(phSp, sharpSp, 1.80) + 0.22*s.nextNoise(&s.rngL)) * envSp * s.splashPeak * 1.38 * cfg.Events.WaterSplash.Intensity
					spR := (0.78*driveWave(phSp+0.6, sharpSp, 1.80) + 0.22*s.nextNoise(&s.rngR)) * envSp * s.splashPeak * 1.38 * cfg.Events.WaterSplash.Intensity
					impL += spL * s.splashLeftW
					impR += spR * s.splashRightW
					engDuckEnv = math.Max(engDuckEnv, envSp*0.82)
					impDuckEnv = math.Max(impDuckEnv, envSp*0.65)
					s.splashActiveS += dt1
				}
			}
			if maxPuddle >= 0.03 && spd > 5 {
				wFreq := (36.0 + 16.0*clamp(spd/130.0, 0, 1)) * cfg.Events.WaterSplash.FreqScale
				s.phWater += 2 * math.Pi * wFreq * dt1
				s.phWaterSwell += 2 * math.Pi * (6.8 + 3.8*clamp(spd/120.0, 0, 1)) * dt1
				wrapPhase(&s.phWater)
				wrapPhase(&s.phWaterSwell)
				swellL := 0.45 + 0.55*math.Sin(s.phWaterSwell)
				swellR := 0.45 + 0.55*math.Sin(s.phWaterSwell+1.1)
				sf := clamp(spd/70.0, 0.45, 1.0)
				wL, wR := mixStereo(clamp(leftPuddle*2.6, 0, 1)*sf, clamp(rightPuddle*2.6, 0, 1)*sf, cfg.StereoSeparation)
				gain := 0.78 * cfg.Events.WaterSplash.Intensity
				roadL += (0.80*driveWave(s.phWater, 0.35, 1.42) + 0.20*s.nextNoise(&s.rngL)) * swellL * wL * gain
				roadR += (0.80*driveWave(s.phWater+0.9, 0.35, 1.42) + 0.20*s.nextNoise(&s.rngR)) * swellR * wR * gain
			}
		}

		// 9A) BUS 3 (TIRE): 4-TIRE DRIVEN 推头 (UNDERSTEER): Crisp 88-110Hz with staccato 18Hz scrub valley
		if isUndersteer || isFourWheelDrift {
			uFreq := (88.0 + 22.0*clamp(rawFrontSlip/1.8, 0, 1)) * cfg.Events.Understeer.FreqScale
			s.phUnderL += 2 * math.Pi * uFreq * dt1
			s.phUnderR += 2 * math.Pi * (uFreq * 1.05) * dt1
			s.phUnderHop += 2 * math.Pi * 18.0 * dt1
			wrapPhase(&s.phUnderL)
			wrapPhase(&s.phUnderR)
			wrapPhase(&s.phUnderHop)

			hopMod := 0.06 + 0.94*math.Pow(math.Max(0, math.Sin(s.phUnderHop)), 1.35)
			uScale := 1.04
			if isFourWheelDrift {
				uScale = 0.54
			}
			inten := clamp((rawFrontSlip-0.42)/0.95, 0.56, 1.0) * cfg.Events.Understeer.Intensity * car.SlipExpressiveness
			tireL += driveWave(s.phUnderL, cfg.Events.Understeer.Sharpness*0.80, 1.55) * hopMod * underL * inten * uScale
			tireR += driveWave(s.phUnderR, cfg.Events.Understeer.Sharpness*0.80, 1.55) * hopMod * underR * inten * uScale
			engDuckEnv = math.Max(engDuckEnv, clamp(inten*0.92, 0.72, 1.0))
			tireActivityEnv = math.Max(tireActivityEnv, clamp(inten*0.82, 0.48, 1.0))
		}

		// 9B) BUS 3 (TIRE): 4-TIRE DRIVEN 甩尾/漂移 (OVERSTEER/DRIFT): Crisp Tight 36-50Hz Dry Tire Judder
		if isOversteer || isLaunchSpin || isFourWheelDrift {
			oBaseHz := (36.0 + 14.0*clamp(rawRearSlip/2.2, 0, 1)) * cfg.Events.OversteerDrift.FreqScale
			s.phOverL += 2 * math.Pi * oBaseHz * dt1
			s.phOverR += 2 * math.Pi * (oBaseHz * 1.04) * dt1
			s.phOverSweep += 2 * math.Pi * 7.5 * dt1
			wrapPhase(&s.phOverL)
			wrapPhase(&s.phOverR)
			wrapPhase(&s.phOverSweep)

			gScale := cfg.Events.OversteerDrift.Intensity * car.SlipExpressiveness
			if isLaunchSpin {
				inten := clamp(drivenSlip/1.30, 0.56, 1.0) * gScale
				hop := 0.52 + 0.48*math.Sin(s.phOverSweep*2.6)
				dl, dr := contrastLR(slipFL+slipRL, slipFR+slipRR, 1.2)
				wL := 0.82*math.Sin(s.phOverL*1.25) + 0.28*math.Sin(2.5*s.phOverL)
				wR := 0.82*math.Sin(s.phOverR*1.25) + 0.28*math.Sin(2.5*s.phOverR)
				tireL += wL * hop * dl * inten * 0.96
				tireR += wR * hop * dr * inten * 0.96
				engDuckEnv = math.Max(engDuckEnv, clamp(inten*0.88, 0.68, 1.0))
				tireActivityEnv = math.Max(tireActivityEnv, clamp(inten*0.75, 0.45, 1.0))
			} else {
				inten := clamp((rawRearSlip-0.38)/1.05, 0.56, 1.0) * gScale
				oScale := 1.06
				if isFourWheelDrift {
					oScale = 0.80
				}
				swell := 0.76 + 0.24*math.Sin(s.phOverSweep)
				wL := 0.84*math.Sin(s.phOverL) + 0.28*math.Sin(2*s.phOverL)
				wR := 0.84*math.Sin(s.phOverR) + 0.28*math.Sin(2*s.phOverR)
				tireL += wL * swell * overL * inten * oScale
				tireR += wR * swell * overR * inten * oScale
				engDuckEnv = math.Max(engDuckEnv, clamp(inten*0.90, 0.70, 1.0))
				tireActivityEnv = math.Max(tireActivityEnv, clamp(inten*0.78, 0.48, 1.0))
			}
		}

		// 10) BUS 3 (TIRE): V4 开心散仙竞速2 Two-Stage Braking (70Hz Threshold Warning + 10Hz Crisp ABS / 50%-Scaled Dry Lock Body)
		if cfg.Events.BrakingABS.Enabled && (isWheelLock || isABS || isBrakeWarning) {
			fScale := cfg.Events.BrakingABS.FreqScale
			// V4 kaixin_race2_clear_lock: abs_freq = 10.0 Hz (100ms crisp pressure-release cycle)
			s.phAbsPulse += 2 * math.Pi * (10.0 * fScale) * dt1
			wrapPhase(&s.phAbsPulse)
			if isWheelLock {
				// Stage 2 Lockup on Grip: 72Hz tire-lock shudder + 50Hz weight-transfer thump, scaled by 50% * 1.198 = 0.60x so Left Trigger 100Hz lock dominates!
				s.phAbsGrip += 2 * math.Pi * (72.0 * fScale) * dt1
				wrapPhase(&s.phAbsGrip)
				judder := 0.76 + 0.24*math.Sin(2*s.phAbsPulse)
				w := (0.78*driveWave(s.phAbsGrip, 0.45, 1.38) + 0.26*math.Sin(s.phAbsGrip*0.70)) * judder
				tireL += w * absL * 0.60 * cfg.Events.BrakingABS.Intensity
				tireR += w * absR * 0.60 * cfg.Events.BrakingABS.Intensity
				engDuckEnv = math.Max(engDuckEnv, 0.84)
				tireActivityEnv = math.Max(tireActivityEnv, 0.68)
			} else if isABS {
				// Stage 2 ABS on Grip: 70Hz + 50Hz body carrier gated by crisp 10Hz raised-front ABS pulse (50% * 1.294 = 0.65x)
				s.phAbsGrip += 2 * math.Pi * (70.0 * fScale) * dt1
				wrapPhase(&s.phAbsGrip)
				cyc := s.phAbsPulse / (2 * math.Pi)
				pulse := 0.0
				if cyc < 0.56 {
					uAbs := cyc / 0.56
					pulse = math.Sin(math.Pi*uAbs) * math.Exp(-0.85*uAbs) * 1.18
				}
				w := 0.80*driveWave(s.phAbsGrip, 0.38, 1.35) + 0.24*math.Sin(s.phAbsGrip*0.71)
				tireL += w * pulse * absL * 0.65 * cfg.Events.BrakingABS.Intensity
				tireR += w * pulse * absR * 0.65 * cfg.Events.BrakingABS.Intensity
				engDuckEnv = math.Max(engDuckEnv, 0.80)
				tireActivityEnv = math.Max(tireActivityEnv, 0.62)
			} else if isBrakeWarning {
				// Stage 1 Threshold Warning on Grip: V4 kaixin 70Hz restrained 40%..55% curve tension hum
				s.phAbsGrip += 2 * math.Pi * (70.0 * fScale) * dt1
				wrapPhase(&s.phAbsGrip)
				warnCurve := 0.40 + 0.15*clamp((brakeTireLoad-0.29)/0.71, 0, 1)
				w := math.Sin(s.phAbsGrip) * warnCurve * 0.34 * cfg.Events.BrakingABS.Intensity
				tireL += w * absL
				tireR += w * absR
				engDuckEnv = math.Max(engDuckEnv, warnCurve*0.55)
			}
		}

		// 11) BUS 4 (IMP): CONTINUOUS HIGH-FREQ DIRECTIONAL GUARDRAIL SCRAPE (76Hz + 112Hz steel screech + grit, 100% vs 0% L/R!)
		if s.guardActiveS > 0 && cfg.Events.Guardrail.Enabled {
			s.guardActiveS = math.Max(0, s.guardActiveS-dt1)
			s.guardElapsedS += dt1
			s.phGuard += 2 * math.Pi * (112.0 * cfg.Events.Guardrail.FreqScale) * dt1
			wrapPhase(&s.phGuard)
			// Continuous dense metallic rasp (smooth high-rate ripple 0.78..1.0, NO slow stuttering gaps!)
			mod := 0.78 + 0.22*math.Sin(2*math.Pi*34.0*s.guardElapsedS)
			gw := (0.45*driveWave(s.phGuard, cfg.Events.Guardrail.Sharpness, 1.95) + 0.35*driveWave(s.phGuard*0.68, 0.55, 1.80) + 0.28*s.nextNoise(&s.rngL)) * mod * s.guardPeak * 1.48 * cfg.Events.Guardrail.Intensity
			if s.guardSide < 0 {
				impL += gw
			} else if s.guardSide > 0 {
				impR += gw
			} else {
				impL += gw
				impR += gw
			}
			engDuckEnv = math.Max(engDuckEnv, clamp(s.guardPeak*1.05, 0.85, 1.0))
			impDuckEnv = math.Max(impDuckEnv, s.guardPeak*0.92)
		} else {
			s.guardElapsedS = 0
		}

		// 12) BUS 4 (IMP): Crisp Smashable Debris (55-90ms fast exp(-5.2*u) dry snap)
		if s.smashActiveS > 0 && cfg.Events.CollisionWall.Enabled {
			if s.smashActiveS >= s.smashDurS {
				s.smashActiveS = 0
			} else {
				u := s.smashActiveS / s.smashDurS
				s.phSmash += 2 * math.Pi * (132.0 * cfg.Events.CollisionWall.FreqScale) * dt1
				wrapPhase(&s.phSmash)
				sw := (0.72*driveWave(s.phSmash, 0.88, 1.88) + 0.28*s.nextNoise(&s.rngL)) * math.Exp(-5.2*u) * s.smashPeak * 1.12 * cfg.Events.CollisionWall.Intensity
				impL += sw
				impR += sw
				engDuckEnv = math.Max(engDuckEnv, (1.0-u)*0.80)
				impDuckEnv = math.Max(impDuckEnv, (1.0-u)*0.65)
				s.smashActiveS += dt1
			}
		}

		// 13) BUS 4 (IMP): CRISP DRY 360° DIRECTIONAL COLLISION (62-88Hz Chassis Slam + 132Hz Crack + 52..88ms Rebound, Fast Dry Cutoff!)
		if s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled {
			if s.impactActiveS >= s.impactDurS {
				s.impactActiveS = 0
			} else {
				t := s.impactActiveS
				u := t / s.impactDurS
				inten := s.impactPeak * cfg.Events.CollisionWall.Intensity
				sharp := cfg.Events.CollisionWall.Sharpness

				// Crisp dry decay + clean tail fade to exact zero in final 20% of duration
				tailFade := clamp((1.0-u)/0.20, 0, 1)
				envBody := math.Pow(1.0-u, 1.35) * math.Exp(-3.4*u) * tailFade

				bodyHz := (88.0 - 26.0*u) * cfg.Events.CollisionWall.FreqScale // 88Hz -> 62Hz tight solid chassis slam
				if s.impactKind == "rear" || s.impactKind == "rear_left" || s.impactKind == "rear_right" {
					bodyHz = (64.0 + 28.0*u) * cfg.Events.CollisionWall.FreqScale // 64Hz -> 92Hz rising rear shove
				} else if s.impactLowSpeed {
					bodyHz = (102.0 - 28.0*u) * cfg.Events.CollisionWall.FreqScale // 102Hz -> 74Hz dry bumper thud
				}
				crackHz := 132.0 * cfg.Events.CollisionWall.FreqScale
				s.phWallA += 2 * math.Pi * bodyHz * dt1
				s.phWallB += 2 * math.Pi * crackHz * dt1
				wrapPhase(&s.phWallA)
				wrapPhase(&s.phWallB)

				crackEnv := 0.0
				if t < 0.024 {
					crackEnv = 1.0 - t/0.024
				}
				crack := (0.76*driveWave(s.phWallB, sharp, 1.98) + 0.24*s.nextNoise(&s.rngL)) * crackEnv * inten * 1.68
				bodyWave := 0.80*driveWave(s.phWallA, 0.54+0.30*sharp, 1.92) + 0.30*math.Sin(s.phWallA*0.68)
				body := bodyWave * envBody * inten * 2.02
				rebound := 0.0
				if !s.impactLowSpeed && t >= 0.052 && t <= 0.088 {
					ru := (t - 0.052) / 0.036
					rebound = driveWave(s.phWallA*0.85, 0.48, 1.68) * math.Sin(math.Pi*ru) * tailFade * inten * 0.78
				}

				cL, cR := 0.0, 0.0
				switch s.impactKind {
				case "left":
					cL = crack + body + rebound
					if t >= s.impactDelayR {
						cR = math.Sin(s.phWallA*0.75) * envBody * inten * 0.22
					}
				case "right":
					cR = crack + body + rebound
					if t >= s.impactDelayL {
						cL = math.Sin(s.phWallA*0.75) * envBody * inten * 0.22
					}
				default:
					sig := crack + body + rebound
					if t >= s.impactDelayL {
						cL = sig * s.impactLeftW
					}
					if t >= s.impactDelayR {
						cR = sig * s.impactRightW
					}
				}
				impL += cL
				impR += cR
				impEnv := clamp(envBody*1.45+crackEnv, 0, 1)
				engDuckEnv = math.Max(engDuckEnv, impEnv)
				impDuckEnv = math.Max(impDuckEnv, impEnv)
				s.impactActiveS += dt1
			}
		}

		// Two-Level Sidechain Ducking + Fast Recovery (0.045 / 0.050) for Immediate Post-Event Clarity
		engDuckDepth := clamp(cfg.DuckingStrength*1.32, 0.0, 0.82)
		targetEngDuck := 1.0 - engDuckDepth*clamp(engDuckEnv, 0, 1)
		if targetEngDuck < s.engineDuckGain {
			s.engineDuckGain += 0.28 * (targetEngDuck - s.engineDuckGain)
		} else {
			s.engineDuckGain += 0.045 * (targetEngDuck - s.engineDuckGain)
		}

		midDuckDepth := clamp(cfg.DuckingStrength*1.15, 0.0, 0.78)
		targetMidDuck := 1.0 - midDuckDepth*clamp(impDuckEnv, 0, 1)
		if targetMidDuck < s.midDuckGain {
			s.midDuckGain += 0.26 * (targetMidDuck - s.midDuckGain)
		} else {
			s.midDuckGain += 0.050 * (targetMidDuck - s.midDuckGain)
		}

		roadTireYield := 1.0 - 0.35*clamp(tireActivityEnv, 0, 1)

		if collisionExclusive || muteFarL {
			s.collisionRampL = 0.0
		} else {
			s.collisionRampL += 0.038 * (1.0 - s.collisionRampL)
		}
		if collisionExclusive || muteFarR {
			s.collisionRampR = 0.0
		} else {
			s.collisionRampR += 0.038 * (1.0 - s.collisionRampR)
		}

		mixL := (bedL*s.engineDuckGain*s.collisionRampL +
			(roadL*roadTireYield+tireL)*s.midDuckGain*s.collisionRampL +
			1.15*impL) * cfg.MasterGripGain
		mixR := (bedR*s.engineDuckGain*s.collisionRampR +
			(roadR*roadTireYield+tireR)*s.midDuckGain*s.collisionRampR +
			1.15*impR) * cfg.MasterGripGain
		if muteFarL {
			mixL = 0
		}
		if muteFarR {
			mixR = 0
		}

		encL := int(math.Round(128.0 + math.Tanh(mixL*0.92)*124.0))
		encR := int(math.Round(128.0 + math.Tanh(mixR*0.92)*124.0))
		if encL < 2 || encL > 253 || encR < 2 || encR > 253 {
			res.Clipped = true
		}
		bL := byte(clamp(float64(encL), 2, 253))
		bR := byte(clamp(float64(encR), 2, 253))
		res.GripL[ms] = bL
		res.GripR[ms] = bR
		dL := float64(bL) - 128.0
		dR := float64(bR) - 128.0
		sumSqL += dL * dL
		sumSqR += dR * dR

		// --- ANTI-CLATTER 4-TIRE TRIGGER SYNTHESIS (<=100Hz Rattle-Free Ceiling + 52-74Hz Silky Micro-Feel + 10ms Soft Release Tail) ---
		tLVal, tRVal := 0.0, 0.0
		tLFreq, tLAmp := 0.0, 0.0
		tRFreq, tRAmp := 0.0, 0.0
		candPL, candPR := tpOff, tpOff
		shiftTrigNow := false

		if !collisionExclusive {
			if cfg.Trigger.EnableLeftBrake {
				fScale := cfg.Events.BrakingABS.FreqScale
				if isWheelLock {
					// V4 开心散仙竞速2: traction_lock_trigger_frequency_hz = 100 Hz (clamped <=100Hz rattle-free ceiling)
					candPL = tpDynamicEvent
					tLFreq = clamp(100.0*fScale, 68.0, 100.0)
					lockMod := 0.82 + 0.18*math.Sin(2*s.phAbsPulse)
					tLAmp = lockMod * 0.94 * cfg.Events.BrakingABS.Intensity
				} else if isABS {
					// V4 开心散仙竞速2: 100 Hz trigger carrier modulated by crisp 10 Hz ABS pressure-release gate
					candPL = tpDynamicEvent
					tLFreq = clamp(100.0*fScale, 68.0, 100.0)
					cyc := s.phAbsPulse / (2 * math.Pi)
					pulse := 0.24
					if cyc < 0.62 {
						uAbs := cyc / 0.62
						pulse = 0.24 + 0.76*math.Pow(math.Sin(math.Pi*uAbs), 0.75)
					}
					tLAmp = pulse * 0.92 * cfg.Events.BrakingABS.Intensity
				} else if isUndersteer && (brakeF > 0.08 || (accelF <= 0.12 && slipFL >= slipFR)) {
					candPL = tpDynamicEvent
					tLFreq = 92.0 + 2.0*clamp(rawFrontSlip/1.8, 0, 1) // 92..94 Hz (below 106-112Hz rattle zone)
					hop := 0.32 + 0.68*math.Max(0, math.Sin(s.phUnderHop))
					tLAmp = hop * clamp(math.Max(slipFL, rawFrontSlip)/1.20, 0.55, 1.0) * 0.88 * cfg.Events.Understeer.Intensity
				} else if (isOversteer || isFourWheelDrift) && (brakeF > 0.12 || hbActive || (accelF <= 0.08 && slipRL > slipRR*1.12)) {
					candPL = tpDynamicEvent
					tLFreq = 84.0 + 10.0*clamp(rawRearSlip/2.0, 0, 1) // 84..94 Hz (below 106-112Hz rattle zone)
					slipProg := clamp((math.Max(slipRL, rawRearSlip)-0.62)/0.73, 0, 1)
					tLAmp = (0.20 + 0.80*slipProg) * 0.84 * cfg.Events.OversteerDrift.Intensity
				} else if isBrakeWarning {
					// V4 开心散仙竞速2: 90 Hz progressive pre-lock warning, smoothly scaled from brakeResistance (no 0.78 cliff!)
					candPL = tpEdgeWarning
					tLFreq = clamp(90.0*fScale, 62.0, 96.0)
					wProg := clamp((brakeTireLoad-0.29)/0.71, 0, 1)
					resistScale := 0.35 + 0.95*clamp(cfg.Trigger.BrakeResistance, 0, 1)
					tLAmp = (0.22 + 0.26*wProg) * resistScale * cfg.Events.BrakingABS.Intensity
				} else if brakeF > 0.16 && cfg.Trigger.BrakeResistance > 0.01 {
					candPL = tpStaticResistance
					tLFreq = 90.0
					tLAmp = clamp((brakeF-0.16)/0.84, 0, 1) * 0.22 * cfg.Trigger.BrakeResistance
				}
			}

			if cfg.Trigger.EnableRightAccel {
				if shiftT > 0 && shiftT < 0.032 && cfg.Events.GearShift.Enabled {
					candPR = tpDynamicEvent
					shiftTrigNow = true
					shiftTrigActiveFrame = true
					u := shiftT / 0.032
					// 32ms 92Hz bipolar damped AC sine (~3 full symmetric cycles, zero DC bias, cleanly below 106-112Hz rattle!)
					shiftFreq := clamp(92.0*cfg.Events.GearShift.FreqScale, 68.0, 100.0)
					tRVal = math.Sin(2*math.Pi*shiftFreq*shiftT) * clamp(shiftT/0.003, 0, 1) * math.Exp(-3.5*u) * 0.85 * s.shiftStrength * cfg.Events.GearShift.Intensity
				} else if isLaunchSpin {
					candPR = tpDynamicEvent
					tRFreq = 96.0 // cleanly below 106-112Hz rattle zone
					tRAmp = 0.85 * clamp(drivenSlip/1.35, 0.50, 1.0) * cfg.Events.OversteerDrift.Intensity
				} else if (isOversteer || isFourWheelDrift) && (accelF > 0.08 || (brakeF <= 0.12 && !hbActive && slipRR >= slipRL*0.88)) {
					candPR = tpDynamicEvent
					tRFreq = 84.0 + 10.0*clamp(rawRearSlip/2.0, 0, 1) // 84..94 Hz (below 106-112Hz rattle zone!)
					slipProg := clamp((math.Max(slipRR, rawRearSlip)-0.62)/0.73, 0, 1)
					tRAmp = (0.20 + 0.80*slipProg) * 0.88 * cfg.Events.OversteerDrift.Intensity
				} else if isUndersteer && ((accelF > 0.12 && brakeF <= 0.08) || (brakeF <= 0.08 && slipFR > slipFL)) {
					candPR = tpDynamicEvent
					tRFreq = 94.0 + 2.0*clamp(rawFrontSlip/1.8, 0, 1) // 94..96 Hz (below 106-112Hz rattle zone)
					hop := 0.32 + 0.68*math.Max(0, math.Sin(s.phUnderHop))
					tRAmp = hop * clamp(math.Max(slipFR, rawFrontSlip)/1.20, 0.55, 1.0) * 0.88 * cfg.Events.Understeer.Intensity
				} else if effMicro := clamp(cfg.Trigger.ThrottleMicroFeel, 0.0, 1.0); accelF > 0.16 && effMicro > 0.001 {
					// Single C-infinity continuous engine micro-feel across 0..1.05 rpmRatio in V4's clean 52-74Hz band
					// (far below APEX 6's 106-112Hz RT gearbox resonance, linear from 0.0 with no hidden 0.45 floor!)
					if isRedline {
						candPR = tpEdgeWarning
					} else {
						candPR = tpStaticResistance
					}
					tRFreq = clamp((52.0+22.0*clamp(rpmRatio, 0, 1.05))*cfg.Events.EngineRev.FreqScale, 42.0, 92.0)
					rpmFeel := 0.08 + 0.12*math.Pow(clamp(rpmRatio, 0, 1.05), 1.35)
					tRAmp = accelF * rpmFeel * effMicro
				}
			}
		}

		// Per-ms continuous amplitude slewing (<=0.065/ms) & phase-continuous frequency smoothing (0.18 EMA/ms) on BOTH triggers
		const trigRampStep = 0.10     // 10ms linear attack/release envelope
		const trigAmpSlewStep = 0.065 // prevents any 1ms branch-switch amplitude step clack!
		if tLAmp > 0.0001 {
			if s.trigEnvL <= 0.0001 || s.trigLastFreqL <= 0 {
				s.trigLastFreqL = tLFreq
			} else {
				s.trigLastFreqL += 0.18 * (tLFreq - s.trigLastFreqL)
			}
			s.trigEnvL = math.Min(1.0, s.trigEnvL+trigRampStep)
			if tLAmp > s.trigLastAmpL+trigAmpSlewStep {
				s.trigLastAmpL += trigAmpSlewStep
			} else if tLAmp < s.trigLastAmpL-trigAmpSlewStep {
				s.trigLastAmpL -= trigAmpSlewStep
			} else {
				s.trigLastAmpL = tLAmp
			}
			s.phTrigL += 2 * math.Pi * s.trigLastFreqL * dt1
			wrapPhase(&s.phTrigL)
			tLVal = math.Sin(s.phTrigL) * s.trigLastAmpL * s.trigEnvL
		} else if s.trigEnvL > 0.0001 && s.trigLastAmpL > 0.0001 && s.trigLastFreqL > 0 {
			s.trigEnvL = math.Max(0.0, s.trigEnvL-trigRampStep)
			s.trigLastAmpL = math.Max(0.0, s.trigLastAmpL-trigAmpSlewStep)
			s.phTrigL += 2 * math.Pi * s.trigLastFreqL * dt1
			wrapPhase(&s.phTrigL)
			tLVal = math.Sin(s.phTrigL) * s.trigLastAmpL * s.trigEnvL
			if candPL == tpOff && !collisionExclusive && cfg.Trigger.EnableLeftBrake {
				candPL = tpReleaseTail
			}
		} else {
			s.trigEnvL = 0
			s.trigLastAmpL = 0
		}

		if shiftTrigNow {
			s.trigEnvR = math.Max(0.0, s.trigEnvR-trigRampStep)
			s.trigLastAmpR = math.Max(0.0, s.trigLastAmpR-trigAmpSlewStep)
		} else if tRAmp > 0.0001 {
			if s.trigEnvR <= 0.0001 || s.trigLastFreqR <= 0 {
				s.trigLastFreqR = tRFreq
			} else {
				s.trigLastFreqR += 0.18 * (tRFreq - s.trigLastFreqR)
			}
			s.trigEnvR = math.Min(1.0, s.trigEnvR+trigRampStep)
			if tRAmp > s.trigLastAmpR+trigAmpSlewStep {
				s.trigLastAmpR += trigAmpSlewStep
			} else if tRAmp < s.trigLastAmpR-trigAmpSlewStep {
				s.trigLastAmpR -= trigAmpSlewStep
			} else {
				s.trigLastAmpR = tRAmp
			}
			s.phTrigR += 2 * math.Pi * s.trigLastFreqR * dt1
			wrapPhase(&s.phTrigR)
			tRVal = math.Sin(s.phTrigR) * s.trigLastAmpR * s.trigEnvR
		} else if s.trigEnvR > 0.0001 && s.trigLastAmpR > 0.0001 && s.trigLastFreqR > 0 {
			s.trigEnvR = math.Max(0.0, s.trigEnvR-trigRampStep)
			s.trigLastAmpR = math.Max(0.0, s.trigLastAmpR-trigAmpSlewStep)
			s.phTrigR += 2 * math.Pi * s.trigLastFreqR * dt1
			wrapPhase(&s.phTrigR)
			tRVal = math.Sin(s.phTrigR) * s.trigLastAmpR * s.trigEnvR
			if candPR == tpOff && !collisionExclusive && cfg.Trigger.EnableRightAccel {
				candPR = tpReleaseTail
			}
		} else {
			s.trigEnvR = 0
			s.trigLastAmpR = 0
		}

		if candPL > pL {
			pL = candPL
		}
		if candPR > pR {
			pR = candPR
		}

		// Hardware-safe ±64 trigger ceiling, hard-clamped to [60, 196]
		encTL := int(math.Round(128.0 + math.Tanh(tLVal*cfg.MasterTriggerGain*0.92)*64.0))
		encTR := int(math.Round(128.0 + math.Tanh(tRVal*cfg.MasterTriggerGain*0.92)*64.0))
		trigLBuf[ms] = byte(clamp(float64(encTL), 60, 196))
		trigRBuf[ms] = byte(clamp(float64(encTR), 60, 196))
	}

	res.GripLRms = math.Sqrt(sumSqL / 8.0)
	res.GripRRms = math.Sqrt(sumSqR / 8.0)

	// Trigger route arbitration + 70ms hold hysteresis on non-urgent switches
	s.trigRouteHoldS = math.Max(0, s.trigRouteHoldS-0.008)
	bothDriftTrigs := (isFourWheelDrift || (isOversteer && (brakeF > 0.12 || hbActive) && accelF > 0.08)) && pL > tpReleaseTail && pR > tpReleaseTail

	var wantFlags byte
	var wantTrig [8]byte
	for i := 0; i < 8; i++ {
		wantTrig[i] = 128
	}
	wantRoute := -1
	wantRightNeed := false
	wantPrio := tpOff

	if collisionExclusive {
		wantFlags = 0x98
		wantRoute = -1
		wantRightNeed = false
		wantPrio = tpOff
	} else if bothDriftTrigs {
		wantFlags = 0x9E
		wantTrig = trigRBuf
		wantRoute = 2
		wantRightNeed = true
		wantPrio = tpDynamicEvent
	} else if pL == tpOff && pR == tpOff {
		wantFlags = 0x9C
		wantRoute = -1
		wantRightNeed = false
		wantPrio = tpOff
	} else if pL != tpOff && pR != tpOff {
		if pL > tpReleaseTail && pR > tpReleaseTail {
			conflictSeen = true
		}
		if (cfg.Trigger.SmartArbitration || pL == tpReleaseTail || pR == tpReleaseTail) && pL != pR {
			if pL > pR {
				wantFlags = 0x9C
				wantTrig = trigLBuf
				wantRoute = 0
				wantPrio = pL
			} else {
				wantFlags = 0x9D
				wantTrig = trigRBuf
				wantRoute = 1
				wantRightNeed = true
				wantPrio = pR
			}
		} else if peakDev(trigLBuf) >= peakDev(trigRBuf) {
			wantFlags = 0x9C
			wantTrig = trigLBuf
			wantRoute = 0
			wantPrio = pL
		} else {
			wantFlags = 0x9D
			wantTrig = trigRBuf
			wantRoute = 1
			wantRightNeed = true
			wantPrio = pR
		}
	} else if pL != tpOff {
		wantFlags = 0x9C
		wantTrig = trigLBuf
		wantRoute = 0
		wantPrio = pL
	} else {
		wantFlags = 0x9D
		wantTrig = trigRBuf
		wantRoute = 1
		wantRightNeed = true
		wantPrio = pR
	}

	urgentSwitch := collisionExclusive || shiftTrigActiveFrame ||
		(cfg.Trigger.EnableLeftBrake && (isWheelLock || isABS)) ||
		bothDriftTrigs || (wantPrio == tpDynamicEvent && wantPrio > s.lastTrigPrio) ||
		(s.lastTrigRoute < 0 && wantRoute >= 0) ||
		(s.lastTrigPrio == tpReleaseTail && wantPrio > tpReleaseTail)
	if !urgentSwitch && s.trigRouteHoldS > 0 && s.lastTrigRoute >= 0 && wantRoute != s.lastTrigRoute {
		if s.lastTrigRoute == 0 && (pL != tpOff || (wantRoute >= 0 && wantPrio < tpDynamicEvent)) {
			wantFlags = 0x9C
			wantTrig = trigLBuf
			wantRoute = 0
			wantRightNeed = false
			wantPrio = pL
		} else if s.lastTrigRoute == 1 && (pR != tpOff || (wantRoute >= 0 && wantPrio < tpDynamicEvent)) {
			wantFlags = 0x9D
			wantTrig = trigRBuf
			wantRoute = 1
			wantRightNeed = true
			wantPrio = pR
		}
	}

	if wantRoute != s.lastTrigRoute {
		s.lastTrigRoute = wantRoute
		if wantRoute >= 0 {
			s.trigRouteHoldS = 0.070
		} else {
			s.trigRouteHoldS = 0
		}
	}
	s.lastTrigPrio = wantPrio

	res.Flags = wantFlags
	res.Trig = wantTrig
	res.TrigRoute = wantRoute
	res.RightTrigNeed = wantRightNeed
	res.ConflictSolved = conflictSeen
	res.Frame = hid.BuildWave0x57(res.Flags, res.Trig, res.GripL, res.GripR)
	return res
}

func (s *Synthesizer) stepPebble(g *pebbleGrain, nextMs *float64, rng *uint32, meanGapMs, fMin, fMax, durS, sharp float64) float64 {
	*nextMs -= 1.0
	if *nextMs <= 0 && g.activeS <= 0 {
		g.activeS = 0.0001
		g.durS = durS * (0.78 + 0.42*s.nextRand01(rng))
		g.freqHz = fMin + (fMax-fMin)*s.nextRand01(rng)
		g.amp = 0.68 + 0.32*s.nextRand01(rng)
		g.sharp = sharp
		*nextMs = clamp(meanGapMs*(0.65+0.70*s.nextRand01(rng)), 24.0, 62.0)
	}
	if g.activeS <= 0 {
		return 0
	}
	if g.activeS >= g.durS {
		g.activeS = 0
		return 0
	}
	u := g.activeS / g.durS
	env := math.Sin(math.Pi*u) * math.Exp(-1.35*u)
	ph := 2 * math.Pi * g.freqHz * g.activeS
	out := (math.Sin(ph) + 0.28*g.sharp*math.Sin(2*ph)) * env * g.amp
	g.activeS += 0.001
	return out
}

func (s *Synthesizer) renderTest8ms(cfg NextConfig, car CarPersonality) RenderResult {
	var res RenderResult
	for i := 0; i < 8; i++ {
		res.Trig[i] = 128
		res.GripL[i] = 128
		res.GripR[i] = 128
	}
	res.Flags = 0x9C
	res.TrigRoute = -1
	res.SurfaceLabel = "试震演示: " + s.testEventID
	res.ActiveEvents = []ActiveEvent{{ID: s.testEventID, Label: "试震中", Category: "test", Intensity: 0.95, Channel: "grip"}}

	var sumSqL, sumSqR float64
	for ms := 0; ms < 8; ms++ {
		const dt1 = 0.001
		t := s.testElapsedS
		u := clamp(t/math.Max(s.testTotalS, 0.05), 0, 1)
		env := math.Sin(math.Pi * u)
		gl, gr, tr := 0.0, 0.0, 0.0

		switch s.testEventID {
		case "engineIdle":
			if !car.IsElectric {
				f := car.EngineFreqBaseHz * cfg.Events.EngineIdle.FreqScale
				w := driveWave(2*math.Pi*f*t, 1-car.EngineRoundness, 1.55)
				if car.EngineFamily == "muscle_v8" {
					w = (driveWave(2*math.Pi*f*t, 0.48, 1.65)*0.84 + 0.24*math.Sin(4*math.Pi*f*t)) * (0.78 + 0.22*math.Sin(2*math.Pi*15.5*t))
				} else if car.EngineFamily == "v12_silk" {
					w = math.Sin(2*math.Pi*f*t)*0.82 + math.Sin(4*math.Pi*f*t)*0.20
				}
				out := w * 0.42 * car.EngineBedScale * cfg.Events.EngineIdle.Intensity * env
				gl, gr = out, out
			}
		case "engineRev":
			// True linear chirp integral from EngineFreqBaseHz to EngineFreqTopHz
			fAvg := (car.EngineFreqBaseHz + 0.5*(car.EngineFreqTopHz-car.EngineFreqBaseHz)*u) * cfg.Events.EngineRev.FreqScale
			ph := 2 * math.Pi * fAvg * t
			var w float64
			switch car.EngineFamily {
			case "ev":
				w = math.Sin(ph) * 0.78
			case "muscle_v8":
				w = driveWave(ph, 0.48, 1.65)*0.84 + 0.24*math.Sin(2*ph)
			case "v12_silk":
				w = math.Sin(ph)*0.82 + math.Sin(2*ph)*0.20
			case "rotary_f1":
				w = driveWave(ph, 0.85, 1.55)*0.78 + math.Sin(2*ph)*0.24
			default:
				w = driveWave(ph, 1-car.EngineRoundness, 1.45)
			}
			surge := 1.0
			if car.EngineFamily == "turbo_punch" && u > 0.35 {
				surge = 1.0 + 0.55*((u-0.35)/0.65)
			}
			out := w * (0.26 + 0.34*u) * surge * car.EngineBedScale * cfg.Events.EngineRev.Intensity
			gl, gr = out, out
		case "gearShift":
			sharp := clamp(car.ShiftCrispness*0.6+cfg.Events.GearShift.Sharpness*0.4, 0, 1)
			click := 0.0
			if t < 0.018 {
				click = driveWave(2*math.Pi*(108.0+24.0*sharp)*cfg.Events.GearShift.FreqScale*t, sharp, 1.88) * (1.0 - t/0.018) * 1.48
			}
			tail := clamp((1.0-u)/0.22, 0, 1)
			thud := driveWave(2*math.Pi*(64.0-9.0*u)*cfg.Events.GearShift.FreqScale*t, 0.46, 1.78) * math.Pow(1.0-u, 0.85) * math.Exp(-2.6*u) * tail * 1.50
			w := (click + thud) * cfg.Events.GearShift.Intensity
			gl, gr = w, w
			if cfg.Trigger.EnableRightAccel {
				if t < 0.032 {
					uTrig := t / 0.032
					shiftFreq := clamp(92.0*cfg.Events.GearShift.FreqScale, 68.0, 100.0)
					tr = math.Sin(2*math.Pi*shiftFreq*t) * clamp(t/0.003, 0, 1) * math.Exp(-3.5*uTrig) * 0.85 * cfg.Events.GearShift.Intensity
				}
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9C, false, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		case "redline":
			sharp := cfg.Events.Redline.Sharpness
			fScale := cfg.Events.Redline.FreqScale
			subHz := (46.0 + 10.0*sharp) * fScale
			roarHz := (78.0 + 16.0*sharp) * fScale
			cutHz := (17.0 + 2.0*sharp) * clamp(fScale, 0.85, 1.15)
			_, cyc := math.Modf(t * cutHz)
			burstEnv := 0.0
			popEnv := 0.0
			if cyc < 0.46 {
				uCut := cyc / 0.46
				burstEnv = math.Pow(1.0-uCut*0.50, 1.40)
				if cyc < 0.18 {
					popEnv = 1.0 - cyc/0.18
				}
			}
			phSub := 2 * math.Pi * subHz * t
			phRoar := 2 * math.Pi * roarHz * t
			thump := driveWave(phSub, 0.60+0.35*sharp, 1.95)
			roar := driveWave(phRoar, 0.76+0.24*sharp, 1.95)
			popL := (0.72*driveWave(phRoar*1.42, 0.95, 2.0) + 0.28*s.nextNoise(&s.rngL)) * popEnv * 0.60
			popR := (0.72*driveWave(phRoar*1.42+0.4, 0.95, 2.0) + 0.28*s.nextNoise(&s.rngR)) * popEnv * 0.60
			engBed := driveWave(2*math.Pi*car.EngineFreqTopHz*fScale*t, 1.0-car.EngineRoundness, 1.55) * 0.26 * car.EngineBedScale * (0.12 + 0.88*burstEnv)
			gl = engBed + ((0.62*thump+0.54*roar)*burstEnv+popL)*1.54*cfg.Events.Redline.Intensity
			gr = engBed + ((0.62*thump+0.54*roar)*burstEnv+popR)*1.54*cfg.Events.Redline.Intensity
			// Continue smooth high-RPM engine micro-feel on Right Trigger in V4's clean 52-74Hz band!
			effMicro := clamp(cfg.Trigger.ThrottleMicroFeel, 0.0, 1.0)
			if cfg.Trigger.EnableRightAccel && effMicro > 0.001 {
				trFreq := clamp(74.0*cfg.Events.EngineRev.FreqScale, 42.0, 92.0)
				tr = math.Sin(2*math.Pi*trFreq*t) * (0.20 * effMicro)
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9C, false, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		case "roadUndulation":
			sharp := clamp(car.SuspensionStiffness*0.65+cfg.Events.RoadUndulation.Sharpness*0.35, 0, 1)
			f := (28.0 + 16.0*sharp) * cfg.Events.RoadUndulation.FreqScale
			gl = (math.Sin(2*math.Pi*f*t) + 0.22*sharp*math.Sin(4*math.Pi*f*t)) * (0.45 + 0.55*math.Sin(2*math.Pi*3.2*t)) * 0.60 * cfg.Events.RoadUndulation.Intensity
			gr = (math.Sin(2*math.Pi*f*1.08*t) + 0.22*sharp*math.Sin(4*math.Pi*f*1.08*t)) * (0.45 + 0.55*math.Cos(2*math.Pi*3.2*t)) * 0.60 * cfg.Events.RoadUndulation.Intensity
		case "surfTarmac":
			gl = math.Sin(2*math.Pi*48*t) * 0.055 * cfg.Events.RoadSurface.Intensity * env
			gr = math.Sin(2*math.Pi*51.5*t) * 0.055 * cfg.Events.RoadSurface.Intensity * env
		case "surfDirt", "roadSurface":
			gl = (s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, 38, 66, 104, 0.0085, 0.55)*0.94 + math.Sin(2*math.Pi*30*t)*0.10) * cfg.Events.RoadSurface.Intensity * env
			gr = (s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, 38, 66, 104, 0.0085, 0.55)*0.94 + math.Sin(2*math.Pi*30*t+0.9)*0.10) * cfg.Events.RoadSurface.Intensity * env
		case "surfGravel":
			gl = (s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, 34, 68, 108, 0.010, 0.75)*1.02 + math.Sin(2*math.Pi*28*t)*0.10) * cfg.Events.RoadSurface.Intensity * env
			gr = (s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, 34, 68, 108, 0.010, 0.75)*1.02 + math.Sin(2*math.Pi*29*t+1.1)*0.10) * cfg.Events.RoadSurface.Intensity * env
		case "surfGrass":
			sw := 0.55 + 0.45*math.Sin(2*math.Pi*7.5*t)
			gl = math.Sin(2*math.Pi*24.5*t) * sw * 0.48 * cfg.Events.RoadSurface.Intensity * env
			gr = math.Sin(2*math.Pi*27.5*t) * (1.1 - sw*0.5) * 0.48 * cfg.Events.RoadSurface.Intensity * env
		case "curbLeft", "curbRumble":
			rib := math.Pow(math.Max(0, math.Sin(2*math.Pi*16.5*t)), 2.35)
			toneHz := (70.0 + 26.0*cfg.Events.CurbRumble.Sharpness) * cfg.Events.CurbRumble.FreqScale
			cw := 0.76*driveWave(2*math.Pi*toneHz*t, cfg.Events.CurbRumble.Sharpness*0.85, 1.72) + 0.36*driveWave(2*math.Pi*toneHz*0.76*t, 0.50, 1.55)
			gl = cw * rib * 1.30 * cfg.Events.CurbRumble.Intensity
			gr = 0
		case "curbRight":
			rib := math.Pow(math.Max(0, math.Sin(2*math.Pi*16.5*t)), 2.35)
			toneHz := (70.0 + 26.0*cfg.Events.CurbRumble.Sharpness) * cfg.Events.CurbRumble.FreqScale
			cw := 0.76*driveWave(2*math.Pi*toneHz*t, cfg.Events.CurbRumble.Sharpness*0.85, 1.72) + 0.36*driveWave(2*math.Pi*toneHz*0.76*t, 0.50, 1.55)
			gl = 0
			gr = cw * rib * 1.30 * cfg.Events.CurbRumble.Intensity
		case "jumpLanding":
			slap := 0.0
			if t < 0.020 {
				f := (108.0 + 20.0*cfg.Events.JumpLanding.Sharpness) * cfg.Events.JumpLanding.FreqScale
				slap = driveWave(2*math.Pi*f*t, cfg.Events.JumpLanding.Sharpness, 1.88) * (1 - t/0.020) * 1.40
			}
			tail := clamp((1.0-u)/0.20, 0, 1)
			phLow := 2 * math.Pi * (76.0 - 14.0*u) * cfg.Events.JumpLanding.FreqScale * t
			thump := driveWave(phLow, 0.42, 1.82) * math.Exp(-4.2*u) * tail * 1.50
			rebound := 0.0
			if t >= 0.055 && t <= 0.090 {
				ru := (t - 0.055) / 0.035
				rebound = driveWave(phLow*0.86, 0.42, 1.72) * math.Sin(math.Pi*ru) * tail * 0.78
			}
			w := (slap + thump + rebound) * cfg.Events.JumpLanding.Intensity
			gl, gr = w, w
		case "impactLowSpeed":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 0, "低速正面碰擦"
			tail := clamp((1.0-u)/0.20, 0, 1)
			envBody := math.Pow(1.0-u, 1.35) * math.Exp(-3.4*u) * tail
			ph := 2 * math.Pi * (102.0*t - 14.0*t*u) // exact integral of 102 -> 74Hz dry bumper thud!
			crack := 0.0
			if t < 0.020 {
				crack = driveWave(2*math.Pi*132*t, 0.82, 1.95) * (1 - t/0.020) * 1.50
			}
			bodyWave := 0.82*driveWave(ph, 0.58, 1.90) + 0.28*math.Sin(ph*0.68)
			w := (crack + bodyWave*envBody*1.92) * cfg.Events.CollisionWall.Intensity
			gl, gr = w, w
			res.Flags = 0x98
		case "collisionWall", "impactFront":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 0, "正面重撞"
			tail := clamp((1.0-u)/0.20, 0, 1)
			envBody := math.Pow(1.0-u, 1.35) * math.Exp(-3.4*u) * tail
			ph := 2 * math.Pi * (88.0*t - 13.0*t*u) // exact integral of 88 -> 62Hz tight chassis slam!
			crack := 0.0
			if t < 0.024 {
				crack = (0.76*driveWave(2*math.Pi*132*t, cfg.Events.CollisionWall.Sharpness, 1.98) + 0.24*s.nextNoise(&s.rngL)) * (1 - t/0.024) * 1.85
			}
			rebound := 0.0
			if t >= 0.052 && t <= 0.088 {
				ru := (t - 0.052) / 0.036
				rebound = driveWave(ph*0.85, 0.48, 1.68) * math.Sin(math.Pi*ru) * tail * 0.84
			}
			bodyWave := 0.80*driveWave(ph, 0.58, 1.92) + 0.30*math.Sin(ph*0.68)
			w := (crack + bodyWave*envBody*2.10 + rebound) * cfg.Events.CollisionWall.Intensity
			gl, gr = w, w
			res.Flags = 0x98
		case "impactRear":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 180, "后方追尾撞击"
			tail := clamp((1.0-u)/0.20, 0, 1)
			envBody := math.Pow(1.0-u, 1.35) * math.Exp(-3.4*u) * tail
			ph := 2 * math.Pi * (64.0*t + 14.0*t*u) // exact integral of 64 -> 92Hz rising rear shove!
			bodyWave := 0.80*driveWave(ph, 0.52, 1.90) + 0.30*math.Sin(ph*0.68)
			shove := bodyWave * envBody * 2.10 * cfg.Events.CollisionWall.Intensity
			gl, gr = shove, shove
			res.Flags = 0x98
		case "impactLeft":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 90, "左侧撞击"
			tail := clamp((1.0-u)/0.20, 0, 1)
			envBody := math.Pow(1.0-u, 1.35) * math.Exp(-3.4*u) * tail
			ph := 2 * math.Pi * (88.0*t - 13.0*t*u)
			crack := 0.0
			if t < 0.024 {
				crack = (0.76*driveWave(2*math.Pi*132*t, cfg.Events.CollisionWall.Sharpness, 1.98) + 0.24*s.nextNoise(&s.rngL)) * (1 - t/0.024) * 1.85
			}
			bodyWave := 0.80*driveWave(ph, 0.60, 1.92) + 0.30*math.Sin(ph*0.68)
			gl = (crack + bodyWave*envBody*2.10) * cfg.Events.CollisionWall.Intensity
			gr = 0
			res.Flags = 0x98
		case "impactRight":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, -90, "右侧撞击"
			tail := clamp((1.0-u)/0.20, 0, 1)
			envBody := math.Pow(1.0-u, 1.35) * math.Exp(-3.4*u) * tail
			ph := 2 * math.Pi * (88.0*t - 13.0*t*u)
			crack := 0.0
			if t < 0.024 {
				crack = (0.76*driveWave(2*math.Pi*132*t, cfg.Events.CollisionWall.Sharpness, 1.98) + 0.24*s.nextNoise(&s.rngR)) * (1 - t/0.024) * 1.85
			}
			bodyWave := 0.80*driveWave(ph, 0.60, 1.92) + 0.30*math.Sin(ph*0.68)
			gl = 0
			gr = (crack + bodyWave*envBody*2.10) * cfg.Events.CollisionWall.Intensity
			res.Flags = 0x98
		case "guardrail", "guardrailLeft":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 90, "左侧护栏刮擦"
			mod := 0.78 + 0.22*math.Sin(2*math.Pi*34.0*t)
			gl = (0.45*driveWave(2*math.Pi*112*t, cfg.Events.Guardrail.Sharpness, 1.95) + 0.35*driveWave(2*math.Pi*76*t, 0.55, 1.80) + 0.28*s.nextNoise(&s.rngL)) * mod * 1.55 * cfg.Events.Guardrail.Intensity * env
			gr = 0
		case "guardrailRight":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, -90, "右侧护栏刮擦"
			mod := 0.78 + 0.22*math.Sin(2*math.Pi*34.0*t)
			gl = 0
			gr = (0.45*driveWave(2*math.Pi*112*t, cfg.Events.Guardrail.Sharpness, 1.95) + 0.35*driveWave(2*math.Pi*76*t, 0.55, 1.80) + 0.28*s.nextNoise(&s.rngR)) * mod * 1.55 * cfg.Events.Guardrail.Intensity * env
		case "waterSplash":
			swL := 0.45 + 0.55*math.Sin(2*math.Pi*7.5*t)
			swR := 0.45 + 0.55*math.Sin(2*math.Pi*7.5*t+1.1)
			phSp := 2 * math.Pi * (96.0 - 22.0*u) * cfg.Events.WaterSplash.FreqScale * t
			sp := (0.78*driveWave(phSp, 0.58, 1.80) + 0.22*s.nextNoise(&s.rngL)) * math.Exp(-3.2*u) * 1.48
			dragL := (0.80*driveWave(2*math.Pi*42*t, 0.35, 1.42) + 0.20*s.nextNoise(&s.rngL)) * swL * 0.78 * env
			dragR := (0.80*driveWave(2*math.Pi*42*t+0.9, 0.35, 1.42) + 0.20*s.nextNoise(&s.rngR)) * swR * 0.78 * env
			gl = (sp + dragL) * cfg.Events.WaterSplash.Intensity
			gr = (sp*0.88 + dragR) * cfg.Events.WaterSplash.Intensity
		case "understeer", "understeerLeft":
			hopGrip := 0.06 + 0.94*math.Pow(math.Max(0, math.Sin(2*math.Pi*18.0*t)), 1.35)
			hopTrig := 0.32 + 0.68*math.Max(0, math.Sin(2*math.Pi*18.0*t))
			w := driveWave(2*math.Pi*96.0*t, cfg.Events.Understeer.Sharpness*0.80, 1.55) * hopGrip * 1.12 * cfg.Events.Understeer.Intensity * env
			gl, gr = w, w*0.12
			if cfg.Trigger.EnableLeftBrake {
				tr = math.Sin(2*math.Pi*92.0*t) * hopTrig * 0.85 * cfg.Events.Understeer.Intensity * env
				res.Flags, res.TrigRoute = 0x9C, 0
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.TrigRoute = 0x9C, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		case "understeerRight":
			hopGrip := 0.06 + 0.94*math.Pow(math.Max(0, math.Sin(2*math.Pi*18.0*t)), 1.35)
			hopTrig := 0.32 + 0.68*math.Max(0, math.Sin(2*math.Pi*18.0*t))
			w := driveWave(2*math.Pi*96.0*t, cfg.Events.Understeer.Sharpness*0.80, 1.55) * hopGrip * 1.12 * cfg.Events.Understeer.Intensity * env
			gl, gr = w*0.12, w
			if cfg.Trigger.EnableRightAccel {
				tr = math.Sin(2*math.Pi*96.0*t) * hopTrig * 0.85 * cfg.Events.Understeer.Intensity * env
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9C, false, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		case "oversteerDrift", "oversteerLeft", "tireSlipDrift":
			swell := 0.76 + 0.24*math.Sin(2*math.Pi*7.5*t)
			w := (0.84*math.Sin(2*math.Pi*40.0*t) + 0.28*math.Sin(4*math.Pi*40.0*t)) * swell * 1.14 * cfg.Events.OversteerDrift.Intensity * env
			gl, gr = w, w*0.36
			if cfg.Trigger.EnableRightAccel {
				tr = math.Sin(2*math.Pi*86.0*t) * 0.84 * cfg.Events.OversteerDrift.Intensity * env
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9C, false, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		case "oversteerRight":
			swell := 0.76 + 0.24*math.Sin(2*math.Pi*7.5*t)
			w := (0.84*math.Sin(2*math.Pi*40.0*t) + 0.28*math.Sin(4*math.Pi*40.0*t)) * swell * 1.14 * cfg.Events.OversteerDrift.Intensity * env
			gl, gr = w*0.36, w
			if cfg.Trigger.EnableRightAccel {
				tr = math.Sin(2*math.Pi*90.0*t) * 0.84 * cfg.Events.OversteerDrift.Intensity * env
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9C, false, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		case "brakingAbs":
			// V4 开心散仙竞速2 (kaixin_race2_clear_lock): Two-Stage Braking Demo
			// Stage 1 (u < 0.24): 90Hz progressive Left Trigger warning + restrained 70Hz (40..55% curve) grip tension
			// Stage 2 (u >= 0.24): 100Hz full-clarity Left Trigger lock/ABS carrier + 10Hz ABS gate + 50%-scaled 70Hz/50Hz dry grip body pulse
			fScale := cfg.Events.BrakingABS.FreqScale
			if u < 0.24 {
				wProg := u / 0.24
				warnCurve := 0.40 + 0.15*wProg
				w := math.Sin(2*math.Pi*(70.0*fScale)*t) * warnCurve * 0.36 * cfg.Events.BrakingABS.Intensity
				gl, gr = w, w
				if cfg.Trigger.EnableLeftBrake {
					warnFreq := clamp(90.0*fScale, 62.0, 96.0)
					tr = math.Sin(2*math.Pi*warnFreq*t) * (0.18 + 0.24*wProg) * cfg.Events.BrakingABS.Intensity
				}
			} else {
				_, cyc := math.Modf((t - 0.24*s.testTotalS) * (10.0 * fScale))
				pulseGrip := 0.0
				pulseTrig := 0.24
				if cyc < 0.56 {
					uAbs := cyc / 0.56
					pulseGrip = math.Sin(math.Pi*uAbs) * math.Exp(-0.85*uAbs) * 1.18
				}
				if cyc < 0.62 {
					uAbs := cyc / 0.62
					pulseTrig = 0.24 + 0.76*math.Pow(math.Sin(math.Pi*uAbs), 0.75)
				}
				phG := 2 * math.Pi * (70.0 * fScale) * t
				w := (0.80*driveWave(phG, 0.38, 1.35) + 0.24*math.Sin(phG*0.71)) * pulseGrip * 0.65 * cfg.Events.BrakingABS.Intensity
				gl, gr = w, w
				if cfg.Trigger.EnableLeftBrake {
					absFreq := clamp(100.0*fScale, 68.0, 100.0)
					tr = math.Sin(2*math.Pi*absFreq*t) * pulseTrig * 0.94 * cfg.Events.BrakingABS.Intensity
				}
			}
			if cfg.Trigger.EnableLeftBrake {
				res.Flags, res.TrigRoute = 0x9C, 0
				res.ActiveEvents[0].Channel = "both"
			} else {
				tr = 0
				res.Flags, res.TrigRoute = 0x9C, -1
				res.ActiveEvents[0].Channel = "grip"
			}
		}

		bL := byte(clamp(math.Round(128.0+math.Tanh(gl*cfg.MasterGripGain*0.92)*124.0), 2, 253))
		bR := byte(clamp(math.Round(128.0+math.Tanh(gr*cfg.MasterGripGain*0.92)*124.0), 2, 253))
		res.GripL[ms] = bL
		res.GripR[ms] = bR
		res.Trig[ms] = byte(clamp(math.Round(128.0+math.Tanh(tr*cfg.MasterTriggerGain*0.92)*64.0), 60, 196))
		dL := float64(bL) - 128.0
		dR := float64(bR) - 128.0
		sumSqL += dL * dL
		sumSqR += dR * dR

		s.testElapsedS += dt1
		s.testRemainS -= dt1
	}

	res.GripLRms = math.Sqrt(sumSqL / 8.0)
	res.GripRRms = math.Sqrt(sumSqR / 8.0)
	res.Frame = hid.BuildWave0x57(res.Flags, res.Trig, res.GripL, res.GripR)
	return res
}

func contrastLR(l, r, gamma float64) (float64, float64) {
	m := math.Max(l, r)
	if m < 0.05 {
		return 0.7, 0.7
	}
	wl := math.Pow(clamp(l/m, 0, 1), gamma)
	wr := math.Pow(clamp(r/m, 0, 1), gamma)
	return clamp(wl, 0.08, 1.0), clamp(wr, 0.08, 1.0)
}

func driveWave(phase, sharpness, fullness float64) float64 {
	s := clamp(sharpness, 0, 1)
	raw := math.Sin(phase)
	if s > 0.04 {
		raw += 0.34*s*math.Sin(2*phase) + 0.22*s*math.Sin(3*phase)
	}
	return math.Tanh(raw * fullness * (1.0 + 0.45*s))
}

func mixStereo(l, r, sep float64) (float64, float64) {
	avg := 0.5 * (l + r)
	s := clamp(sep, 0, 1)
	return avg + s*(l-avg), avg + s*(r-avg)
}

func wrapPhase(p *float64) {
	const twoPi = 2 * math.Pi
	if math.IsNaN(*p) || math.IsInf(*p, 0) {
		*p = 0
		return
	}
	if *p >= twoPi || *p < 0 {
		*p -= twoPi * math.Floor(*p/twoPi)
	}
}

func max4f(a [4]float32) float64 {
	m := float64(a[0])
	for i := 1; i < 4; i++ {
		if float64(a[i]) > m {
			m = float64(a[i])
		}
	}
	return m
}

func max4f64(a [4]float64) float64 {
	m := a[0]
	for i := 1; i < 4; i++ {
		if a[i] > m {
			m = a[i]
		}
	}
	return m
}

func avg4f(a [4]float32) float64 {
	return float64(a[0]+a[1]+a[2]+a[3]) * 0.25
}

func peakDev(b [8]byte) int {
	m := 0
	for _, v := range b {
		d := int(v) - 128
		if d < 0 {
			d = -d
		}
		if d > m {
			m = d
		}
	}
	return m
}
