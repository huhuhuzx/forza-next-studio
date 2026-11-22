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
	tpStaticResistance trigPriority = 1
	tpEdgeWarning      trigPriority = 2
	tpDynamicEvent     trigPriority = 3
)

type pebbleGrain struct {
	activeS float64
	durS    float64
	freqHz  float64
	amp     float64
	sharp   float64
}

// Synthesizer v3:
// - 4-Tire physical understeer/oversteer routing to L/R Grips AND L/R Triggers
// - Two-Tier 360° Directional Collisions (strong low-speed bumps + razor-sharp 100% vs 0% L/R isolation)
// - High-sensitivity Directional Guardrail Scrape (opposing-steer + micro-jerk, 100% vs 0% L/R isolation)
// - 7 Distinct Engine Families with massive frequency (17..148Hz), amplitude (0.36x..1.55x) & rhythm contrast
type Synthesizer struct {
	phEngBase    float64
	phEngSub     float64
	phEngLope    float64
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
	phRedGate    float64
	phTrigL      float64
	phTrigR      float64
	phAbsPulse   float64
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

	curbEnvL float64
	curbEnvR float64

	airborne      bool
	airTimeS      float64
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
	emaAx          float64
	emaAz          float64
	prevAx         float64
	prevAz         float64
	spdWin         [6]float64
	spdIdx         int
	spdCount       int
	latWin         [10]float64
	latIdx         int
	latCount       int
	impactActiveS  float64
	impactDurS     float64
	impactPeak     float64
	impactAngleDeg float64
	impactLabel    string
	impactKind     string
	impactLeftW    float64
	impactRightW   float64
	impactMuteL    bool
	impactMuteR    bool
	impactDelayL   float64
	impactDelayR   float64
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

	duckGain       float64
	collisionRampL float64
	collisionRampR float64
	hasPrevPkt     bool
	hasWheelRot    bool

	testEventID  string
	testRemainS  float64
	testElapsedS float64
	testTotalS   float64
}

func NewSynthesizer() *Synthesizer {
	return &Synthesizer{
		rngL:             0x13579BDF,
		rngR:             0x2468ACE1,
		duckGain:         1.0,
		collisionRampL:   1.0,
		collisionRampR:   1.0,
		lastImpactSinceS: 10.0,
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

func (s *Synthesizer) TriggerTest(eventID string) {
	s.testEventID = eventID
	s.testElapsedS = 0
	switch eventID {
	case "gearShift":
		s.testTotalS = 0.28
	case "jumpLanding":
		s.testTotalS = 0.32
	case "impactLowSpeed":
		s.testTotalS = 0.13
	case "collisionWall", "impactFront", "impactRear", "impactLeft", "impactRight":
		s.testTotalS = 0.18
	case "guardrail", "guardrailLeft", "guardrailRight":
		s.testTotalS = 0.65
	case "waterSplash":
		s.testTotalS = 0.95
	case "curbLeft", "curbRight", "curbRumble":
		s.testTotalS = 0.80
	default:
		s.testTotalS = 0.95
	}
	s.testRemainS = s.testTotalS
}

func (s *Synthesizer) StepPacket(pkt telemetry.Packet, cfg NextConfig, car CarPersonality, dtS float64) {
	if !pkt.IsRaceOn {
		s.suspInit = false
		s.airborne = false
		s.airTimeS = 0
		s.hasPrevPkt = false
		s.impactActiveS = 0
		s.guardActiveS = 0
		s.smashActiveS = 0
		s.landActiveS = 0
		s.shiftActiveS = 0
		s.splashActiveS = 0
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

	// 1. True High-Pass Suspension Velocity + Body Heave
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

	decay := math.Exp(-dt / (car.SuspensionReboundMs * 0.001))
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
		s.heaveEnv *= math.Exp(-dt / 0.13)
	}

	// 2. Curb / Rumble Strip Detector
	leftNorm := 0.5 * float64(pkt.NormSusp[0]+pkt.NormSusp[2])
	rightNorm := 0.5 * float64(pkt.NormSusp[1]+pkt.NormSusp[3])
	asym := leftNorm - rightNorm
	leftStep := math.Max(stepJump[0], stepJump[2])
	rightStep := math.Max(stepJump[1], stepJump[3])
	leftTelemCurb := pkt.RumbleStrip[0] != 0 || pkt.RumbleStrip[2] != 0
	rightTelemCurb := pkt.RumbleStrip[1] != 0 || pkt.RumbleStrip[3] != 0

	curbHitL, curbHitR := 0.0, 0.0
	if speedGate > 0.15 {
		if leftTelemCurb {
			curbHitL = 0.95
		} else if (leftStep > 0.010 && math.Abs(asym) > 0.09) || (asym > 0.14 && rawL > 0.24) {
			curbHitL = clamp((leftStep/0.022)*0.7+math.Abs(asym)*1.9, 0.42, 1.0)
		}
		if rightTelemCurb {
			curbHitR = 0.95
		} else if (rightStep > 0.010 && math.Abs(asym) > 0.09) || (asym < -0.14 && rawR > 0.24) {
			curbHitR = clamp((rightStep/0.022)*0.7+math.Abs(asym)*1.9, 0.42, 1.0)
		}
	}
	if curbHitL > s.curbEnvL {
		s.curbEnvL = curbHitL
	} else {
		s.curbEnvL *= math.Exp(-dt / 0.11)
	}
	if curbHitR > s.curbEnvR {
		s.curbEnvR = curbHitR
	} else {
		s.curbEnvR *= math.Exp(-dt / 0.11)
	}

	// 3. Two-Stage Jump Landing Engine
	maxNorm := max4f(pkt.NormSusp)
	if maxNorm < 0.05 && pkt.SpeedKmh > 14 {
		s.airborne = true
		s.airTimeS = math.Min(s.airTimeS+dt, 2.5)
	} else if s.airborne {
		if s.airTimeS >= 0.050 && (maxNorm >= 0.34 || math.Abs(float64(pkt.AccelY)) >= 4.0 || max4f64(stepJump) >= 0.022) {
			airFac := clamp((s.airTimeS-0.040)/0.48, 0.35, 1.0)
			compFac := clamp((maxNorm-0.28)/0.65, 0.30, 1.0)
			vertFac := clamp(math.Abs(float64(pkt.AccelY))/20.0, 0.20, 1.0)
			peak := clamp((0.45*airFac+0.40*compFac+0.15*vertFac)*car.LandingSensitivity, 0.42, 1.0)
			s.landPeak = peak
			s.landReboundMs = car.SuspensionReboundMs
			s.landDurS = 0.10 + 0.12*peak + 0.05*(1.0-car.SuspensionStiffness)
			s.landActiveS = 0.0001
			lComp := 0.5 * float64(pkt.NormSusp[0]+pkt.NormSusp[2])
			rComp := 0.5 * float64(pkt.NormSusp[1]+pkt.NormSusp[3])
			mComp := math.Max(math.Max(lComp, rComp), 0.10)
			s.landLeftW = clamp(lComp/mComp, 0.52, 1.0)
			s.landRightW = clamp(rComp/mComp, 0.52, 1.0)
		}
		s.airborne = false
		s.airTimeS = 0
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

	isHeavySide := sideOnset && math.Abs(shockX) >= 14.0 && (dAx >= 5.5 || math.Abs(axNow) >= 24.0) && (maxRecentSpd >= 6.0 || dAx >= 12.0)
	isHeavyFront := false
	if frontOnset {
		if hardBraking {
			isHeavyFront = (shockZ <= -19.5 && maxRecentSpd >= 10.0) ||
				(dvWin >= 15.0 && shockZ <= -13.5 && maxRecentSpd >= 12.0) ||
				(azNow <= -26.0 && dvWin >= 4.5)
		} else {
			isHeavyFront = (shockZ <= -13.0 && maxRecentSpd >= 8.0) ||
				(dvWin >= 9.5 && shockZ <= -8.5 && maxRecentSpd >= 10.0) ||
				(azNow <= -22.0 && dvWin >= 3.5)
		}
	}
	isHeavyRear := rearOnset && azNow >= 11.5 && shockZ >= 12.0 && pkt.Accel < 190
	isHeavyVec := (frontOnset || rearOnset || sideOnset) && shockMag >= 18.5 && (dAx >= 5.5 || dAz >= 5.5) && maxRecentSpd >= 8.0 && (!hardBraking || math.Abs(shockX) >= 11.0 || shockMag >= 23.0)

	// Tier A: LOW-SPEED BUMP / FENDER BENDER (3..80 km/h, plus stationary T-bone/rear-end — crisp, short, onset-only!)
	isLowFront := frontOnset && maxRecentSpd >= 3.0 && curSpd <= 70.0 &&
		((dvWin >= 3.0 && shockZ <= -4.8 && pkt.Brake < 140) ||
			(dvWin >= 4.5 && shockZ <= -7.5 && dAz >= 5.2) ||
			(dvWin >= 3.2 && curSpd < 3.5 && shockZ <= -4.5))
	isLowSide := sideOnset && ((maxRecentSpd >= 3.0 && curSpd <= 80.0 && math.Abs(shockX) >= 6.5 && dAx >= 5.2) ||
		(curSpd <= 25.0 && math.Abs(shockX) >= 8.5 && dAx >= 7.0))
	isLowRear := rearOnset && azNow >= 7.5 && shockZ >= 6.5 && pkt.Accel < 160 && curSpd <= 75.0

	isHeavy := isHeavySide || isHeavyFront || isHeavyRear || isHeavyVec
	isLow := !isHeavy && (isLowFront || isLowSide || isLowRear)

	if s.impactCoolS <= 0 && (isHeavy || isLow) {
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
			sevShock := clamp((shockMag-10.0)/30.0, 0.0, 1.0)
			sevDv := clamp((dvWin-4.0)/34.0, 0.0, 1.0)
			sev = clamp(math.Max(sevShock, 0.65*sevShock+0.55*sevDv), 0.72, 1.0)
		} else {
			sevLow := clamp((shockMag-4.0)/11.0*0.65+(dvWin-2.5)/9.0*0.35, 0.0, 1.0)
			sev = clamp(0.68+0.24*sevLow, 0.68, 0.92)
		}
		// Flush speed window to current speed so the same crash deceleration never re-triggers after cooldown!
		for i := range s.spdWin {
			s.spdWin[i] = curSpd
		}
		s.emaAx = clamp(axNow, -12.0, 12.0)
		s.emaAz = clamp(azNow, -12.0, 12.0)
		s.triggerDirectionalImpact(angleDeg, sev, math.Abs(float64(pkt.AngVelY)), isLow)
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
		massFac := clamp(math.Log1p(float64(pkt.SmashableMass))/4.8, 0.35, 1.0)
		velFac := clamp(float64(pkt.SmashableVelDiff)/16.0, 0.35, 1.0)
		s.smashPeak = clamp(0.55*velFac+0.45*massFac, 0.48, 1.0)
		s.smashDurS = 0.080 + 0.055*s.smashPeak
		s.smashActiveS = 0.0001
	}
	s.prevSmashDiff = pkt.SmashableVelDiff

	// 8. Water Entry Splash
	curMaxPuddle := max4f(pkt.PuddleDepth)
	if curMaxPuddle >= 0.035 && s.prevMaxPuddle < 0.022 && curSpd >= 10 {
		spdFac := clamp(curSpd/105.0, 0.45, 1.0)
		depFac := clamp(curMaxPuddle/0.40, 0.45, 1.0)
		s.splashPeak = clamp(0.55*spdFac+0.45*depFac, 0.50, 1.0)
		s.splashDurS = 0.20 + 0.14*s.splashPeak
		s.splashActiveS = 0.0001
		lp := math.Max(float64(pkt.PuddleDepth[0]), float64(pkt.PuddleDepth[2]))
		rp := math.Max(float64(pkt.PuddleDepth[1]), float64(pkt.PuddleDepth[3]))
		mp := math.Max(math.Max(lp, rp), 0.05)
		s.splashLeftW = clamp(lp/mp, 0.40, 1.0)
		s.splashRightW = clamp(rp/mp, 0.40, 1.0)
	}
	s.prevMaxPuddle = curMaxPuddle
}

func (s *Synthesizer) triggerDirectionalImpact(angleDeg, sev, yawKick float64, lowSpeed bool) {
	s.impactAngleDeg = angleDeg
	s.impactPeak = sev
	s.impactLowSpeed = lowSpeed
	if lowSpeed {
		// Tight, crisp 85..120ms bumper thump (zero dragging tail!)
		s.impactDurS = 0.085 + 0.035*sev
		s.impactCoolS = 0.34
	} else {
		// Tight, V4-grade 125..190ms solid crash punch + 420ms refractory guard (zero bounce re-triggers!)
		s.impactDurS = 0.125 + 0.065*sev
		s.impactCoolS = 0.42
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
	isRedline := !car.IsElectric && cfg.Events.Redline.Enabled && rpmRatio >= car.RedlineRatio && pkt.Gear >= 1 && spd > 12

	isLaunchSpin := cfg.Events.OversteerDrift.Enabled && spd < 46 && pkt.Accel >= 180 && drivenSlip >= 0.62
	isOversteer := cfg.Events.OversteerDrift.Enabled && !isLaunchSpin && spd >= 18 &&
		((rawRearSlip >= 0.58 && rawRearSlip > rawFrontSlip*1.06+0.10) || rearLatSlip >= 0.38 || (hbActive && spd > 15))
	isUndersteer := cfg.Events.Understeer.Enabled && !isOversteer && spd >= 20 &&
		(math.Abs(steerF) > 0.14 || math.Abs(float64(pkt.AccelX)) > 3.2 || math.Max(slipFL, slipFR) > 0.72) &&
		rawFrontSlip >= 0.55 && rawFrontSlip > rawRearSlip*1.05+0.08
	isFourWheelDrift := cfg.Events.OversteerDrift.Enabled && !isLaunchSpin && !isOversteer && !isUndersteer &&
		spd >= 22 && rawFrontSlip >= 0.75 && rawRearSlip >= 0.75

	minRot := math.Min(math.Min(math.Abs(float64(pkt.WheelRotSpeed[0])), math.Abs(float64(pkt.WheelRotSpeed[1]))),
		math.Min(math.Abs(float64(pkt.WheelRotSpeed[2])), math.Abs(float64(pkt.WheelRotSpeed[3]))))
	maxNegSlip := math.Max(
		math.Max(math.Max(0, -float64(pkt.SlipRatio[0])), math.Max(0, -float64(pkt.SlipRatio[1]))),
		math.Max(math.Max(0, -float64(pkt.SlipRatio[2])), math.Max(0, -float64(pkt.SlipRatio[3]))),
	)
	isWheelLock := cfg.Events.BrakingABS.Enabled && pkt.Brake >= 75 && spd > 12 &&
		((s.hasWheelRot && minRot < 0.85) || (maxNegSlip >= 0.78 && pkt.Brake > 135) || (rawFrontSlip > 1.45 && pkt.Brake >= 175 && !isUndersteer))
	isABS := cfg.Events.BrakingABS.Enabled && !isWheelLock && pkt.Brake >= 65 && spd > 12 &&
		(maxNegSlip >= 0.45 || (!isUndersteer && maxSlip >= 0.80) || (pkt.Brake >= 150 && maxSlip >= 0.88))

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
		engAmp = 0.54 * cfg.Events.EngineIdle.Intensity * car.EngineBedScale
		events = append(events, ActiveEvent{ID: "engineIdle", Label: "怠速呼吸", Category: "engine", Intensity: clamp(engAmp, 0, 1), Channel: "grip"})
	} else if cfg.Events.EngineRev.Enabled && (pkt.CurrentRpm > 200 || car.IsElectric) {
		rpmCurve := 0.25 + 0.52*math.Pow(clamp(rpmRatio, 0, 1), 1.20) + 0.25*accelF
		if car.IsElectric {
			rpmCurve = 0.12 + 0.55*accelF + 0.25*clamp(spd/240.0, 0, 1)
		}
		engAmp = 0.45 * rpmCurve * boostExp * cfg.Events.EngineRev.Intensity * car.EngineBedScale
		if engAmp > 0.05 {
			events = append(events, ActiveEvent{ID: "engineRev", Label: "引擎拉转", Category: "engine", Intensity: clamp(engAmp*1.3, 0, 1), Channel: "grip"})
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
		events = append(events, ActiveEvent{ID: "redline", Label: "红线换挡提醒", Category: "engine", Intensity: clamp((rpmRatio-car.RedlineRatio)/0.07+0.6, 0.55, 1), Channel: "both"})
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
		events = append(events, ActiveEvent{ID: "brakingAbs", Label: "重刹轮胎抱死", Category: "tire", Intensity: 0.95, Channel: "trig_l"})
	} else if isABS {
		events = append(events, ActiveEvent{ID: "brakingAbs", Label: "ABS 防抱死脉动", Category: "tire", Intensity: 0.82, Channel: "trig_l"})
	}
	res.ActiveEvents = events

	var trigLBuf, trigRBuf [8]byte
	var pL, pR trigPriority
	var conflictSeen bool
	var sumSqL, sumSqR float64

	collisionExclusive := s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled && !s.impactLowSpeed && s.impactPeak >= 0.65

	for ms := 0; ms < 8; ms++ {
		const dt1 = 0.001
		lowL, lowR := 0.0, 0.0
		midL, midR := 0.0, 0.0
		highL, highR := 0.0, 0.0
		transientEnv := 0.0

		impactLive := s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled
		guardLive := s.guardActiveS > 0 && cfg.Events.Guardrail.Enabled
		isCenterImpact := impactLive && (s.impactKind == "front" || s.impactKind == "rear")
		muteFarL := (impactLive && s.impactMuteL && s.impactActiveS < s.impactDelayL) || (guardLive && !isCenterImpact && s.guardSide > 0)
		muteFarR := (impactLive && s.impactMuteR && s.impactActiveS < s.impactDelayR) || (guardLive && !isCenterImpact && s.guardSide < 0)

		// 1) 7-FAMILY ENGINE SYNTHESIS
		if engAmp > 0.005 {
			s.phEngBase += 2 * math.Pi * engFreq * dt1
			s.phEngSub += 2 * math.Pi * (engFreq * 0.5) * dt1
			wrapPhase(&s.phEngBase)
			wrapPhase(&s.phEngSub)

			lopeHz := 3.1 + 3.2*(1.0-car.EngineRoundness)
			s.phEngLope += 2 * math.Pi * lopeHz * dt1
			wrapPhase(&s.phEngLope)

			sharp := engSharp
			if isIdle {
				sharp = idleSharp
			}

			var w float64
			switch car.EngineFamily {
			case "ev":
				w = math.Sin(s.phEngBase) * 0.85
			case "muscle_v8":
				w = driveWave(s.phEngBase, 0.45, 2.0)*0.82 + math.Sin(s.phEngSub)*0.58
				if isIdle {
					chop := 0.22 + 1.05*math.Pow(0.5+0.5*math.Sin(s.phEngLope), 1.8)
					w *= chop
				} else if rpmRatio < 0.70 {
					w *= (0.82 + 0.38*math.Sin(s.phEngLope*2.2))
				}
			case "v12_silk":
				w = math.Sin(s.phEngBase)*0.82 + math.Sin(2*s.phEngBase)*0.22
			case "rotary_f1":
				w = driveWave(s.phEngBase, 0.92, 1.75)*0.78 + math.Sin(2*s.phEngBase)*0.28
			case "flat_gt":
				w = driveWave(s.phEngBase, clamp(sharp+0.25, 0, 1), 1.80)
			case "small_na":
				w = math.Sin(s.phEngBase)*0.85 + 0.15*math.Sin(2*s.phEngBase)
			default: // turbo_punch
				w = driveWave(s.phEngBase, sharp, 1.65)
				if isIdle && car.EngineBurble > 0.15 {
					w *= (0.72 + 0.45*car.EngineBurble*(0.5+0.5*math.Sin(s.phEngLope)))
				}
			}

			engOut := w * engAmp * cfg.AntiFatigueClamp
			if car.EngineFamily == "ev" || car.EngineFamily == "rotary_f1" {
				midL += engOut * 0.86
				midR += engOut * 0.86
			} else {
				lowL += engOut
				lowR += engOut
			}
		}

		// 2) Road Undulation & Suspension Direct-Drive
		if cfg.Events.RoadUndulation.Enabled && (s.roadEnvL > 0.015 || s.roadEnvR > 0.015 || s.heaveEnv > 0.025) {
			uFreq := (32.0 + 34.0*suspSharp) * cfg.Events.RoadUndulation.FreqScale
			s.phRoadL += 2 * math.Pi * uFreq * dt1
			s.phRoadR += 2 * math.Pi * (uFreq * 1.07) * dt1
			s.phHeave += 2 * math.Pi * ((22.0 + 12.0*suspSharp) * cfg.Events.RoadUndulation.FreqScale) * dt1
			wrapPhase(&s.phRoadL)
			wrapPhase(&s.phRoadR)
			wrapPhase(&s.phHeave)

			eL, eR := mixStereo(s.roadEnvL, s.roadEnvR, cfg.StereoSeparation)
			gain := 0.88 * cfg.Events.RoadUndulation.Intensity
			midL += driveWave(s.phRoadL, suspSharp, 1.45) * eL * gain
			midR += driveWave(s.phRoadR, suspSharp, 1.45) * eR * gain
			heaveW := math.Sin(s.phHeave) * s.heaveEnv * 0.58 * cfg.Events.RoadUndulation.Intensity
			lowL += heaveW
			lowR += heaveW
		}

		// 3) 6 Distinct Road Surfaces
		if cfg.Events.RoadSurface.Enabled && spd > 4.0 && surfaceKind != "water" {
			sg := clamp((spd-4.0)/22.0, 0, 1)
			surfGain := cfg.Events.RoadSurface.Intensity * car.DirtSensitivity
			surfSharp := clamp(cfg.Events.RoadSurface.Sharpness*0.65+car.SuspensionStiffness*0.35, 0, 1)

			switch surfaceKind {
			case "dirt":
				lInt, rInt := mixStereo(clamp(leftRumble*4.2, 0.35, 0.95)*sg, clamp(rightRumble*4.2, 0.35, 0.95)*sg, cfg.StereoSeparation)
				pebRateMs := clamp(48.0-spd*0.26, 12.0, 44.0)
				midL += s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, pebRateMs, 52.0, 74.0, 0.011, surfSharp*0.7) * lInt * 0.95 * surfGain
				midR += s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, pebRateMs, 52.0, 74.0, 0.011, surfSharp*0.7) * rInt * 0.95 * surfGain
				lowL += math.Sin(s.phHeave*1.25) * lInt * 0.36 * surfGain
				lowR += math.Sin(s.phHeave*1.25+0.9) * rInt * 0.36 * surfGain
			case "gravel":
				lInt, rInt := mixStereo(clamp(leftRumble*2.6, 0.55, 1.0)*sg, clamp(rightRumble*2.6, 0.55, 1.0)*sg, cfg.StereoSeparation)
				stoneRateMs := clamp(38.0-spd*0.20, 10.0, 34.0)
				kL := s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, stoneRateMs, 34.0, 58.0, 0.020, clamp(surfSharp+0.25, 0, 1)) * lInt
				kR := s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, stoneRateMs, 34.0, 58.0, 0.020, clamp(surfSharp+0.25, 0, 1)) * rInt
				midL += (kL*1.15 + s.nextNoise(&s.rngL)*0.22*lInt) * surfGain
				midR += (kR*1.15 + s.nextNoise(&s.rngR)*0.22*rInt) * surfGain
				lowL += driveWave(s.phHeave*1.05, 0.4, 1.4) * lInt * 0.56 * surfGain
				lowR += driveWave(s.phHeave*1.05+1.3, 0.4, 1.4) * rInt * 0.56 * surfGain
			case "grass_snow":
				s.phGrassL += 2 * math.Pi * (23.0 * cfg.Events.RoadSurface.FreqScale) * dt1
				s.phGrassR += 2 * math.Pi * (25.5 * cfg.Events.RoadSurface.FreqScale) * dt1
				s.phGrassMod += 2 * math.Pi * (6.5 + 3.5*sg) * dt1
				wrapPhase(&s.phGrassL)
				wrapPhase(&s.phGrassR)
				wrapPhase(&s.phGrassMod)
				swell := 0.55 + 0.45*math.Sin(s.phGrassMod)
				lInt, rInt := mixStereo(clamp(leftRumble*3.5, 0.35, 0.85)*sg, clamp(rightRumble*3.5, 0.35, 0.85)*sg, cfg.StereoSeparation)
				lowL += math.Sin(s.phGrassL) * swell * lInt * 0.68 * surfGain
				lowR += math.Sin(s.phGrassR) * (1.1 - swell*0.5) * rInt * 0.68 * surfGain
			default:
				if spd > 35 {
					tarm := clamp((spd-35)/190.0, 0, 1) * 0.14 * surfGain * cfg.AntiFatigueClamp
					midL += math.Sin(s.phRoadL*1.35) * tarm
					midR += math.Sin(s.phRoadR*1.35) * tarm
				}
			}
		}

		// 4) Speed-Synced Periodic Curb Ribs
		if cfg.Events.CurbRumble.Enabled && (s.curbEnvL > 0.05 || s.curbEnvR > 0.05) {
			ribHz := clamp(spd/8.5, 9.0, 30.0) * cfg.Events.CurbRumble.FreqScale
			toneHz := (56.0 + 26.0*cfg.Events.CurbRumble.Sharpness) * cfg.Events.CurbRumble.FreqScale
			s.phCurbGateL += 2 * math.Pi * ribHz * dt1
			s.phCurbGateR += 2 * math.Pi * (ribHz * 1.02) * dt1
			s.phCurbToneL += 2 * math.Pi * toneHz * dt1
			s.phCurbToneR += 2 * math.Pi * (toneHz * 1.03) * dt1
			wrapPhase(&s.phCurbGateL)
			wrapPhase(&s.phCurbGateR)
			wrapPhase(&s.phCurbToneL)
			wrapPhase(&s.phCurbToneR)

			ribEnvL := math.Pow(math.Max(0, math.Sin(s.phCurbGateL)), 1.6)
			ribEnvR := math.Pow(math.Max(0, math.Sin(s.phCurbGateR)), 1.6)
			cL, cR := mixStereo(s.curbEnvL, s.curbEnvR, cfg.StereoSeparation)
			midL += driveWave(s.phCurbToneL, cfg.Events.CurbRumble.Sharpness, 1.8) * ribEnvL * cL * 1.25 * cfg.Events.CurbRumble.Intensity
			midR += driveWave(s.phCurbToneR, cfg.Events.CurbRumble.Sharpness, 1.8) * ribEnvR * cR * 1.25 * cfg.Events.CurbRumble.Intensity
			transientEnv = math.Max(transientEnv, math.Max(cL*ribEnvL, cR*ribEnvR)*0.60)
		}

		// 5) Two-Stage Jump Landing
		if s.landActiveS > 0 && cfg.Events.JumpLanding.Enabled {
			if s.landActiveS >= s.landDurS {
				s.landActiveS = 0
			} else {
				t := s.landActiveS
				u := t / s.landDurS
				inten := s.landPeak * cfg.Events.JumpLanding.Intensity
				if t < 0.024 {
					f := (108.0 + 22.0*cfg.Events.JumpLanding.Sharpness) * cfg.Events.JumpLanding.FreqScale
					slap := driveWave(2*math.Pi*f*t, cfg.Events.JumpLanding.Sharpness, 1.9) * (1.0 - t/0.026) * inten * 1.35
					highL += slap * s.landLeftW
					highR += slap * s.landRightW
				}
				// True linear chirp integral: 76Hz -> 42Hz (never passes through 0Hz!)
				phLow := 2 * math.Pi * (76.0 - 17.0*u) * cfg.Events.JumpLanding.FreqScale * t
				thump := driveWave(phLow, 0.25, 1.6) * math.Exp(-3.4*u) * inten * 1.30
				lowL += thump * s.landLeftW
				lowR += thump * s.landRightW
				transientEnv = math.Max(transientEnv, math.Exp(-2.5*u)*s.landPeak)
				s.landActiveS += dt1
			}
		}

		// 6) Gear Shift Kick
		if s.shiftActiveS > 0 && cfg.Events.GearShift.Enabled {
			if s.shiftActiveS >= s.shiftDurS {
				s.shiftActiveS = 0
			} else {
				t := s.shiftActiveS
				u := t / s.shiftDurS
				inten := s.shiftStrength * cfg.Events.GearShift.Intensity
				sharp := clamp(car.ShiftCrispness*0.6+cfg.Events.GearShift.Sharpness*0.4, 0, 1)
				if t < 0.028 {
					click := driveWave(2*math.Pi*(96.0+24.0*sharp)*cfg.Events.GearShift.FreqScale*t, sharp, 1.9) * (1.0 - t/0.028) * inten * 1.15
					highL += click
					highR += click
				}
				// True linear chirp integral: 52Hz -> 34Hz
				phThud := 2 * math.Pi * (52.0 - 9.0*u) * cfg.Events.GearShift.FreqScale * t
				thud := driveWave(phThud, 0.3, 1.7) * math.Exp(-4.0*u) * inten * 1.18
				lowL += thud
				lowR += thud
				transientEnv = math.Max(transientEnv, (1.0-u)*0.82)
				s.shiftActiveS += dt1
			}
		}

		// 7) Adaptive Redline
		if isRedline {
			rFreq := (92.0 + 18.0*cfg.Events.Redline.Sharpness) * cfg.Events.Redline.FreqScale
			s.phRedline += 2 * math.Pi * rFreq * dt1
			s.phRedGate += 2 * math.Pi * 22.0 * dt1
			wrapPhase(&s.phRedline)
			wrapPhase(&s.phRedGate)
			gate := 0.0
			if math.Sin(s.phRedGate) > -0.15 {
				gate = 1.0
			}
			excess := clamp((rpmRatio-car.RedlineRatio)/0.07+0.62, 0.58, 1.0)
			rw := driveWave(s.phRedline, cfg.Events.Redline.Sharpness, 1.85) * gate * excess * 1.15 * cfg.Events.Redline.Intensity
			highL += rw
			highR += rw
			transientEnv = math.Max(transientEnv, gate*0.62)
		}

		// 8) Water Entry Splash + Liquid Wave Swells
		if cfg.Events.WaterSplash.Enabled && (s.splashActiveS > 0 || (maxPuddle >= 0.03 && spd > 5)) {
			if s.splashActiveS > 0 {
				if s.splashActiveS >= s.splashDurS {
					s.splashActiveS = 0
				} else {
					t := s.splashActiveS
					u := t / s.splashDurS
					phSp := 2 * math.Pi * (105.0 - 30.0*u) * cfg.Events.WaterSplash.FreqScale * t
					spL := (0.72*math.Sin(phSp) + 0.28*s.nextNoise(&s.rngL)) * math.Exp(-2.6*u) * s.splashPeak * 1.32 * cfg.Events.WaterSplash.Intensity
					spR := (0.72*math.Sin(phSp+0.7) + 0.28*s.nextNoise(&s.rngR)) * math.Exp(-2.6*u) * s.splashPeak * 1.32 * cfg.Events.WaterSplash.Intensity
					highL += spL * s.splashLeftW
					highR += spR * s.splashRightW
					transientEnv = math.Max(transientEnv, (1.0-u)*0.72)
					s.splashActiveS += dt1
				}
			}
			if maxPuddle >= 0.03 && spd > 5 {
				wFreq := (34.0 + 16.0*clamp(spd/130.0, 0, 1)) * cfg.Events.WaterSplash.FreqScale
				s.phWater += 2 * math.Pi * wFreq * dt1
				s.phWaterSwell += 2 * math.Pi * (6.2 + 3.8*clamp(spd/120.0, 0, 1)) * dt1
				wrapPhase(&s.phWater)
				wrapPhase(&s.phWaterSwell)
				swellL := 0.45 + 0.55*math.Sin(s.phWaterSwell)
				swellR := 0.45 + 0.55*math.Sin(s.phWaterSwell+1.1)
				sf := clamp(spd/75.0, 0.35, 1.0)
				wL, wR := mixStereo(clamp(leftPuddle*2.4, 0, 1)*sf, clamp(rightPuddle*2.4, 0, 1)*sf, cfg.StereoSeparation)
				gain := 0.96 * cfg.Events.WaterSplash.Intensity
				midL += (0.78*math.Sin(s.phWater) + 0.22*s.nextNoise(&s.rngL)) * swellL * wL * gain
				midR += (0.78*math.Sin(s.phWater+0.9) + 0.22*s.nextNoise(&s.rngR)) * swellR * wR * gain
			}
		}

		// 9A) 4-TIRE DRIVEN 推头 (UNDERSTEER): FL -> GL, FR -> GR
		if isUndersteer || isFourWheelDrift {
			uFreq := (78.0 + 22.0*clamp(rawFrontSlip/1.8, 0, 1)) * cfg.Events.Understeer.FreqScale
			s.phUnderL += 2 * math.Pi * uFreq * dt1
			s.phUnderR += 2 * math.Pi * (uFreq * 1.05) * dt1
			s.phUnderHop += 2 * math.Pi * 18.0 * dt1
			wrapPhase(&s.phUnderL)
			wrapPhase(&s.phUnderR)
			wrapPhase(&s.phUnderHop)

			hopMod := 0.50 + 0.50*math.Max(0, math.Sin(s.phUnderHop))
			uScale := 1.28
			if isFourWheelDrift {
				uScale = 0.65
			}
			inten := clamp((rawFrontSlip-0.45)/1.05, 0.48, 1.0) * cfg.Events.Understeer.Intensity * car.SlipExpressiveness
			midL += driveWave(s.phUnderL, cfg.Events.Understeer.Sharpness, 1.92) * hopMod * underL * inten * uScale
			midR += driveWave(s.phUnderR, cfg.Events.Understeer.Sharpness, 1.92) * hopMod * underR * inten * uScale
		}

		// 9B) 4-TIRE DRIVEN 甩尾/漂移 (OVERSTEER/DRIFT): RL -> GL, RR -> GR
		if isOversteer || isLaunchSpin || isFourWheelDrift {
			oBaseHz := (27.0 + 14.0*clamp(rawRearSlip/2.2, 0, 1)) * cfg.Events.OversteerDrift.FreqScale
			s.phOverL += 2 * math.Pi * oBaseHz * dt1
			s.phOverR += 2 * math.Pi * (oBaseHz * 1.04) * dt1
			s.phOverSweep += 2 * math.Pi * 5.0 * dt1
			wrapPhase(&s.phOverL)
			wrapPhase(&s.phOverR)
			wrapPhase(&s.phOverSweep)

			gScale := cfg.Events.OversteerDrift.Intensity * car.SlipExpressiveness
			if isLaunchSpin {
				inten := clamp(drivenSlip/1.35, 0.48, 1.0) * gScale
				hop := 0.58 + 0.42*math.Sin(s.phOverSweep*3.2)
				dl, dr := contrastLR(slipFL+slipRL, slipFR+slipRR, 1.2)
				lowL += driveWave(s.phOverL*1.3, 0.55, 1.65) * hop * dl * inten * 0.98
				lowR += driveWave(s.phOverR*1.3, 0.55, 1.65) * hop * dr * inten * 0.98
			} else {
				inten := clamp((rawRearSlip-0.42)/1.25, 0.48, 1.0) * gScale
				swell := 0.78 + 0.22*math.Sin(s.phOverSweep)
				lowL += driveWave(s.phOverL, cfg.Events.OversteerDrift.Sharpness, 1.65) * swell * overL * inten * 1.22
				lowR += driveWave(s.phOverR, cfg.Events.OversteerDrift.Sharpness, 1.65) * swell * overR * inten * 1.22
				midL += math.Sin(s.phOverL*1.9) * overL * inten * 0.42
				midR += math.Sin(s.phOverR*1.9) * overR * inten * 0.42
			}
		}

		// 10) 4-Tire Weighted ABS / Wheel Lock Chassis Pulse
		if cfg.Events.BrakingABS.Enabled && (isABS || isWheelLock) {
			s.phAbsPulse += 2 * math.Pi * (14.5 * cfg.Events.BrakingABS.FreqScale) * dt1
			wrapPhase(&s.phAbsPulse)
			if isWheelLock {
				midL += driveWave(s.phUnderL+2*math.Pi*62*dt1*float64(ms), 0.75, 1.7) * absL * 0.80 * cfg.Events.BrakingABS.Intensity
				midR += driveWave(s.phUnderR+2*math.Pi*62*dt1*float64(ms), 0.75, 1.7) * absR * 0.80 * cfg.Events.BrakingABS.Intensity
			} else {
				pulse := math.Max(0, math.Sin(s.phAbsPulse))
				midL += driveWave(s.phEngBase*1.4, 0.5, 1.6) * pulse * absL * 0.76 * cfg.Events.BrakingABS.Intensity
				midR += driveWave(s.phEngBase*1.4, 0.5, 1.6) * pulse * absR * 0.76 * cfg.Events.BrakingABS.Intensity
			}
		}

		// 11) CONTINUOUS HIGH-FREQ DIRECTIONAL GUARDRAIL SCRAPE (76Hz + 112Hz steel screech + grit, 100% vs 0% L/R!)
		if s.guardActiveS > 0 && cfg.Events.Guardrail.Enabled {
			s.guardActiveS = math.Max(0, s.guardActiveS-dt1)
			s.guardElapsedS += dt1
			s.phGuard += 2 * math.Pi * (112.0 * cfg.Events.Guardrail.FreqScale) * dt1
			wrapPhase(&s.phGuard)
			// Continuous dense metallic rasp (smooth high-rate ripple 0.72..1.0, NO slow stuttering gaps!)
			mod := 0.78 + 0.22*math.Sin(2*math.Pi*34.0*s.guardElapsedS)
			gw := (0.45*driveWave(s.phGuard, cfg.Events.Guardrail.Sharpness, 1.95) + 0.35*driveWave(s.phGuard*0.68, 0.55, 1.80) + 0.28*s.nextNoise(&s.rngL)) * mod * s.guardPeak * 1.45 * cfg.Events.Guardrail.Intensity
			if s.guardSide < 0 {
				highL += gw
			} else if s.guardSide > 0 {
				highR += gw
			} else {
				highL += gw
				highR += gw
			}
			transientEnv = math.Max(transientEnv, s.guardPeak*0.95)
		} else {
			s.guardElapsedS = 0
		}

		// 12) Smashable Debris
		if s.smashActiveS > 0 && cfg.Events.CollisionWall.Enabled {
			if s.smashActiveS >= s.smashDurS {
				s.smashActiveS = 0
			} else {
				u := s.smashActiveS / s.smashDurS
				s.phSmash += 2 * math.Pi * (128.0 * cfg.Events.CollisionWall.FreqScale) * dt1
				wrapPhase(&s.phSmash)
				sw := (0.70*driveWave(s.phSmash, 0.85, 1.85) + 0.30*s.nextNoise(&s.rngL)) * math.Exp(-3.8*u) * s.smashPeak * 1.18 * cfg.Events.CollisionWall.Intensity
				highL += sw
				highR += sw
				transientEnv = math.Max(transientEnv, (1.0-u)*0.68)
				s.smashActiveS += dt1
			}
		}

		// 13) CRISP, TIGHT 360° DIRECTIONAL COLLISION (72–135Hz APEX 6 LRA Punch, Fast Decay, ZERO After-Crash Tail!)
		if s.impactActiveS > 0 && cfg.Events.CollisionWall.Enabled {
			if s.impactActiveS >= s.impactDurS {
				s.impactActiveS = 0
			} else {
				t := s.impactActiveS
				u := t / s.impactDurS
				inten := s.impactPeak * cfg.Events.CollisionWall.Intensity
				sharp := cfg.Events.CollisionWall.Sharpness

				// Clean tail fade to exact zero in final 18% of duration
				tailFade := clamp((1.0-u)/0.18, 0, 1)
				envFast := math.Exp(-4.6*u) * tailFade

				// Integrated true frequencies in APEX 6 crisp LRA punch band (NEVER low sluggish 24-32Hz!)
				bodyHz := (94.0 - 30.0*u) * cfg.Events.CollisionWall.FreqScale // 94Hz -> 64Hz crisp solid thump
				if s.impactKind == "rear" || s.impactKind == "rear_left" || s.impactKind == "rear_right" {
					bodyHz = (64.0 + 32.0*u) * cfg.Events.CollisionWall.FreqScale // 64Hz -> 96Hz rising rear shove
				} else if s.impactLowSpeed {
					bodyHz = (108.0 - 32.0*u) * cfg.Events.CollisionWall.FreqScale // 108Hz -> 76Hz crisp bumper bonk
				}
				crackHz := 132.0 * cfg.Events.CollisionWall.FreqScale
				s.phWallA += 2 * math.Pi * bodyHz * dt1
				s.phWallB += 2 * math.Pi * crackHz * dt1
				wrapPhase(&s.phWallA)
				wrapPhase(&s.phWallB)

				crackEnv := 0.0
				if t < 0.032 {
					crackEnv = (1.0 - t/0.032)
				}
				crack := (0.72*driveWave(s.phWallB, sharp, 1.95) + 0.28*s.nextNoise(&s.rngL)) * crackEnv * inten * 1.55
				body := driveWave(s.phWallA, 0.45+0.25*sharp, 1.85) * envFast * inten * 1.75
				// Subtle tight secondary chassis rebound around 65..105ms for heavy impacts (like V4's real 2-stage punch)
				rebound := 0.0
				if !s.impactLowSpeed && t >= 0.062 && t <= 0.118 {
					ru := (t - 0.062) / 0.056
					rebound = math.Sin(s.phWallA*0.85) * math.Sin(math.Pi*ru) * inten * 0.52
				}

				cL, cR := 0.0, 0.0
				switch s.impactKind {
				case "left":
					cL = crack + body + rebound
					if t >= s.impactDelayR {
						cR = math.Sin(s.phWallA*0.75) * envFast * inten * 0.22
					}
				case "right":
					cR = crack + body + rebound
					if t >= s.impactDelayL {
						cL = math.Sin(s.phWallA*0.75) * envFast * inten * 0.22
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
				highL += cL
				highR += cR
				transientEnv = clamp(envFast*1.4+crackEnv, 0, 1)
				s.impactActiveS += dt1
			}
		}

		// Per-Channel Sidechain Ducking + Far-Side Directional Isolation Mute
		targetDuck := 1.0 - cfg.DuckingStrength*clamp(transientEnv, 0, 1)
		if targetDuck < s.duckGain {
			s.duckGain += 0.16 * (targetDuck - s.duckGain)
		} else {
			s.duckGain += 0.025 * (targetDuck - s.duckGain)
		}
		if collisionExclusive || muteFarL {
			s.collisionRampL = 0.0
		} else {
			s.collisionRampL += 0.020 * (1.0 - s.collisionRampL)
		}
		if collisionExclusive || muteFarR {
			s.collisionRampR = 0.0
		} else {
			s.collisionRampR += 0.020 * (1.0 - s.collisionRampR)
		}

		mixL := ((0.88*lowL + 0.96*midL) * s.duckGain * s.collisionRampL + 1.18*highL) * cfg.MasterGripGain
		mixR := ((0.88*lowR + 0.96*midR) * s.duckGain * s.collisionRampR + 1.18*highR) * cfg.MasterGripGain
		if muteFarL {
			mixL = 0
		}
		if muteFarR {
			mixR = 0
		}

		encL := int(math.Round(128.0 + math.Tanh(mixL*1.15)*125.0))
		encR := int(math.Round(128.0 + math.Tanh(mixR*1.15)*125.0))
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

		// --- 4-TIRE DRIVEN LEFT & RIGHT TRIGGER SYNTHESIS ---
		tLVal, tRVal := 0.0, 0.0
		if !collisionExclusive {
			if cfg.Trigger.EnableLeftBrake {
				if isWheelLock {
					pL = tpDynamicEvent
					s.phTrigL += 2 * math.Pi * 96.0 * dt1
					wrapPhase(&s.phTrigL)
					tLVal = driveWave(s.phTrigL, 0.65, 1.65) * 0.90 * cfg.Events.BrakingABS.Intensity
				} else if isABS {
					pL = tpDynamicEvent
					s.phTrigL += 2 * math.Pi * 72.0 * dt1
					wrapPhase(&s.phTrigL)
					pulse := math.Max(0, math.Sin(s.phAbsPulse))
					tLVal = driveWave(s.phTrigL, 0.45, 1.55) * pulse * 0.82 * cfg.Events.BrakingABS.Intensity
				} else if isUndersteer && (brakeF > 0.08 || (accelF <= 0.12 && slipFL >= slipFR)) {
					pL = tpDynamicEvent
					s.phTrigL += 2 * math.Pi * 86.0 * dt1
					wrapPhase(&s.phTrigL)
					hop := 0.55 + 0.45*math.Max(0, math.Sin(s.phUnderHop))
					tLVal = driveWave(s.phTrigL, 0.72, 1.65) * hop * clamp(math.Max(slipFL, rawFrontSlip)/1.20, 0.50, 1.0) * 0.88 * cfg.Events.Understeer.Intensity
				} else if (isOversteer || isFourWheelDrift) && (brakeF > 0.12 || hbActive || (accelF <= 0.08 && slipRL > slipRR*1.12)) {
					pL = tpDynamicEvent
					s.phTrigL += 2 * math.Pi * 56.0 * dt1
					wrapPhase(&s.phTrigL)
					tLVal = driveWave(s.phTrigL, 0.30, 1.50) * clamp(math.Max(slipRL, rawRearSlip)/1.25, 0.48, 1.0) * 0.78 * cfg.Events.OversteerDrift.Intensity
				} else if brakeF > 0.16 && cfg.Trigger.BrakeResistance > 0.01 {
					pL = tpStaticResistance
					s.phTrigL += 2 * math.Pi * 36.0 * dt1
					wrapPhase(&s.phTrigL)
					tLVal = math.Sin(s.phTrigL) * (brakeF - 0.16) * 0.34 * cfg.Trigger.BrakeResistance
				}
			}

			if cfg.Trigger.EnableRightAccel {
				if s.shiftActiveS > 0 && s.shiftActiveS < 0.080 && cfg.Events.GearShift.Enabled {
					pR = tpDynamicEvent
					u := s.shiftActiveS / 0.080
					tRVal = math.Sin(math.Pi*u) * 0.96 * s.shiftStrength * cfg.Events.GearShift.Intensity
				} else if isLaunchSpin {
					pR = tpDynamicEvent
					s.phTrigR += 2 * math.Pi * 86.0 * dt1
					wrapPhase(&s.phTrigR)
					tRVal = driveWave(s.phTrigR, 0.5, 1.55) * 0.82 * clamp(drivenSlip/1.35, 0.45, 1) * cfg.Events.OversteerDrift.Intensity
				} else if isRedline {
					pR = tpDynamicEvent
					s.phTrigR += 2 * math.Pi * 100.0 * dt1
					wrapPhase(&s.phTrigR)
					tRVal = driveWave(s.phTrigR, 0.7, 1.65) * 0.75 * cfg.Events.Redline.Intensity
				} else if (isOversteer || isFourWheelDrift) && (accelF > 0.08 || (brakeF <= 0.12 && !hbActive && slipRR >= slipRL*0.88)) {
					pR = tpDynamicEvent
					trFreq := 62.0 + 16.0*clamp(rawRearSlip/2.0, 0, 1)
					s.phTrigR += 2 * math.Pi * trFreq * dt1
					wrapPhase(&s.phTrigR)
					tRVal = driveWave(s.phTrigR, 0.25, 1.50) * clamp(math.Max(slipRR, rawRearSlip)/1.25, 0.50, 1.0) * 0.86 * cfg.Events.OversteerDrift.Intensity
				} else if isUndersteer && ((accelF > 0.12 && brakeF <= 0.08) || (brakeF <= 0.08 && slipFR > slipFL)) {
					pR = tpDynamicEvent
					s.phTrigR += 2 * math.Pi * 92.0 * dt1
					wrapPhase(&s.phTrigR)
					hop := 0.55 + 0.45*math.Max(0, math.Sin(s.phUnderHop))
					tRVal = driveWave(s.phTrigR, 0.78, 1.65) * hop * clamp(math.Max(slipFR, rawFrontSlip)/1.20, 0.50, 1.0) * 0.85 * cfg.Events.Understeer.Intensity
				} else if accelF > 0.18 && cfg.Trigger.ThrottleMicroFeel > 0.01 {
					pR = tpStaticResistance
					s.phTrigR += 2 * math.Pi * (40.0 + 36.0*rpmRatio) * dt1
					wrapPhase(&s.phTrigR)
					tRVal = math.Sin(s.phTrigR) * accelF * 0.28 * cfg.Trigger.ThrottleMicroFeel
				}
			}
		}

		encTL := int(math.Round(128.0 + math.Tanh(tLVal*cfg.MasterTriggerGain*1.15)*115.0))
		encTR := int(math.Round(128.0 + math.Tanh(tRVal*cfg.MasterTriggerGain*1.15)*115.0))
		trigLBuf[ms] = byte(clamp(float64(encTL), 6, 250))
		trigRBuf[ms] = byte(clamp(float64(encTR), 6, 250))
	}

	res.GripLRms = math.Sqrt(sumSqL / 8.0)
	res.GripRRms = math.Sqrt(sumSqR / 8.0)

	bothDriftTrigs := (isFourWheelDrift || (isOversteer && (brakeF > 0.12 || hbActive) && accelF > 0.08)) && pL != tpOff && pR != tpOff
	if collisionExclusive {
		res.Flags = 0x98
		res.TrigRoute = -1
		res.RightTrigNeed = false
	} else if bothDriftTrigs {
		res.Flags = 0x9E
		res.Trig = trigRBuf
		res.TrigRoute = 2
		res.RightTrigNeed = true
	} else if pL == tpOff && pR == tpOff {
		res.Flags = 0x9C
		res.TrigRoute = -1
		res.RightTrigNeed = false
	} else if pL != tpOff && pR != tpOff {
		conflictSeen = true
		if cfg.Trigger.SmartArbitration && pL != pR {
			if pL > pR {
				res.Flags = 0x9C
				res.Trig = trigLBuf
				res.TrigRoute = 0
			} else {
				res.Flags = 0x9D
				res.Trig = trigRBuf
				res.TrigRoute = 1
				res.RightTrigNeed = true
			}
		} else if peakDev(trigLBuf) >= peakDev(trigRBuf) {
			res.Flags = 0x9C
			res.Trig = trigLBuf
			res.TrigRoute = 0
		} else {
			res.Flags = 0x9D
			res.Trig = trigRBuf
			res.TrigRoute = 1
			res.RightTrigNeed = true
		}
	} else if pL != tpOff {
		res.Flags = 0x9C
		res.Trig = trigLBuf
		res.TrigRoute = 0
	} else {
		res.Flags = 0x9D
		res.Trig = trigRBuf
		res.TrigRoute = 1
		res.RightTrigNeed = true
	}

	res.ConflictSolved = conflictSeen
	res.Frame = hid.BuildWave0x57(res.Flags, res.Trig, res.GripL, res.GripR)
	return res
}

func (s *Synthesizer) stepPebble(g *pebbleGrain, nextMs *float64, rng *uint32, meanGapMs, fMin, fMax, durS, sharp float64) float64 {
	*nextMs -= 1.0
	if *nextMs <= 0 && g.activeS <= 0 {
		g.activeS = 0.0001
		g.durS = durS * (0.75 + 0.55*s.nextRand01(rng))
		g.freqHz = fMin + (fMax-fMin)*s.nextRand01(rng)
		g.amp = 0.55 + 0.45*s.nextRand01(rng)
		g.sharp = sharp
		*nextMs = meanGapMs * (0.45 + 1.15*s.nextRand01(rng))
	}
	if g.activeS <= 0 {
		return 0
	}
	if g.activeS >= g.durS {
		g.activeS = 0
		return 0
	}
	u := g.activeS / g.durS
	env := math.Sin(math.Pi * u)
	out := driveWave(2*math.Pi*g.freqHz*g.activeS, g.sharp, 1.7) * env * g.amp
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
	res.SurfaceLabel = "试震演示: " + s.testEventID
	res.ActiveEvents = []ActiveEvent{{ID: s.testEventID, Label: "试震中", Category: "test", Intensity: 0.95, Channel: "both"}}

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
				w := driveWave(2*math.Pi*f*t, 1-car.EngineRoundness, 1.8)
				if car.EngineFamily == "muscle_v8" {
					w = (w*0.82 + math.Sin(math.Pi*f*t)*0.58) * (0.22 + 1.05*math.Pow(0.5+0.5*math.Sin(2*math.Pi*3.4*t), 1.8))
				} else if car.EngineFamily == "v12_silk" {
					w = math.Sin(2*math.Pi*f*t)*0.82 + math.Sin(4*math.Pi*f*t)*0.22
				}
				out := w * 0.54 * car.EngineBedScale * cfg.Events.EngineIdle.Intensity * env
				gl, gr = out, out
			}
		case "engineRev":
			// True linear chirp integral from EngineFreqBaseHz to EngineFreqTopHz
			fAvg := (car.EngineFreqBaseHz + 0.5*(car.EngineFreqTopHz-car.EngineFreqBaseHz)*u) * cfg.Events.EngineRev.FreqScale
			ph := 2 * math.Pi * fAvg * t
			var w float64
			switch car.EngineFamily {
			case "ev":
				w = math.Sin(ph) * 0.85
			case "muscle_v8":
				w = driveWave(ph, 0.45, 2.0)*0.82 + math.Sin(0.5*ph)*0.58
			case "v12_silk":
				w = math.Sin(ph)*0.82 + math.Sin(2*ph)*0.22
			case "rotary_f1":
				w = driveWave(ph, 0.92, 1.75)*0.78 + math.Sin(2*ph)*0.28
			default:
				w = driveWave(ph, 1-car.EngineRoundness, 1.70)
			}
			surge := 1.0
			if car.EngineFamily == "turbo_punch" && u > 0.35 {
				surge = 1.0 + 0.65*((u-0.35)/0.65)
			}
			out := w * (0.34 + 0.44*u) * surge * car.EngineBedScale * cfg.Events.EngineRev.Intensity
			gl, gr = out, out
		case "gearShift":
			sharp := clamp(car.ShiftCrispness*0.6+cfg.Events.GearShift.Sharpness*0.4, 0, 1)
			w := driveWave(2*math.Pi*(52-9*u)*t, 0.3, 1.7)*math.Exp(-4.0*u)*1.15 +
				driveWave(2*math.Pi*108*t, sharp, 1.9)*math.Exp(-14*u)*1.05
			w *= cfg.Events.GearShift.Intensity
			gl, gr, tr = w, w, math.Sin(math.Pi*clamp(u*2.4, 0, 1))*0.95*cfg.Events.GearShift.Intensity
			res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
		case "redline":
			gate := 0.0
			if int(t*1000)%44 < 26 {
				gate = 1.0
			}
			w := driveWave(2*math.Pi*98*cfg.Events.Redline.FreqScale*t, cfg.Events.Redline.Sharpness, 1.85) * gate * 1.15 * cfg.Events.Redline.Intensity
			gl, gr, tr = w, w, w*0.68
			res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
		case "roadUndulation":
			sharp := clamp(car.SuspensionStiffness*0.65+cfg.Events.RoadUndulation.Sharpness*0.35, 0, 1)
			f := (32 + 34*sharp) * cfg.Events.RoadUndulation.FreqScale
			gl = driveWave(2*math.Pi*f*t, sharp, 1.55) * (0.45 + 0.55*math.Sin(2*math.Pi*2.8*t)) * 0.85 * cfg.Events.RoadUndulation.Intensity
			gr = driveWave(2*math.Pi*f*1.08*t, sharp, 1.55) * (0.45 + 0.55*math.Cos(2*math.Pi*2.8*t)) * 0.85 * cfg.Events.RoadUndulation.Intensity
		case "surfTarmac":
			gl = math.Sin(2*math.Pi*52*t) * 0.16 * cfg.Events.RoadSurface.Intensity * env
			gr = math.Sin(2*math.Pi*55*t) * 0.16 * cfg.Events.RoadSurface.Intensity * env
		case "surfDirt", "roadSurface":
			gl = (s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, 20, 52, 74, 0.011, 0.45)*0.95 + math.Sin(2*math.Pi*30*t)*0.32) * cfg.Events.RoadSurface.Intensity * env
			gr = (s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, 20, 52, 74, 0.011, 0.45)*0.95 + math.Sin(2*math.Pi*30*t+0.9)*0.32) * cfg.Events.RoadSurface.Intensity * env
		case "surfGravel":
			gl = (s.stepPebble(&s.pebL, &s.pebNextMsL, &s.rngL, 15, 34, 58, 0.020, 0.78)*1.20 + driveWave(2*math.Pi*25*t, 0.4, 1.5)*0.52) * cfg.Events.RoadSurface.Intensity * env
			gr = (s.stepPebble(&s.pebR, &s.pebNextMsR, &s.rngR, 15, 34, 58, 0.020, 0.78)*1.20 + driveWave(2*math.Pi*26*t+1.1, 0.4, 1.5)*0.52) * cfg.Events.RoadSurface.Intensity * env
		case "surfGrass":
			sw := 0.55 + 0.45*math.Sin(2*math.Pi*7.5*t)
			gl = math.Sin(2*math.Pi*23*t) * sw * 0.62 * cfg.Events.RoadSurface.Intensity * env
			gr = math.Sin(2*math.Pi*25.5*t) * (1.1 - sw*0.5) * 0.62 * cfg.Events.RoadSurface.Intensity * env
		case "curbLeft", "curbRumble":
			rib := math.Pow(math.Max(0, math.Sin(2*math.Pi*16*t)), 1.6)
			gl = driveWave(2*math.Pi*68*t, cfg.Events.CurbRumble.Sharpness, 1.85) * rib * 1.25 * cfg.Events.CurbRumble.Intensity
			gr = 0
		case "curbRight":
			rib := math.Pow(math.Max(0, math.Sin(2*math.Pi*16*t)), 1.6)
			gl = 0
			gr = driveWave(2*math.Pi*68*t, cfg.Events.CurbRumble.Sharpness, 1.85) * rib * 1.25 * cfg.Events.CurbRumble.Intensity
		case "jumpLanding":
			slap := 0.0
			if t < 0.025 {
				slap = driveWave(2*math.Pi*118*t, cfg.Events.JumpLanding.Sharpness, 1.9) * (1 - t/0.026) * 1.35
			}
			thump := driveWave(2*math.Pi*(76-17*u)*t, 0.25, 1.6) * math.Exp(-3.4*u) * 1.30
			w := (slap + thump) * cfg.Events.JumpLanding.Intensity
			gl, gr = w, w
		case "impactLowSpeed":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 0, "低速正面碰擦"
			tail := clamp((1.0-u)/0.18, 0, 1)
			envFast := math.Exp(-4.8*u) * tail
			ph := 2 * math.Pi * (108.0*t - 16.0*t*u) // exact integral of 108 -> 76Hz!
			crack := 0.0
			if t < 0.025 {
				crack = driveWave(2*math.Pi*132*t, 0.75, 1.9) * (1 - t/0.025) * 1.25
			}
			w := (crack + driveWave(ph, 0.55, 1.85)*envFast*1.50) * cfg.Events.CollisionWall.Intensity
			gl, gr = w, w
			res.Flags = 0x98
		case "collisionWall", "impactFront":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 0, "正面重撞"
			tail := clamp((1.0-u)/0.18, 0, 1)
			envFast := math.Exp(-4.5*u) * tail
			ph := 2 * math.Pi * (94.0*t - 15.0*t*u) // exact integral of 94 -> 64Hz!
			crack := 0.0
			if t < 0.032 {
				crack = (0.72*driveWave(2*math.Pi*132*t, cfg.Events.CollisionWall.Sharpness, 1.95) + 0.28*s.nextNoise(&s.rngL)) * (1 - t/0.032) * 1.60
			}
			rebound := 0.0
			if t >= 0.062 && t <= 0.118 {
				ru := (t - 0.062) / 0.056
				rebound = math.Sin(ph*0.85) * math.Sin(math.Pi*ru) * 0.52
			}
			w := (crack + driveWave(ph, 0.55, 1.85)*envFast*1.75 + rebound) * cfg.Events.CollisionWall.Intensity
			gl, gr = w, w
			res.Flags = 0x98
		case "impactRear":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 180, "后方追尾撞击"
			tail := clamp((1.0-u)/0.18, 0, 1)
			envFast := math.Exp(-4.4*u) * tail
			ph := 2 * math.Pi * (64.0*t + 16.0*t*u) // exact integral of 64 -> 96Hz rising rear shove!
			shove := driveWave(ph, 0.45, 1.85) * envFast * 1.75 * cfg.Events.CollisionWall.Intensity
			gl, gr = shove, shove
			res.Flags = 0x98
		case "impactLeft":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 90, "左侧撞击"
			tail := clamp((1.0-u)/0.18, 0, 1)
			envFast := math.Exp(-4.5*u) * tail
			ph := 2 * math.Pi * (92.0*t - 15.0*t*u)
			crack := 0.0
			if t < 0.032 {
				crack = (0.72*driveWave(2*math.Pi*130*t, cfg.Events.CollisionWall.Sharpness, 1.95) + 0.28*s.nextNoise(&s.rngL)) * (1 - t/0.032) * 1.60
			}
			gl = (crack + driveWave(ph, 0.58, 1.90)*envFast*1.78) * cfg.Events.CollisionWall.Intensity
			gr = 0
			res.Flags = 0x98
		case "impactRight":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, -90, "右侧撞击"
			tail := clamp((1.0-u)/0.18, 0, 1)
			envFast := math.Exp(-4.5*u) * tail
			ph := 2 * math.Pi * (92.0*t - 15.0*t*u)
			crack := 0.0
			if t < 0.032 {
				crack = (0.72*driveWave(2*math.Pi*130*t, cfg.Events.CollisionWall.Sharpness, 1.95) + 0.28*s.nextNoise(&s.rngR)) * (1 - t/0.032) * 1.60
			}
			gl = 0
			gr = (crack + driveWave(ph, 0.58, 1.90)*envFast*1.78) * cfg.Events.CollisionWall.Intensity
			res.Flags = 0x98
		case "guardrail", "guardrailLeft":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, 90, "左侧护栏刮擦"
			mod := 0.78 + 0.22*math.Sin(2*math.Pi*34.0*t)
			gl = (0.45*driveWave(2*math.Pi*112*t, cfg.Events.Guardrail.Sharpness, 1.95) + 0.35*driveWave(2*math.Pi*76*t, 0.55, 1.80) + 0.28*s.nextNoise(&s.rngL)) * mod * 1.45 * cfg.Events.Guardrail.Intensity * env
			gr = 0
		case "guardrailRight":
			res.ImpactActive, res.ImpactAngleDeg, res.ImpactLabel = true, -90, "右侧护栏刮擦"
			mod := 0.78 + 0.22*math.Sin(2*math.Pi*34.0*t)
			gl = 0
			gr = (0.45*driveWave(2*math.Pi*112*t, cfg.Events.Guardrail.Sharpness, 1.95) + 0.35*driveWave(2*math.Pi*76*t, 0.55, 1.80) + 0.28*s.nextNoise(&s.rngR)) * mod * 1.45 * cfg.Events.Guardrail.Intensity * env
		case "waterSplash":
			swL := 0.45 + 0.55*math.Sin(2*math.Pi*7.5*t)
			swR := 0.45 + 0.55*math.Sin(2*math.Pi*7.5*t+1.1)
			sp := math.Sin(2*math.Pi*(105-30*u)*t) * math.Exp(-2.6*u) * 1.25
			dragL := (0.78*math.Sin(2*math.Pi*40*t) + 0.22*s.nextNoise(&s.rngL)) * swL * 0.85 * env
			dragR := (0.78*math.Sin(2*math.Pi*40*t+0.9) + 0.22*s.nextNoise(&s.rngR)) * swR * 0.85 * env
			gl = (sp + dragL) * cfg.Events.WaterSplash.Intensity
			gr = (sp*0.85 + dragR) * cfg.Events.WaterSplash.Intensity
		case "understeer", "understeerLeft":
			hop := 0.48 + 0.52*math.Max(0, math.Sin(2*math.Pi*18*t))
			w := driveWave(2*math.Pi*86*t, cfg.Events.Understeer.Sharpness, 1.9) * hop * 1.15 * cfg.Events.Understeer.Intensity * env
			gl, gr, tr = w, w*0.12, driveWave(2*math.Pi*86*t, 0.72, 1.65)*hop*0.85*cfg.Events.Understeer.Intensity*env
			res.Flags, res.TrigRoute = 0x9C, 0
		case "understeerRight":
			hop := 0.48 + 0.52*math.Max(0, math.Sin(2*math.Pi*18*t))
			w := driveWave(2*math.Pi*86*t, cfg.Events.Understeer.Sharpness, 1.9) * hop * 1.15 * cfg.Events.Understeer.Intensity * env
			gl, gr, tr = w*0.12, w, driveWave(2*math.Pi*92*t, 0.78, 1.65)*hop*0.85*cfg.Events.Understeer.Intensity*env
			res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
		case "oversteerDrift", "oversteerLeft", "tireSlipDrift":
			swell := 0.78 + 0.22*math.Sin(2*math.Pi*5.0*t)
			w := (driveWave(2*math.Pi*32*t, cfg.Events.OversteerDrift.Sharpness, 1.60)*1.12 + math.Sin(2*math.Pi*62*t)*0.34) * swell * cfg.Events.OversteerDrift.Intensity * env
			gl, gr, tr = w, w*0.42, driveWave(2*math.Pi*68*t, 0.25, 1.5)*0.84*cfg.Events.OversteerDrift.Intensity*env
			res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
		case "oversteerRight":
			swell := 0.78 + 0.22*math.Sin(2*math.Pi*5.0*t)
			w := (driveWave(2*math.Pi*32*t, cfg.Events.OversteerDrift.Sharpness, 1.60)*1.12 + math.Sin(2*math.Pi*62*t)*0.34) * swell * cfg.Events.OversteerDrift.Intensity * env
			gl, gr, tr = w*0.42, w, driveWave(2*math.Pi*68*t, 0.25, 1.5)*0.84*cfg.Events.OversteerDrift.Intensity*env
			res.Flags, res.RightTrigNeed, res.TrigRoute = 0x9D, true, 1
		case "brakingAbs":
			pulse := math.Max(0, math.Sin(2*math.Pi*14.5*t))
			w := driveWave(2*math.Pi*68*t, 0.55, 1.65) * pulse * 0.92 * cfg.Events.BrakingABS.Intensity
			gl, gr, tr = w*0.72, w*0.72, w*1.05
			res.Flags, res.TrigRoute = 0x9C, 0
		}

		bL := byte(clamp(math.Round(128+math.Tanh(gl*cfg.MasterGripGain*1.12)*125), 2, 253))
		bR := byte(clamp(math.Round(128+math.Tanh(gr*cfg.MasterGripGain*1.12)*125), 2, 253))
		res.GripL[ms] = bL
		res.GripR[ms] = bR
		res.Trig[ms] = byte(clamp(math.Round(128+math.Tanh(tr*cfg.MasterTriggerGain*1.15)*115), 6, 250))
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
	if *p >= twoPi {
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
