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
	off := false
	if got := EffectiveExposedHosts(&Snapshot{HostsExposed: &off}, configured); len(got) != 0 {
		t.Fatalf("legacy off = %v", got)
	}
	on := true
	got := EffectiveExposedHosts(&Snapshot{HostsExposed: &on}, configured)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("legacy on = %v", got)
	}
	got = EffectiveExposedHosts(&Snapshot{ExposedHosts: []string{"b", "gone", "b"}}, configured)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("explicit list = %v", got)
	}
	empty := []string{}
	if got := EffectiveExposedHosts(&Snapshot{ExposedHosts: empty, HostsExposed: &on}, configured); len(got) != 0 {
		t.Fatalf("explicit empty beats legacy on: %v", got)
	}
}
