package telemetry

import (
	"encoding/binary"
	"errors"
	"math"
)

// Format identifies the Forza UDP packet layout.
type Format int

const (
	FormatUnknown       Format = 0
	FormatHorizon324    Format = 1 // Standard Forza Horizon 4/5/6 (324 bytes)
	FormatMotorsport323 Format = 2 // Compatible 311/323 bytes (no 12-byte Horizon gap)
	FormatExtended      Format = 3 // >324 bytes extended horizon layout
)

// Packet represents a fully decoded Forza UDP telemetry frame.
type Packet struct {
	RawLen      int    `json:"rawLen"`
	Format      Format `json:"format"`
	IsRaceOn    bool   `json:"active"`
	TimestampMs uint32 `json:"gameTimestampMs"`

	EngineMaxRpm  float32 `json:"maxRpm"`
	EngineIdleRpm float32 `json:"idleRpm"`
	CurrentRpm    float32 `json:"rpm"`
	RpmRatio      float32 `json:"rpmRatio"`

	AccelX float32 `json:"accelX"` // lateral (m/s^2)
	AccelY float32 `json:"accelY"` // vertical (m/s^2)
	AccelZ float32 `json:"accelZ"` // longitudinal (m/s^2)

	VelX     float32 `json:"velX"`
	VelY     float32 `json:"velY"`
	VelZ     float32 `json:"velZ"`
	SpeedMs  float32 `json:"speedMs"`
	SpeedKmh float32 `json:"speedKmh"`

	AngVelX float32 `json:"angVelX"`
	AngVelY float32 `json:"angVelY"`
	AngVelZ float32 `json:"angVelZ"`

	Yaw   float32 `json:"yaw"`
	Pitch float32 `json:"pitch"`
	Roll  float32 `json:"roll"`

	NormSusp      [4]float32 `json:"normalizedSuspensionTravel"` // FL, FR, RL, RR [0..1]
	SlipRatio     [4]float32 `json:"slipRatio"`
	WheelRotSpeed [4]float32 `json:"wheelRotationSpeed"`
	RumbleStrip   [4]int32   `json:"rumbleStrip"`
	PuddleDepth   [4]float32 `json:"puddleDepth"`
	SurfaceRumble [4]float32 `json:"surfaceRumble"`
	SlipAngle     [4]float32 `json:"slipAngle"`
	CombinedSlip  [4]float32 `json:"combinedSlip"`
	SuspTravelM   [4]float32 `json:"suspensionTravel"` // meters

	CarOrdinal int32 `json:"carOrdinal"`
	CarClass   int32 `json:"carClass"` // 0=D..6=X
	CarPI      int32 `json:"carPerformanceIndex"`
	DriveTrain int32 `json:"driveTrain"` // 0=FWD, 1=RWD, 2=AWD
	Cylinders  int32 `json:"cylinders"`

	CarCategory      int32   `json:"carCategory"`
	SmashableVelDiff float32 `json:"smashableVelocityDiff"`
	SmashableMass    float32 `json:"smashableMass"`

	PosX float32 `json:"posX"`
	PosY float32 `json:"posY"`
	PosZ float32 `json:"posZ"`

	PowerW   float32    `json:"power"`
	TorqueNm float32    `json:"torque"`
	TireTemp [4]float32 `json:"tireTemperature"`
	BoostPsi float32    `json:"boost"`
	Fuel     float32    `json:"fuel"`
	Distance float32    `json:"distanceTravelled"`

	Accel     uint8 `json:"accel"`     // 0..255
	Brake     uint8 `json:"brake"`     // 0..255
	Clutch    uint8 `json:"clutch"`    // 0..255
	Handbrake uint8 `json:"handbrake"` // 0..255
	Gear      uint8 `json:"gear"`      // 0=R, 1..10
	Steer     int8  `json:"steer"`     // -127..127
}

var (
	ErrTooShort = errors.New("telemetry packet shorter than 311 bytes")
	ErrNonFinite = errors.New("telemetry packet contains NaN or Inf")
)

func f32(b []byte, off int) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b[off : off+4]))
}

func i32(b []byte, off int) int32 {
	return int32(binary.LittleEndian.Uint32(b[off : off+4]))
}

func u32(b []byte, off int) uint32 {
	return binary.LittleEndian.Uint32(b[off : off+4])
}

func finite32(v float32) bool {
	f := float64(v)
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// Parse decodes a Forza Sled/Dash/Horizon UDP packet.
func Parse(b []byte) (Packet, error) {
	if len(b) < 311 {
		return Packet{}, ErrTooShort
	}

	var p Packet
	p.RawLen = len(b)
	off := 0
	switch {
	case len(b) == 324:
		p.Format = FormatHorizon324
		off = 12
	case len(b) > 324:
		// Forza Motorsport (2023) 331-byte packets have NO 12-byte Horizon gap at offset 232.
		p.Format = FormatExtended
		off = 0
	default:
		p.Format = FormatMotorsport323
		off = 0
	}

	p.IsRaceOn = i32(b, 0) != 0
	p.TimestampMs = u32(b, 4)
	p.EngineMaxRpm = f32(b, 8)
	p.EngineIdleRpm = f32(b, 12)
	p.CurrentRpm = f32(b, 16)
	if p.EngineMaxRpm > 100 {
		p.RpmRatio = p.CurrentRpm / p.EngineMaxRpm
		if p.RpmRatio < 0 {
			p.RpmRatio = 0
		} else if p.RpmRatio > 1.25 {
			p.RpmRatio = 1.25
		}
	}

	p.AccelX = f32(b, 20)
	p.AccelY = f32(b, 24)
	p.AccelZ = f32(b, 28)

	p.VelX = f32(b, 32)
	p.VelY = f32(b, 36)
	p.VelZ = f32(b, 40)

	p.AngVelX = f32(b, 44)
	p.AngVelY = f32(b, 48)
	p.AngVelZ = f32(b, 52)

	p.Yaw = f32(b, 56)
	p.Pitch = f32(b, 60)
	p.Roll = f32(b, 64)

	for i := 0; i < 4; i++ {
		p.NormSusp[i] = f32(b, 68+i*4)
		p.SlipRatio[i] = f32(b, 84+i*4)
		p.WheelRotSpeed[i] = f32(b, 100+i*4)
		p.RumbleStrip[i] = i32(b, 116+i*4)
		p.PuddleDepth[i] = f32(b, 132+i*4)
		p.SurfaceRumble[i] = f32(b, 148+i*4)
		p.SlipAngle[i] = f32(b, 164+i*4)
		p.CombinedSlip[i] = f32(b, 180+i*4)
		p.SuspTravelM[i] = f32(b, 196+i*4)
	}

	p.CarOrdinal = i32(b, 212)
	p.CarClass = i32(b, 216)
	p.CarPI = i32(b, 220)
	p.DriveTrain = i32(b, 224)
	p.Cylinders = i32(b, 228)

	if off == 12 {
		p.CarCategory = i32(b, 232)
		p.SmashableVelDiff = f32(b, 236)
		p.SmashableMass = f32(b, 240)
	}

	p.PosX = f32(b, 232+off)
	p.PosY = f32(b, 236+off)
	p.PosZ = f32(b, 240+off)

	p.SpeedMs = f32(b, 244+off)
	vecSpd := float32(math.Sqrt(float64(p.VelX*p.VelX + p.VelY*p.VelY + p.VelZ*p.VelZ)))
	if p.SpeedMs <= 0.01 && vecSpd > 0.01 {
		p.SpeedMs = vecSpd
	}
	p.SpeedKmh = p.SpeedMs * 3.6

	p.PowerW = f32(b, 248+off)
	p.TorqueNm = f32(b, 252+off)

	for i := 0; i < 4; i++ {
		p.TireTemp[i] = f32(b, 256+off+i*4)
	}
	p.BoostPsi = f32(b, 272+off)
	p.Fuel = f32(b, 276+off)
	p.Distance = f32(b, 280+off)

	if 309+off <= len(b) {
		p.Accel = b[303+off]
		p.Brake = b[304+off]
		p.Clutch = b[305+off]
		p.Handbrake = b[306+off]
		p.Gear = b[307+off]
		p.Steer = int8(b[308+off])
	}

	if !finite32(p.CurrentRpm) || !finite32(p.EngineMaxRpm) || !finite32(p.EngineIdleRpm) || !finite32(p.RpmRatio) ||
		!finite32(p.SpeedMs) || !finite32(p.SpeedKmh) ||
		!finite32(p.AccelX) || !finite32(p.AccelY) || !finite32(p.AccelZ) ||
		!finite32(p.VelX) || !finite32(p.VelY) || !finite32(p.VelZ) ||
		!finite32(p.AngVelX) || !finite32(p.AngVelY) || !finite32(p.AngVelZ) ||
		!finite32(p.PowerW) || !finite32(p.TorqueNm) || !finite32(p.BoostPsi) ||
		!finite32(p.SmashableVelDiff) || !finite32(p.SmashableMass) {
		return Packet{}, ErrNonFinite
	}
	for i := 0; i < 4; i++ {
		if !finite32(p.NormSusp[i]) || !finite32(p.SuspTravelM[i]) || !finite32(p.CombinedSlip[i]) ||
			!finite32(p.SlipRatio[i]) || !finite32(p.WheelRotSpeed[i]) || !finite32(p.SlipAngle[i]) ||
			!finite32(p.SurfaceRumble[i]) || !finite32(p.PuddleDepth[i]) {
			return Packet{}, ErrNonFinite
		}
	}

	return p, nil
}
