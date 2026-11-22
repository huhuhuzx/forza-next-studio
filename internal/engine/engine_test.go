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
	resU := synU.Render8ms(pU, cfg, car, true)
	if resU.GripLRms <= resU.GripRRms*1.8 || resU.TrigRoute != 0 || peakDev(resU.Trig) < 35 {
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
	resUR := synUR.Render8ms(pUR, cfg, car, true)
	if resUR.GripRRms <= resUR.GripLRms*1.8 || resUR.TrigRoute != 1 || peakDev(resUR.Trig) < 35 {
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
	resO := synO.Render8ms(pO, cfg, car, true)
	if resO.GripRRms <= resO.GripLRms*1.3 || resO.TrigRoute != 1 || peakDev(resO.Trig) < 40 {
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
	resOL := synOL.Render8ms(pOL, cfg, car, true)
	if resOL.GripLRms <= resOL.GripRRms*1.3 || resOL.TrigRoute != 0 || peakDev(resOL.Trig) < 35 {
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
	res4 := syn4.Render8ms(p4, cfg, car, true)
	if res4.Flags != 0x9E || res4.TrigRoute != 2 {
		t.Fatalf("4-wheel drift dual-trigger routing failed: flags=0x%02X trigRoute=%d", res4.Flags, res4.TrigRoute)
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
	replayFolder := func(name string) (frames, dirtHits, curbHits, landHits, impactHits, clipped int, avgRms float64, car CarPersonality) {
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
			_ = syn.Render8ms(pkt, cfg, car, true)
			res := syn.Render8ms(pkt, cfg, car, true)
			frames++
			rmsSum += 0.5 * (res.GripLRms + res.GripRRms)
			if res.Clipped {
				clipped++
			}
			if res.ImpactActive {
				impactHits++
			}
			for _, ev := range res.ActiveEvents {
				switch ev.ID {
				case "roadSurface":
					dirtHits++
				case "curbRumble":
					curbHits++
				case "jumpLanding":
					landHits++
				}
			}
		}
		if frames > 0 {
			avgRms = rmsSum / float64(frames)
		}
		return
	}

	f1, dirt1, _, _, _, clip1, rms1, car1 := replayFolder("非铺装握把震动似乎没效果")
	t.Logf("[Dirt] frames=%d dirtHits=%d (%.1f%%) avgRMS=%.1f clipped=%d car=%s",
		f1, dirt1, float64(dirt1)/math.Max(float64(f1), 1)*100, rms1, clip1, car1.Label)
	if dirt1 < 2500 || rms1 < 22.0 || clip1 != 0 {
		t.Fatalf("dirt regression failed")
	}

	f2, _, _, land2, imp2, clip2, rms2, car2 := replayFolder("session-39036-4540600071600_Cyberpangk_高速公路环道_370Z-Nismo_丢失小幅度飞坡落地底盘振动")
	t.Logf("[370Z] frames=%d landHits=%d impactHits=%d avgRMS=%.1f clipped=%d redline=%.3f learned=%v",
		f2, land2, imp2, rms2, clip2, car2.RedlineRatio, car2.RedlineLearned)
	if land2 < 10 || imp2 < 10 || imp2 > 1200 || !car2.RedlineLearned {
		t.Fatalf("370Z regression failed: land2=%d imp2=%d (want 10..1200)", land2, imp2)
	}

	f3, _, curb3, _, imp3, clip3, rms3, _ := replayFolder("路肩“游龙”")
	t.Logf("[Curb] frames=%d curbHits=%d impactHits=%d avgRMS=%.1f clipped=%d", f3, curb3, imp3, rms3, clip3)
	if curb3 < 80 || imp3 > 120 || rms3 < 20.0 {
		t.Fatalf("curb regression failed: curb3=%d imp3=%d", curb3, imp3)
	}

	f4, _, _, _, imp4, clip4, rms4, _ := replayFolder("session-32640-5063531786500")
	t.Logf("[WallSession] frames=%d impactHits=%d avgRMS=%.1f clipped=%d", f4, imp4, rms4, clip4)
	if imp4 < 40 || imp4 > 950 || clip4 != 0 {
		t.Fatalf("wall session regression failed: imp4=%d clip4=%d", imp4, clip4)
	}
}
