package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestDaemonInstallCommands_Registered verifies `guild daemon
// install|uninstall` are wired under the daemon command and respond to
// --help (mirrors TestSubcommandsHelpable in root_test.go).
func TestDaemonInstallCommands_Registered(t *testing.T) {
	cases := [][]string{
		{"daemon", "install", "--help"},
		{"daemon", "uninstall", "--help"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(args)
			t.Cleanup(func() { rootCmd.SetArgs(nil) })

			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if !strings.Contains(buf.String(), "login service") {
				t.Errorf("%v help missing 'login service':\n%s", args, buf.String())
			}
		})
	}
}

// TestDaemonInstallHelp_DocumentsManualPath pins the help promises the
// command makes: manual instructions when the service manager is
// absent, and idempotent repeat installs.
func TestDaemonInstallHelp_DocumentsManualPath(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"daemon", "install", "--help"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("daemon install --help: %v", err)
	}
	for _, want := range []string{
		"manual load instructions",
		"Idempotent",
		"daemon\nrun",
		"guild daemon uninstall",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("daemon install --help missing %q:\n%s", want, buf.String())
		}
	}
}
