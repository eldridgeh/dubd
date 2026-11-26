package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestFlagsFirst(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"3", "-from", "a"}, []string{"-from", "a", "3"}},
		{[]string{"-from", "a", "3"}, []string{"-from", "a", "3"}},
		{[]string{"7", "-addr", "127.0.0.1:8401"}, []string{"-addr", "127.0.0.1:8401", "7"}},
		{[]string{"-addr=127.0.0.1:1", "7"}, []string{"-addr=127.0.0.1:1", "7"}},
		{[]string{"-2"}, []string{"-2"}}, // a negative metric is a value, not a flag
		{[]string{"-from", "a", "--", "-2"}, []string{"-from", "a", "--", "-2"}},
		{nil, nil},
	} {
		if got := flagsFirst(tc.in); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("flagsFirst(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAddrDefault(t *testing.T) {
	t.Setenv("DUBD_ADDR", "")
	if got := addrDefault(); got != defaultAddr {
		t.Fatalf("without DUBD_ADDR: %q", got)
	}
	t.Setenv("DUBD_ADDR", "127.0.0.1:9001")
	if got := addrDefault(); got != "127.0.0.1:9001" {
		t.Fatalf("with DUBD_ADDR: %q", got)
	}
}

func TestSurfaceFilePerInstance(t *testing.T) {
	plain, a, b := surfaceFile(""), surfaceFile("alpha"), surfaceFile("beta")
	if plain == "" {
		t.Skip("no user cache directory here")
	}
	if filepath.Base(plain) != "surface" || filepath.Base(a) != "surface.alpha" || a == b || a == plain {
		t.Fatalf("files: %q %q %q", plain, a, b)
	}
	if filepath.Dir(a) != filepath.Dir(plain) {
		t.Fatalf("named instance file is outside the dubd directory: %q", a)
	}
}

func TestInstanceNames(t *testing.T) {
	for _, ok := range []string{"alpha", "team-2", "army_3.east", "A1"} {
		if !nameRE.MatchString(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "has space", "-dash"} {
		if nameRE.MatchString(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRunRefusesABadName(t *testing.T) {
	if err := run([]string{"-name", "../escape"}); err == nil {
		t.Fatal("expected an error for a name that is a path")
	}
}
