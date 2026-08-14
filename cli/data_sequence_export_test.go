package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	datapb "go.viam.com/api/app/data/v1"
	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/rdk/utils"
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

func sequenceBinaryMeta(id string) *datapb.BinaryMetadata {
	return &datapb.BinaryMetadata{BinaryDataId: id, FileName: id + ".jpg", FileExt: ".jpg"}
}

// sequenceBinaryPath is where a sequence-exported binary datum must land: `data export binary`'s
// data/ layout, rooted under binary/ rather than at the top level of the destination.
func sequenceBinaryPath(dst, id string) string {
	return dataFilePath(filepath.Join(dst, sequenceBinaryExportDir), filenameForDownload(sequenceBinaryMeta(id)), ".jpg")
}

// sequenceExportClient wires an inject.DataServiceClient that serves the given sequence, streams
// one tabular row per resource, and serves the given binary data records. It returns the client
// and a pointer to the tabular requests it observed, in call order.
func sequenceExportClient(
	t *testing.T, sequence *datapb.Sequence, binary []*datapb.BinaryData,
) (*viamClient, *[]*datapb.ExportTabularDataRequest) {
	t.Helper()

	var mu sync.Mutex
	var tabularRequests []*datapb.ExportTabularDataRequest

	dsc := &inject.DataServiceClient{
		GetSequenceFunc: func(_ context.Context, in *datapb.GetSequenceRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceResponse, error) {
			if sequence == nil || in.GetId() != sequence.GetId() {
				return &datapb.GetSequenceResponse{}, nil
			}
			return &datapb.GetSequenceResponse{Sequence: sequence}, nil
		},
		ExportTabularDataFunc: func(_ context.Context, in *datapb.ExportTabularDataRequest, _ ...grpc.CallOption,
		) (datapb.DataService_ExportTabularDataClient, error) {
			mu.Lock()
			tabularRequests = append(tabularRequests, in)
			mu.Unlock()
			return newMockExportStream([]*datapb.ExportTabularDataResponse{
				{LocationId: "loc-id", ResourceName: in.GetResourceName(), MethodName: in.GetMethodName()},
			}, nil), nil
		},
		GetSequenceBinaryDataFunc: func(_ context.Context, _ *datapb.GetSequenceBinaryDataRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceBinaryDataResponse, error) {
			return &datapb.GetSequenceBinaryDataResponse{Data: binary}, nil
		},
		BinaryDataByIDsFunc: func(_ context.Context, in *datapb.BinaryDataByIDsRequest, _ ...grpc.CallOption,
		) (*datapb.BinaryDataByIDsResponse, error) {
			resp := &datapb.BinaryDataByIDsResponse{}
			for _, id := range in.GetBinaryDataIds() {
				datum := &datapb.BinaryData{
					Metadata: &datapb.BinaryMetadata{BinaryDataId: id, FileName: id + ".jpg", FileExt: ".jpg"},
				}
				if in.GetIncludeBinary() {
					datum.Binary = []byte("bytes-" + id)
				}
				resp.Data = append(resp.Data, datum)
			}
			return resp, nil
		},
	}

	cCtx, ac, _, _ := setup(&inject.AppServiceClient{}, dsc, nil, nil, "token")
	_ = cCtx
	return ac, &tabularRequests
}

func TestSequenceTabularFileNames(t *testing.T) {
	names := sequenceTabularFileNames([]*datapb.SequenceResourceFilter{
		sequenceResource("camera-1", "ReadImage"),
		// Path separators and other unsafe characters must not escape the tabular directory.
		sequenceResource("../../etc/passwd", "ReadImage"),
		// Sanitizes to the same base as the previous entry, so it must get a suffix.
		sequenceResource("etc_passwd", "ReadImage"),
		// Neither name populated: falls back to a fixed base.
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

func TestDataExportSequenceAction_RejectsConflictingOnlyFlags(t *testing.T) {
	ac, _ := sequenceExportClient(t, testSequence(), nil)

	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: t.TempDir(),
		SequenceID:  testSequenceID,
		OnlyTabular: true,
		OnlyBinary:  true,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "cannot both be provided")
}

func TestDataExportSequenceAction_SurfacesMissingSequence(t *testing.T) {
	ac, _ := sequenceExportClient(t, nil, nil)

	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: t.TempDir(),
		SequenceID:  "nope",
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "not found")
}

func TestDataExportSequenceAction_ExportsTabularPerResource(t *testing.T) {
	sequence := testSequence(
		sequenceResource("camera-1", "ReadImage"),
		sequenceResource("sensor-1", "Readings"),
	)
	ac, tabularRequests := sequenceExportClient(t, sequence, nil)

	dst := t.TempDir()
	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination:     dst,
		SequenceID:      testSequenceID,
		ResourceSubtype: "rdk:component:camera",
		OnlyTabular:     true,
	})
	test.That(t, err, test.ShouldBeNil)

	// Each resource is exported over the sequence's part and interval.
	test.That(t, len(*tabularRequests), test.ShouldEqual, 2)
	for _, req := range *tabularRequests {
		test.That(t, req.GetPartId(), test.ShouldEqual, testSequencePart)
		test.That(t, req.GetResourceSubtype(), test.ShouldEqual, "rdk:component:camera")
		test.That(t, req.GetInterval().GetStart().AsTime().Format(time.RFC3339), test.ShouldEqual, testSequenceStart)
		test.That(t, req.GetInterval().GetEnd().AsTime().Format(time.RFC3339), test.ShouldEqual, testSequenceEnd)
	}
	test.That(t, (*tabularRequests)[0].GetResourceName(), test.ShouldEqual, "camera-1")
	test.That(t, (*tabularRequests)[0].GetMethodName(), test.ShouldEqual, "ReadImage")
	test.That(t, (*tabularRequests)[1].GetResourceName(), test.ShouldEqual, "sensor-1")
	test.That(t, (*tabularRequests)[1].GetMethodName(), test.ShouldEqual, "Readings")

	// Each resource lands in its own NDJSON file rather than overwriting a shared one.
	cameraRows := readNDJSON(t, filepath.Join(dst, sequenceTabularDir, "camera-1-ReadImage.ndjson"))
	test.That(t, len(cameraRows), test.ShouldEqual, 1)
	test.That(t, cameraRows[0]["resourceName"], test.ShouldEqual, "camera-1")

	sensorRows := readNDJSON(t, filepath.Join(dst, sequenceTabularDir, "sensor-1-Readings.ndjson"))
	test.That(t, len(sensorRows), test.ShouldEqual, 1)
	test.That(t, sensorRows[0]["resourceName"], test.ShouldEqual, "sensor-1")

	// --only-tabular means no binary data directory.
	_, err = os.Stat(filepath.Join(dst, sequenceBinaryExportDir))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestDataExportSequenceAction_SkipsTabularWhenNoResources(t *testing.T) {
	ac, tabularRequests := sequenceExportClient(t, testSequence(), nil)

	dst := t.TempDir()
	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: dst,
		SequenceID:  testSequenceID,
		OnlyTabular: true,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(*tabularRequests), test.ShouldEqual, 0)

	_, err = os.Stat(filepath.Join(dst, sequenceTabularDir))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestDataExportSequenceAction_DownloadsBinaryData(t *testing.T) {
	sequence := testSequence(sequenceResource("camera-1", "ReadImage"))
	binary := []*datapb.BinaryData{mkBinaryData("bd-1", ".jpg"), mkBinaryData("bd-2", ".jpg")}
	ac, tabularRequests := sequenceExportClient(t, sequence, binary)

	dst := t.TempDir()
	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: dst,
		SequenceID:  testSequenceID,
		Parallel:    2,
		OnlyBinary:  true,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(*tabularRequests), test.ShouldEqual, 0)

	// Binary data is rooted at binary/, carrying `data export binary`'s data/ plus metadata/
	// layout beneath it so nothing lands at the top level of the destination.
	for _, id := range []string{"bd-1", "bd-2"} {
		fileName := filenameForDownload(sequenceBinaryMeta(id))
		test.That(t, mustReadFile(t, sequenceBinaryPath(dst, id)), test.ShouldResemble, []byte("bytes-"+id))
		_, err := os.Stat(filepath.Join(dst, sequenceBinaryExportDir, metadataDir, fileName+".json"))
		test.That(t, err, test.ShouldBeNil)
	}
	// Nothing may land directly under the destination.
	_, err = os.Stat(filepath.Join(dst, dataDir))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	_, err = os.Stat(filepath.Join(dst, metadataDir))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestDataExportSequenceAction_ExportsTabularAndBinaryByDefault(t *testing.T) {
	sequence := testSequence(sequenceResource("camera-1", "ReadImage"))
	ac, tabularRequests := sequenceExportClient(t, sequence, []*datapb.BinaryData{mkBinaryData("bd-1", ".jpg")})

	dst := t.TempDir()
	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: dst,
		SequenceID:  testSequenceID,
		Parallel:    2,
	})
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(*tabularRequests), test.ShouldEqual, 1)
	test.That(t, len(readNDJSON(t, filepath.Join(dst, sequenceTabularDir, "camera-1-ReadImage.ndjson"))), test.ShouldEqual, 1)

	test.That(t, mustReadFile(t, sequenceBinaryPath(dst, "bd-1")), test.ShouldResemble, []byte("bytes-bd-1"))
}

func TestDataExportSequenceAction_SurfacesTabularErrors(t *testing.T) {
	sequence := testSequence(sequenceResource("camera-1", "ReadImage"))
	dsc := &inject.DataServiceClient{
		GetSequenceFunc: func(_ context.Context, _ *datapb.GetSequenceRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceResponse, error) {
			return &datapb.GetSequenceResponse{Sequence: sequence}, nil
		},
		ExportTabularDataFunc: func(_ context.Context, _ *datapb.ExportTabularDataRequest, _ ...grpc.CallOption,
		) (datapb.DataService_ExportTabularDataClient, error) {
			return newMockExportStream(nil, errors.New("boom")), nil
		},
	}
	cCtx, ac, _, _ := setup(&inject.AppServiceClient{}, dsc, nil, nil, "token")
	_ = cCtx

	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: t.TempDir(),
		SequenceID:  testSequenceID,
		OnlyTabular: true,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "camera-1")
	test.That(t, err.Error(), test.ShouldContainSubstring, "boom")
}

func TestDataExportSequenceAction_SurfacesBinaryErrors(t *testing.T) {
	dsc := &inject.DataServiceClient{
		GetSequenceFunc: func(_ context.Context, _ *datapb.GetSequenceRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceResponse, error) {
			return &datapb.GetSequenceResponse{Sequence: testSequence()}, nil
		},
		GetSequenceBinaryDataFunc: func(_ context.Context, _ *datapb.GetSequenceBinaryDataRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceBinaryDataResponse, error) {
			return nil, errors.New("server boom")
		},
	}
	cCtx, ac, _, _ := setup(&inject.AppServiceClient{}, dsc, nil, nil, "token")
	_ = cCtx

	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: t.TempDir(),
		SequenceID:  testSequenceID,
		Parallel:    2,
		OnlyBinary:  true,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "server boom")
}

// TestDataExportSequenceAction_PagesBinaryData guards the paging loop in
// forEachSequenceBinaryData: every page must be consumed, and each request after the first must
// carry the token the previous response returned.
func TestDataExportSequenceAction_PagesBinaryData(t *testing.T) {
	pages := map[string]*datapb.GetSequenceBinaryDataResponse{
		"": {Data: []*datapb.BinaryData{mkBinaryData("bd-1", ".jpg")}, NextPageToken: "page-2"},
		"page-2": {
			Data:          []*datapb.BinaryData{mkBinaryData("bd-2", ".jpg"), mkBinaryData("bd-3", ".jpg")},
			NextPageToken: "page-3",
		},
		// Terminal page: empty next token ends the loop.
		"page-3": {Data: []*datapb.BinaryData{mkBinaryData("bd-4", ".jpg")}},
	}

	var mu sync.Mutex
	var requestedTokens []string
	dsc := &inject.DataServiceClient{
		GetSequenceFunc: func(_ context.Context, _ *datapb.GetSequenceRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceResponse, error) {
			return &datapb.GetSequenceResponse{Sequence: testSequence()}, nil
		},
		GetSequenceBinaryDataFunc: func(_ context.Context, in *datapb.GetSequenceBinaryDataRequest, _ ...grpc.CallOption,
		) (*datapb.GetSequenceBinaryDataResponse, error) {
			mu.Lock()
			requestedTokens = append(requestedTokens, in.GetPageToken())
			mu.Unlock()
			resp, ok := pages[in.GetPageToken()]
			if !ok {
				return nil, errors.New("unexpected page token " + in.GetPageToken())
			}
			return resp, nil
		},
		BinaryDataByIDsFunc: func(_ context.Context, in *datapb.BinaryDataByIDsRequest, _ ...grpc.CallOption,
		) (*datapb.BinaryDataByIDsResponse, error) {
			resp := &datapb.BinaryDataByIDsResponse{}
			for _, id := range in.GetBinaryDataIds() {
				datum := &datapb.BinaryData{
					Metadata: &datapb.BinaryMetadata{BinaryDataId: id, FileName: id + ".jpg", FileExt: ".jpg"},
				}
				if in.GetIncludeBinary() {
					datum.Binary = []byte("bytes-" + id)
				}
				resp.Data = append(resp.Data, datum)
			}
			return resp, nil
		},
	}
	cCtx, ac, _, _ := setup(&inject.AppServiceClient{}, dsc, nil, nil, "token")
	_ = cCtx

	dst := t.TempDir()
	err := ac.dataExportSequenceAction(context.Background(), dataExportSequenceArgs{
		Destination: dst,
		SequenceID:  testSequenceID,
		Parallel:    2,
		OnlyBinary:  true,
	})
	test.That(t, err, test.ShouldBeNil)

	test.That(t, requestedTokens, test.ShouldResemble, []string{"", "page-2", "page-3"})
	for _, id := range []string{"bd-1", "bd-2", "bd-3", "bd-4"} {
		test.That(t, mustReadFile(t, sequenceBinaryPath(dst, id)), test.ShouldResemble, []byte("bytes-"+id))
	}
}

// TestDataExportSequenceCommandFlags guards that every flag registered on `data export sequence`
// maps onto a field of dataExportSequenceArgs, since that binding is reflective.
func TestDataExportSequenceCommandFlags(t *testing.T) {
	out := &testWriter{}
	errOut := &testWriter{}
	cCtx := buildTestCmd(out, errOut, map[string]any{
		generalFlagDestination:     utils.ResolveFile(""),
		dataFlagSequenceID:         testSequenceID,
		generalFlagResourceSubtype: "rdk:component:camera",
		dataFlagParallelDownloads:  uint(4),
		dataFlagTimeout:            uint(7),
		dataFlagOnlyTabular:        true,
		dataFlagOnlyBinary:         true,
	})

	args := parseStructFromCtx[dataExportSequenceArgs](cCtx)
	test.That(t, args.Destination, test.ShouldEqual, utils.ResolveFile(""))
	test.That(t, args.SequenceID, test.ShouldEqual, testSequenceID)
	test.That(t, args.ResourceSubtype, test.ShouldEqual, "rdk:component:camera")
	test.That(t, args.Parallel, test.ShouldEqual, uint(4))
	test.That(t, args.Timeout, test.ShouldEqual, uint(7))
	test.That(t, args.OnlyTabular, test.ShouldBeTrue)
	test.That(t, args.OnlyBinary, test.ShouldBeTrue)
}

func readNDJSON(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	contents := mustReadFile(t, path)

	var rows []map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	for decoder.More() {
		var row map[string]interface{}
		test.That(t, decoder.Decode(&row), test.ShouldBeNil)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return len(rows[i]) < len(rows[j]) })
	return rows
}
