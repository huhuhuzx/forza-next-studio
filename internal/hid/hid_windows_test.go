//go:build windows

package hid

import (
	"errors"
	"testing"
	"time"
)

func TestBuildCtrl0x53PayloadCap(t *testing.T) {
	oversized := make([]byte, 40)
	for i := range oversized {
		oversized[i] = byte(i + 1)
	}
	fr := BuildCtrl0x53(oversized...)
	if fr[4] != 29 { // 27 payload bytes + 2 header bytes
		t.Fatalf("BuildCtrl0x53 len byte = %d, want 29", fr[4])
	}
	if fr[31] != 27 {
		t.Fatalf("BuildCtrl0x53 last payload byte = %d, want 27", fr[31])
	}
	if fr[32] != Checksum(&fr) {
		t.Fatalf("BuildCtrl0x53 checksum mismatch")
	}
}

func TestSyncRightTriggerHoldOff500ms(t *testing.T) {
	now := time.Unix(1000, 0)
	var sent [][33]byte
	w := NewWriter()
	w.initialized = true
	w.nowFn = func() time.Time { return now }
	w.writeHook = func(frame [33]byte) error {
		sent = append(sent, frame)
		return nil
	}

	// Enable immediately sends 0x53 ... 0x40
	w.SyncRightTrigger(true)
	if len(sent) != 1 || sent[0][9] != 0x40 {
		t.Fatalf("expected immediate RT enable 0x40, got len=%d", len(sent))
	}

	// Brief false gaps (<500ms) must NOT disengage RT!
	w.SyncRightTrigger(false)
	now = now.Add(250 * time.Millisecond)
	w.SyncRightTrigger(false)
	now = now.Add(249 * time.Millisecond) // 499ms total
	w.SyncRightTrigger(false)
	if len(sent) != 1 {
		t.Fatalf("RT disengaged prematurely before 500ms hold-off: sent=%d", len(sent))
	}

	// Re-enabling resets the hold-off timer
	w.SyncRightTrigger(true)
	now = now.Add(10 * time.Millisecond)
	w.SyncRightTrigger(false)
	now = now.Add(400 * time.Millisecond)
	w.SyncRightTrigger(false)
	if len(sent) != 1 {
		t.Fatalf("RT disengaged after timer reset at 400ms: sent=%d", len(sent))
	}

	// Reaching >= 500ms continuously unneeded sends 0x53 ... 0x00 cleanly once
	now = now.Add(100 * time.Millisecond) // 500ms since last false onset
	w.SyncRightTrigger(false)
	if len(sent) != 2 || sent[1][9] != 0x00 {
		t.Fatalf("expected RT disable 0x00 at 500ms, got len=%d", len(sent))
	}
	// Additional false calls do not resend
	now = now.Add(1 * time.Second)
	w.SyncRightTrigger(false)
	if len(sent) != 2 {
		t.Fatalf("RT disable resent unexpectedly: sent=%d", len(sent))
	}
}

func TestWriterAutoDisconnectAfterFiveConsecutiveFailures(t *testing.T) {
	fail := true
	w := NewWriter()
	w.initialized = true
	w.writeHook = func(frame [33]byte) error {
		if fail {
			return errors.New("usb device disconnected")
		}
		return nil
	}

	// 4 failures + 1 success resets consecutive counter
	for i := 0; i < 4; i++ {
		_ = w.WriteReport(BuildNeutral0x57())
	}
	if !w.IsOpen() {
		t.Fatalf("writer closed early after only 4 failures")
	}
	fail = false
	if err := w.WriteReport(BuildNeutral0x57()); err != nil {
		t.Fatalf("unexpected error on recovery write: %v", err)
	}

	// Now 5 consecutive failures must auto-close/disconnect
	fail = true
	for i := 0; i < 5; i++ {
		_ = w.WriteReport(BuildNeutral0x57())
	}
	if w.IsOpen() {
		t.Fatalf("writer did not auto-close after 5 consecutive write failures")
	}
	_, fails, lastErr, open, _ := w.Stats()
	if open || fails != 9 || lastErr != "usb device disconnected" {
		t.Fatalf("unexpected stats after disconnect: open=%v fails=%d lastErr=%q", open, fails, lastErr)
	}
}
