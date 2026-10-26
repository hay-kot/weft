package paths

import (
	"path/filepath"
	"testing"
)

func TestXDGDirectories(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		resolve func() (string, error)
	}{
		{name: "config", envName: "XDG_CONFIG_HOME", resolve: ConfigDir},
		{name: "data", envName: "XDG_DATA_HOME", resolve: DataDir},
		{name: "cache", envName: "XDG_CACHE_HOME", resolve: CacheDir},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseDir := t.TempDir()
			t.Setenv(test.envName, baseDir)

			dir, err := test.resolve()
			if err != nil {
				t.Fatalf("resolve directory: %v", err)
			}

			want := filepath.Join(baseDir, appName)
			if dir != want {
				t.Fatalf("directory = %q, want %q", dir, want)
			}
		})
	}
}
