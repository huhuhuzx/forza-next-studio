package engine

import (
	"encoding/hex"
	"sync"
	"time"

	"forzanext/internal/hid"
	"forzanext/internal/telemetry"
)

// LiveSnapshot is streamed via SSE to the modern frontend at 25 Hz.
type LiveSnapshot struct {
	TimestampMs    int64            `json:"ts"`
	EngineActive   bool             `json:"engineActive"`
	GameConnected  bool             `json:"gameConnected"`
	PacketsTotal   uint64           `json:"packetsTotal"`
	RendersTotal   uint64           `json:"rendersTotal"`
	ClippedFrames  uint64           `json:"clippedFrames"`
	ConflictsFixed uint64           `json:"conflictsFixed"`
	SurfaceLabel   string           `json:"surfaceLabel"`
	ImpactActive   bool             `json:"impactActive"`
	ImpactAngleDeg float64          `json:"impactAngleDeg"`
	ImpactLabel    string           `json:"impactLabel"`
	GripLRms       float64          `json:"gripLRms"`
	GripRRms       float64          `json:"gripRRms"`
	Packet         telemetry.Packet `json:"packet"`
	Car            CarPersonality   `json:"car"`
	Events         []ActiveEvent    `json:"events"`
	Trig           [8]byte          `json:"trig"`
	GripL          [8]byte          `json:"gripL"`
	GripR          [8]byte          `json:"gripR"`
	TrigRoute      int              `json:"trigRoute"`
	FlagsHex       string           `json:"flagsHex"`
	LastReportHex  string           `json:"lastReportHex"`
}

// Runtime runs the 8 ms real-time loop for Entertainment (Next) mode.
type Runtime struct {
	mu           sync.RWMutex
	cfgPath      string
	cfg          NextConfig
	profiler     *Profiler
	synth        *Synthesizer
	writer       *hid.Writer
	active       bool
	lastPkt      telemetry.Packet
	lastPktAt    time.Time
	prevPktAt    time.Time
	packetsTotal uint64
	rendersTotal uint64
	clippedTotal uint64
	conflictTot  uint64
	latestSnap   LiveSnapshot

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewRuntime(cfgPath, garagePath string, writer *hid.Writer) *Runtime {
	return &Runtime{
		cfgPath:  cfgPath,
		cfg:      LoadNextConfig(cfgPath),
		profiler: NewProfiler(garagePath),
		synth:    NewSynthesizer(),
		writer:   writer,
		active:   true,
		stopCh:   make(chan struct{}),
	}
}

func (r *Runtime) Start() {
	r.wg.Add(1)
	go r.loop()
}

func (r *Runtime) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	r.wg.Wait()
}

func (r *Runtime) SetActive(on bool) {
	r.mu.Lock()
	r.active = on
	if !on {
		r.synth.Reset()
		r.latestSnap.EngineActive = false
		r.latestSnap.ImpactActive = false
		r.latestSnap.Events = nil
		r.latestSnap.TrigRoute = -1
	}
	r.mu.Unlock()
}

func (r *Runtime) IsActive() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

func (r *Runtime) Config() NextConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *Runtime) UpdateConfig(cfg NextConfig) error {
	cfg.Sanitize()
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	return SaveNextConfig(r.cfgPath, cfg)
}

func (r *Runtime) Profiler() *Profiler {
	return r.profiler
}

func (r *Runtime) TriggerTest(eventID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synth.TriggerTest(eventID)
}

func (r *Runtime) IngestPacket(pkt telemetry.Packet) {
	now := time.Now()
	r.mu.Lock()
	dt := 0.016
	if !r.prevPktAt.IsZero() {
		dt = now.Sub(r.prevPktAt).Seconds()
	}
	r.prevPktAt = now
	r.lastPkt = pkt
	r.lastPktAt = now
	r.packetsTotal++
	cfg := r.cfg
	car := r.profiler.Observe(pkt, cfg.AutoCarMatching, cfg.AutoRedlineLearn)
	if r.active {
		r.synth.StepPacket(pkt, cfg, car, dt)
	}
	r.mu.Unlock()
}

func (r *Runtime) Snapshot() LiveSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := r.latestSnap
	snap.Car = r.profiler.Active()
	return snap
}

func (r *Runtime) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(8 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case now := <-ticker.C:
			r.tick(now)
		}
	}
}

func (r *Runtime) tick(now time.Time) {
	r.mu.Lock()
	active := r.active
	cfg := r.cfg
	pkt := r.lastPkt
	fresh := !r.lastPktAt.IsZero() && now.Sub(r.lastPktAt) < 800*time.Millisecond
	car := r.profiler.Active()

	if !active {
		r.mu.Unlock()
		return
	}

	res := r.synth.Render8ms(pkt, cfg, car, fresh)
	r.rendersTotal++
	if res.Clipped {
		r.clippedTotal++
	}
	if res.ConflictSolved {
		r.conflictTot++
	}

	r.latestSnap = LiveSnapshot{
		TimestampMs:    now.UnixMilli(),
		EngineActive:   true,
		GameConnected:  fresh && pkt.IsRaceOn,
		PacketsTotal:   r.packetsTotal,
		RendersTotal:   r.rendersTotal,
		ClippedFrames:  r.clippedTotal,
		ConflictsFixed: r.conflictTot,
		SurfaceLabel:   res.SurfaceLabel,
		ImpactActive:   res.ImpactActive,
		ImpactAngleDeg: res.ImpactAngleDeg,
		ImpactLabel:    res.ImpactLabel,
		GripLRms:       res.GripLRms,
		GripRRms:       res.GripRRms,
		Packet:         pkt,
		Car:            car,
		Events:         res.ActiveEvents,
		Trig:           res.Trig,
		GripL:          res.GripL,
		GripR:          res.GripR,
		TrigRoute:      res.TrigRoute,
		FlagsHex:       hex.EncodeToString([]byte{res.Flags}),
		LastReportHex:  hid.FrameHex(res.Frame),
	}
	r.mu.Unlock()

	if r.writer.IsOpen() {
		r.writer.SyncRightTrigger(res.RightTrigNeed)
		_ = r.writer.WriteReport(res.Frame)
	}
}
