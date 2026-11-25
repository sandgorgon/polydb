package version

import "testing"

func TestReleased(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.0.0": true, "v1.2.3-rc.1": true, "": false, "(devel)": false,
		"v0.0.0-20261125050144-dc855ceed651": false, "v1.0.1-0.20261125050144-dc855ceed651": false,
		"v1.0.0+dirty": false,
	} {
		if released(v) != want {
			t.Errorf("released(%q) = %v", v, !want)
		}
	}
	Version = "v1.2.3"
	defer func() { Version = "" }()
	if String() != "1.2.3" {
		t.Errorf("String() = %q", String())
	}
}
