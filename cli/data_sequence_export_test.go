package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	datapb "go.viam.com/api/app/data/v1"
	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.viam.com/rdk/testutils/inject"
)

const (
	testSequenceID    = "seq-1"
	testSequencePart  = "part-1"
	testSequenceStart = "2024-01-01T00:00:00Z"
	testSequenceEnd   = "2024-01-01T01:00:00Z"
)

func testSequence(resources ...*datapb.SequenceResourceFilter) *datapb.Sequence {
	start, _ := time.Parse(time.RFC3339, testSequenceStart)
	end, _ := time.Parse(time.RFC3339, testSequenceEnd)
	return &datapb.Sequence{
		Id:        testSequenceID,
		PartId:    testSequencePart,
		StartTime: timestamppb.New(start),
		EndTime:   timestamppb.New(end),
		Resources: resources,
	}
}

func sequenceResource(name, method string) *datapb.SequenceResourceFilter {
	return &datapb.SequenceResourceFilter{ResourceName: name, MethodName: method}
}

// sequenceBinaryPath is where a sequence-exported binary datum lands: `data export binary`'s data/
// layout, rooted under binary/ rather than at the top level of the destination.
func sequenceBinaryPath(dst, id string) string {
	return dataFilePath(filepath.Join(dst, sequenceBinaryExportDir), filenameForDownload(binaryMeta(id)), ".jpg")
}

// seqFake serves the RPCs a sequence export makes and records what it was asked for. Only the
// fields a test cares about need setting; the rest behave benignly.
type seqFake struct {
	sequence *datapb.Sequence
	binary   []*datapb.BinaryData
	// pages, when set, serves GetSequenceBinaryData by page token instead of binary.
	pages map[string]*datapb.GetSequenceBinaryDataResponse

	// subtype maps a resource name to its subtype; "" means it has no tabular data. Defaults to
	// deriving one from the name, so a test can tell which lookup fed which export.
	subtype    func(resource string) string
	subtypeErr error
	exportErr  error
	binaryErr  error

	mu       sync.Mutex
	exported []*datapb.ExportTabularDataRequest
	lookedUp []string
	tokens   []string
}

func (f *seqFake) client(t *testing.T) (*viamClient, *testWriter) {
	t.Helper()
	dsc := &inject.DataServiceClient{
		GetSequenceFunc: func(_ context.Context, in *datapb.GetSequenceRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceResponse, error) {
			if f.sequence == nil || in.GetId() != f.sequence.GetId() {
				return &datapb.GetSequenceResponse{}, nil
			}
			return &datapb.GetSequenceResponse{Sequence: f.sequence}, nil
		},
		//nolint:staticcheck
		TabularDataByFilterFunc: func(_ context.Context, in *datapb.TabularDataByFilterRequest, _ ...grpc.CallOption,
		) (*datapb.TabularDataByFilterResponse, error) {
			if f.subtypeErr != nil {
				return nil, f.subtypeErr
			}
			name := in.GetDataRequest().GetFilter().GetComponentName()

			f.mu.Lock()
			f.lookedUp = append(f.lookedUp, name)
			f.mu.Unlock()

			subtype := "rdk:component:" + name
			if f.subtype != nil {
				subtype = f.subtype(name)
			}
			if subtype == "" {
				return &datapb.TabularDataByFilterResponse{}, nil
			}
			return &datapb.TabularDataByFilterResponse{
				Metadata: []*datapb.CaptureMetadata{{ComponentType: subtype}},
			}, nil
		},
		ExportTabularDataFunc: func(_ context.Context, in *datapb.ExportTabularDataRequest, _ ...grpc.CallOption,
		) (datapb.DataService_ExportTabularDataClient, error) {
			f.mu.Lock()
			f.exported = append(f.exported, in)
			f.mu.Unlock()

			if f.exportErr != nil {
				// The stream carries the error, not the call that opens it.
				return newMockExportStream(nil, f.exportErr), nil //nolint:nilerr
			}
			return newMockExportStream([]*datapb.ExportTabularDataResponse{
				{LocationId: "loc-id", ResourceName: in.GetResourceName(), MethodName: in.GetMethodName()},
			}, nil), nil
		},
		GetSequenceBinaryDataFunc: func(_ context.Context, in *datapb.GetSequenceBinaryDataRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceBinaryDataResponse, error) {
			if f.binaryErr != nil {
				return nil, f.binaryErr
			}
			f.mu.Lock()
			f.tokens = append(f.tokens, in.GetPageToken())
			f.mu.Unlock()

			if f.pages == nil {
				return &datapb.GetSequenceBinaryDataResponse{Data: f.binary}, nil
			}
			resp, ok := f.pages[in.GetPageToken()]
			if !ok {
				return nil, errors.New("unexpected page token " + in.GetPageToken())
			}
			return resp, nil
		},
		BinaryDataByIDsFunc: echoBinaryDataByIDs,
	}
	_, ac, out, _ := setup(&inject.AppServiceClient{}, dsc, nil, nil, "token")
	return ac, out
}

type argMutator = func(*dataExportSequenceArgs)

func exportArgs(dst string, mutate ...argMutator) dataExportSequenceArgs {
	args := dataExportSequenceArgs{Destination: dst, SequenceID: testSequenceID, Parallel: 2}
	for _, m := range mutate {
		m(&args)
	}
	return args
}

func onlyTabular(a *dataExportSequenceArgs) { a.OnlyTabular = true }
func onlyBinary(a *dataExportSequenceArgs)  { a.OnlyBinary = true }

func TestSequenceTabularFileNames(t *testing.T) {
	names := sequenceTabularFileNames([]*datapb.SequenceResourceFilter{
		sequenceResource("camera-1", "ReadImage"),
		sequenceResource("../../etc/passwd", "ReadImage"), // must not escape the tabular directory
		sequenceResource("etc_passwd", "ReadImage"),       // sanitizes to the same base, so gets a suffix
		sequenceResource("", ""),
	})

	test.That(t, names, test.ShouldResemble, []string{
		"camera-1-ReadImage.ndjson",
		"etc_passwd-ReadImage.ndjson",
		"etc_passwd-ReadImage-2.ndjson",
		"resource.ndjson",
	})
	for _, name := range names {
		test.That(t, strings.Contains(name, string(filepath.Separator)), test.ShouldBeFalse)
	}
}

func TestDataExportSequenceAction_Errors(t *testing.T) {
	sensor := testSequence(sequenceResource("sensor-1", "Readings"))
	for _, tc := range []struct {
		name    string
		fake    *seqFake
		args    []argMutator
		wantErr []string
	}{
		{
			"both only flags", &seqFake{sequence: testSequence()},
			[]argMutator{onlyTabular, onlyBinary},
			[]string{"cannot both be provided"},
		},
		{"missing sequence", &seqFake{}, nil, []string{"not found"}},
		{
			"subtype lookup fails", &seqFake{sequence: sensor, subtypeErr: errors.New("lookup boom")},
			[]argMutator{onlyTabular},
			[]string{"sensor-1", "lookup boom"},
		},
		{
			"tabular export fails", &seqFake{sequence: sensor, exportErr: errors.New("export boom")},
			[]argMutator{onlyTabular},
			[]string{"sensor-1", "export boom"},
		},
		{
			"binary listing fails", &seqFake{sequence: testSequence(), binaryErr: errors.New("server boom")},
			[]argMutator{onlyBinary},
			[]string{"server boom"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ac, _ := tc.fake.client(t)

			err := ac.dataExportSequenceAction(context.Background(), exportArgs(t.TempDir(), tc.args...))
			test.That(t, err, test.ShouldNotBeNil)
			for _, want := range tc.wantErr {
				test.That(t, err.Error(), test.ShouldContainSubstring, want)
			}
		})
	}
}

func TestDataExportSequenceAction_ExportsTabularPerResource(t *testing.T) {
	fake := &seqFake{sequence: testSequence(
		sequenceResource("sensor-1", "Readings"),
		sequenceResource("power_sensor-1", "Voltage"),
	)}
	ac, _ := fake.client(t)

	dst := t.TempDir()
	test.That(t, ac.dataExportSequenceAction(context.Background(), exportArgs(dst, onlyTabular)), test.ShouldBeNil)

	test.That(t, len(fake.exported), test.ShouldEqual, 2)
	for _, req := range fake.exported {
		test.That(t, req.GetPartId(), test.ShouldEqual, testSequencePart)
		test.That(t, req.GetInterval().GetStart().AsTime().Format(time.RFC3339), test.ShouldEqual, testSequenceStart)
		test.That(t, req.GetInterval().GetEnd().AsTime().Format(time.RFC3339), test.ShouldEqual, testSequenceEnd)
	}
	test.That(t, fake.exported[0].GetResourceName(), test.ShouldEqual, "sensor-1")
	test.That(t, fake.exported[1].GetResourceName(), test.ShouldEqual, "power_sensor-1")

	// Each resource carries its own resolved subtype, not one value applied to all of them.
	test.That(t, fake.exported[0].GetResourceSubtype(), test.ShouldEqual, "rdk:component:sensor-1")
	test.That(t, fake.exported[1].GetResourceSubtype(), test.ShouldEqual, "rdk:component:power_sensor-1")

	// Each resource lands in its own file rather than overwriting a shared one.
	test.That(t, readNDJSON(t, dst, "sensor-1-Readings.ndjson")["resourceName"], test.ShouldEqual, "sensor-1")
	test.That(t, readNDJSON(t, dst, "power_sensor-1-Voltage.ndjson")["resourceName"], test.ShouldEqual, "power_sensor-1")

	_, err := os.Stat(filepath.Join(dst, sequenceBinaryExportDir))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

// A camera captures binary, so its resource has no tabular data by definition: no subtype lookup
// should be spent on it, and it should produce no output here.
// Run with neither --only flag, so this also covers both halves being invoked by default.
func TestDataExportSequenceAction_EmptySequence(t *testing.T) {
	fake := &seqFake{sequence: testSequence()}
	ac, out := fake.client(t)

	dst := t.TempDir()
	test.That(t, ac.dataExportSequenceAction(context.Background(), exportArgs(dst)), test.ShouldBeNil)
	test.That(t, len(fake.exported), test.ShouldEqual, 0)

	logged := strings.Join(out.messages, "")
	test.That(t, logged, test.ShouldContainSubstring, "Tabular data (tabular/):\n  none")
	test.That(t, logged, test.ShouldContainSubstring, "Binary data (binary/):\n  none")

	// The claim has to match the disk: nothing written, so neither directory exists.
	for _, dir := range []string{sequenceTabularDir, sequenceBinaryExportDir} {
		_, err := os.Stat(filepath.Join(dst, dir))
		test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	}
}

func TestDataExportSequenceAction_SkipsBinaryCaptureMethods(t *testing.T) {
	fake := &seqFake{sequence: testSequence(
		sequenceResource("camera-1", "GetImages"),
		sequenceResource("camera-2", "ReadImage"),
		sequenceResource("sensor-1", "Readings"),
	)}
	ac, out := fake.client(t)

	test.That(t, ac.dataExportSequenceAction(context.Background(), exportArgs(t.TempDir(), onlyTabular)), test.ShouldBeNil)

	test.That(t, fake.lookedUp, test.ShouldResemble, []string{"sensor-1"})
	test.That(t, len(fake.exported), test.ShouldEqual, 1)

	logged := strings.Join(out.messages, "")
	test.That(t, logged, test.ShouldNotContainSubstring, "camera-")
	test.That(t, logged, test.ShouldNotContainSubstring, "no tabular data")
}

// A resource whose subtype lookup comes back empty is skipped rather than exported with a blank
// subtype, which ExportTabularData requires.
func TestDataExportSequenceAction_SkipsResourceWithNoTabularData(t *testing.T) {
	fake := &seqFake{
		sequence: testSequence(
			sequenceResource("sensor-1", "Readings"),
			sequenceResource("ghost-1", "Readings"),
		),
		subtype: func(resource string) string {
			if resource == "ghost-1" {
				return ""
			}
			return "rdk:component:sensor"
		},
	}
	ac, _ := fake.client(t)

	dst := t.TempDir()
	test.That(t, ac.dataExportSequenceAction(context.Background(), exportArgs(dst, onlyTabular)), test.ShouldBeNil)

	test.That(t, len(fake.exported), test.ShouldEqual, 1)
	test.That(t, fake.exported[0].GetResourceName(), test.ShouldEqual, "sensor-1")

	_, err := os.Stat(filepath.Join(dst, sequenceTabularDir, "ghost-1-Readings.ndjson"))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestDataExportSequenceAction_DownloadsBinaryData(t *testing.T) {
	captured := func(id string) *datapb.BinaryData {
		bd := mkBinaryData(id, ".jpg")
		bd.Metadata.CaptureMetadata = &datapb.CaptureMetadata{ComponentName: "camera-1", MethodName: "GetImages"}
		return bd
	}
	fake := &seqFake{
		sequence: testSequence(sequenceResource("sensor-1", "Readings")),
		binary:   []*datapb.BinaryData{captured("bd-1"), captured("bd-2")},
	}
	ac, out := fake.client(t)

	dst := t.TempDir()
	test.That(t, ac.dataExportSequenceAction(context.Background(), exportArgs(dst, onlyBinary)), test.ShouldBeNil)
	test.That(t, len(fake.exported), test.ShouldEqual, 0)

	// Rooted at binary/, carrying `data export binary`'s data/ plus metadata/ layout beneath it.
	for _, id := range []string{"bd-1", "bd-2"} {
		test.That(t, mustReadFile(t, sequenceBinaryPath(dst, id)), test.ShouldResemble, []byte("bytes-"+id))
		_, err := os.Stat(filepath.Join(dst, sequenceBinaryExportDir, metadataDir, filenameForDownload(binaryMeta(id))+".json"))
		test.That(t, err, test.ShouldBeNil)
	}
	for _, dir := range []string{dataDir, metadataDir} {
		_, err := os.Stat(filepath.Join(dst, dir))
		test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	}

	// Counted against the resource they came from, matching how the tabular section reads.
	test.That(t, strings.Join(out.messages, ""), test.ShouldContainSubstring,
		"Binary data (binary/):\n  camera-1 GetImages: 2 files")
}

// Every page must be consumed, and each request after the first must carry the token the previous
// response returned.
func TestDataExportSequenceAction_PagesBinaryData(t *testing.T) {
	fake := &seqFake{
		sequence: testSequence(),
		pages: map[string]*datapb.GetSequenceBinaryDataResponse{
			"": {Data: []*datapb.BinaryData{mkBinaryData("bd-1", ".jpg")}, NextPageToken: "page-2"},
			"page-2": {
				Data:          []*datapb.BinaryData{mkBinaryData("bd-2", ".jpg"), mkBinaryData("bd-3", ".jpg")},
				NextPageToken: "page-3",
			},
			"page-3": {Data: []*datapb.BinaryData{mkBinaryData("bd-4", ".jpg")}},
		},
	}
	ac, _ := fake.client(t)

	dst := t.TempDir()
	test.That(t, ac.dataExportSequenceAction(context.Background(), exportArgs(dst, onlyBinary)), test.ShouldBeNil)

	test.That(t, fake.tokens, test.ShouldResemble, []string{"", "page-2", "page-3"})
	for _, id := range []string{"bd-1", "bd-2", "bd-3", "bd-4"} {
		test.That(t, mustReadFile(t, sequenceBinaryPath(dst, id)), test.ShouldResemble, []byte("bytes-"+id))
	}
}

// readNDJSON returns the single row the fake's export stream writes for a resource.
func readNDJSON(t *testing.T, dst, name string) map[string]any {
	t.Helper()
	contents := mustReadFile(t, filepath.Join(dst, sequenceTabularDir, name))

	var row map[string]any
	test.That(t, json.NewDecoder(strings.NewReader(string(contents))).Decode(&row), test.ShouldBeNil)
	return row
}
