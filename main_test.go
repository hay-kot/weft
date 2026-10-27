package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/commands"
)

// runApp runs weft's command tree on args with urfave's own output captured.
func runApp(t *testing.T, args ...string) (flags *commands.Flags, out string, err error) {
	t.Helper()
	flags = &commands.Flags{}
	app := newApp(flags)
	var buf bytes.Buffer
	app.Writer, app.ErrWriter = &buf, &buf
	err = app.Run(context.Background(), append([]string{"weft"}, args...))
	return flags, buf.String(), err
}

func writeWorkflow(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "one.yml")
	require.NoError(t, os.WriteFile(path, []byte("nodes:\n  a:\n    run: true\n"), 0o600))
	return path
}

func TestNoColorEnvAcceptsAnyValue(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on", "anything"} {
		assert.True(t, noColorEnv(func(string) string { return v }), v)
	}
	assert.False(t, noColorEnv(func(string) string { return "" }))
}

func TestNoColorEnvDoesNotFailParsing(t *testing.T) {
	t.Setenv("NO_COLOR", "yes")
	flags, _, err := runApp(t, "validate", writeWorkflow(t))
	require.NoError(t, err)
	assert.True(t, flags.NoColor)
}

func TestFlagErrorsSkipHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"bad value", []string{"run", "--ui=bogus", "x"}, "--ui must be auto, tui, or plain"},
		{"typo", []string{"run", "--resum", "x"}, "did you mean --resume? Run 'weft run --help'"},
		{"global typo", []string{"--log-levl=debug", "validate"}, "did you mean --log-level? Run 'weft --help'"},
		{"bad level", []string{"--log-level=bogus", "validate", "x.yml"}, `"bogus" is not a level`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, out, err := runApp(t, tt.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, out, "USAGE:", "help text printed on a flag error")
		})
	}
}
