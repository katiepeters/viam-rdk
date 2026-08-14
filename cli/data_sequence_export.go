package cli

import (
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/urfave/cli/v3"
	datapb "go.viam.com/api/app/data/v1"
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
	resources := sequence.GetResources()
	if len(resources) == 0 {
		printf(c.c.Root().Writer, "Sequence %s references no resources; skipping tabular export", sequence.GetId())
		return nil
	}

	tabularDir := filepath.Join(dst, sequenceTabularDir)
	if err := makeDestinationDirs(tabularDir); err != nil {
		return errors.Wrapf(err, "could not create %s", tabularDir)
	}

	interval := &datapb.CaptureInterval{Start: sequence.GetStartTime(), End: sequence.GetEndTime()}
	names := sequenceTabularFileNames(resources)
	for i, resource := range resources {
		subtype, err := c.resolveResourceSubtype(ctx, sequence.GetPartId(), resource, interval)
		if err != nil {
			return err
		}
		if subtype == "" {
			printf(c.c.Root().Writer, "No captured data for resource %q method %q in the sequence's interval; skipping",
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

		path := filepath.Join(tabularDir, names[i])
		printf(c.c.Root().Writer, "Exporting tabular data for resource %q method %q to %s",
			resource.GetResourceName(), resource.GetMethodName(), path)
		if err := c.tabularDataToFile(path, request); err != nil {
			return errors.Wrapf(err, "failed to export tabular data for resource %q method %q",
				resource.GetResourceName(), resource.GetMethodName())
		}
	}
	return nil
}

// resolveResourceSubtype discovers a resource's subtype, which ExportTabularData requires but a
// SequenceResourceFilter does not record. It asks for a single captured row matching the same
// part, resource, method, and interval the export will use, and reads the subtype off that row's
// CaptureMetadata. Returns "" when the resource captured nothing in the interval, in which case
// there is nothing to export for it either.
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

func sanitizeForFileName(s string) string {
	return strings.Trim(unsafeFileNameChars.ReplaceAllString(s, "_"), "_.")
}

// exportSequenceBinary downloads every binary datum the sequence references into <destination>/binary,
// using the same layout and parallel-download machinery as `data export binary`.
func (c *viamClient) exportSequenceBinary(ctx context.Context, sequenceID, dst string, parallel, timeout uint) error {
	binaryDst := filepath.Join(dst, sequenceBinaryExportDir)

	fetchIDsInto := func(ctx context.Context, ids chan<- string) error {
		defer close(ids)
		return forEachSequenceBinaryData(ctx, c.dataClient, sequenceID, func(bd *datapb.BinaryData) error {
			select {
			case ids <- bd.GetMetadata().GetBinaryDataId():
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	download := func(ctx context.Context, id string) error {
		return c.downloadBinary(ctx, binaryDst, timeout, id)
	}
	reportProgress := func(i int32) {
		printf(c.c.Root().Writer, "Downloaded %d files", i)
	}

	printf(c.c.Root().Writer, "Downloading binary data for sequence %s to %s", sequenceID, binaryDst)
	return c.performActionOnBinaryDataIDs(ctx, fetchIDsInto, download, parallel, reportProgress)
}
