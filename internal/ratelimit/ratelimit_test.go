package ratelimit

import (
	"testing"
	"time"
)

func lim3(w Window) Limits { return Limits{w: Ptr(3)} }

func TestCheckAndReserveAndUsage(t *testing.T) {
	l := New()
	defer l.Stop()

	limits := Limits{RPM: Ptr(2), TPD: Ptr(100)}
	if r := l.CheckAndReserve("k1", "m", "slot-a", LevelModel, limits, Costs{Tokens: 60}); r != nil {
		t.Fatalf("unexpected rejection: %+v", r)
	}
	if got := l.Usage("k1", "m", "slot-a", LevelModel, RPM); got != 1 {
		t.Errorf("RPM usage = %d, want 1", got)
	}
	if got := l.Usage("k1", "m", "slot-a", LevelModel, TPD); got != 60 {
		t.Errorf("TPD usage = %d, want 60", got)
	}
	if got := l.Usage("k1", "m", "slot-b", LevelModel, RPM); got != 0 {
		t.Errorf("other slot usage = %d, want 0", got)
	}
}

func TestSlotCellsIndependent(t *testing.T) {
	l := New()
	defer l.Stop()

	limits := lim3(RPM)
	// Two slots sharing one model each charge their own cell.
	l.CheckAndReserve("k", "m", "a", LevelModel, limits, Costs{})
	l.CheckAndReserve("k", "m", "b", LevelModel, limits, Costs{})
	if got := l.Usage("k", "m", "a", LevelModel, RPM); got != 1 || l.Usage("k", "m", "b", LevelModel, RPM) != 1 {
		t.Fatalf("slot cells were not independent: %d/%d", l.Usage("k", "m", "a", LevelModel, RPM), l.Usage("k", "m", "b", LevelModel, RPM))
	}

	// Rejection on one slot does not object to the other.
	if r := l.CheckAndReserve("k", "m", "b", LevelModel, limits, Costs{}); r != nil {
		t.Fatalf("unexpected rejection on slot b: %+v", r)
	}
}

func TestRejectionRollsBackStagedWindows(t *testing.T) {
	l := New()
	defer l.Stop()

	limits := Limits{RPD: Ptr(1), TPM: Ptr(5)}
	if r := l.CheckAndReserve("k1", "m", "", LevelModel, limits, Costs{Tokens: 1}); r != nil {
		t.Fatalf("first reservation failed: %+v", r)
	}
	// Windows order runs TPM before RPD, so TPM commits and RPD rejects.
	r := l.CheckAndReserve("k1", "m", "", LevelModel, limits, Costs{Tokens: 1})
	if r == nil {
		t.Fatal("wanted a rejection")
	}
	if got := l.Usage("k1", "m", "", LevelModel, TPM); got != 1 {
		t.Errorf("staged TPM increment not rolled back: %d", got)
	}
}

func TestCharAndAudioWindows(t *testing.T) {
	l := New()
	defer l.Stop()

	limits := Limits{CPD: Ptr(10), ASEC: Ptr(30), AMIN: Ptr(1)}
	if r := l.CheckAndReserve("k", "tts", "s", LevelModel, limits, Costs{Chars: 8, AudioSec: 20}); r != nil {
		t.Fatalf("unexpected rejection: %+v", r)
	}
	// 8 chars consumed 8 of 10; two more reject.
	if r := l.CheckAndReserve("k", "tts", "s", LevelModel, limits, Costs{Chars: 3}); r == nil || r.Window != CPD {
		t.Errorf("want CPD rejection, got %+v", r)
	}
	// AMIN charges ceil(seconds/60): 20s -> 1 minute, at the cap.
	if got := l.Usage("k", "tts", "s", LevelModel, AMIN); got != 1 {
		t.Errorf("AMIN usage = %d, want 1", got)
	}
}

func TestRefundRestoresAllWindows(t *testing.T) {
	l := New()
	defer l.Stop()

	limits := Limits{RPM: Ptr(1), TPM: Ptr(10)}
	c := Costs{Tokens: 7}
	if r := l.CheckAndReserve("k", "m", "a", LevelModel, limits, c); r != nil {
		t.Fatal(r)
	}
	l.Refund("k", "m", "a", LevelModel, limits, c)
	for _, w := range []Window{RPM, TPM} {
		if got := l.Usage("k", "m", "a", LevelModel, w); got != 0 {
			t.Errorf("%s usage after refund = %d, want 0", w, got)
		}
	}
	if l.CheckAndReserve("k", "m", "a", LevelModel, limits, c) != nil {
		t.Error("refund did not release capacity")
	}
}

func TestReconcileAdjustsTokensOnly(t *testing.T) {
	l := New()
	defer l.Stop()

	limits := Limits{RPM: Ptr(1), TPM: Ptr(100)}
	l.CheckAndReserve("k", "m", "", LevelModel, limits, Costs{Tokens: 80})
	l.Reconcile("k", "m", "", LevelModel, 30, 80)
	if got := l.Usage("k", "m", "", LevelModel, TPM); got != 30 {
		t.Errorf("TPM after reconcile = %d, want 30", got)
	}
	if got := l.Usage("k", "m", "", LevelModel, RPM); got != 1 {
		t.Errorf("RPM changed by reconcile: %d", got)
	}
}

func TestMinuteRollover(t *testing.T) {
	l := New()
	defer l.Stop()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	limits := Limits{RPM: Ptr(1)}
	if r := l.CheckAndReserve("k", "m", "", LevelModel, limits, Costs{}); r != nil {
		t.Fatal(r)
	}
	if r := l.CheckAndReserve("k", "m", "", LevelModel, limits, Costs{}); r == nil {
		t.Fatal("wanted a rejection in the same bucket")
	}
	now = now.Add(time.Minute)
	if r := l.CheckAndReserve("k", "m", "", LevelModel, limits, Costs{}); r != nil {
		t.Fatalf("bucket did not roll over: %+v", r)
	}
}
