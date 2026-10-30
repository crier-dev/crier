package testsupport

import "testing"

func TestHangGuardStaysWithinBounds(t *testing.T) {
	budget := HangGuard(t)
	if budget < HangGuardFloor {
		t.Fatalf("HangGuard = %s, below floor %s", budget, HangGuardFloor)
	}
	if budget > HangGuardCeiling {
		t.Fatalf("HangGuard = %s, above ceiling %s", budget, HangGuardCeiling)
	}
	load, factor, cpus := LoadFactor()
	if cpus <= 0 {
		t.Fatalf("LoadFactor returned cpus=%d", cpus)
	}
	if factor < 1 {
		t.Fatalf("LoadFactor factor %.2f < 1", factor)
	}
	if load >= 0 && load <= float64(cpus) && factor != 1 {
		t.Fatalf("LoadFactor load=%.2f over %d CPUs produced factor %.2f, want 1", load, cpus, factor)
	}
}
