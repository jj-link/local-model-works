package runs

import "testing"

func TestPausedTransitions(t *testing.T) {
	allowed := [][2]State{{Running, Paused}, {Paused, Running}, {Paused, Cancelling}, {Paused, Failed}, {Paused, Interrupted}}
	for _, transition := range allowed {
		if !canTransition(transition[0], transition[1]) {
			t.Errorf("transition %s -> %s rejected", transition[0], transition[1])
		}
	}
	for _, target := range []State{Queued, Planning, Waiting, Verifying, Succeeded, Cancelled} {
		if canTransition(Paused, target) {
			t.Errorf("transition paused -> %s accepted", target)
		}
	}
	if Paused.Terminal() {
		t.Error("paused is terminal")
	}
}
