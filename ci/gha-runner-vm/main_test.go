package main

import "testing"

func TestParseImageExists(t *testing.T) {
	const name, version = "ubuntu-24.04-amd64-gha-image", "ubuntu24/20260901.1"
	match := `{"data":[{"operating-system":"ubuntu-24.04-amd64-gha-image","operating-system-version":"ubuntu24/20260901.1","id":"ocid1.image.oc1..x"}]}`
	other := `{"data":[{"operating-system":"rc-ubuntu-24.04-amd64-gha-image","operating-system-version":"ubuntu24/20260901.1"}]}`

	cases := []struct {
		name    string
		output  string
		want    bool
		wantErr bool
	}{
		{"empty output means absent", "", false, false},
		{"whitespace only means absent", "\n  \n", false, false},
		{"empty data list means absent", `{"data":[]}`, false, false},
		{"exact match", match, true, false},
		{"release candidate does not count", other, false, false},
		{"garbage is an error", "not json", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImageExists([]byte(tc.output), name, version)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPackerBakesFuseOverlayfs(t *testing.T) {
	for _, v := range replacements {
		if containsAll(v, "fuse-overlayfs", "linux-oracle") {
			return
		}
	}
	t.Fatal("packer post-install step must bake fuse-overlayfs (dockerd storage driver inside kata guests)")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
