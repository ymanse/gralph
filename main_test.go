package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for issue #3: the profile path must be accepted both before and
// after the flags.
func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		path     string
		instance string
		maxIter  int
		wantErr  bool
	}{
		{"path first", []string{"profile.yaml", "--max-iterations", "3"}, "profile.yaml", "", 3, false},
		{"flags first", []string{"--max-iterations", "3", "profile.yaml"}, "profile.yaml", "", 3, false},
		{"path only", []string{"profile.yaml"}, "profile.yaml", "", 0, false},
		{"with name", []string{"profile.yaml", "--name", "feat-a"}, "profile.yaml", "feat-a", 0, false},
		{"name first", []string{"--name=feat-a", "profile.yaml"}, "profile.yaml", "feat-a", 0, false},
		{"no path", []string{"--max-iterations", "3"}, "", "", 0, true},
		{"empty", nil, "", "", 0, true},
		{"two paths", []string{"a.yaml", "b.yaml"}, "", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, instance, maxIter, err := parseRunArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got path=%q instance=%q maxIter=%d", path, instance, maxIter)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if path != tc.path || instance != tc.instance || maxIter != tc.maxIter {
				t.Fatalf("got path=%q instance=%q maxIter=%d, want path=%q instance=%q maxIter=%d",
					path, instance, maxIter, tc.path, tc.instance, tc.maxIter)
			}
		})
	}
}

// The orchestrator injects $GRALPH_INSTANCE_NAME so in-session subcommands
// land on the same instance's state dir; an explicit --name overrides it.
func TestSessionInstanceResolution(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(pp, []byte("commands:\n  - name: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRALPH_PROFILE", pp)

	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("GRALPH_INSTANCE_NAME", "feat-a")
		p, rest, err := profileFromSessionArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rest) != 0 || p.StateDir != filepath.Join(dir, ".gralph", "feat-a") {
			t.Fatalf("rest=%v state dir=%q", rest, p.StateDir)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("GRALPH_INSTANCE_NAME", "feat-a")
		p, rest, err := profileFromSessionArgs([]string{"--name", "feat-b", "--goal", "g"})
		if err != nil {
			t.Fatal(err)
		}
		if p.StateDir != filepath.Join(dir, ".gralph", "feat-b") {
			t.Fatalf("state dir=%q", p.StateDir)
		}
		// --name is consumed; the command's own args pass through.
		if len(rest) != 2 || rest[0] != "--goal" {
			t.Fatalf("rest=%v", rest)
		}
	})

	t.Run("default is filename stem", func(t *testing.T) {
		// ponytail: the harness itself runs under $GRALPH_INSTANCE_NAME, so an
		// ambient value would silently decide this subtest. Clear it explicitly.
		t.Setenv("GRALPH_INSTANCE_NAME", "")
		p, _, err := profileFromSessionArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "profile" || p.StateDir != filepath.Join(dir, ".gralph", "profile") {
			t.Fatalf("name=%q state dir=%q", p.Name, p.StateDir)
		}
	})
}
