package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"go.viam.com/test"
)

func TestProgressLine(t *testing.T) {
	tail := func(n int) string { return fmt.Sprintf(" (%d %s)", n, pluralize(n, "row")) }
	// The '%' proves prefix never reaches a format function.
	const prefix = "  rate%s Readings: f.ndjson"

	t.Run("redraws in place on a terminal", func(t *testing.T) {
		var buf bytes.Buffer
		line := &progressLine{w: &buf, prefix: prefix, tail: tail, terminal: true}
		line.start()
		line.update(100)
		line.finish(250)

		test.That(t, buf.String(), test.ShouldEqual,
			prefix+"\r"+prefix+" (100 rows)"+"\r"+prefix+" (250 rows)\n")
	})

	t.Run("off a terminal writes the line once", func(t *testing.T) {
		var buf bytes.Buffer
		line := &progressLine{w: &buf, prefix: prefix, tail: tail}
		line.start()
		line.update(100) // dropped: no cursor to move
		line.finish(250)

		test.That(t, buf.String(), test.ShouldEqual, prefix+" (250 rows)\n")
	})

	t.Run("pads a shorter redraw over a longer one", func(t *testing.T) {
		var buf bytes.Buffer
		line := &progressLine{
			w: &buf, terminal: true,
			tail: func(n int) string { return strings.Repeat("x", n) },
		}
		line.update(5)
		line.finish(2)

		test.That(t, buf.String(), test.ShouldEqual, "\rxxxxx"+"\rxx   \n")
	})

	t.Run("writes a whole line when start was skipped", func(t *testing.T) {
		var buf bytes.Buffer
		line := &progressLine{w: &buf, prefix: prefix, tail: tail}
		line.finish(1)

		test.That(t, buf.String(), test.ShouldEqual, prefix+" (1 row)\n")
	})
}
