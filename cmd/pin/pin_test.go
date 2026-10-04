package pin

import "testing"

func TestIsMountCommand(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"mount", true},
		{"cmount", true},
		{"nfsmount", true},
		{"login", false},
		{"pin", false},
		{"unpin", false},
		{"lsl", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMountCommand(c.name); got != c.want {
				t.Errorf("isMountCommand(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
