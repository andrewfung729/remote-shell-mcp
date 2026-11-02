package state

import "testing"

func TestEffectiveExposedHosts(t *testing.T) {
	configured := []string{"a", "b"}
	if got := EffectiveExposedHosts(nil, configured); len(got) != 0 {
		t.Fatalf("nil snap = %v", got)
	}
	if got := EffectiveExposedHosts(&Snapshot{}, configured); len(got) != 0 {
		t.Fatalf("unset = %v", got)
	}
	got := EffectiveExposedHosts(&Snapshot{ExposedHosts: []string{"b", "gone", "b"}}, configured)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("explicit list = %v", got)
	}
	empty := []string{}
	if got := EffectiveExposedHosts(&Snapshot{ExposedHosts: empty}, configured); len(got) != 0 {
		t.Fatalf("explicit empty: %v", got)
	}
}
