package binaries

import (
	"runtime"
	"strings"
	"testing"
)

func TestBinaryValidate(t *testing.T) {
	cases := []struct {
		name    string
		bin     Binary
		wantErr string
	}{
		{
			name: "ok",
			bin: Binary{
				Name: "gh",
				Install: []InstallSpec{{
					OS: "linux", Manager: "apt", Package: "gh",
				}},
			},
		},
		{
			name:    "missing name",
			bin:     Binary{Install: []InstallSpec{{OS: "linux", Manager: "apt", Package: "x"}}},
			wantErr: "name required",
		},
		{
			name: "empty OS is wildcard (matches brew on any host)",
			bin: Binary{
				Name: "gh",
				Install: []InstallSpec{{Manager: "brew", Package: "gh"}},
			},
		},
		{
			name: "bad name char",
			bin: Binary{
				Name: "gh!",
				Install: []InstallSpec{{OS: "linux", Manager: "apt", Package: "gh"}},
			},
			wantErr: "lowercase",
		},
		{
			name:    "no install spec",
			bin:     Binary{Name: "gh"},
			wantErr: "at least one install spec",
		},
		{
			name: "unknown manager",
			bin: Binary{
				Name: "gh",
				Install: []InstallSpec{{OS: "linux", Manager: "snap", Package: "gh"}},
			},
			wantErr: "unknown manager",
		},
		{
			name: "curl-sh missing checksum",
			bin: Binary{
				Name: "uvx",
				Install: []InstallSpec{{
					OS: "linux", Manager: "curl-sh", URL: "https://astral.sh/uv/install.sh",
				}},
			},
			wantErr: "checksum",
		},
		{
			name: "curl-sh wrong-shape checksum",
			bin: Binary{
				Name: "uvx",
				Install: []InstallSpec{{
					OS:       "linux",
					Manager:  "curl-sh",
					URL:      "https://astral.sh/uv/install.sh",
					Checksum: "md5:abc",
				}},
			},
			wantErr: "checksum",
		},
		{
			name: "apt missing package",
			bin: Binary{
				Name: "gh",
				Install: []InstallSpec{{OS: "linux", Manager: "apt"}},
			},
			wantErr: "requires package",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.bin.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q missing substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestInstallSpecMatch(t *testing.T) {
	cases := []struct {
		spec InstallSpec
		want bool
	}{
		{InstallSpec{OS: runtime.GOOS}, true},
		{InstallSpec{OS: "freebsd"}, runtime.GOOS == "freebsd"},
		{InstallSpec{OS: runtime.GOOS, Arch: runtime.GOARCH}, true},
		{InstallSpec{OS: runtime.GOOS, Arch: "imaginary"}, false},
	}
	for i, tc := range cases {
		got := tc.spec.Match()
		if got != tc.want {
			t.Errorf("case %d: spec=%+v match=%v want=%v", i, tc.spec, got, tc.want)
		}
	}
}
