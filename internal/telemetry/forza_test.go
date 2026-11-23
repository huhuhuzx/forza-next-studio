package telemetry

import (
	"encoding/binary"
	"math"
	"testing"
)

func putF32(b []byte, off int, v float32) {
	binary.LittleEndian.PutUint32(b[off:off+4], math.Float32bits(v))
}

func putI32(b []byte, off int, v int32) {
	binary.LittleEndian.PutUint32(b[off:off+4], uint32(v))
}

func makeValidPacket(size int) []byte {
	b := make([]byte, size)
	putI32(b, 0, 1) // IsRaceOn = true
	putF32(b, 8, 8000)
	putF32(b, 12, 800)
	putF32(b, 16, 4800)
	off := 0
	if size == 324 {
		off = 12
		putI32(b, 232, 11)
		putF32(b, 236, 4.5)
		putF32(b, 240, 12.0)
	}
	putF32(b, 244+off, 30.0) // SpeedMs = 30 m/s = 108 km/h
	putF32(b, 248+off, 200000)
	putF32(b, 252+off, 420)
	putF32(b, 272+off, 14.5)
	b[303+off] = 220
	b[304+off] = 15
	b[307+off] = 4
	steer := int8(-30)
	b[308+off] = byte(steer)
	return b
}

func TestParseHorizon324AndFM2023_331(t *testing.T) {
	p324, err := Parse(makeValidPacket(324))
	if err != nil {
		t.Fatalf("Parse(324) failed: %v", err)
	}
	if p324.Format != FormatHorizon324 || math.Abs(float64(p324.SpeedMs-30.0)) > 0.01 ||
		p324.CarCategory != 11 || p324.Accel != 220 || p324.Gear != 4 || p324.Steer != -30 {
		t.Fatalf("Parse(324) mismatch: %+v", p324)
	}

	// Forza Motorsport (2023) 331B must NOT shift by +12!
	p331, err := Parse(makeValidPacket(331))
	if err != nil {
		t.Fatalf("Parse(331) failed: %v", err)
	}
	if p331.Format != FormatExtended || math.Abs(float64(p331.SpeedMs-30.0)) > 0.01 ||
		math.Abs(float64(p331.TorqueNm-420.0)) > 0.01 || p331.Accel != 220 || p331.Brake != 15 || p331.Gear != 4 || p331.Steer != -30 {
		t.Fatalf("Parse(331) mis-shifted FM2023 offsets: %+v", p331)
	}
}

func TestParseRejectsNonFiniteFields(t *testing.T) {
	offsets := []struct {
		name string
		off  int
	}{
		{"AngVelY", 48},
		{"SlipRatio0", 84},
		{"WheelRotSpeed0", 100},
		{"SlipAngle0", 164},
		{"SmashableVelDiff", 236},
		{"SmashableMass", 240},
		{"PowerW", 248 + 12},
		{"TorqueNm", 252 + 12},
		{"BoostPsi", 272 + 12},
	}
	for _, tc := range offsets {
		b := makeValidPacket(324)
		putF32(b, tc.off, float32(math.NaN()))
		if _, err := Parse(b); err != ErrNonFinite {
			t.Fatalf("Parse did not reject NaN in %s: err=%v", tc.name, err)
		}
		putF32(b, tc.off, float32(math.Inf(1)))
		if _, err := Parse(b); err != ErrNonFinite {
			t.Fatalf("Parse did not reject +Inf in %s: err=%v", tc.name, err)
		}
	}
}
