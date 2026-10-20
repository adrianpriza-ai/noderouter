package main

// Every subcommand that logs takes -v, and a subcommand that silently lost the
// flag is invisible: nothing errors, the command just says less than it used
// to. The flag sets are built inside each run function, so the only place that
// can see them is the command itself - which is what this asks for, in a child
// process, because flag.ExitOnError answers -h by printing usage and exiting.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// helpEnv marks the child process spawned by TestEverySubcommandTakesVerbose.
const helpEnv = "NODEROUTER_TEST_FLAG_HELP"

// TestEverySubcommandTakesVerbose pins that each command that logs accepts -v
// and describes it the same way. serve and convert have had it all along;
// check was the odd one out, and the fix for that is only worth anything if
// the flag cannot go missing again unnoticed.
//
// The child re-enters this test with helpEnv set and calls the command with
// -h, which prints its flag defaults and exits 0. The parent reads that back,
// so the assertion is on what a user would actually see.
func TestEverySubcommandTakesVerbose(t *testing.T) {
	for _, tc := range []struct {
		command string
		run     func([]string)
	}{
		{"serve", runServe},
		{"convert", runConvert},
		{"check", runCheck},
	} {
		t.Run(tc.command, func(t *testing.T) {
			if os.Getenv(helpEnv) == tc.command {
				// -h prints usage and exits 0 without running the command.
				tc.run([]string{"-h"})
				return
			}
			out := flagHelp(t, tc.command)
			if !strings.Contains(out, "-v") {
				t.Errorf("`noderouter %s -h` does not offer -v:\n%s", tc.command, out)
			}
			// The description matters as much as the flag: "-v" alone in a
			// usage block is indistinguishable from a value named v.
			if !strings.Contains(out, "debug logging") {
				t.Errorf("`noderouter %s -v` is not described as debug logging:\n%s", tc.command, out)
			}
		})
	}
}

// flagHelp runs the test binary as `noderouter <command> -h` and returns
// everything it printed. A failure to spawn is a test failure rather than a
// skip: the binary is the test binary, so there is nothing environment
// dependent about it.
func flagHelp(t *testing.T, command string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestEverySubcommandTakesVerbose/"+command)
	cmd.Env = append(os.Environ(), helpEnv+"="+command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`noderouter %s -h`: %v\n%s", command, err, out)
	}
	return string(out)
}
