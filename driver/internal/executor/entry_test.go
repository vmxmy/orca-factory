package executor

import (
	"path/filepath"
	"testing"
)

func TestExecutorStateRoot(t *testing.T) {
	for _, tc := range []struct {
		name, root, home string
		wantErr          bool
	}{
		{name: "explicit", root: "/custom/state", home: "/home/test"},
		{name: "explicit without home", root: "/custom/state"},
		{name: "default", home: "/home/test"},
		{name: "unavailable", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_DRIVER_STATE_ROOT", tc.root)
			t.Setenv("HOME", tc.home)
			root, err := executorStateRoot()
			if tc.wantErr {
				if err == nil {
					t.Fatal("missing home accepted")
				}
				return
			}
			want := tc.root
			if want == "" {
				want = filepath.Join(tc.home, ".local", "state", "orca-factory")
			}
			if err != nil || root != want {
				t.Fatalf("root=%q want=%q err=%v", root, want, err)
			}
		})
	}
}
