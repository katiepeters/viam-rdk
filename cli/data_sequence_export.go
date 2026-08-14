package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/urfave/cli/v3"
	datapb "go.viam.com/api/app/data/v1"

	"go.viam.com/rdk/data"
)

const (
	sequenceTabularDir      = "tabular"
	sequenceBinaryExportDir = "binary"
)

// unsafeFileNameChars matches everything we refuse to put in a generated file name. Resource and
// method names come from user config, so they can contain path separators and other characters
// that would escape the destination directory.
var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type dataExportSequenceArgs struct {
	Destination string
	SequenceID  string
	Parallel    uint
	Timeout     uint
	OnlyTabular bool
	OnlyBinary  bool
}

// DataExportSequenceAction is the corresponding action for 'data export sequence'.
func DataExportSequenceAction(ctx context.Context, cmd *cli.Command, args dataExportSequenceArgs) error {
	client, err := newViamClient(ctx, cmd)
	if err != nil {
		return err
	}

	return client.dataExportSequenceAction(ctx, args)
}

// dataExportSequenceAction exports a sequence's binary and tabular data.
func (c *viamClient) dataExportSequenceAction(ctx context.Context, args dataExportSequenceArgs) error {
	if args.OnlyTabular && args.OnlyBinary {
		return errors.Errorf("--%s and --%s cannot both be provided", dataFlagOnlyTabular, dataFlagOnlyBinary)
	}

	resp, err := c.dataClient.GetSequence(ctx, &datapb.GetSequenceRequest{Id: args.SequenceID})
	if err != nil {
		return errors.Wrapf(err, "failed to look up sequence %s", args.SequenceID)
	}
	sequence := resp.GetSequence()
	if sequence == nil {
		return errors.Errorf("sequence %s not found", args.SequenceID)
	}

	if err := makeDestinationDirs(args.Destination); err != nil {
		return errors.Wrap(err, "could not create destination directory")
	}
	printf(c.c.Root().Writer, "Exporting sequence %s to %s", sequence.GetId(), args.Destination)

	if !args.OnlyBinary {
		if err := c.exportSequenceTabular(ctx, sequence, args.Destination); err != nil {
			return err
		}
	}
	if !args.OnlyTabular {
		if err := c.exportSequenceBinary(ctx, sequence.GetId(), args.Destination, args.Parallel, args.Timeout); err != nil {
			return err
		}
	}
	return nil
}

// exportSequenceTabular runs one tabular export per resource the sequence references, scoped to
// the sequence's part and capture interval, writing each to its own NDJSON file under dst/tabular/.
func (c *viamClient) exportSequenceTabular(ctx context.Context, sequence *datapb.Sequence, dst string) error {
	printf(c.c.Root().Writer, "")
	printf(c.c.Root().Writer, "Tabular data (%s/):", sequenceTabularDir)

	resources := sequence.GetResources()
	if len(resources) == 0 {
		printf(c.c.Root().Writer, "  none")
		return nil
	}

	tabularDir := filepath.Join(dst, sequenceTabularDir)
	if err := makeDestinationDirs(tabularDir); err != nil {
		return errors.Wrapf(err, "could not create %s", tabularDir)
	}

	interval := &datapb.CaptureInterval{Start: sequence.GetStartTime(), End: sequence.GetEndTime()}
	names := sequenceTabularFileNames(resources)
	for i, resource := range resources {
		// A binary capture method (GetImages, ReadImage, NextPointCloud, ...) has no tabular data
		// by definition. Skip silently -- the binary half of the export covers these resources, so
		// naming them here would only be noise.
		if data.MethodToCaptureType(resource.GetMethodName()) == data.CaptureTypeBinary {
			continue
		}

		subtype, err := c.resolveResourceSubtype(ctx, sequence.GetPartId(), resource, interval)
		if err != nil {
			return err
		}
		if subtype == "" {
			// MethodToCaptureType defaults unknown methods to tabular, so a module's binary method
			// that isn't on its list still reaches here; no rows means nothing to export either way.
			printf(c.c.Root().Writer, "  %s %s: no tabular data in the sequence's interval, skipping",
				resource.GetResourceName(), resource.GetMethodName())
			continue
		}

		request := &datapb.ExportTabularDataRequest{
			PartId:          sequence.GetPartId(),
			ResourceName:    resource.GetResourceName(),
			ResourceSubtype: subtype,
			MethodName:      resource.GetMethodName(),
			Interval:        interval,
		}

		line := newProgressLine(c.c.Root().Writer,
			fmt.Sprintf("  %s %s: %s", resource.GetResourceName(), resource.GetMethodName(), names[i]),
			" (%d rows)")
		line.start()

		// io.Discard for the writer: its only output is a dot per retry attempt, which the row
		// count supersedes. Progress comes through onRows instead.
		rows, err := c.tabularDataToFile(filepath.Join(tabularDir, names[i]), request, io.Discard, line.update)
		if err != nil {
			line.abandon()
			return errors.Wrapf(err, "failed to export tabular data for resource %q method %q",
				resource.GetResourceName(), resource.GetMethodName())
		}
		line.finish(rows)
	}
	return nil
}

// resolveResourceSubtype discovers a resource's subtype, which ExportTabularData requires but a
// SequenceResourceFilter does not record. It asks for a single captured row matching the same
// part, resource, method, and interval the export will use, and reads the subtype off that row's
// CaptureMetadata. Returns "" when the resource has no tabular data in the interval -- which is
// the normal case for a binary-only resource such as a camera, so it means "nothing to export
// here", not "this resource is empty".
func (c *viamClient) resolveResourceSubtype(
	ctx context.Context, partID string, resource *datapb.SequenceResourceFilter, interval *datapb.CaptureInterval,
) (string, error) {
	//nolint:staticcheck // TabularDataByFilter is deprecated, but we are using this to get the missing subtype.
	// We can safely remove if/when we update sequence resources to record subtypes.
	resp, err := c.dataClient.TabularDataByFilter(ctx, &datapb.TabularDataByFilterRequest{
		DataRequest: &datapb.DataRequest{
			Filter: &datapb.Filter{
				PartId:        partID,
				ComponentName: resource.GetResourceName(),
				Method:        resource.GetMethodName(),
				Interval:      interval,
			},
			Limit: 1,
		},
	})
	if err != nil {
		return "", errors.Wrapf(err, "failed to look up the subtype of resource %q method %q",
			resource.GetResourceName(), resource.GetMethodName())
	}
	for _, meta := range resp.GetMetadata() {
		if subtype := meta.GetComponentType(); subtype != "" {
			return subtype, nil
		}
	}
	return "", nil
}

// sequenceTabularFileNames builds one NDJSON file name per resource, positionally matching
// resources. Names are derived from the resource and method name; resources that sanitize to the
// same string get a numeric suffix so no export silently overwrites another.
func sequenceTabularFileNames(resources []*datapb.SequenceResourceFilter) []string {
	names := make([]string, 0, len(resources))
	seen := map[string]int{}
	for _, resource := range resources {
		var parts []string
		if name := sanitizeForFileName(resource.GetResourceName()); name != "" {
			parts = append(parts, name)
		}
		if method := sanitizeForFileName(resource.GetMethodName()); method != "" {
			parts = append(parts, method)
		}
		if len(parts) == 0 {
			parts = append(parts, "resource")
		}

		base := strings.Join(parts, "-")
		seen[base]++
		if n := seen[base]; n > 1 {
			base += "-" + strconv.Itoa(n)
		}
		names = append(names, base+".ndjson")
	}
	return names
}

// progressLine renders a single line whose only changing part is a running count. On a terminal it
// rewrites that line in place, so the count stays legible whether it counts to ten or ten million.
// Piped or redirected there is no cursor to move, so carriage returns would just pile up in the
// log; there the line is written once, when the count is final.
type progressLine struct {
	w        io.Writer
	prefix   string
	tail     string // a format taking the running count, e.g. " (%d rows)"
	terminal bool
	started  bool
	width    int // characters last drawn, so erase knows how much to blank
}

func newProgressLine(w io.Writer, prefix, tail string) *progressLine {
	return &progressLine{w: w, prefix: prefix, tail: tail, terminal: isTerminalOutput()}
}

// render builds the line by concatenation rather than one format string, because prefix is
// caller-supplied (a resource name may well contain a '%').
func (l *progressLine) render(count int) string {
	return l.prefix + fmt.Sprintf(l.tail, count)
}

// start writes the prefix before any counting begins, so slow work is attributable while it runs.
// Callers that cannot commit to a line yet (the count may end up zero) skip it.
func (l *progressLine) start() {
	l.started = true
	fmt.Fprint(l.w, l.prefix) //nolint:errcheck
}

// update redraws the line with the running count. The count only grows, so a redraw never has to
// clear characters left behind by a longer previous value.
func (l *progressLine) update(count int) {
	if l.terminal {
		drawn := l.render(count)
		l.width = len(drawn)
		fmt.Fprint(l.w, "\r"+drawn) //nolint:errcheck
	}
}

func (l *progressLine) finish(count int) {
	switch {
	case l.terminal:
		fmt.Fprint(l.w, "\r"+l.render(count)+"\n") //nolint:errcheck
	case l.started:
		// The prefix is already on the line; only the count is outstanding.
		fmt.Fprint(l.w, fmt.Sprintf(l.tail, count)+"\n") //nolint:errcheck
	default:
		fmt.Fprint(l.w, l.render(count)+"\n") //nolint:errcheck
	}
}

// abandon closes the line so whatever follows starts on its own.
func (l *progressLine) abandon() {
	fmt.Fprintln(l.w) //nolint:errcheck
}

// erase removes the line entirely, for a caller that replaces a running total with a fuller
// breakdown. Off a terminal nothing was drawn, so there is nothing to take back.
func (l *progressLine) erase() {
	if l.terminal {
		fmt.Fprint(l.w, "\r"+strings.Repeat(" ", l.width)+"\r") //nolint:errcheck
	}
}

func sanitizeForFileName(s string) string {
	return strings.Trim(unsafeFileNameChars.ReplaceAllString(s, "_"), "_.")
}

// exportSequenceBinary downloads every binary datum the sequence references into
// <destination>/binary, using the same layout and parallel-download machinery as
// `data export binary`, and reports the result per resource so the section reads like the
// tabular one above it.
func (c *viamClient) exportSequenceBinary(ctx context.Context, sequenceID, dst string, parallel, timeout uint) error {
	binaryDst := filepath.Join(dst, sequenceBinaryExportDir)

	// resourceOf is populated while paging and read by the download workers, so both sides take
	// progressMu. Downloads run in parallel across resources, so a per-resource line cannot update
	// live; a single running total does that, and the per-resource split is reported at the end.
	var progressMu sync.Mutex
	resourceOf := map[string]string{}
	countByResource := map[string]int{}
	var downloaded atomic.Int32

	fetchIDsInto := func(ctx context.Context, ids chan<- string) error {
		defer close(ids)
		return forEachSequenceBinaryData(ctx, c.dataClient, sequenceID, func(bd *datapb.BinaryData) error {
			id := bd.GetMetadata().GetBinaryDataId()

			progressMu.Lock()
			resourceOf[id] = sequenceResourceLabel(bd.GetMetadata().GetCaptureMetadata())
			progressMu.Unlock()

			select {
			case ids <- id:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}

	line := newProgressLine(c.c.Root().Writer, "  ", "%d files")
	download := func(ctx context.Context, id string) error {
		if err := c.downloadBinary(ctx, binaryDst, timeout, id); err != nil {
			return err
		}
		total := downloaded.Add(1)

		progressMu.Lock()
		defer progressMu.Unlock()
		countByResource[resourceOf[id]]++
		line.update(int(total))
		return nil
	}

	printf(c.c.Root().Writer, "")
	printf(c.c.Root().Writer, "Binary data (%s/):", sequenceBinaryExportDir)
	if err := c.performActionOnBinaryDataIDs(ctx, fetchIDsInto, download, parallel, func(int32) {}); err != nil {
		line.erase()
		return err
	}
	if downloaded.Load() == 0 {
		printf(c.c.Root().Writer, "  none")
		return nil
	}

	// Replace the running total with the per-resource split.
	line.erase()
	for _, resource := range slices.Sorted(maps.Keys(countByResource)) {
		printf(c.c.Root().Writer, "  %s: %d files", resource, countByResource[resource])
	}
	return nil
}

// sequenceResourceLabel names the resource a datum was captured from, matching how the tabular
// section labels its lines.
func sequenceResourceLabel(meta *datapb.CaptureMetadata) string {
	name, method := meta.GetComponentName(), meta.GetMethodName()
	switch {
	case name == "" && method == "":
		return "unknown resource"
	case method == "":
		return name
	case name == "":
		return method
	default:
		return name + " " + method
	}
}
