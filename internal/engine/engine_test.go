package engine

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"forzanext/internal/hid"
	"forzanext/internal/telemetry"
)

func TestHIDWireChecksumsMatchV4Recordings(t *testing.T) {
	cases := []struct {
		name string
		got  [33]byte
		want string
	}{
		{"init_right_off", hid.BuildCtrl0x53(0x01, 0x01, 0x03, 0x06, 0x00), "035aa5530701010306000000000000000000000000000000000000000000000065"},
		{"init_right_on", hid.BuildCtrl0x53(0x01, 0x01, 0x03, 0x06, 0x40), "035aa55307010103064000000000000000000000000000000000000000000000a5"},
		{"init_left_on", hid.BuildCtrl0x53(0x01, 0x00, 0x03, 0x06, 0x40), "035aa55307010003064000000000000000000000000000000000000000000000a4"},
		{"init_grip_en", hid.BuildCtrl0x53(0x01, 0x12, 0x02, 0x40, 0x00), "035aa55307011202400000000000000000000000000000000000000000000000af"},
		{"neutral_0x57", hid.BuildNeutral0x57(), "035aa5571b848080808080808080808080808080808080808080808080800000f6"},
	}
	for _, tc := range cases {
		if gotHex := hid.FrameHex(tc.got); gotHex != tc.want {
			t.Fatalf("%s mismatch:\n got: %s\nwant: %s", tc.name, gotHex, tc.want)
		}
	}
}

func TestLowSpeedAndExtremeLRSideImpacts(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")

	runImpact := func(spd0, spd1, ax, az float32, brk uint8) (label string, peakL, peakR int) {
		syn := NewSynthesizer()
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: spd0, CurrentRpm: 3200, EngineMaxRpm: 8000, RpmRatio: 0.40,
			CarOrdinal: 1001, Cylinders: 6, DriveTrain: 2, CarPI: 750, Gear: 2, Accel: 100, Brake: brk,
			NormSusp: [4]float32{0.45, 0.45, 0.45, 0.45}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
		}
		car := prof.Observe(pkt, true, true)
		for i := 0; i < 6; i++ {
			syn.StepPacket(pkt, cfg, car, 0.016)
		}
		pkt.SpeedKmh = spd1
		pkt.AccelX = ax
		pkt.AccelZ = az
		syn.StepPacket(pkt, cfg, car, 0.016)
		res := syn.Render8ms(pkt, cfg, car, true)
		return res.ImpactLabel, peakDev(res.GripL), peakDev(res.GripR)
	}

	// 1. Low-speed frontal bump at 15 km/h -> 9 km/h (previously missed!)
	lblLow, pLLow, pRLow := runImpact(15, 9, 0, -7.5, 0)
	if lblLow != "低速正面撞击" || pLLow < 75 || pRLow < 75 {
		t.Fatalf("low-speed front bump failed: label=%q pL=%d pR=%d", lblLow, pLLow, pRLow)
	}

	// 1b. Low-speed left side bump at 22 km/h -> crisp Left Grip ONLY, Right Grip 0!
	lblLowL, pLowLL, pLowLR := runImpact(22, 21, 8.5, 0, 0)
	if lblLowL != "低速左侧撞击" || pLowLL < 85 || pLowLR != 0 {
		t.Fatalf("low-speed left side bump failed: label=%q pL=%d pR=%d", lblLowL, pLowLL, pLowLR)
	}

	// 2. Left side hit (pushed right: ax=+20) -> 100% Left Grip, PURE 0% SILENCE on Right Grip!
	lblL, pLL, pLR := runImpact(80, 78, 20, 0, 0)
	if lblL != "左侧撞击" || pLL < 100 || pLR != 0 {
		t.Fatalf("left side impact isolation failed: label=%q pL=%d pR=%d (want pR==0)", lblL, pLL, pLR)
	}

	// 3. Right side hit (pushed left: ax=-20) -> 100% Right Grip, PURE 0% SILENCE on Left Grip!
	lblR, pRL, pRR := runImpact(80, 78, -20, 0, 0)
	if lblR != "右侧撞击" || pRR < 100 || pRL != 0 {
		t.Fatalf("right side impact isolation failed: label=%q pL=%d pR=%d (want pL==0)", lblR, pRL, pRR)
	}

	// 4. High-speed frontal crash while full braking (brk=255)
	lblF, pFL, pFR := runImpact(140, 110, 0, -28, 255)
	if lblF != "正面撞击" || pFL < 100 || pFR < 100 {
		t.Fatalf("braking front crash failed: label=%q pL=%d pR=%d", lblF, pFL, pFR)
	}

	// 5. Rear-end impact even at low speed
	lblB, pBL, pBR := runImpact(10, 18, 0, 14, 0)
	if pBL < 80 || pBR < 80 {
		t.Fatalf("rear impact failed: label=%q pL=%d pR=%d", lblB, pBL, pBR)
	}
	t.Logf("Impacts Verified: LowSpd(%d/%d) Front(%d/%d) Left(%d/%d) Right(%d/%d) Rear(%d/%d)",
		pLLow, pRLow, pFL, pFR, pLL, pLR, pRL, pRR, pBL, pBR)
}

func TestCollisionNoAftershockAndCrispFrequency(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")
	car := prof.ApplyDemoArchetype("electric_street") // quiet engine so we measure pure collision!
	syn := NewSynthesizer()

	pkt := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 130, CurrentRpm: 0, EngineMaxRpm: 12000,
		Gear: 1, Accel: 100, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
	}
	for i := 0; i < 6; i++ {
		syn.StepPacket(pkt, cfg, car, 0.016)
		syn.Render8ms(pkt, cfg, car, true)
	}

	// Simulate a violent 130 -> 0 km/h frontal wall crash followed by bounce-back & full stop (1.5 seconds total!)
	crashProfile := []struct{ spd, ax, az float32 }{
		{90, 0, -55}, {45, 0, -48}, {15, 0, -32}, {3, 0, -14}, {0, 0, -4},
		{0, 0, 2}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0},
	}
	var activeMs []int
	var waveSamples []float64
	triggers := 0
	prevActive := false
	for frame := 0; frame < 95; frame++ { // ~1.52 seconds at 60Hz
		if frame < len(crashProfile) {
			pkt.SpeedKmh = crashProfile[frame].spd
			pkt.AccelX = crashProfile[frame].ax
			pkt.AccelZ = crashProfile[frame].az
		} else {
			pkt.SpeedKmh = 0
			pkt.AccelX = 0
			pkt.AccelZ = 0
		}
		syn.StepPacket(pkt, cfg, car, 0.016)
		for sub := 0; sub < 2; sub++ {
			res := syn.Render8ms(pkt, cfg, car, true)
			tMs := (frame*2 + sub) * 8
			if res.ImpactActive {
				if !prevActive {
					triggers++
				}
				activeMs = append(activeMs, tMs)
				for _, b := range res.GripL {
					waveSamples = append(waveSamples, float64(b)-128.0)
				}
			}
			prevActive = res.ImpactActive
		}
	}

	if triggers != 1 {
		t.Fatalf("expected EXACTLY 1 collision trigger (zero post-crash re-trigger!), got %d", triggers)
	}
	lastActiveMs := activeMs[len(activeMs)-1]
	if lastActiveMs > 210 {
		t.Fatalf("collision lingered too long after impact: lastActiveMs=%d (want <=210ms)", lastActiveMs)
	}
	// Estimate collision frequency in first 120ms: must be crisp >= 65 Hz (NOT sluggish 20-35 Hz!)
	n := len(waveSamples)
	if n > 120 {
		n = 120
	}
	zc, lastSign := 0, 0
	for _, v := range waveSamples[:n] {
		if v > 3 {
			if lastSign < 0 {
				zc++
			}
			lastSign = 1
		} else if v < -3 {
			if lastSign > 0 {
				zc++
			}
			lastSign = -1
		}
	}
	estHz := float64(zc) / 2.0 / (float64(n) / 1000.0)
	if estHz < 65.0 {
		t.Fatalf("collision frequency too low/sluggish: %.1f Hz (want >= 65 Hz)", estHz)
	}

	// Now test Left-Side slam -> rebound (ax: 0 -> +45 -> +20 -> 0): must NOT trigger a phantom Right-Side impact on release!
	synS := NewSynthesizer()
	pktS := telemetry.Packet{IsRaceOn: true, SpeedKmh: 90, Gear: 3, Accel: 120, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5}}
	for i := 0; i < 6; i++ {
		synS.StepPacket(pktS, cfg, car, 0.016)
		synS.Render8ms(pktS, cfg, car, true)
	}
	sideSeq := []float32{45, 38, 25, 14, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	sideTriggers := 0
	prevSide := false
	for _, ax := range sideSeq {
		pktS.AccelX = ax
		synS.StepPacket(pktS, cfg, car, 0.016)
		for sub := 0; sub < 2; sub++ {
			r := synS.Render8ms(pktS, cfg, car, true)
			if r.ImpactActive && !prevSide {
				sideTriggers++
				if r.ImpactLabel != "左侧撞击" {
					t.Fatalf("unexpected phantom impact label on left crash recovery: %q", r.ImpactLabel)
				}
			}
			prevSide = r.ImpactActive
		}
	}
	if sideTriggers != 1 {
		t.Fatalf("left-side crash triggered %d times (expected 1, zero phantom right rebound!)", sideTriggers)
	}
	t.Logf("Crisp Collision Verified: triggers=%d, endsAt=%dms, estFreq=%.1fHz | LeftCrashRecovery triggers=%d", triggers, lastActiveMs, estHz, sideTriggers)
}

func TestDirectionalGuardrailScrape(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")
	car := prof.ApplyDemoArchetype("rwd_drift_jdm")
	syn := NewSynthesizer()

	// Simulate rubbing LEFT guardrail at 60 km/h: steering slightly left (Steer=-25) while wall pushes right (AccelX oscillating +3..+6)
	pkt := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 60, CurrentRpm: 4000, EngineMaxRpm: 8000, RpmRatio: 0.50,
		Gear: 3, Accel: 120, Steer: -25, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
	}
	axSeq := []float32{0, 3.2, 5.8, 2.8, 5.5, 3.0, 5.6}
	var last RenderResult
	for _, ax := range axSeq {
		pkt.AccelX = ax
		syn.StepPacket(pkt, cfg, car, 0.016)
		last = syn.Render8ms(pkt, cfg, car, true)
	}
	pL, pR := peakDev(last.GripL), peakDev(last.GripR)
	if pL < 95 || pR != 0 {
		t.Fatalf("left guardrail scrape isolation failed: pL=%d pR=%d (want pL>=95, pR==0)", pL, pR)
	}
	t.Logf("Left Guardrail Verified: label=%s pL=%d pR=%d", last.ImpactLabel, pL, pR)
}

func TestFourTireUndersteerAndOversteerRouting(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")
	car := prof.ApplyDemoArchetype("rwd_drift_jdm")

	// 1. Left-Front Understeer while trail-braking into corner -> Left Grip >> Right Grip AND Left Trigger active!
	synU := NewSynthesizer()
	pU := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 90, CurrentRpm: 4800, EngineMaxRpm: 8000, RpmRatio: 0.60,
		Gear: 3, Accel: 0, Brake: 110, Steer: 85, AccelX: 8.0,
		WheelRotSpeed: [4]float32{48, 52, 54, 54},
		SlipRatio:     [4]float32{-0.18, -0.05, 0, 0},
		CombinedSlip:  [4]float32{1.55, 0.65, 0.25, 0.25}, // FL slipping much more than FR!
		NormSusp:      [4]float32{0.82, 0.35, 0.55, 0.35},
	}
	synU.StepPacket(pU, cfg, car, 0.016)
	synU.StepPacket(pU, cfg, car, 0.016)
	_ = synU.Render8ms(pU, cfg, car, true)
	resU := synU.Render8ms(pU, cfg, car, true)
	if resU.GripLRms <= resU.GripRRms*1.8 || resU.TrigRoute != 0 || peakDev(resU.Trig) < 22 || peakDev(resU.Trig) > 68 {
		t.Fatalf("FL understeer routing failed: RMS L=%.1f R=%.1f trigRoute=%d trigPeak=%d",
			resU.GripLRms, resU.GripRRms, resU.TrigRoute, peakDev(resU.Trig))
	}

	// 2. Right-Front Power-Understeer under throttle -> Right Grip >> Left Grip AND Right Trigger active!
	synUR := NewSynthesizer()
	pUR := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 88, CurrentRpm: 5000, EngineMaxRpm: 8000, RpmRatio: 0.62,
		Gear: 3, Accel: 180, Brake: 0, Steer: -85, AccelX: -8.0,
		WheelRotSpeed: [4]float32{54, 62, 52, 52},
		CombinedSlip:  [4]float32{0.58, 1.55, 0.22, 0.22}, // FR slipping much more than FL!
		NormSusp:      [4]float32{0.34, 0.84, 0.35, 0.55},
	}
	synUR.StepPacket(pUR, cfg, car, 0.016)
	synUR.StepPacket(pUR, cfg, car, 0.016)
	_ = synUR.Render8ms(pUR, cfg, car, true)
	resUR := synUR.Render8ms(pUR, cfg, car, true)
	if resUR.GripRRms <= resUR.GripLRms*1.8 || resUR.TrigRoute != 1 || peakDev(resUR.Trig) < 22 || peakDev(resUR.Trig) > 68 {
		t.Fatalf("FR power-understeer routing failed: RMS L=%.1f R=%.1f trigRoute=%d trigPeak=%d",
			resUR.GripLRms, resUR.GripRRms, resUR.TrigRoute, peakDev(resUR.Trig))
	}

	// 3. Right-Rear Oversteer under throttle -> Right Grip > Left Grip AND Right Trigger active!
	synO := NewSynthesizer()
	pO := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 85, CurrentRpm: 5500, EngineMaxRpm: 8000, RpmRatio: 0.68,
		Gear: 3, Accel: 220, Brake: 0, Steer: 40, AccelX: -5.5, AngVelY: -0.8,
		WheelRotSpeed: [4]float32{45, 45, 58, 74},
		CombinedSlip:  [4]float32{0.28, 0.28, 0.70, 1.65}, // RR slipping much more than RL!
		SlipAngle:     [4]float32{0.10, 0.10, 0.40, 0.72},
		NormSusp:      [4]float32{0.40, 0.55, 0.35, 0.78},
	}
	synO.StepPacket(pO, cfg, car, 0.016)
	synO.StepPacket(pO, cfg, car, 0.016)
	_ = synO.Render8ms(pO, cfg, car, true)
	resO := synO.Render8ms(pO, cfg, car, true)
	if resO.GripRRms <= resO.GripLRms*1.3 || resO.TrigRoute != 1 || peakDev(resO.Trig) < 22 || peakDev(resO.Trig) > 68 {
		t.Fatalf("RR oversteer routing failed: RMS L=%.1f R=%.1f trigRoute=%d trigPeak=%d",
			resO.GripLRms, resO.GripRRms, resO.TrigRoute, peakDev(resO.Trig))
	}

	// 4. Left-Rear Handbrake Oversteer -> Left Grip > Right Grip AND Left Trigger active!
	synOL := NewSynthesizer()
	pOL := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 72, CurrentRpm: 4200, EngineMaxRpm: 8000, RpmRatio: 0.52,
		Gear: 2, Accel: 0, Brake: 0, Handbrake: 200, Steer: -60, AccelX: 5.5, AngVelY: 0.9,
		WheelRotSpeed: [4]float32{42, 42, 12, 22},
		CombinedSlip:  [4]float32{0.25, 0.25, 1.65, 0.65}, // RL slipping much more than RR!
		SlipAngle:     [4]float32{0.10, 0.10, 0.78, 0.40},
		NormSusp:      [4]float32{0.55, 0.38, 0.80, 0.34},
	}
	synOL.StepPacket(pOL, cfg, car, 0.016)
	synOL.StepPacket(pOL, cfg, car, 0.016)
	_ = synOL.Render8ms(pOL, cfg, car, true)
	resOL := synOL.Render8ms(pOL, cfg, car, true)
	if resOL.GripLRms <= resOL.GripRRms*1.3 || resOL.TrigRoute != 0 || peakDev(resOL.Trig) < 22 || peakDev(resOL.Trig) > 68 {
		t.Fatalf("RL handbrake oversteer routing failed: RMS L=%.1f R=%.1f trigRoute=%d trigPeak=%d",
			resOL.GripLRms, resOL.GripRRms, resOL.TrigRoute, peakDev(resOL.Trig))
	}

	// 5. Four-wheel drift (all 4 tires sliding) -> BOTH triggers (0x9E, TrigRoute==2)
	syn4 := NewSynthesizer()
	p4 := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 95, CurrentRpm: 6000, EngineMaxRpm: 8000, RpmRatio: 0.75,
		Gear: 3, Accel: 200, Brake: 80, Steer: -70, AccelX: -6.0, AngVelY: 0.7,
		WheelRotSpeed: [4]float32{50, 52, 65, 68},
		CombinedSlip:  [4]float32{1.20, 1.15, 1.35, 1.30},
		NormSusp:      [4]float32{0.60, 0.60, 0.60, 0.60},
	}
	syn4.StepPacket(p4, cfg, car, 0.016)
	syn4.StepPacket(p4, cfg, car, 0.016)
	_ = syn4.Render8ms(p4, cfg, car, true)
	res4 := syn4.Render8ms(p4, cfg, car, true)
	if res4.Flags != 0x9E || res4.TrigRoute != 2 || peakDev(res4.Trig) > 68 {
		t.Fatalf("4-wheel drift dual-trigger routing failed: flags=0x%02X trigRoute=%d trigPeak=%d", res4.Flags, res4.TrigRoute, peakDev(res4.Trig))
	}
	t.Logf("4-Tire Routing Verified: FL-Under(%.1f/%.1f,TL=%d) FR-Under(%.1f/%.1f,TR=%d) RR-Over(%.1f/%.1f,TR=%d) RL-HB(%.1f/%.1f,TL=%d) 4WD-Drift(0x%02X)",
		resU.GripLRms, resU.GripRRms, peakDev(resU.Trig),
		resUR.GripLRms, resUR.GripRRms, peakDev(resUR.Trig),
		resO.GripLRms, resO.GripRRms, peakDev(resO.Trig),
		resOL.GripLRms, resOL.GripRRms, peakDev(resOL.Trig),
		res4.Flags)
}

func TestEngineFamiliesWideContrast(t *testing.T) {
	cfg := DefaultNextConfig()
	measureEngine := func(archID string) (avgRms float64, estHz float64) {
		prof := NewProfiler("")
		car := prof.ApplyDemoArchetype(archID)
		syn := NewSynthesizer()
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 110, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
			Gear: 3, Accel: 200, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
		}
		var samples []float64
		for i := 0; i < 16; i++ {
			syn.StepPacket(pkt, cfg, car, 0.008)
			res := syn.Render8ms(pkt, cfg, car, true)
			avgRms += res.GripLRms
			for _, b := range res.GripL {
				samples = append(samples, float64(b)-128.0)
			}
		}
		avgRms /= 16.0
		zc := 0
		lastSign := 0
		for _, v := range samples {
			if v > 0.5 {
				if lastSign < 0 {
					zc++
				}
				lastSign = 1
			} else if v < -0.5 {
				if lastSign > 0 {
					zc++
				}
				lastSign = -1
			}
		}
		estHz = float64(zc) / 2.0 / (float64(len(samples)) / 1000.0)
		return avgRms, estHz
	}

	rmsEV, hzEV := measureEngine("electric_street")
	rmsSmall, hzSmall := measureEngine("fwd_classic_economy")
	rmsV8, hzV8 := measureEngine("v8_rwd_muscle")
	rmsRot, hzRot := measureEngine("rotary_screamer")

	t.Logf("Engine Contrast: EV(rms=%.1f, %.0fHz) | SmallNA(rms=%.1f, %.0fHz) | MuscleV8(rms=%.1f, %.0fHz) | Rotary(rms=%.1f, %.0fHz)",
		rmsEV, hzEV, rmsSmall, hzSmall, rmsV8, hzV8, rmsRot, hzRot)

	if rmsV8 < rmsEV*2.0 || rmsV8 < rmsSmall*1.7 {
		t.Fatalf("expected Muscle V8 amplitude to be dramatically stronger than EV/SmallNA: V8=%.1f Small=%.1f EV=%.1f", rmsV8, rmsSmall, rmsEV)
	}
	if hzRot < hzV8*1.8 {
		t.Fatalf("expected Rotary frequency to be dramatically higher than Muscle V8: Rotary=%.0fHz V8=%.0fHz", hzRot, hzV8)
	}
}

func TestReplayRealRecordingsRegression(t *testing.T) {
	recRoot := os.Getenv("FORZA_RECORDINGS_DIR")
	if recRoot == "" {
		t.Skip("FORZA_RECORDINGS_DIR not set; skipping local recording replay regression")
	}
	if _, err := os.Stat(recRoot); err != nil {
		t.Skip("recordings directory not present")
	}
	type recPkt struct {
		ReceiveMonotonicNs int64  `json:"receiveMonotonicNs"`
		Hex                string `json:"hex"`
	}
	replayFolder := func(name string) (frames, dirtHits, curbHits, landHits, impactHits, clipped, maxTrig int, avgRms, satPct, cruiseSatPct float64, car CarPersonality) {
		f, err := os.Open(filepath.Join(recRoot, name, "packets.jsonl"))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		defer f.Close()
		cfg := DefaultNextConfig()
		prof := NewProfiler("")
		syn := NewSynthesizer()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		var prevNs int64
		rmsSum := 0.0
		satFrames := 0
		cruiseFrames := 0
		cruiseSatFrames := 0
		for sc.Scan() {
			var rp recPkt
			if err := json.Unmarshal(sc.Bytes(), &rp); err != nil {
				continue
			}
			raw, err := hex.DecodeString(rp.Hex)
			if err != nil {
				continue
			}
			pkt, err := telemetry.Parse(raw)
			if err != nil || !pkt.IsRaceOn {
				continue
			}
			dt := 0.016
			if prevNs > 0 {
				dt = float64(rp.ReceiveMonotonicNs-prevNs) / 1e9
			}
			prevNs = rp.ReceiveMonotonicNs
			car = prof.Observe(pkt, true, true)
			syn.StepPacket(pkt, cfg, car, dt)
			// Match real-time 125 Hz render loop (2x 8ms frames per 16ms UDP packet)
			res1 := syn.Render8ms(pkt, cfg, car, true)
			if pt := peakDev(res1.Trig); pt > maxTrig {
				maxTrig = pt
			}
			res := syn.Render8ms(pkt, cfg, car, true)
			if pt := peakDev(res.Trig); pt > maxTrig {
				maxTrig = pt
			}
			frames++
			rmsSum += 0.5 * (res.GripLRms + res.GripRRms)
			gripPeak := peakDev(res.GripL)
			if pr := peakDev(res.GripR); pr > gripPeak {
				gripPeak = pr
			}
			if gripPeak >= 105 {
				satFrames++
			}
			if res.Clipped || res1.Clipped {
				clipped++
			}
			if res.ImpactActive {
				impactHits++
			}
			hasDynamicPunch := res.ImpactActive
			for _, ev := range res.ActiveEvents {
				switch ev.ID {
				case "roadSurface":
					dirtHits++
				case "curbRumble":
					curbHits++
					hasDynamicPunch = true
				case "jumpLanding":
					landHits++
					hasDynamicPunch = true
				case "redline", "gearShift", "collisionWall", "guardrail", "smashable", "waterSplash":
					hasDynamicPunch = true
				}
			}
			if !hasDynamicPunch {
				cruiseFrames++
				if gripPeak >= 105 {
					cruiseSatFrames++
				}
			}
		}
		if frames > 0 {
			avgRms = rmsSum / float64(frames)
			satPct = float64(satFrames) / float64(frames) * 100.0
		}
		if cruiseFrames > 0 {
			cruiseSatPct = float64(cruiseSatFrames) / float64(cruiseFrames) * 100.0
		}
		return
	}

	f1, dirt1, _, _, _, clip1, trig1, rms1, sat1, cSat1, car1 := replayFolder("非铺装握把震动似乎没效果")
	t.Logf("[Dirt] frames=%d dirtHits=%d (%.1f%%) avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% maxTrig=%d clipped=%d car=%s",
		f1, dirt1, float64(dirt1)/math.Max(float64(f1), 1)*100, rms1, sat1, cSat1, trig1, clip1, car1.Label)
	if dirt1 < 2500 || rms1 < 18.0 || rms1 > 48.0 || sat1 >= 10.0 || cSat1 >= 2.0 || clip1 != 0 || trig1 > 68 {
		t.Fatalf("dirt regression failed: dirt=%d avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% clipped=%d maxTrig=%d",
			dirt1, rms1, sat1, cSat1, clip1, trig1)
	}

	f2, _, _, land2, imp2, clip2, trig2, rms2, sat2, cSat2, car2 := replayFolder("session-39036-4540600071600_Cyberpangk_高速公路环道_370Z-Nismo_丢失小幅度飞坡落地底盘振动")
	t.Logf("[370Z] frames=%d landHits=%d impactHits=%d avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% maxTrig=%d clipped=%d redline=%.3f learned=%v",
		f2, land2, imp2, rms2, sat2, cSat2, trig2, clip2, car2.RedlineRatio, car2.RedlineLearned)
	if land2 < 80 || imp2 < 10 || imp2 > 1200 || !car2.RedlineLearned || rms2 < 18.0 || rms2 > 52.0 || sat2 >= 24.0 || cSat2 >= 2.0 || clip2 != 0 || trig2 > 68 {
		t.Fatalf("370Z regression failed: land2=%d imp2=%d avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% clipped=%d maxTrig=%d learned=%v",
			land2, imp2, rms2, sat2, cSat2, clip2, trig2, car2.RedlineLearned)
	}

	f3, _, curb3, _, imp3, clip3, trig3, rms3, sat3, cSat3, _ := replayFolder("路肩“游龙”")
	t.Logf("[Curb] frames=%d curbHits=%d impactHits=%d avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% maxTrig=%d clipped=%d",
		f3, curb3, imp3, rms3, sat3, cSat3, trig3, clip3)
	if curb3 < 350 || imp3 > 120 || rms3 < 24.0 || rms3 > 52.0 || sat3 >= 22.0 || cSat3 >= 2.0 || clip3 != 0 || trig3 > 68 {
		t.Fatalf("curb regression failed: curb3=%d imp3=%d avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% clipped=%d maxTrig=%d",
			curb3, imp3, rms3, sat3, cSat3, clip3, trig3)
	}

	f4, _, _, _, imp4, clip4, trig4, rms4, sat4, cSat4, _ := replayFolder("session-32640-5063531786500")
	t.Logf("[WallSession] frames=%d impactHits=%d avgRMS=%.1f satPct=%.2f%% cruiseSat=%.2f%% maxTrig=%d clipped=%d",
		f4, imp4, rms4, sat4, cSat4, trig4, clip4)
	if imp4 < 40 || imp4 > 950 || cSat4 >= 2.0 || clip4 != 0 || trig4 > 68 {
		t.Fatalf("wall session regression failed: imp4=%d cruiseSat=%.2f%% clip4=%d maxTrig=%d", imp4, cSat4, clip4, trig4)
	}
}

func assertTrigBounds(t *testing.T, label string, trig [8]byte) int {
	t.Helper()
	for i, b := range trig {
		if b < 60 || b > 196 {
			t.Fatalf("%s trigger sample[%d]=%d out of safe anti-clatter bounds [60, 196] (peakDev=%d > 68)",
				label, i, b, peakDev(trig))
		}
	}
	return peakDev(trig)
}

func estimateActiveFreqHz(samples []float64) (hz float64, activeMs int) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	for i := 0; i < n; i++ {
		lo := i - 2
		if lo < 0 {
			lo = 0
		}
		hi := i + 2
		if hi >= n {
			hi = n - 1
		}
		active := false
		for j := lo; j <= hi; j++ {
			if math.Abs(samples[j]) >= 3.0 {
				active = true
				break
			}
		}
		if active {
			activeMs++
		}
	}
	if activeMs == 0 {
		return 0, 0
	}
	zc, lastSign := 0, 0
	for _, v := range samples {
		if v > 2.0 {
			if lastSign < 0 {
				zc++
			}
			lastSign = 1
		} else if v < -2.0 {
			if lastSign > 0 {
				zc++
			}
			lastSign = -1
		}
	}
	return (float64(zc) / 2.0) / (float64(activeMs) / 1000.0), activeMs
}

func TestTriggerAntiClatterSafety(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")
	car := prof.ApplyDemoArchetype("rwd_drift_jdm")

	// 1. Live gearShift: strictly bipolar damped AC impulse (~32ms), zero DC bias, cleanly settled by 36-40ms!
	{
		// 1a. Verify default config (with ThrottleMicroFeel active) never exceeds [60, 196]
		synDef := NewSynthesizer()
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 95, CurrentRpm: 6800, EngineMaxRpm: 8000, RpmRatio: 0.85,
			Gear: 2, Accel: 220, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
		}
		for i := 0; i < 4; i++ {
			synDef.StepPacket(pkt, cfg, car, 0.016)
			assertTrigBounds(t, "live_gearShift_default_warmup", synDef.Render8ms(pkt, cfg, car, true).Trig)
		}
		pkt.Gear = 3
		synDef.StepPacket(pkt, cfg, car, 0.016)
		for f := 0; f < 6; f++ {
			if f > 0 && f%2 == 0 {
				synDef.StepPacket(pkt, cfg, car, 0.016)
			}
			res := synDef.Render8ms(pkt, cfg, car, true)
			assertTrigBounds(t, "live_gearShift_default", res.Trig)
		}

		// 1b. Isolate pure gearShift trigger waveform over 40ms (5 frames) + settlement check (6th frame)
		cfgShift := DefaultNextConfig()
		cfgShift.Trigger.ThrottleMicroFeel = 0
		synS := NewSynthesizer()
		pktS := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 95, CurrentRpm: 6800, EngineMaxRpm: 8000, RpmRatio: 0.85,
			Gear: 2, Accel: 220, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
		}
		for i := 0; i < 4; i++ {
			synS.StepPacket(pktS, cfgShift, car, 0.016)
			_ = synS.Render8ms(pktS, cfgShift, car, true)
		}
		pktS.Gear = 3
		synS.StepPacket(pktS, cfgShift, car, 0.016)

		var shiftSamples []byte
		for f := 0; f < 6; f++ { // 48ms total: 5 frames (40ms) + 6th frame (40..48ms)
			if f > 0 && f%2 == 0 {
				synS.StepPacket(pktS, cfgShift, car, 0.016)
			}
			res := synS.Render8ms(pktS, cfgShift, car, true)
			assertTrigBounds(t, "live_gearShift_bipolar", res.Trig)
			shiftSamples = append(shiftSamples, res.Trig[:]...)
		}

		maxS, minS := byte(128), byte(128)
		sum40 := 0.0
		for _, b := range shiftSamples[:40] {
			if b > maxS {
				maxS = b
			}
			if b < minS {
				minS = b
			}
			sum40 += float64(b)
		}
		dcMean := sum40 / 40.0
		if maxS <= 128+12 || minS >= 128-12 {
			t.Fatalf("gearShift trigger not bipolar AC impulse: max=%d (want >140) min=%d (want <116)", maxS, minS)
		}
		if math.Abs(dcMean-128.0) >= 12.0 {
			t.Fatalf("gearShift trigger has excessive DC bias: mean=%.2f (|mean-128|=%.2f, want <12)", dcMean, math.Abs(dcMean-128.0))
		}
		tail36 := 0
		for _, b := range shiftSamples[36:40] {
			if d := int(math.Abs(float64(b) - 128.0)); d > tail36 {
				tail36 = d
			}
		}
		tail40 := 0
		for _, b := range shiftSamples[40:48] {
			if d := int(math.Abs(float64(b) - 128.0)); d > tail40 {
				tail40 = d
			}
		}
		if tail36 > 8 || tail40 > 4 {
			t.Fatalf("gearShift trigger did not cleanly settle after 36-40ms: dev[36..40ms]=%d dev[40..48ms]=%d (NO 80ms DC push!)", tail36, tail40)
		}
		t.Logf("gearShift Bipolar AC Verified: min=%d max=%d dcMean=%.2f tail36=%d tail40=%d", minS, maxS, dcMean, tail36, tail40)
	}

	// 2. All other Live Render8ms Trigger Regimes (64ms = 8 frames each after 16ms ramp warmup)
	type trigCase struct {
		name         string
		wantRoute    int
		minPeak      int
		checkResFreq bool
		setupWarmup  bool
		pkt          telemetry.Packet
	}
	cases := []trigCase{
		{
			name: "brakingAbs", wantRoute: 0, minPeak: 15, checkResFreq: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 120, CurrentRpm: 4500, EngineMaxRpm: 8000, RpmRatio: 0.56,
				Gear: 3, Accel: 0, Brake: 200,
				WheelRotSpeed: [4]float32{45, 45, 45, 45},
				SlipRatio:     [4]float32{-0.60, -0.60, -0.55, -0.55},
				CombinedSlip:  [4]float32{0.95, 0.95, 0.85, 0.85},
				NormSusp:      [4]float32{0.72, 0.72, 0.35, 0.35},
			},
		},
		{
			name: "wheelLock", wantRoute: 0, minPeak: 20, checkResFreq: true, setupWarmup: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 100, CurrentRpm: 3500, EngineMaxRpm: 8000, RpmRatio: 0.44,
				Gear: 2, Accel: 0, Brake: 240,
				WheelRotSpeed: [4]float32{0.2, 0.2, 10, 10},
				SlipRatio:     [4]float32{-0.95, -0.95, -0.60, -0.60},
				CombinedSlip:  [4]float32{1.60, 1.60, 0.90, 0.90},
				NormSusp:      [4]float32{0.80, 0.80, 0.30, 0.30},
			},
		},
		{
			name: "understeerFL", wantRoute: 0, minPeak: 20, checkResFreq: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 90, CurrentRpm: 4800, EngineMaxRpm: 8000, RpmRatio: 0.60,
				Gear: 3, Accel: 0, Brake: 110, Steer: 85, AccelX: 8.0,
				WheelRotSpeed: [4]float32{48, 52, 54, 54},
				SlipRatio:     [4]float32{-0.18, -0.05, 0, 0},
				CombinedSlip:  [4]float32{1.55, 0.65, 0.25, 0.25},
				NormSusp:      [4]float32{0.82, 0.35, 0.55, 0.35},
			},
		},
		{
			name: "understeerFR", wantRoute: 1, minPeak: 20, checkResFreq: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 88, CurrentRpm: 5000, EngineMaxRpm: 8000, RpmRatio: 0.62,
				Gear: 3, Accel: 180, Brake: 0, Steer: -85, AccelX: -8.0,
				WheelRotSpeed: [4]float32{54, 62, 52, 52},
				CombinedSlip:  [4]float32{0.58, 1.55, 0.22, 0.22},
				NormSusp:      [4]float32{0.34, 0.84, 0.35, 0.55},
			},
		},
		{
			name: "oversteerRL", wantRoute: 0, minPeak: 20, checkResFreq: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 72, CurrentRpm: 4200, EngineMaxRpm: 8000, RpmRatio: 0.52,
				Gear: 2, Accel: 0, Brake: 0, Handbrake: 200, Steer: -60, AccelX: 5.5, AngVelY: 0.9,
				WheelRotSpeed: [4]float32{42, 42, 12, 22},
				CombinedSlip:  [4]float32{0.25, 0.25, 1.65, 0.65},
				SlipAngle:     [4]float32{0.10, 0.10, 0.78, 0.40},
				NormSusp:      [4]float32{0.55, 0.38, 0.80, 0.34},
			},
		},
		{
			name: "oversteerRR", wantRoute: 1, minPeak: 20, checkResFreq: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 85, CurrentRpm: 5500, EngineMaxRpm: 8000, RpmRatio: 0.68,
				Gear: 3, Accel: 220, Brake: 0, Steer: 40, AccelX: -5.5, AngVelY: -0.8,
				WheelRotSpeed: [4]float32{45, 45, 58, 74},
				CombinedSlip:  [4]float32{0.28, 0.28, 0.70, 1.65},
				SlipAngle:     [4]float32{0.10, 0.10, 0.40, 0.72},
				NormSusp:      [4]float32{0.40, 0.55, 0.35, 0.78},
			},
		},
		{
			name: "launchSpin", wantRoute: 1, minPeak: 18, checkResFreq: true,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 20, CurrentRpm: 6200, EngineMaxRpm: 8000, RpmRatio: 0.78,
				Gear: 1, Accel: 255, Brake: 0,
				WheelRotSpeed: [4]float32{12, 12, 48, 48},
				CombinedSlip:  [4]float32{0.10, 0.10, 1.40, 1.40},
				NormSusp:      [4]float32{0.35, 0.35, 0.75, 0.75},
			},
		},
		{
			name: "brakeResistance", wantRoute: 0, minPeak: 2, checkResFreq: false,
			pkt: telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 100, CurrentRpm: 3800, EngineMaxRpm: 8000, RpmRatio: 0.48,
				Gear: 3, Accel: 0, Brake: 180,
				WheelRotSpeed: [4]float32{45, 45, 45, 45},
				SlipRatio:     [4]float32{-0.15, -0.15, -0.10, -0.10},
				CombinedSlip:  [4]float32{0.20, 0.20, 0.20, 0.20},
				NormSusp:      [4]float32{0.65, 0.65, 0.38, 0.38},
			},
		},
	}

	for _, tc := range cases {
		syn := NewSynthesizer()
		if tc.setupWarmup {
			warm := tc.pkt
			warm.WheelRotSpeed = [4]float32{50, 50, 50, 50}
			warm.Brake = 0
			syn.StepPacket(warm, cfg, car, 0.016)
			syn.StepPacket(warm, cfg, car, 0.016)
		}
		syn.StepPacket(tc.pkt, cfg, car, 0.016)
		syn.StepPacket(tc.pkt, cfg, car, 0.016)
		// 2 warmup frames (16ms) for 10ms soft attack ramp, still checking safe bounds on every sample!
		for w := 0; w < 2; w++ {
			assertTrigBounds(t, tc.name+"_warmup", syn.Render8ms(tc.pkt, cfg, car, true).Trig)
		}

		// Measure 8 frames (64ms)
		var samples []float64
		maxPeak := 0
		lastRoute := -1
		for f := 0; f < 8; f++ {
			if f%2 == 0 {
				syn.StepPacket(tc.pkt, cfg, car, 0.016)
			}
			res := syn.Render8ms(tc.pkt, cfg, car, true)
			pk := assertTrigBounds(t, tc.name, res.Trig)
			if pk > maxPeak {
				maxPeak = pk
			}
			lastRoute = res.TrigRoute
			for _, b := range res.Trig {
				samples = append(samples, float64(b)-128.0)
			}
		}
		if lastRoute != tc.wantRoute {
			t.Fatalf("%s wrong trigger route: got %d, want %d", tc.name, lastRoute, tc.wantRoute)
		}
		if maxPeak < tc.minPeak {
			t.Fatalf("%s trigger peak too weak: got %d, want >= %d", tc.name, maxPeak, tc.minPeak)
		}
		estHz, activeMs := estimateActiveFreqHz(samples)
		if tc.checkResFreq {
			if activeMs < 16 || estHz < 75.0 {
				t.Fatalf("%s trigger resonant frequency too low: %.1f Hz (activeMs=%d, want >= 75 Hz)", tc.name, estHz, activeMs)
			}
		}
		t.Logf("Live Trigger [%s]: route=%d maxPeak=%d estHz=%.1fHz (active=%dms)", tc.name, lastRoute, maxPeak, estHz, activeMs)
	}

	// 2b. Extreme MasterTriggerGain=1.6 stress check must still strictly obey [60, 196] hard clamp
	{
		cfgMax := DefaultNextConfig()
		cfgMax.MasterTriggerGain = 1.6
		synMax := NewSynthesizer()
		pLock := cases[1].pkt
		synMax.StepPacket(pLock, cfgMax, car, 0.016)
		synMax.StepPacket(pLock, cfgMax, car, 0.016)
		for f := 0; f < 6; f++ {
			assertTrigBounds(t, "extreme_gain_wheelLock", synMax.Render8ms(pLock, cfgMax, car, true).Trig)
		}
	}

	// 3. Test Bench triggers: gearShift, brakingAbs, understeerLeft, oversteerRight
	benchIDs := []string{"gearShift", "brakingAbs", "understeerLeft", "oversteerRight"}
	for _, id := range benchIDs {
		synB := NewSynthesizer()
		synB.TriggerTest(id)
		maxPeak := 0
		var first40 []byte
		for f := 0; f < 120 && synB.testRemainS > 0; f++ {
			res := synB.Render8ms(telemetry.Packet{}, cfg, car, true)
			pk := assertTrigBounds(t, "testBench_"+id, res.Trig)
			if pk > maxPeak {
				maxPeak = pk
			}
			if len(first40) < 40 {
				first40 = append(first40, res.Trig[:]...)
			}
		}
		if maxPeak < 15 {
			t.Fatalf("testBench %s trigger too weak: maxPeak=%d (want >= 15)", id, maxPeak)
		}
		if id == "gearShift" && len(first40) >= 40 {
			maxS, minS := byte(128), byte(128)
			sum := 0.0
			for _, b := range first40[:40] {
				if b > maxS {
					maxS = b
				}
				if b < minS {
					minS = b
				}
				sum += float64(b)
			}
			dcMean := sum / 40.0
			if maxS <= 128+12 || minS >= 128-12 || math.Abs(dcMean-128.0) >= 12.0 {
				t.Fatalf("testBench gearShift not bipolar AC: min=%d max=%d dcMean=%.2f", minS, maxS, dcMean)
			}
		}
		t.Logf("TestBench Trigger [%s]: maxPeak=%d (safe <= 68)", id, maxPeak)
	}

	// 4. Redline: Explosive multi-layer rev-limiter on Grips + Continuous Engine Carrier on Right Trigger!
	{
		synRL := NewSynthesizer()
		pRL := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 160, CurrentRpm: 7920, EngineMaxRpm: 8000, RpmRatio: 0.99,
			Gear: 4, Accel: 240, Brake: 0,
			WheelRotSpeed: [4]float32{80, 80, 80, 80},
			CombinedSlip:  [4]float32{0.10, 0.10, 0.10, 0.10},
			NormSusp:      [4]float32{0.50, 0.50, 0.50, 0.50},
		}
		synRL.StepPacket(pRL, cfg, car, 0.016)
		synRL.StepPacket(pRL, cfg, car, 0.016)
		for w := 0; w < 2; w++ {
			assertTrigBounds(t, "live_redline_warmup", synRL.Render8ms(pRL, cfg, car, true).Trig)
		}
		maxGripPeak := 0
		maxTrigPeak := 0
		maxGripRms, minGripRms := 0.0, 999.0
		sawBothChannel := false
		var trigSamples []float64
		for f := 0; f < 16; f++ { // 128ms > 2 full 16.5Hz fuel-cut cycles
			if f%2 == 0 {
				synRL.StepPacket(pRL, cfg, car, 0.016)
			}
			res := synRL.Render8ms(pRL, cfg, car, true)
			pk := assertTrigBounds(t, "live_redline_trig", res.Trig)
			if pk > maxTrigPeak {
				maxTrigPeak = pk
			}
			if res.TrigRoute != 1 || !res.RightTrigNeed || res.Flags != 0x9D {
				t.Fatalf("live redline must continue Right Trigger engine vibration: trigPeak=%d route=%d rightNeed=%v flags=0x%02X",
					pk, res.TrigRoute, res.RightTrigNeed, res.Flags)
			}
			for _, b := range res.Trig {
				trigSamples = append(trigSamples, float64(b)-128.0)
			}
			for _, ev := range res.ActiveEvents {
				if ev.ID == "redline" {
					if ev.Channel != "both" {
						t.Fatalf("live redline ActiveEvent channel = %q, want \"both\"", ev.Channel)
					}
					sawBothChannel = true
				}
			}
			if p := peakDev(res.GripL); p > maxGripPeak {
				maxGripPeak = p
			}
			if res.GripLRms > maxGripRms {
				maxGripRms = res.GripLRms
			}
			if res.GripLRms < minGripRms {
				minGripRms = res.GripLRms
			}
			if res.Clipped {
				t.Fatalf("live redline clipped grips")
			}
		}
		if !sawBothChannel {
			t.Fatalf("live redline did not emit ActiveEvent id=redline with channel=both")
		}
		if maxTrigPeak < 4 || maxTrigPeak > 12 {
			t.Fatalf("live redline right-trigger engine continuation out of silky subtle range [4, 12]: trigPeak=%d", maxTrigPeak)
		}
		trigHz, trigActiveMs := estimateActiveFreqHz(trigSamples)
		if trigActiveMs < 64 || trigHz < 50.0 || trigHz > 80.0 {
			t.Fatalf("live redline right-trigger engine carrier out of rattle-free 52-74Hz band: hz=%.1f activeMs=%d", trigHz, trigActiveMs)
		}
		if maxGripPeak < 105 || maxGripRms < 72.0 || minGripRms > maxGripRms*0.42 {
			t.Fatalf("live redline grip eruption too weak or lacking cut valley contrast: maxPeak=%d maxRms=%.1f minRms=%.1f", maxGripPeak, maxGripRms, minGripRms)
		}
		t.Logf("Live Redline (Explosive Grips + Silky 52-74Hz RT Engine): trigPeak=%d (%.1fHz) gripPeak=%d gripRms=[%.1f..%.1f]",
			maxTrigPeak, trigHz, maxGripPeak, minGripRms, maxGripRms)

		// Test Bench redline: explosive grip rev-limiter slams + continuous right-trigger engine carrier (Flags=0x9D, RightTrigNeed=true, TrigRoute=1)
		synB := NewSynthesizer()
		synB.TriggerTest("redline")
		benchGripPeak := 0
		benchTrigPeak := 0
		benchMaxRms, benchMinRms := 0.0, 999.0
		var benchTrigSamples []float64
		for f := 0; f < 120 && synB.testRemainS > 0; f++ {
			res := synB.Render8ms(telemetry.Packet{}, cfg, car, true)
			pk := assertTrigBounds(t, "testBench_redline_trig", res.Trig)
			if pk > benchTrigPeak {
				benchTrigPeak = pk
			}
			for _, b := range res.Trig {
				benchTrigSamples = append(benchTrigSamples, float64(b)-128.0)
			}
			if res.TrigRoute != 1 || !res.RightTrigNeed || res.Flags != 0x9D {
				t.Fatalf("testBench redline must continue Right Trigger engine vibration: trigPeak=%d route=%d rightNeed=%v flags=0x%02X",
					pk, res.TrigRoute, res.RightTrigNeed, res.Flags)
			}
			if len(res.ActiveEvents) == 0 || res.ActiveEvents[0].Channel != "both" {
				t.Fatalf("testBench redline ActiveEvent channel mismatch: %+v", res.ActiveEvents)
			}
			if p := peakDev(res.GripL); p > benchGripPeak {
				benchGripPeak = p
			}
			if res.GripLRms > benchMaxRms {
				benchMaxRms = res.GripLRms
			}
			if res.GripLRms < benchMinRms {
				benchMinRms = res.GripLRms
			}
			if res.Clipped {
				t.Fatalf("testBench redline clipped grips")
			}
		}
		benchTrigHz, _ := estimateActiveFreqHz(benchTrigSamples)
		if benchTrigPeak < 4 || benchTrigPeak > 12 || benchTrigHz < 50.0 || benchTrigHz > 80.0 {
			t.Fatalf("testBench redline right-trigger engine continuation out of silky 52-74Hz range: trigPeak=%d hz=%.1f", benchTrigPeak, benchTrigHz)
		}
		if benchGripPeak < 105 || benchMaxRms < 75.0 || benchMinRms > benchMaxRms*0.42 {
			t.Fatalf("testBench redline grip eruption too weak or muddy: maxPeak=%d maxRms=%.1f minRms=%.1f", benchGripPeak, benchMaxRms, benchMinRms)
		}
		t.Logf("TestBench Redline (Explosive Grips + Silky 52-74Hz RT Engine): trigPeak=%d (%.1fHz) gripPeak=%d gripRms=[%.1f..%.1f]",
			benchTrigPeak, benchTrigHz, benchGripPeak, benchMinRms, benchMaxRms)
	}
}

func TestGripClarityAndAntiMush(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")

	measureCruise := func(archID string) (avgRms float64, maxPeak int, clipped bool) {
		car := prof.ApplyDemoArchetype(archID)
		syn := NewSynthesizer()
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 110, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
			Gear: 3, Accel: 160, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
		}
		for i := 0; i < 4; i++ {
			syn.StepPacket(pkt, cfg, car, 0.016)
			_ = syn.Render8ms(pkt, cfg, car, true)
			_ = syn.Render8ms(pkt, cfg, car, true)
		}
		rmsSum := 0.0
		for f := 0; f < 16; f++ {
			if f%2 == 0 {
				syn.StepPacket(pkt, cfg, car, 0.016)
			}
			res := syn.Render8ms(pkt, cfg, car, true)
			rmsSum += 0.5 * (res.GripLRms + res.GripRRms)
			if p := peakDev(res.GripL); p > maxPeak {
				maxPeak = p
			}
			if p := peakDev(res.GripR); p > maxPeak {
				maxPeak = p
			}
			if res.Clipped {
				clipped = true
			}
		}
		return rmsSum / 16.0, maxPeak, clipped
	}

	// 1. Cruising engine feelable with generous headroom + huge 7-family contrast
	rmsJDM, peakJDM, clipJDM := measureCruise("rwd_drift_jdm")
	rmsEV, _, _ := measureCruise("electric_street")
	rmsSmall, _, _ := measureCruise("fwd_classic_economy")
	rmsV8, _, _ := measureCruise("v8_rwd_muscle")

	t.Logf("Cruising Headroom & Contrast: JDM(rms=%.1f peak=%d) | EV(%.1f) | SmallNA(%.1f) | MuscleV8(%.1f)",
		rmsJDM, peakJDM, rmsEV, rmsSmall, rmsV8)
	if rmsJDM < 15.0 || rmsJDM > 32.0 || clipJDM {
		t.Fatalf("rwd_drift_jdm @ 65%% RPM cruising RMS out of clean headroom range [15, 32]: rms=%.1f peak=%d clipped=%v",
			rmsJDM, peakJDM, clipJDM)
	}
	if rmsV8 < rmsEV*2.0 || rmsV8 < rmsSmall*1.7 {
		t.Fatalf("7-family contrast collapsed: V8=%.1f SmallNA=%.1f EV=%.1f", rmsV8, rmsSmall, rmsEV)
	}

	// 2. Curb + Active Suspension Heave simultaneously preserves "da-silence-da" valley contrast (roadDuck!)
	{
		car := prof.ApplyDemoArchetype("rwd_drift_jdm")
		syn := NewSynthesizer()
		makeCurbSuspPkt := func(step int) telemetry.Packet {
			osc := float32(0.030)
			norm := float32(0.66)
			ay := float32(5.5)
			if step%2 == 1 {
				osc = -0.030
				norm = 0.34
				ay = -5.5
			}
			return telemetry.Packet{
				IsRaceOn: true, SpeedKmh: 95, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
				Gear: 3, Accel: 160, AccelY: ay,
				RumbleStrip: [4]int32{1, 0, 1, 0}, // Left tires on curb
				NormSusp:    [4]float32{norm, norm, norm, norm},
				SuspTravelM: [4]float32{0.08 + osc, 0.08 + osc, 0.08 + osc, 0.08 + osc},
			}
		}
		// Warmup 3 packets (48ms) so curbEnvL, roadEnv, heaveEnv, engineDuck & roadDuck engage
		for step := 0; step < 3; step++ {
			p := makeCurbSuspPkt(step)
			syn.StepPacket(p, cfg, car, 0.016)
			_ = syn.Render8ms(p, cfg, car, true)
			_ = syn.Render8ms(p, cfg, car, true)
		}
		// Observe 16 frames (128ms >= 1 full 89ms curb rib cycle at 95 km/h)
		maxPeakL := 0
		maxRmsL := 0.0
		minRmsL := 999.0
		maxRmsR := 0.0
		for f := 0; f < 16; f++ {
			p := makeCurbSuspPkt(3 + f/2)
			if f%2 == 0 {
				syn.StepPacket(p, cfg, car, 0.016)
			}
			res := syn.Render8ms(p, cfg, car, true)
			if pk := peakDev(res.GripL); pk > maxPeakL {
				maxPeakL = pk
			}
			if res.GripLRms > maxRmsL {
				maxRmsL = res.GripLRms
			}
			if res.GripLRms < minRmsL {
				minRmsL = res.GripLRms
			}
			if res.GripRRms > maxRmsR {
				maxRmsR = res.GripRRms
			}
		}
		t.Logf("Curb+Susp Valley Contrast: maxPeakL=%d maxRmsL=%.1f minValleyRmsL=%.1f maxRmsR=%.1f",
			maxPeakL, maxRmsL, minRmsL, maxRmsR)
		if maxPeakL < 50 {
			t.Fatalf("curb peak too weak during simultaneous suspension heave: maxPeakL=%d (want >= 50)", maxPeakL)
		}
		if minRmsL > float64(maxPeakL)*0.45 || minRmsL > maxRmsL*0.60 {
			t.Fatalf("curb valley filled with suspension mush (roadDuck failed!): minValleyRmsL=%.1f vs maxPeakL=%d maxRmsL=%.1f",
				minRmsL, maxPeakL, maxRmsL)
		}
		if maxRmsL <= maxRmsR*1.4 {
			t.Fatalf("left curb localization washed out by suspension: maxRmsL=%.1f maxRmsR=%.1f", maxRmsL, maxRmsR)
		}
	}

	// 3. engineDuck on Muscle V8: strong understeer & curb stay crisp, un-clipped, with L/R localization intact
	{
		carV8 := prof.ApplyDemoArchetype("v8_rwd_muscle")

		// 3a. Muscle V8 + Strong FL Understeer
		synU := NewSynthesizer()
		pU := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 90, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
			Gear: 3, Accel: 0, Brake: 110, Steer: 85, AccelX: 8.0,
			WheelRotSpeed: [4]float32{48, 52, 54, 54},
			SlipRatio:     [4]float32{-0.18, -0.05, 0, 0},
			CombinedSlip:  [4]float32{1.55, 0.65, 0.25, 0.25},
			NormSusp:      [4]float32{0.82, 0.35, 0.55, 0.35},
		}
		for i := 0; i < 3; i++ {
			synU.StepPacket(pU, cfg, carV8, 0.016)
			_ = synU.Render8ms(pU, cfg, carV8, true)
			_ = synU.Render8ms(pU, cfg, carV8, true)
		}
		sumL, sumR := 0.0, 0.0
		clippedU := false
		for f := 0; f < 8; f++ {
			if f%2 == 0 {
				synU.StepPacket(pU, cfg, carV8, 0.016)
			}
			res := synU.Render8ms(pU, cfg, carV8, true)
			sumL += res.GripLRms
			sumR += res.GripRRms
			if res.Clipped {
				clippedU = true
			}
		}
		avgL, avgR := sumL/8.0, sumR/8.0
		t.Logf("MuscleV8 + FL Understeer (engineDuck): baseV8=%.1f avgL=%.1f avgR=%.1f clipped=%v", rmsV8, avgL, avgR, clippedU)
		if clippedU || avgL <= avgR*1.6 || avgR >= rmsV8*0.90 {
			t.Fatalf("MuscleV8 understeer engineDuck/clarity failed: baseV8=%.1f avgL=%.1f avgR=%.1f clipped=%v",
				rmsV8, avgL, avgR, clippedU)
		}

		// 3b. Muscle V8 + Left Curb
		synC := NewSynthesizer()
		pC := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 95, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
			Gear: 3, Accel: 160,
			RumbleStrip: [4]int32{1, 0, 1, 0},
			NormSusp:    [4]float32{0.5, 0.5, 0.5, 0.5},
			SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
		}
		for i := 0; i < 3; i++ {
			synC.StepPacket(pC, cfg, carV8, 0.016)
			_ = synC.Render8ms(pC, cfg, carV8, true)
			_ = synC.Render8ms(pC, cfg, carV8, true)
		}
		maxCL, sumCR := 0.0, 0.0
		clippedC := false
		for f := 0; f < 16; f++ {
			if f%2 == 0 {
				synC.StepPacket(pC, cfg, carV8, 0.016)
			}
			res := synC.Render8ms(pC, cfg, carV8, true)
			if res.GripLRms > maxCL {
				maxCL = res.GripLRms
			}
			sumCR += res.GripRRms
			if res.Clipped {
				clippedC = true
			}
		}
		avgCR := sumCR / 16.0
		t.Logf("MuscleV8 + Left Curb (engineDuck): baseV8=%.1f maxL=%.1f avgR=%.1f clipped=%v", rmsV8, maxCL, avgCR, clippedC)
		if clippedC || maxCL <= avgCR*1.6 || avgCR >= rmsV8*0.90 {
			t.Fatalf("MuscleV8 curb engineDuck/clarity failed: baseV8=%.1f maxL=%.1f avgR=%.1f clipped=%v",
				rmsV8, maxCL, avgCR, clippedC)
		}
	}
}

func TestStrengthenedGripFeelEvents(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")
	car := prof.ApplyDemoArchetype("rwd_drift_jdm")

	// 1. Understeer (live + Test Bench): strong grip output + clear 16Hz hop valley + stereo localization
	{
		synU := NewSynthesizer()
		pU := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 90, CurrentRpm: 4800, EngineMaxRpm: 8000, RpmRatio: 0.60,
			Gear: 3, Accel: 0, Brake: 110, Steer: 85, AccelX: 8.0,
			WheelRotSpeed: [4]float32{48, 52, 54, 54},
			SlipRatio:     [4]float32{-0.18, -0.05, 0, 0},
			CombinedSlip:  [4]float32{1.55, 0.65, 0.25, 0.25},
			NormSusp:      [4]float32{0.82, 0.35, 0.55, 0.35},
		}
		synU.StepPacket(pU, cfg, car, 0.016)
		synU.StepPacket(pU, cfg, car, 0.016)
		maxPeakL := 0
		maxRmsL, minRmsL := 0.0, 999.0
		sumL, sumR := 0.0, 0.0
		for f := 0; f < 16; f++ { // 128ms = 2 full 16Hz hop cycles
			if f%2 == 0 {
				synU.StepPacket(pU, cfg, car, 0.016)
			}
			res := synU.Render8ms(pU, cfg, car, true)
			if res.Clipped {
				t.Fatalf("live understeer clipped")
			}
			if p := peakDev(res.GripL); p > maxPeakL {
				maxPeakL = p
			}
			if res.GripLRms > maxRmsL {
				maxRmsL = res.GripLRms
			}
			if res.GripLRms < minRmsL {
				minRmsL = res.GripLRms
			}
			sumL += res.GripLRms
			sumR += res.GripRRms
		}
		avgL, avgR := sumL/16.0, sumR/16.0
		t.Logf("Understeer Live Grip: maxPeakL=%d rmsL=[%.1f..%.1f] avgL=%.1f avgR=%.1f", maxPeakL, minRmsL, maxRmsL, avgL, avgR)
		if maxPeakL < 80 || minRmsL > maxRmsL*0.50 || avgL <= avgR*2.5 {
			t.Fatalf("live understeer grip feel/clarity check failed: maxPeakL=%d rmsL=[%.1f..%.1f] avgL=%.1f avgR=%.1f",
				maxPeakL, minRmsL, maxRmsL, avgL, avgR)
		}

		synUB := NewSynthesizer()
		synUB.TriggerTest("understeerLeft")
		bPeakL, bPeakR := 0, 0
		for f := 0; f < 120 && synUB.testRemainS > 0; f++ {
			res := synUB.Render8ms(telemetry.Packet{}, cfg, car, true)
			if res.Clipped {
				t.Fatalf("testBench understeerLeft clipped")
			}
			if p := peakDev(res.GripL); p > bPeakL {
				bPeakL = p
			}
			if p := peakDev(res.GripR); p > bPeakR {
				bPeakR = p
			}
		}
		t.Logf("Understeer TestBench Grip: peakL=%d peakR=%d", bPeakL, bPeakR)
		if bPeakL < 85 || bPeakL <= bPeakR*3 {
			t.Fatalf("testBench understeerLeft grip too weak or poorly localized: peakL=%d peakR=%d", bPeakL, bPeakR)
		}
	}

	// 2. Oversteer / Drift (live + Test Bench): meaty clean 28-42Hz grip feel + stereo contrast
	{
		synO := NewSynthesizer()
		pO := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 85, CurrentRpm: 5500, EngineMaxRpm: 8000, RpmRatio: 0.68,
			Gear: 3, Accel: 220, Brake: 0, Steer: 40, AccelX: -5.5, AngVelY: -0.8,
			WheelRotSpeed: [4]float32{45, 45, 58, 74},
			CombinedSlip:  [4]float32{0.28, 0.28, 0.70, 1.65},
			SlipAngle:     [4]float32{0.10, 0.10, 0.40, 0.72},
			NormSusp:      [4]float32{0.40, 0.55, 0.35, 0.78},
		}
		synO.StepPacket(pO, cfg, car, 0.016)
		synO.StepPacket(pO, cfg, car, 0.016)
		maxPeakR := 0
		sumL, sumR := 0.0, 0.0
		for f := 0; f < 16; f++ { // 128ms > 4 full 34Hz drift cycles
			if f%2 == 0 {
				synO.StepPacket(pO, cfg, car, 0.016)
			}
			res := synO.Render8ms(pO, cfg, car, true)
			if res.Clipped {
				t.Fatalf("live oversteer clipped")
			}
			if p := peakDev(res.GripR); p > maxPeakR {
				maxPeakR = p
			}
			sumL += res.GripLRms
			sumR += res.GripRRms
		}
		avgL, avgR := sumL/16.0, sumR/16.0
		t.Logf("Oversteer Live Grip: maxPeakR=%d avgL=%.1f avgR=%.1f", maxPeakR, avgL, avgR)
		if maxPeakR < 75 || avgR < 45.0 || avgR <= avgL*2.0 {
			t.Fatalf("live oversteer grip too weak or poorly localized: maxPeakR=%d avgL=%.1f avgR=%.1f", maxPeakR, avgL, avgR)
		}

		synOB := NewSynthesizer()
		synOB.TriggerTest("oversteerLeft")
		bPeakL := 0
		bSumL, bSumR, bFrames := 0.0, 0.0, 0
		for f := 0; f < 120 && synOB.testRemainS > 0; f++ {
			res := synOB.Render8ms(telemetry.Packet{}, cfg, car, true)
			if res.Clipped {
				t.Fatalf("testBench oversteerLeft clipped")
			}
			if p := peakDev(res.GripL); p > bPeakL {
				bPeakL = p
			}
			bSumL += res.GripLRms
			bSumR += res.GripRRms
			bFrames++
		}
		bAvgL, bAvgR := bSumL/float64(bFrames), bSumR/float64(bFrames)
		t.Logf("Oversteer TestBench Grip: peakL=%d avgL=%.1f avgR=%.1f", bPeakL, bAvgL, bAvgR)
		if bPeakL < 85 || bAvgL < 34.0 || bAvgL <= bAvgR*2.0 {
			t.Fatalf("testBench oversteerLeft grip too weak: peakL=%d avgL=%.1f avgR=%.1f", bPeakL, bAvgL, bAvgR)
		}
	}

	// 3. GearShift (live + Test Bench): stronger crisp thump+click on grips while right trigger stays bipolar AC!
	{
		synG := NewSynthesizer()
		pG := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 95, CurrentRpm: 6800, EngineMaxRpm: 8000, RpmRatio: 0.85,
			Gear: 2, Accel: 220, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
		}
		for i := 0; i < 4; i++ {
			synG.StepPacket(pG, cfg, car, 0.016)
			_ = synG.Render8ms(pG, cfg, car, true)
		}
		pG.Gear = 3
		synG.StepPacket(pG, cfg, car, 0.016)
		liveShiftPeak := 0
		liveRms24 := 0.0
		for f := 0; f < 5; f++ { // 40ms
			if f > 0 && f%2 == 0 {
				synG.StepPacket(pG, cfg, car, 0.016)
			}
			res := synG.Render8ms(pG, cfg, car, true)
			assertTrigBounds(t, "live_shift_grip_check", res.Trig)
			if res.Clipped {
				t.Fatalf("live gearShift clipped")
			}
			if p := peakDev(res.GripL); p > liveShiftPeak {
				liveShiftPeak = p
			}
			if f < 3 {
				liveRms24 += res.GripLRms
			}
		}
		liveRms24 /= 3.0
		t.Logf("GearShift Live Grip: peak=%d first24msRms=%.1f", liveShiftPeak, liveRms24)
		if liveShiftPeak < 95 || liveRms24 < 60.0 {
			t.Fatalf("live gearShift grip kick too weak: peak=%d first24msRms=%.1f", liveShiftPeak, liveRms24)
		}

		synGB := NewSynthesizer()
		synGB.TriggerTest("gearShift")
		benchShiftPeak := 0
		benchRms24 := 0.0
		for f := 0; f < 35 && synGB.testRemainS > 0; f++ {
			res := synGB.Render8ms(telemetry.Packet{}, cfg, car, true)
			assertTrigBounds(t, "testBench_shift_grip_check", res.Trig)
			if res.Clipped {
				t.Fatalf("testBench gearShift clipped")
			}
			if p := peakDev(res.GripL); p > benchShiftPeak {
				benchShiftPeak = p
			}
			if f < 3 {
				benchRms24 += res.GripLRms
			}
		}
		benchRms24 /= 3.0
		t.Logf("GearShift TestBench Grip: peak=%d first24msRms=%.1f", benchShiftPeak, benchRms24)
		if benchShiftPeak < 100 || benchRms24 < 65.0 {
			t.Fatalf("testBench gearShift grip kick too weak: peak=%d first24msRms=%.1f", benchShiftPeak, benchRms24)
		}
	}

	// 4. Jump Landing (Live multi-frame touchdown compression ramp + Test Bench two-stage rebound):
	// Previously, first-contact frame (maxNorm=0.14 < 0.34) immediately cleared airborne=false and lost the landing!
	{
		synJ := NewSynthesizer()
		pJ := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 135, CurrentRpm: 5800, EngineMaxRpm: 8000, RpmRatio: 0.72,
			Gear: 4, Accel: 180, NormSusp: [4]float32{0.50, 0.50, 0.50, 0.50}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
		}
		for i := 0; i < 3; i++ {
			synJ.StepPacket(pJ, cfg, car, 0.016)
		}
		// Airborne for 6 frames (96ms)
		for i := 0; i < 6; i++ {
			pJ.NormSusp = [4]float32{0.01, 0.01, 0.01, 0.01}
			pJ.SuspTravelM = [4]float32{0.01, 0.01, 0.01, 0.01}
			pJ.AccelY = -9.5
			synJ.StepPacket(pJ, cfg, car, 0.016)
		}
		// Multi-frame touchdown compression: frame 1 touches lightly (0.14), frame 2 compresses (0.44), frame 3 bottoms out (0.88)!
		touchdownSeq := []struct {
			norm   float32
			travel float32
			ay     float32
		}{
			{0.14, 0.025, 1.5},  // First wheel contact (<0.34!)
			{0.44, 0.072, 6.2},  // Mid compression
			{0.88, 0.142, 13.5}, // Full bottom-out peak!
			{0.65, 0.105, 4.0},
			{0.52, 0.084, 0.5},
		}
		liveLandPeak := 0
		maxLandRms := 0.0
		sawLanding := false
		for _, td := range touchdownSeq {
			pJ.NormSusp = [4]float32{td.norm, td.norm, td.norm, td.norm}
			pJ.SuspTravelM = [4]float32{td.travel, td.travel, td.travel, td.travel}
			pJ.AccelY = td.ay
			synJ.StepPacket(pJ, cfg, car, 0.016)
			for sub := 0; sub < 2; sub++ {
				res := synJ.Render8ms(pJ, cfg, car, true)
				if res.Clipped {
					t.Fatalf("live jumpLanding clipped")
				}
				if p := peakDev(res.GripL); p > liveLandPeak {
					liveLandPeak = p
				}
				if res.GripLRms > maxLandRms {
					maxLandRms = res.GripLRms
				}
				for _, ev := range res.ActiveEvents {
					if ev.ID == "jumpLanding" {
						sawLanding = true
					}
				}
			}
		}
		t.Logf("JumpLanding Live Multi-Frame Touchdown: saw=%v landPeak=%.2f gripPeak=%d maxRms=%.1f",
			sawLanding, synJ.landPeak, liveLandPeak, maxLandRms)
		if !sawLanding || synJ.landPeak < 0.78 || liveLandPeak < 95 || maxLandRms < 65.0 {
			t.Fatalf("live multi-frame jumpLanding too weak or lost on first contact: saw=%v landPeak=%.2f gripPeak=%d maxRms=%.1f",
				sawLanding, synJ.landPeak, liveLandPeak, maxLandRms)
		}
	}

	// 5. Water Entry Splash (Live gradual depth ramp 0 -> 0.025 -> 0.05 -> 0.18 -> 0.34 on imp bus):
	// Previously, 0.025 frame set prevMaxPuddle >= 0.022 and blocked splashActiveS from ever triggering!
	{
		synW := NewSynthesizer()
		pW := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 105, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
			Gear: 3, Accel: 180, NormSusp: [4]float32{0.50, 0.50, 0.50, 0.50}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
		}
		for i := 0; i < 3; i++ {
			synW.StepPacket(pW, cfg, car, 0.016)
		}
		depths := []float32{0.025, 0.055, 0.18, 0.34, 0.30, 0.25}
		maxSplashGripPeak := 0
		maxSplashRms := 0.0
		for _, d := range depths {
			pW.PuddleDepth = [4]float32{d, d, d, d}
			synW.StepPacket(pW, cfg, car, 0.016)
			for sub := 0; sub < 2; sub++ {
				res := synW.Render8ms(pW, cfg, car, true)
				if res.Clipped {
					t.Fatalf("live waterSplash clipped")
				}
				if p := peakDev(res.GripL); p > maxSplashGripPeak {
					maxSplashGripPeak = p
				}
				if res.GripLRms > maxSplashRms {
					maxSplashRms = res.GripLRms
				}
			}
		}
		t.Logf("WaterSplash Live Depth Ramp: splashPeak=%.2f gripPeak=%d maxRms=%.1f",
			synW.splashPeak, maxSplashGripPeak, maxSplashRms)
		if synW.splashPeak < 0.85 || maxSplashGripPeak < 92 || maxSplashRms < 62.0 {
			t.Fatalf("live gradual water entry splash too weak: splashPeak=%.2f gripPeak=%d maxRms=%.1f",
				synW.splashPeak, maxSplashGripPeak, maxSplashRms)
		}
	}

	// 6. Live Curb Ride (Asymmetric SurfaceRumble + sparse stepJump + fat exponent 2.0 + 48-62Hz sub-thud):
	{
		synC := NewSynthesizer()
		pC := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 100, CurrentRpm: 5200, EngineMaxRpm: 8000, RpmRatio: 0.65,
			Gear: 3, Accel: 160,
			SurfaceRumble: [4]float32{0.16, 0.00, 0.15, 0.00}, // Left tires riding FH5 curb (RumbleStrip=0!)
			NormSusp:      [4]float32{0.58, 0.46, 0.57, 0.46},
			SuspTravelM:   [4]float32{0.092, 0.075, 0.090, 0.075},
		}
		maxCurbPeakL := 0
		maxCurbRmsL, minCurbRmsL := 0.0, 999.0
		for step := 0; step < 8; step++ {
			if step%4 == 0 {
				pC.SuspTravelM = [4]float32{0.104, 0.075, 0.102, 0.075}
			} else {
				pC.SuspTravelM = [4]float32{0.092, 0.075, 0.090, 0.075}
			}
			synC.StepPacket(pC, cfg, car, 0.016)
			for sub := 0; sub < 2; sub++ {
				res := synC.Render8ms(pC, cfg, car, true)
				if res.Clipped {
					t.Fatalf("live asymmetric curb clipped")
				}
				if step >= 1 {
					if p := peakDev(res.GripL); p > maxCurbPeakL {
						maxCurbPeakL = p
					}
					if res.GripLRms > maxCurbRmsL {
						maxCurbRmsL = res.GripLRms
					}
					if res.GripLRms < minCurbRmsL {
						minCurbRmsL = res.GripLRms
					}
				}
			}
		}
		t.Logf("Curb Live Asymmetric SurfaceRumble: curbEnvL=%.2f maxPeakL=%d rmsL=[%.1f..%.1f]",
			synC.curbEnvL, maxCurbPeakL, minCurbRmsL, maxCurbRmsL)
		if synC.curbEnvL < 0.70 || maxCurbPeakL < 88 || maxCurbRmsL < 58.0 || minCurbRmsL > maxCurbRmsL*0.42 {
			t.Fatalf("live asymmetric curb ride too weak or lost envelope: curbEnvL=%.2f maxPeakL=%d rmsL=[%.1f..%.1f]",
				synC.curbEnvL, maxCurbPeakL, minCurbRmsL, maxCurbRmsL)
		}
	}

	// 7. Live Collision Onset Upgrade (mild onset frame -> heavy crash frame 16ms later while braking):
	{
		synCol := NewSynthesizer()
		pCol := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 82, CurrentRpm: 4800, EngineMaxRpm: 8000, RpmRatio: 0.60,
			Gear: 3, Accel: 0, Brake: 210, AccelZ: -7.0,
			NormSusp: [4]float32{0.62, 0.62, 0.40, 0.40}, SuspTravelM: [4]float32{0.09, 0.09, 0.07, 0.07},
		}
		for i := 0; i < 5; i++ {
			synCol.StepPacket(pCol, cfg, car, 0.016)
		}
		// Frame 1: Leading edge of crash while braking (76 km/h, AccelZ=-14.0 -> triggers low-speed onset)
		pCol.SpeedKmh = 76
		pCol.AccelZ = -14.0
		synCol.StepPacket(pCol, cfg, car, 0.016)
		_ = synCol.Render8ms(pCol, cfg, car, true)
		_ = synCol.Render8ms(pCol, cfg, car, true)
		// Frame 2 (16ms later): True wall slam peak arrives (48 km/h, AccelZ=-32.0 -> must upgrade to heavy frontal crash!)
		pCol.SpeedKmh = 48
		pCol.AccelZ = -32.0
		synCol.StepPacket(pCol, cfg, car, 0.016)
		resCol := synCol.Render8ms(pCol, cfg, car, true)
		pkCol := peakDev(resCol.GripL)
		t.Logf("Collision Onset Upgrade (While Braking): label=%q lowSpeed=%v impactPeak=%.2f gripPeak=%d rms=%.1f",
			resCol.ImpactLabel, synCol.impactLowSpeed, synCol.impactPeak, pkCol, resCol.GripLRms)
		if resCol.ImpactLabel != "正面撞击" || synCol.impactLowSpeed || synCol.impactPeak < 0.90 || pkCol < 105 || resCol.GripLRms < 75.0 {
			t.Fatalf("collision onset upgrade while braking failed: label=%q lowSpeed=%v impactPeak=%.2f gripPeak=%d rms=%.1f",
				resCol.ImpactLabel, synCol.impactLowSpeed, synCol.impactPeak, pkCol, resCol.GripLRms)
		}
	}
}

func TestUserFeedbackFiveFixes(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")

	// 1. Fix 1: 7–8k RPM Right Trigger Smoothness & Per-ms Slewing (Zero Abnormal Step Noise!)
	{
		car := prof.ApplyDemoArchetype("awd_performance_gt")
		syn := NewSynthesizer()
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 145, CurrentRpm: 6000, EngineMaxRpm: 10000, RpmRatio: 0.60,
			Gear: 3, Accel: 240, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
		}
		for i := 0; i < 4; i++ {
			syn.StepPacket(pkt, cfg, car, 0.016)
			_ = syn.Render8ms(pkt, cfg, car, true)
		}

		// Sweep RPM continuously from 6200 -> 9600 RPM across 7-8k RPM and RedlineRatio (0.925)
		prevPeak := -1
		maxFramePeakJump := 0
		maxSampleStep := 0
		prevSample := byte(128)
		hasPrevSample := false
		for step := 0; step <= 34; step++ {
			rpm := float32(6200 + step*100)
			pkt.CurrentRpm = rpm
			pkt.RpmRatio = rpm / 10000.0
			syn.StepPacket(pkt, cfg, car, 0.008)
			res := syn.Render8ms(pkt, cfg, car, true)
			pk := assertTrigBounds(t, "rpm_sweep_right_trig", res.Trig)
			if prevPeak >= 0 {
				d := pk - prevPeak
				if d < 0 {
					d = -d
				}
				if d > maxFramePeakJump {
					maxFramePeakJump = d
				}
			}
			prevPeak = pk
			for _, b := range res.Trig {
				if hasPrevSample {
					sd := int(b) - int(prevSample)
					if sd < 0 {
						sd = -sd
					}
					if sd > maxSampleStep {
						maxSampleStep = sd
					}
				}
				prevSample = b
				hasPrevSample = true
			}
		}
		if maxFramePeakJump > 2 || maxSampleStep > 16 {
			t.Fatalf("7-8k RPM right trigger sweep had abrupt step jump: maxFramePeakJump=%d (want <=2) maxSampleStep=%d (want <=16)",
				maxFramePeakJump, maxSampleStep)
		}

		// Now flicker rear slip across isOversteer on/off at 7500 RPM and verify per-ms slewing prevents 1ms step clicks!
		pkt.CurrentRpm = 7500
		pkt.RpmRatio = 0.75
		maxFlickerSampleStep := 0
		for f := 0; f < 8; f++ {
			if f%2 == 1 {
				pkt.CombinedSlip = [4]float32{0.20, 0.20, 0.85, 0.90}
				pkt.SlipAngle = [4]float32{0.05, 0.05, 0.42, 0.45}
			} else {
				pkt.CombinedSlip = [4]float32{0.10, 0.10, 0.15, 0.15}
				pkt.SlipAngle = [4]float32{0.02, 0.02, 0.05, 0.05}
			}
			syn.StepPacket(pkt, cfg, car, 0.008)
			res := syn.Render8ms(pkt, cfg, car, true)
			assertTrigBounds(t, "slip_flicker_7500rpm", res.Trig)
			for _, b := range res.Trig {
				sd := int(b) - int(prevSample)
				if sd < 0 {
					sd = -sd
				}
				if sd > maxFlickerSampleStep {
					maxFlickerSampleStep = sd
				}
				prevSample = b
			}
		}
		if maxFlickerSampleStep > 28 {
			t.Fatalf("7500 RPM branch flicker caused sharp 1ms trigger discontinuity: maxFlickerSampleStep=%d (want <=28)", maxFlickerSampleStep)
		}
		t.Logf("Fix 1 (7-8k RPM Right Trigger Smoothness) Verified: maxFramePeakJump=%d maxSampleStep=%d maxFlickerSampleStep=%d",
			maxFramePeakJump, maxSampleStep, maxFlickerSampleStep)
	}

	// 2. Fix 2: Large-Displacement (muscle_v8) Low-Mid RPM Continuous Growl (Zero 8-15Hz "通通通" Pulsing!)
	{
		for _, id := range []string{"baja_trophy_offroad", "heavy_pickup_suv", "v8_rwd_muscle", "vintage_classic_cruiser"} {
			c := prof.ApplyDemoArchetype(id)
			if c.EngineFreqBaseHz < 28.0 || c.EngineFreqBaseHz > 33.0 || c.EngineBedScale < 1.20 || c.EngineBedScale > 1.38 {
				t.Fatalf("muscle_v8 archetype %s out of crisp bounds: baseHz=%.1f (want 28..33) bedScale=%.2f (want 1.20..1.38)",
					id, c.EngineFreqBaseHz, c.EngineBedScale)
			}
		}
		carV8 := prof.ApplyDemoArchetype("v8_rwd_muscle")
		synV8 := NewSynthesizer()
		pktV8 := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 58, CurrentRpm: 2450, EngineMaxRpm: 7000, RpmRatio: 0.35,
			Gear: 3, Accel: 150, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
		}
		for i := 0; i < 4; i++ {
			synV8.StepPacket(pktV8, cfg, carV8, 0.008)
			_ = synV8.Render8ms(pktV8, cfg, carV8, true)
		}
		var samples []float64
		minWinPeak, maxWinPeak := 999, 0
		for win := 0; win < 8; win++ { // 8 x 32ms windows = 256ms (>2 cycles of old 8-10Hz lope)
			winPeak := 0
			for f := 0; f < 4; f++ {
				synV8.StepPacket(pktV8, cfg, carV8, 0.008)
				res := synV8.Render8ms(pktV8, cfg, carV8, true)
				if pk := peakDev(res.GripL); pk > winPeak {
					winPeak = pk
				}
				for _, b := range res.GripL {
					samples = append(samples, float64(b)-128.0)
				}
			}
			if winPeak < minWinPeak {
				minWinPeak = winPeak
			}
			if winPeak > maxWinPeak {
				maxWinPeak = winPeak
			}
		}
		estHz, _ := estimateActiveFreqHz(samples)
		if estHz < 38.0 || (maxWinPeak-minWinPeak) > 2 {
			t.Fatalf("muscle_v8 low-mid RPM still has low-freq '通通通' pulsing: estHz=%.1fHz (want >=38Hz) winPeakSpan=[%d..%d] (diff=%d, want <=2)",
				estHz, minWinPeak, maxWinPeak, maxWinPeak-minWinPeak)
		}
		t.Logf("Fix 2 (Muscle V8 Low-Mid RPM Crisp Continuous Growl) Verified: estHz=%.1fHz 32msWinPeaks=[%d..%d] (diff=%d)",
			estHz, minWinPeak, maxWinPeak, maxWinPeak-minWinPeak)
	}

	// 3. Fix 3: V4 开心散仙竞速2 (kaixin_race2_clear_lock) Two-Stage Braking Waveform
	{
		car := prof.ApplyDemoArchetype("v8_flatplane_gt")
		// Stage 1: Threshold Brake Traction Warning (90Hz Left Trigger + Restrained 70Hz Grip Warning)
		synW := NewSynthesizer()
		pWarn := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 115, CurrentRpm: 5000, EngineMaxRpm: 8500, RpmRatio: 0.59,
			Gear: 3, Accel: 0, Brake: 120,
			WheelRotSpeed: [4]float32{48, 48, 50, 50},
			SlipRatio:     [4]float32{-0.26, -0.26, -0.18, -0.18},
			CombinedSlip:  [4]float32{0.42, 0.42, 0.30, 0.30},
			NormSusp:      [4]float32{0.68, 0.68, 0.36, 0.36},
		}
		for i := 0; i < 3; i++ {
			synW.StepPacket(pWarn, cfg, car, 0.016)
			_ = synW.Render8ms(pWarn, cfg, car, true)
		}
		var warnTrigSamples []float64
		warnTrigPeak := 0
		warnGripRms := 0.0
		for f := 0; f < 8; f++ {
			res := synW.Render8ms(pWarn, cfg, car, true)
			if pk := assertTrigBounds(t, "kaixin_stage1_warn", res.Trig); pk > warnTrigPeak {
				warnTrigPeak = pk
			}
			warnGripRms += res.GripLRms
			for _, b := range res.Trig {
				warnTrigSamples = append(warnTrigSamples, float64(b)-128.0)
			}
		}
		warnGripRms /= 8.0
		warnTrigHz, _ := estimateActiveFreqHz(warnTrigSamples)
		if warnTrigPeak < 10 || warnTrigPeak > 30 || warnTrigHz < 82.0 || warnTrigHz > 98.0 {
			t.Fatalf("V4 kaixin Stage-1 threshold warning failed: trigPeak=%d (want 10..30) trigHz=%.1fHz (want ~90Hz)",
				warnTrigPeak, warnTrigHz)
		}

		// Stage 2: Clear 100Hz Left Trigger Lock/ABS + 10Hz ABS Gate + 50%-Scaled 70/50Hz Dry Grip Body
		synA := NewSynthesizer()
		pAbs := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 125, CurrentRpm: 4800, EngineMaxRpm: 8500, RpmRatio: 0.56,
			Gear: 3, Accel: 0, Brake: 210,
			WheelRotSpeed: [4]float32{42, 42, 46, 46},
			SlipRatio:     [4]float32{-0.58, -0.58, -0.50, -0.50},
			CombinedSlip:  [4]float32{0.92, 0.92, 0.82, 0.82},
			NormSusp:      [4]float32{0.75, 0.75, 0.34, 0.34},
		}
		for i := 0; i < 3; i++ {
			synA.StepPacket(pAbs, cfg, car, 0.016)
			_ = synA.Render8ms(pAbs, cfg, car, true)
		}
		var absTrigSamples []float64
		absTrigPeak := 0
		maxAbsGripRms, minAbsGripRms := 0.0, 999.0
		for f := 0; f < 16; f++ { // 128ms > 1 full 10Hz (100ms) V4 ABS cycle
			res := synA.Render8ms(pAbs, cfg, car, true)
			if pk := assertTrigBounds(t, "kaixin_stage2_abs", res.Trig); pk > absTrigPeak {
				absTrigPeak = pk
			}
			if res.GripLRms > maxAbsGripRms {
				maxAbsGripRms = res.GripLRms
			}
			if res.GripLRms < minAbsGripRms {
				minAbsGripRms = res.GripLRms
			}
			for _, b := range res.Trig {
				absTrigSamples = append(absTrigSamples, float64(b)-128.0)
			}
		}
		absTrigHz, _ := estimateActiveFreqHz(absTrigSamples)
		if absTrigPeak < 40 || absTrigPeak > 64 || absTrigHz < 92.0 || absTrigHz > 108.0 || maxAbsGripRms < 32.0 || minAbsGripRms > 14.0 {
			t.Fatalf("V4 kaixin Stage-2 10Hz ABS / 100Hz trigger lock failed: trigPeak=%d trigHz=%.1fHz gripRms=[%.1f..%.1f]",
				absTrigPeak, absTrigHz, minAbsGripRms, maxAbsGripRms)
		}
		t.Logf("Fix 3 (V4 开心散仙竞速2 Braking) Verified: Stage1(trigPeak=%d @ %.1fHz, gripRms=%.1f) | Stage2-ABS(trigPeak=%d @ %.1fHz, 10Hz gripRms=[%.1f..%.1f])",
			warnTrigPeak, warnTrigHz, warnGripRms, absTrigPeak, absTrigHz, minAbsGripRms, maxAbsGripRms)
	}

	// 4. Fix 4: All Grip Vibrations Crisper & Tighter (60-95ms shifts, fast curb exit release, fast duck recovery)
	{
		for _, info := range AllArchetypes() {
			if info.EngineFamily == "ev" {
				continue
			}
			c := prof.ApplyDemoArchetype(info.ID)
			if c.ShiftDurationMs < 59.5 || c.ShiftDurationMs > 95.5 {
				t.Fatalf("archetype %s ShiftDurationMs=%.1f out of crisp [60, 95]ms range", info.ID, c.ShiftDurationMs)
			}
		}
		// Curb exit release: after leaving curb, curbEnvL must drop below 0.14 within <= 128ms
		car := prof.ApplyDemoArchetype("rwd_drift_jdm")
		synC := NewSynthesizer()
		pCurb := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 100, CurrentRpm: 5000, EngineMaxRpm: 8000, RpmRatio: 0.62,
			Gear: 3, Accel: 150, RumbleStrip: [4]int32{1, 0, 1, 0},
			NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
		}
		for i := 0; i < 3; i++ {
			synC.StepPacket(pCurb, cfg, car, 0.016)
		}
		if synC.curbEnvL < 0.90 {
			t.Fatalf("expected active curbEnvL >= 0.90, got %.2f", synC.curbEnvL)
		}
		pCurb.RumbleStrip = [4]int32{0, 0, 0, 0}
		for i := 0; i < 9; i++ { // 9 * 16ms = 144ms off curb
			synC.StepPacket(pCurb, cfg, car, 0.016)
		}
		if synC.curbEnvL >= 0.14 {
			t.Fatalf("curb release too sluggish after leaving curb: curbEnvL=%.2f after 144ms (want <0.14)", synC.curbEnvL)
		}
		t.Logf("Fix 4 (Crisp Grip Envelopes) Verified: ShiftDurationMs=[60..95ms], CurbExit144msEnv=%.3f (<0.14)", synC.curbEnvL)
	}

	// 5. Fix 5: Premature Redline Judgment Fixed (Defaults 0.925..0.945, Learner 0.982*lim >= 0.90, Stale Garage Reset)
	{
		cDef := prof.ApplyDemoArchetype("awd_performance_gt")
		cExo := prof.ApplyDemoArchetype("v12_na_hypercar")
		cRot := prof.ApplyDemoArchetype("rotary_screamer")
		if math.Abs(cDef.RedlineRatio-0.925) > 0.001 || math.Abs(cExo.RedlineRatio-0.935) > 0.001 || math.Abs(cRot.RedlineRatio-0.945) > 0.001 {
			t.Fatalf("unlearned RedlineRatio defaults wrong: def=%.3f (want 0.925) exo=%.3f (want 0.935) rot=%.3f (want 0.945)",
				cDef.RedlineRatio, cExo.RedlineRatio, cRot.RedlineRatio)
		}

		// Verify stale garage entry (<0.90 without userOverride) is automatically sanitized on load/initVehicleLocked
		tmpGarage := filepath.Join(t.TempDir(), "garage.json")
		staleJSON := `{"schemaVersion":1,"cars":{"4223":{"carOrdinal":4223,"archetypeId":"awd_performance_gt","cylinders":6,"driveTrain":2,"carPi":590,"maxRpm":10000,"learnedLimiterRpm":8898.65,"redlineRatio":0.8409,"redlineLearned":true,"userOverride":false}}}`
		if err := os.WriteFile(tmpGarage, []byte(staleJSON), 0644); err != nil {
			t.Fatal(err)
		}
		pStale := NewProfiler(tmpGarage)
		obs := pStale.Observe(telemetry.Packet{
			IsRaceOn: true, CarOrdinal: 4223, Cylinders: 6, DriveTrain: 2, CarPI: 590,
			EngineMaxRpm: 10000, CurrentRpm: 4500, SpeedKmh: 80, Gear: 3, Accel: 120,
		}, true, true)
		if obs.RedlineRatio < 0.925 || obs.RedlineLearned {
			t.Fatalf("stale premature redlineRatio (0.8409) was not reset on load: got ratio=%.4f learned=%v",
				obs.RedlineRatio, obs.RedlineLearned)
		}
		t.Logf("Fix 5 (Redline Premature Judgment Fixed) Verified: defaults=0.925/0.935/0.945, stale 4223 reset to %.3f (learned=%v)",
			obs.RedlineRatio, obs.RedlineLearned)
	}
}

func TestSixKRpmAndSevenEightKRpmNoResonance(t *testing.T) {
	prof := NewProfiler("")

	measureRT := func(cfg NextConfig, car CarPersonality, pkt telemetry.Packet) (peak int, hz float64, route int) {
		syn := NewSynthesizer()
		for i := 0; i < 4; i++ {
			syn.StepPacket(pkt, cfg, car, 0.016)
			_ = syn.Render8ms(pkt, cfg, car, true)
		}
		var samples []float64
		for f := 0; f < 20; f++ { // 160ms
			if f%2 == 0 {
				syn.StepPacket(pkt, cfg, car, 0.016)
			}
			res := syn.Render8ms(pkt, cfg, car, true)
			pk := assertTrigBounds(t, "rt_resonance_check", res.Trig)
			if pk > peak {
				peak = pk
			}
			route = res.TrigRoute
			for _, b := range res.Trig {
				samples = append(samples, float64(b)-128.0)
			}
		}
		hz, _ = estimateActiveFreqHz(samples)
		return peak, hz, route
	}

	// 1. User's active Car 3173 (7500 MaxRpm, rwd_drift_jdm) @ 5500 / 6000 / 6500 RPM with exact user config!
	cfgUser := DefaultNextConfig()
	cfgUser.MasterTriggerGain = 0.95
	cfgUser.Trigger.ThrottleMicroFeel = 0.45
	cfgUser.Events.OversteerDrift.Intensity = 1.60

	car3173 := prof.ApplyDemoArchetype("rwd_drift_jdm")
	car3173.CarOrdinal = 3173
	car3173.MaxRpm = 7500
	car3173.LearnedLimiterRpm = 6845.36
	car3173.RedlineRatio = 0.90
	car3173.RedlineLearned = true

	for _, rpm := range []float32{5500, 6000, 6500} {
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 135, CurrentRpm: rpm, EngineMaxRpm: 7500, RpmRatio: rpm / 7500.0,
			Gear: 3, Accel: 255, NormSusp: [4]float32{0.50, 0.50, 0.50, 0.50},
		}
		pk, hz, route := measureRT(cfgUser, car3173, pkt)
		if route != 1 || pk < 3 || pk > 9 || hz < 52.0 || hz > 78.0 {
			t.Fatalf("Car 3173 @ %.0f RPM RT micro-feel violated rattle-free 52-74Hz target: route=%d peak=%d hz=%.1fHz",
				rpm, route, pk, hz)
		}
		t.Logf("Car 3173 @ %.0f RPM (ratio=%.2f): peak=±%d freq=%.1fHz (cleanly below 106Hz rattle!)", rpm, rpm/7500.0, pk, hz)
	}

	// 1b. ThrottleMicroFeel == 0.0 MUST produce exact zero (no hidden 0.45 floor!) even at 6000 RPM and Redline!
	cfgZero := cfgUser
	cfgZero.Trigger.ThrottleMicroFeel = 0.0
	for _, rpm := range []float32{6000, 6800} {
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 135, CurrentRpm: rpm, EngineMaxRpm: 7500, RpmRatio: rpm / 7500.0,
			Gear: 3, Accel: 255, NormSusp: [4]float32{0.50, 0.50, 0.50, 0.50},
		}
		pk, _, route := measureRT(cfgZero, car3173, pkt)
		if pk != 0 || route != -1 {
			t.Fatalf("ThrottleMicroFeel=0.0 at %.0f RPM must be silent: got peak=%d route=%d", rpm, pk, route)
		}
	}

	// 1c. Mild rear slip at 6000 RPM on Car 3173 (oversteerDrift.intensity=1.6) must stay <= 100Hz AND ramp smoothly (no jump!)
	pktMildOver := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 120, CurrentRpm: 6000, EngineMaxRpm: 7500, RpmRatio: 0.80,
		Gear: 3, Accel: 255, CombinedSlip: [4]float32{0.15, 0.15, 0.62, 0.64},
		NormSusp: [4]float32{0.50, 0.50, 0.55, 0.55},
	}
	pkOver, hzOver, routeOver := measureRT(cfgUser, car3173, pktMildOver)
	if routeOver != 1 || pkOver > 24 || hzOver < 76.0 || hzOver > 98.0 {
		t.Fatalf("Car 3173 @ 6000 RPM mild oversteer failed smooth/rattle-free check: route=%d peak=%d hz=%.1fHz",
			routeOver, pkOver, hzOver)
	}
	t.Logf("Car 3173 @ 6000 RPM mild oversteer (int=1.6): peak=±%d freq=%.1fHz (smooth & <100Hz!)", pkOver, hzOver)

	// 2. Previous Car 4223 (10000 MaxRpm, awd_performance_gt) @ 7000 / 7500 / 8000 RPM
	car4223 := prof.ApplyDemoArchetype("awd_performance_gt")
	car4223.CarOrdinal = 4223
	car4223.MaxRpm = 10000
	car4223.LearnedLimiterRpm = 8927.6
	car4223.RedlineRatio = 0.90
	car4223.RedlineLearned = true

	for _, rpm := range []float32{7000, 7500, 8000} {
		pkt := telemetry.Packet{
			IsRaceOn: true, SpeedKmh: 170, CurrentRpm: rpm, EngineMaxRpm: 10000, RpmRatio: rpm / 10000.0,
			Gear: 4, Accel: 255, NormSusp: [4]float32{0.50, 0.50, 0.50, 0.50},
		}
		pk, hz, route := measureRT(cfgUser, car4223, pkt)
		if route != 1 || pk < 3 || pk > 9 || hz < 52.0 || hz > 78.0 {
			t.Fatalf("Car 4223 @ %.0f RPM RT micro-feel violated rattle-free 52-74Hz target: route=%d peak=%d hz=%.1fHz",
				rpm, route, pk, hz)
		}
		t.Logf("Car 4223 @ %.0f RPM (ratio=%.2f): peak=±%d freq=%.1fHz", rpm, rpm/10000.0, pk, hz)
	}
}

func TestEffectiveRedlineRatioAndProfilerContracts(t *testing.T) {
	// 1. Exact EffectiveRedlineRatio parity across all 4 real user cars from garage.json
	cars := []struct {
		name     string
		car      CarPersonality
		maxRpm   float32
		wantMin  float64
		wantMax  float64
	}{
		{"#262", CarPersonality{RedlineRatio: 0.90, RedlineLearned: true, LearnedLimiterRpm: 8296.5}, 9200, 0.885, 0.886},
		{"#3173", CarPersonality{RedlineRatio: 0.90, RedlineLearned: true, LearnedLimiterRpm: 6845.4}, 7500, 0.896, 0.897},
		{"#4223", CarPersonality{RedlineRatio: 0.90, RedlineLearned: true, LearnedLimiterRpm: 8927.6}, 10000, 0.876, 0.877},
		{"#433", CarPersonality{RedlineRatio: 0.90, RedlineLearned: true, LearnedLimiterRpm: 8544.0}, 9500, 0.883, 0.884},
		{"user_override_0.84", CarPersonality{UserOverride: true, RedlineRatio: 0.84, RedlineLearned: true, LearnedLimiterRpm: 9500}, 10000, 0.8399, 0.8401},
		{"unlearned_default", CarPersonality{RedlineRatio: 0.925, RedlineLearned: false}, 8000, 0.9249, 0.9251},
	}
	for _, tc := range cars {
		got := tc.car.EffectiveRedlineRatio(tc.maxRpm)
		if got < tc.wantMin || got > tc.wantMax {
			t.Fatalf("%s EffectiveRedlineRatio(%.0f)=%.5f out of [%.4f, %.4f]", tc.name, tc.maxRpm, got, tc.wantMin, tc.wantMax)
		}
	}

	// Verify synth uses EffectiveRedlineRatio + spd > 10.0
	cfg := DefaultNextConfig()
	car4223 := defaultPersonality()
	car4223.RedlineRatio = 0.90
	car4223.RedlineLearned = true
	car4223.LearnedLimiterRpm = 8927.6
	syn := NewSynthesizer()
	p := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 11.0, CurrentRpm: 8800, EngineMaxRpm: 10000, RpmRatio: 0.880,
		Gear: 2, Accel: 240, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
	}
	syn.StepPacket(p, cfg, car4223, 0.016)
	res11 := syn.Render8ms(p, cfg, car4223, true)
	hasRed11 := false
	for _, ev := range res11.ActiveEvents {
		if ev.ID == "redline" {
			hasRed11 = true
		}
	}
	if !hasRed11 {
		t.Fatalf("expected redline active on Car 4223 at 8800 RPM (effRatio=%.4f) and 11 km/h", car4223.EffectiveRedlineRatio(10000))
	}
	p.SpeedKmh = 9.5
	res95 := syn.Render8ms(p, cfg, car4223, true)
	for _, ev := range res95.ActiveEvents {
		if ev.ID == "redline" {
			t.Fatalf("redline must not trigger below 10.0 km/h (got active at 9.5 km/h)")
		}
	}

	// 2. Profiler SaveCurrentOverride, ResetCurrentOverride, DeleteGarageEntry, FlushGarage, learned restore, downward susp tracking
	gPath := filepath.Join(t.TempDir(), "garage.json")
	prof := NewProfiler(gPath)
	basePkt := telemetry.Packet{
		IsRaceOn: true, CarOrdinal: 7777, Cylinders: 6, DriveTrain: 1, CarPI: 720,
		EngineMaxRpm: 8000, CurrentRpm: 4000, SpeedKmh: 80, Gear: 3, Accel: 120,
		NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5}, SuspTravelM: [4]float32{0.15, 0.15, 0.15, 0.15},
	}
	prof.Observe(basePkt, true, true)
	prof.FlushGarage()
	if _, err := os.Stat(gPath); err != nil {
		t.Fatalf("FlushGarage did not persist dirty garage: %v", err)
	}

	// Save override with name & redlineRatio=0 (keeps existing redline, does NOT lock RedlineLearned!)
	saved := prof.SaveCurrentOverride("  Track Spec  ", 0.5, 0.2, 0.8, 0.9, 1.0, 0)
	if saved.CustomName != "Track Spec" || saved.RedlineLearned || !saved.UserOverride {
		t.Fatalf("SaveCurrentOverride with redlineRatio=0 misbehaved: name=%q learned=%v override=%v",
			saved.CustomName, saved.RedlineLearned, saved.UserOverride)
	}
	// Clearing CustomName with whitespace works
	cleared := prof.SaveCurrentOverride("   ", 0.5, 0.2, 0.8, 0.9, 1.0, 0.85)
	if cleared.CustomName != "" || math.Abs(cleared.RedlineRatio-0.85) > 0.001 || !cleared.RedlineLearned {
		t.Fatalf("SaveCurrentOverride clear name + set redline failed: %+v", cleared)
	}
	// ResetCurrentOverride clears UserOverride, CustomName, RedlineLearned, LearnedLimiterRpm, RedlineConfidence
	reset := prof.ResetCurrentOverride()
	if reset.UserOverride || reset.CustomName != "" || reset.RedlineLearned || reset.LearnedLimiterRpm != 0 || reset.RedlineConfidence != 0 {
		t.Fatalf("ResetCurrentOverride did not clear all override/redline fields: %+v", reset)
	}

	// DeleteGarageEntry resets active car when ordinal matches
	prof.SaveCurrentOverride("ToDelete", 0.3, 0.3, 0.7, 0.8, 1.1, 0.88)
	prof.DeleteGarageEntry(7777)
	actAfterDel := prof.Active()
	if actAfterDel.UserOverride || actAfterDel.CustomName != "" || actAfterDel.RedlineLearned {
		t.Fatalf("DeleteGarageEntry did not reset active override/name: %+v", actAfterDel)
	}

	// Validators
	if !IsValidArchetypeID("rwd_drift_jdm") || IsValidArchetypeID("not_real") {
		t.Fatalf("IsValidArchetypeID check failed")
	}
	if !IsValidTestEventID("redline") || !IsValidTestEventID("brakingAbs") || IsValidTestEventID("") || IsValidTestEventID("evil_id") {
		t.Fatalf("IsValidTestEventID check failed")
	}

	// Verify garage reload restores internal learnedLimRpm / limiterHits and UserOverride blocks learner overwrites
	learnedJSON := `{"schemaVersion":1,"cars":{"3173":{"carOrdinal":3173,"archetypeId":"rwd_drift_jdm","cylinders":6,"driveTrain":1,"carPi":634,"maxRpm":7500,"learnedLimiterRpm":6845.36,"redlineRatio":0.90,"redlineLearned":true,"userOverride":false}}}`
	gReloadPath := filepath.Join(t.TempDir(), "garage_reload.json")
	if err := os.WriteFile(gReloadPath, []byte(learnedJSON), 0644); err != nil {
		t.Fatal(err)
	}
	pReload := NewProfiler(gReloadPath)
	pReload.Observe(telemetry.Packet{
		IsRaceOn: true, CarOrdinal: 3173, Cylinders: 6, DriveTrain: 1, CarPI: 634,
		EngineMaxRpm: 7500, CurrentRpm: 4000, SpeedKmh: 80, Gear: 3, Accel: 120,
	}, true, true)
	if math.Abs(pReload.learnedLimRpm-6845.36) > 0.01 || pReload.limiterHits < 1 {
		t.Fatalf("initVehicleLocked did not restore internal limiter EMA state: learnedLimRpm=%.2f hits=%d",
			pReload.learnedLimRpm, pReload.limiterHits)
	}
	pReload.SaveCurrentOverride("Locked", 0.5, 0.3, 0.6, 0.8, 1.0, 0.86)
	for i := 0; i < 10; i++ {
		rpm := float32(7100)
		if i%2 == 1 {
			rpm = 6950
		}
		pReload.Observe(telemetry.Packet{
			IsRaceOn: true, CarOrdinal: 3173, Cylinders: 6, DriveTrain: 1, CarPI: 634,
			EngineMaxRpm: 7500, CurrentRpm: rpm, SpeedKmh: 120, Gear: 3, Accel: 255, PowerW: 250000,
		}, true, true)
	}
	if math.Abs(pReload.Active().RedlineRatio-0.86) > 0.001 {
		t.Fatalf("stepRedlineLearnerLocked overwrote UserOverride redlineRatio: got %.4f want 0.86", pReload.Active().RedlineRatio)
	}

	// Downward InferredMaxSuspM tracking after stiff suspension swap
	for i := 0; i < 40; i++ {
		pStiff := basePkt
		pStiff.SuspTravelM = [4]float32{0.05, 0.05, 0.05, 0.05} // 0.05/0.50 = 0.10m vs initial 0.30m
		prof.Observe(pStiff, true, false)
	}
	if prof.Active().InferredMaxSuspM >= 0.22 {
		t.Fatalf("InferredMaxSuspM failed to track downward after stiff swap: got %.3f", prof.Active().InferredMaxSuspM)
	}
}

func TestConfigSanitizeAndNaNSafety(t *testing.T) {
	// 1. NextConfig.Sanitize handles NaN, Inf, negative, and out-of-range values
	cfg := DefaultNextConfig()
	cfg.MasterGripGain = math.NaN()
	cfg.MasterTriggerGain = math.Inf(1)
	cfg.DuckingStrength = -5.0
	cfg.AntiFatigueClamp = 99.0
	cfg.Events.Redline.FreqScale = math.NaN()
	cfg.Events.BrakingABS.FreqScale = -2.0
	cfg.Events.EngineRev.Intensity = math.Inf(-1)
	cfg.Trigger.ThrottleMicroFeel = -0.5
	cfg.Trigger.BrakeResistance = 5.0
	cfg.Sanitize()

	def := DefaultNextConfig()
	if cfg.MasterGripGain != def.MasterGripGain || cfg.MasterTriggerGain != def.MasterTriggerGain {
		t.Fatalf("Sanitize did not replace NaN/Inf master gains with defaults: %+v", cfg)
	}
	if cfg.DuckingStrength != 0.0 || cfg.AntiFatigueClamp != 1.2 {
		t.Fatalf("Sanitize did not clamp DuckingStrength/AntiFatigueClamp: duck=%.2f af=%.2f", cfg.DuckingStrength, cfg.AntiFatigueClamp)
	}
	if cfg.Events.Redline.FreqScale != def.Events.Redline.FreqScale || cfg.Events.BrakingABS.FreqScale != 0.65 {
		t.Fatalf("Sanitize did not sanitize FreqScale: red=%.2f abs=%.2f", cfg.Events.Redline.FreqScale, cfg.Events.BrakingABS.FreqScale)
	}
	if cfg.Trigger.ThrottleMicroFeel != 0.0 || cfg.Trigger.BrakeResistance != 1.0 {
		t.Fatalf("Sanitize did not clamp trigger sliders: micro=%.2f brk=%.2f", cfg.Trigger.ThrottleMicroFeel, cfg.Trigger.BrakeResistance)
	}

	// 2. clamp(NaN) & wrapPhase(neg/NaN)
	if got := clamp(math.NaN(), 0.2, 1.0); got != 0.2 {
		t.Fatalf("clamp(NaN) = %v, want 0.2", got)
	}
	ph := -1.5
	wrapPhase(&ph)
	if ph < 0 || ph >= 2*math.Pi {
		t.Fatalf("wrapPhase(-1.5) = %v out of [0, 2pi)", ph)
	}
	phNaN := math.NaN()
	wrapPhase(&phNaN)
	if phNaN != 0 {
		t.Fatalf("wrapPhase(NaN) = %v, want 0", phNaN)
	}

	// 3. Negative SmashableMass & zero SuspensionReboundMs never poison synth
	syn := NewSynthesizer()
	car := defaultPersonality()
	car.SuspensionReboundMs = 0
	pkt := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 80, CurrentRpm: 4500, EngineMaxRpm: 8000, RpmRatio: 0.56,
		Gear: 3, Accel: 120, SmashableVelDiff: 8.0, SmashableMass: -5.0,
		NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5}, SuspTravelM: [4]float32{0.08, 0.08, 0.08, 0.08},
	}
	syn.StepPacket(pkt, cfg, car, 0.016)
	pkt.SmashableVelDiff = 10.0
	syn.StepPacket(pkt, cfg, car, 0.016)
	res := syn.Render8ms(pkt, cfg, car, true)
	if math.IsNaN(syn.smashDurS) || math.IsNaN(res.GripLRms) || math.IsNaN(res.GripRRms) {
		t.Fatalf("negative SmashableMass or 0 reboundMs produced NaN: smashDurS=%v rmsL=%v", syn.smashDurS, res.GripLRms)
	}

	// 4. Airborne wheelspin & handbrake exclusion on brakingAbs
	pktAir := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 90, CurrentRpm: 6000, EngineMaxRpm: 8000, RpmRatio: 0.75,
		Gear: 3, Accel: 220, Brake: 200,
		CombinedSlip: [4]float32{1.5, 1.5, 1.8, 1.8}, SlipRatio: [4]float32{-0.9, -0.9, -0.9, -0.9},
		NormSusp: [4]float32{0.0, 0.0, 0.0, 0.0},
	}
	resAir := syn.Render8ms(pktAir, cfg, car, true)
	for _, ev := range resAir.ActiveEvents {
		if ev.ID == "oversteerDrift" || ev.ID == "understeer" || ev.ID == "brakingAbs" {
			t.Fatalf("airborne vehicle falsely triggered tire event %q", ev.ID)
		}
	}
	pktHB := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 75, CurrentRpm: 4000, EngineMaxRpm: 8000, RpmRatio: 0.50,
		Gear: 2, Brake: 140, Handbrake: 220,
		SlipRatio: [4]float32{-0.1, -0.1, -0.95, -0.95}, CombinedSlip: [4]float32{0.2, 0.2, 1.5, 1.5},
		NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
	}
	resHB := syn.Render8ms(pktHB, cfg, car, true)
	for _, ev := range resHB.ActiveEvents {
		if ev.ID == "brakingAbs" {
			t.Fatalf("handbrake drift falsely triggered brakingAbs")
		}
	}

	// 5. renderTest8ms: grip-only tests have TrigRoute==-1 and disabled triggers are honored
	synT := NewSynthesizer()
	synT.TriggerTest("engineRev")
	resRev := synT.Render8ms(telemetry.Packet{}, cfg, car, true)
	if resRev.TrigRoute != -1 {
		t.Fatalf("grip-only test engineRev had TrigRoute=%d, want -1", resRev.TrigRoute)
	}
	cfgNoTrig := DefaultNextConfig()
	cfgNoTrig.Trigger.EnableLeftBrake = false
	cfgNoTrig.Trigger.EnableRightAccel = false
	for _, id := range []string{"gearShift", "understeerLeft", "oversteerRight", "brakingAbs"} {
		synT.TriggerTest(id)
		r := synT.Render8ms(telemetry.Packet{}, cfgNoTrig, car, true)
		if r.TrigRoute != -1 || peakDev(r.Trig) != 0 || r.RightTrigNeed {
			t.Fatalf("test %s ignored disabled trigger flags: route=%d peak=%d rightNeed=%v", id, r.TrigRoute, peakDev(r.Trig), r.RightTrigNeed)
		}
	}

	// 6. Runtime.SetActive(false) resets synth & sets EngineActive=false, double Stop() safe
	tmpDir := t.TempDir()
	rt := NewRuntime(filepath.Join(tmpDir, "cfg.json"), filepath.Join(tmpDir, "garage.json"), hid.NewWriter())
	rt.Start()
	rt.IngestPacket(pkt)
	rt.SetActive(false)
	if rt.Snapshot().EngineActive {
		t.Fatalf("SetActive(false) left Snapshot().EngineActive == true")
	}
	rt.Stop()
	rt.Stop() // must not panic
}

func TestTriggerReleaseTailAndPriority(t *testing.T) {
	cfg := DefaultNextConfig()
	prof := NewProfiler("")
	car := prof.ApplyDemoArchetype("rwd_drift_jdm")

	// 1. Right Trigger soft-release tail (tpReleaseTail) actually transmits fade-out on abrupt drop!
	synR := NewSynthesizer()
	pActive := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 90, CurrentRpm: 5500, EngineMaxRpm: 8000, RpmRatio: 0.68,
		Gear: 3, Accel: 240, CombinedSlip: [4]float32{0.2, 0.2, 1.2, 1.5},
		NormSusp: [4]float32{0.5, 0.5, 0.6, 0.7},
	}
	synR.StepPacket(pActive, cfg, car, 0.016)
	synR.StepPacket(pActive, cfg, car, 0.016)
	for i := 0; i < 3; i++ {
		_ = synR.Render8ms(pActive, cfg, car, true)
	}
	// Sudden lift-off to pure coasting -> next frame MUST emit tpReleaseTail fade-out on RT (route=1, non-zero decaying)!
	pCoast := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 85, CurrentRpm: 4500, EngineMaxRpm: 8000, RpmRatio: 0.56,
		Gear: 3, Accel: 0, Brake: 0, NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
	}
	synR.StepPacket(pCoast, cfg, car, 0.016)
	tail1 := synR.Render8ms(pCoast, cfg, car, true)
	tail2 := synR.Render8ms(pCoast, cfg, car, true)
	tail3 := synR.Render8ms(pCoast, cfg, car, true)
	if tail1.TrigRoute != 1 || peakDev(tail1.Trig) == 0 {
		t.Fatalf("RT soft-release tail was discarded instead of transmitted: route=%d peak=%d", tail1.TrigRoute, peakDev(tail1.Trig))
	}
	if peakDev(tail2.Trig) >= peakDev(tail1.Trig) || tail3.TrigRoute != -1 || peakDev(tail3.Trig) != 0 {
		t.Fatalf("RT release tail did not decay cleanly to neutral: pk1=%d pk2=%d pk3=%d route3=%d",
			peakDev(tail1.Trig), peakDev(tail2.Trig), peakDev(tail3.Trig), tail3.TrigRoute)
	}

	// 2. Left Trigger soft-release tail on sudden ABS release
	synL := NewSynthesizer()
	pAbs := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 120, CurrentRpm: 4500, EngineMaxRpm: 8000, RpmRatio: 0.56,
		Gear: 3, Brake: 220, WheelRotSpeed: [4]float32{40, 40, 40, 40},
		SlipRatio: [4]float32{-0.60, -0.60, -0.55, -0.55}, CombinedSlip: [4]float32{0.95, 0.95, 0.85, 0.85},
		NormSusp: [4]float32{0.7, 0.7, 0.35, 0.35},
	}
	synL.StepPacket(pAbs, cfg, car, 0.016)
	synL.StepPacket(pAbs, cfg, car, 0.016)
	for i := 0; i < 3; i++ {
		_ = synL.Render8ms(pAbs, cfg, car, true)
	}
	synL.StepPacket(pCoast, cfg, car, 0.016)
	lTail1 := synL.Render8ms(pCoast, cfg, car, true)
	_ = synL.Render8ms(pCoast, cfg, car, true)
	lTail3 := synL.Render8ms(pCoast, cfg, car, true)
	if lTail1.TrigRoute != 0 || peakDev(lTail1.Trig) == 0 || lTail3.TrigRoute != -1 || peakDev(lTail3.Trig) != 0 {
		t.Fatalf("LT soft-release tail failed: tail1(route=%d pk=%d) tail3(route=%d pk=%d)",
			lTail1.TrigRoute, peakDev(lTail1.Trig), lTail3.TrigRoute, peakDev(lTail3.Trig))
	}
	t.Logf("Trigger Soft-Release Tail Verified: RT(%d -> %d -> %d) LT(%d -> 0)",
		peakDev(tail1.Trig), peakDev(tail2.Trig), peakDev(tail3.Trig), peakDev(lTail1.Trig))

	// 3. Mid-8ms-frame shiftT expiry must preserve max priority (tpDynamicEvent) over lower-priority LT brake resistance!
	synPrio := NewSynthesizer()
	synPrio.shiftActiveS = 0.028 // expires at ms=4 inside the 8ms frame
	synPrio.shiftDurS = 0.080
	synPrio.shiftStrength = 0.90
	pMidShift := telemetry.Packet{
		IsRaceOn: true, SpeedKmh: 100, CurrentRpm: 6200, EngineMaxRpm: 8000, RpmRatio: 0.775,
		Gear: 3, Accel: 220, Brake: 80, // LT static resistance (tpStaticResistance) competing with expiring RT shift (tpDynamicEvent)
		NormSusp: [4]float32{0.5, 0.5, 0.5, 0.5},
	}
	resMid := synPrio.Render8ms(pMidShift, cfg, car, true)
	if resMid.TrigRoute != 1 || !resMid.RightTrigNeed {
		t.Fatalf("mid-frame shiftT expiry lost max priority to lower-priority LT: route=%d rightNeed=%v", resMid.TrigRoute, resMid.RightTrigNeed)
	}
}

