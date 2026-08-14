package cli

import (
	"fmt"
	"io"
	"testing"

	"github.com/urfave/cli/v3"
	"go.viam.com/test"
)

// uintFlagsNamed returns every cli.UintFlag called name anywhere in the command tree, so a test
// can assert against all of them without naming the commands that carry it.
func uintFlagsNamed(cmds []*cli.Command, name string) []*cli.UintFlag {
	var found []*cli.UintFlag
	for _, cmd := range cmds {
		for _, f := range cmd.Flags {
			if uintFlag, ok := f.(*cli.UintFlag); ok && uintFlag.Name == name {
				found = append(found, uintFlag)
			}
		}
		found = append(found, uintFlagsNamed(cmd.Commands, name)...)
	}
	return found
}

// TestParallelFlagRejectsZero asserts every --parallel flag rejects an explicit 0 while leaving
// its default intact. Zero workers does no work at all, so the CLI should say so rather than
// quietly substituting the default. Searching the tree (rather than naming the three commands)
// means a fourth --parallel flag added without the validator fails here.
func TestParallelFlagRejectsZero(t *testing.T) {
	flags := uintFlagsNamed(NewApp(io.Discard, io.Discard).Commands, dataFlagParallelDownloads)
	// Guards the search itself: if it stops finding flags, the assertions below are vacuous.
	test.That(t, len(flags), test.ShouldEqual, 3)

	for _, flag := range flags {
		test.That(t, flag.Validator, test.ShouldNotBeNil)
		test.That(t, flag.Validator(0), test.ShouldBeError,
			fmt.Errorf("--%s must be greater than 0", dataFlagParallelDownloads))
		// A sane value and the flag's own default must both pass.
		test.That(t, flag.Validator(1), test.ShouldBeNil)
		test.That(t, flag.Value, test.ShouldEqual, uint(defaultParallelBinaryDownloads))
		test.That(t, flag.Validator(flag.Value), test.ShouldBeNil)
	}
}
