package lighting

import (
	"encoding/hex"
	"testing"

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
		},
		PacketFresh:  true,
		RedlineRatio: 0.90,
		HasWheelRot:  true,
	}

	// 2. Redline shift flash (red on / white off)
	cShiftOn, lit, _, _, sh := EvaluateTelemetry(baseIn, cfg, true, true)
	cShiftOff, _, _, _, _ := EvaluateTelemetry(baseIn, cfg, false, true)
	if !sh || cShiftOn.Cue != "shift-up" || cShiftOn.R != 255 || cShiftOff.G != 255 || lit != 12 {
		t.Fatalf("shift-up failed: on=%+v off=%+v lit=%d", cShiftOn, cShiftOff, lit)
	}

	// 3. Hard brake overrides shift-up -> Pure Red / Off strobe
	brkIn := baseIn
	brkIn.Packet.Brake = 160 // 62% > 35% threshold
	cBrkOn, _, absA, hb, _ := EvaluateTelemetry(brkIn, cfg, true, true)
	cBrkOff, _, _, _, _ := EvaluateTelemetry(brkIn, cfg, true, false)
	if absA || !hb || cBrkOn.Cue != "brake" || cBrkOn.R != 255 || cBrkOn.G != 0 || cBrkOff.R != 0 {
		t.Fatalf("hard brake failed: on=%+v off=%+v", cBrkOn, cBrkOff)
	}

	// 4. ABS overrides hard brake -> Ice-Blue (80, 170, 255) strobe!
	absIn := brkIn
	absIn.Packet.SlipRatio = [4]float32{-0.65, -0.60, -0.45, -0.45}
	absIn.Packet.CombinedSlip = [4]float32{0.95, 0.92, 0.70, 0.70}
	cAbsOn, _, absA2, _, _ := EvaluateTelemetry(absIn, cfg, true, true)
	cAbsOff, _, _, _, _ := EvaluateTelemetry(absIn, cfg, false, true)
	if !absA2 || cAbsOn.Cue != "abs" || cAbsOn.B != 255 || cAbsOn.R != 80 || cAbsOff.B != 0 {
		t.Fatalf("ABS lighting failed: on=%+v off=%+v", cAbsOn, cAbsOff)
	}

	// 5. Light brake below hardBrakeThreshold (e.g. 15% < 35%) stays on RPM strip (no nuisance flashing!)
	lightBrk := baseIn
	lightBrk.Packet.RpmRatio = 0.55
	lightBrk.Packet.Brake = 38 // ~15%
	cLight, litL, absL, hbL, shL := EvaluateTelemetry(lightBrk, cfg, true, true)
	if absL || hbL || shL || cLight.Cue != "rpm" || litL != 7 {
		t.Fatalf("light brake should remain RPM strip: got=%+v lit=%d", cLight, litL)
	}
}
