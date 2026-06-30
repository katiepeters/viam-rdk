package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	v1 "go.viam.com/api/app/datasync/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/datamanager"
)

// lazyCollection holds a *mongo.Collection that may not be set yet while mongod is starting.
// The observer calls get() and silently drops readings when the collection is nil.
type lazyCollection struct {
	mu   sync.RWMutex
	coll *mongo.Collection
}

func (lc *lazyCollection) set(coll *mongo.Collection) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.coll = coll
}

func (lc *lazyCollection) get() *mongo.Collection {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	return lc.coll
}

// pipelineProgressTracker coordinates safe deletion across multiple pipelines
// sharing the same mongo collection. A reading can be deleted only once every
// pipeline attached to the capture method has run at least once past it.
type pipelineProgressTracker struct {
	mu             sync.Mutex
	lastRunTimes   map[string]time.Time // pipeline name → time of last completed run
	totalPipelines int
}

func newPipelineProgressTracker(total int) *pipelineProgressTracker {
	return &pipelineProgressTracker{
		lastRunTimes:   make(map[string]time.Time, total),
		totalPipelines: total,
	}
}

// record notes that pipeline name completed a run at runTime.
func (t *pipelineProgressTracker) record(name string, runTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.lastRunTimes[name]; !ok || runTime.After(existing) {
		t.lastRunTimes[name] = runTime
	}
}

// safeDeleteBefore returns the time before which readings are safe to delete —
// every pipeline has run at least once past that point. Returns (zero, false)
// until all pipelines have recorded at least one run.
func (t *pipelineProgressTracker) safeDeleteBefore() (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lastRunTimes) < t.totalPipelines {
		return time.Time{}, false
	}
	var min time.Time
	for _, rt := range t.lastRunTimes {
		if min.IsZero() || rt.Before(min) {
			min = rt
		}
	}
	return min, !min.IsZero()
}

// pipelineWorker manages a single running pipeline goroutine.
type pipelineWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startPipelineWorkers starts a background goroutine for each pipeline config.
// All workers for the same capture method share coll, tracker, and captureDir.
func startPipelineWorkers(
	ctx context.Context,
	cfgs []datamanager.CaptureMethodPipeline,
	coll *mongo.Collection,
	tracker *pipelineProgressTracker,
	captureDir string,
	componentName string,
	logger logging.Logger,
) []*pipelineWorker {
	workers := make([]*pipelineWorker, 0, len(cfgs))
	for _, cfg := range cfgs {
		pCtx, cancel := context.WithCancel(ctx)
		w := &pipelineWorker{cancel: cancel, done: make(chan struct{})}
		workers = append(workers, w)
		cfgCopy := cfg
		go func() {
			defer close(w.done)
			runPipelineLoop(pCtx, cfgCopy, coll, tracker, captureDir, componentName, logger)
		}()
	}
	return workers
}

// stopPipelineWorkers cancels all workers and waits for them to exit.
func stopPipelineWorkers(workers []*pipelineWorker) {
	for _, w := range workers {
		w.cancel()
	}
	for _, w := range workers {
		<-w.done
	}
}

func runPipelineLoop(
	ctx context.Context,
	cfg datamanager.CaptureMethodPipeline,
	coll *mongo.Collection,
	tracker *pipelineProgressTracker,
	captureDir string,
	componentName string,
	logger logging.Logger,
) {
	d, err := time.ParseDuration(cfg.Schedule)
	if err != nil {
		logger.Warnf("pipeline %q: invalid schedule %q: %v", cfg.Name, cfg.Schedule, err)
		return
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := executePipeline(ctx, cfg, d, coll, tracker, captureDir, componentName); err != nil {
				logger.Warnf("pipeline %q: %v", cfg.Name, err)
			}
		}
	}
}

func executePipeline(
	ctx context.Context,
	cfg datamanager.CaptureMethodPipeline,
	window time.Duration,
	coll *mongo.Collection,
	tracker *pipelineProgressTracker,
	captureDir string,
	componentName string,
) error {
	runTime := time.Now()
	windowStart := runTime.Add(-window)

	pipeline, err := buildPipeline(windowStart, cfg.Stages)
	if err != nil {
		return fmt.Errorf("build pipeline: %w", err)
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return fmt.Errorf("aggregate: %w", err)
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var results []bson.M
	if err := cursor.All(ctx, &results); err != nil {
		return fmt.Errorf("read results: %w", err)
	}

	if len(results) > 0 {
		if err := writePipelineResults(componentName, cfg.Name, results, captureDir); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
	}

	// Record this pipeline's run time, then prune readings that all pipelines
	// have now processed.
	tracker.record(cfg.Name, runTime)
	if safeTime, ok := tracker.safeDeleteBefore(); ok {
		_, _ = coll.DeleteMany(ctx, bson.M{"time_received": bson.M{"$lt": safeTime}})
	}

	return nil
}

// buildPipeline prepends a $match on the time window then appends the user's stages.
func buildPipeline(windowStart time.Time, stages []map[string]interface{}) (bson.A, error) {
	pipeline := bson.A{
		bson.M{"$match": bson.M{"time_received": bson.M{"$gte": windowStart}}},
	}
	for _, stage := range stages {
		jsonBytes, err := json.Marshal(stage)
		if err != nil {
			return nil, err
		}
		var bsonStage bson.M
		if err := bson.UnmarshalExtJSON(jsonBytes, false, &bsonStage); err != nil {
			return nil, err
		}
		pipeline = append(pipeline, bsonStage)
	}
	return pipeline, nil
}

// writePipelineResults writes aggregation output as a single .capture file under
// captureDir/pipelines/<componentName>/<pipelineName>/. The capture metadata
// uses the real component name and tags the result with _viam_pipeline:<pipelineName>
// so the backend can identify and route it.
func writePipelineResults(componentName, pipelineName string, results []bson.M, captureDir string) error {
	outDir := filepath.Join(captureDir, "pipelines", componentName, pipelineName)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	tags := []string{"_viam_pipeline:" + pipelineName}
	md, _ := data.BuildCaptureMetadata(sensor.API, componentName, "Readings", nil, nil, tags)
	cf, err := data.NewCaptureFile(outDir, md)
	if err != nil {
		return fmt.Errorf("create capture file: %w", err)
	}

	now := timestamppb.Now()
	for _, result := range results {
		s, err := bsonMToStruct(result)
		if err != nil {
			continue
		}
		_ = cf.WriteNext(&v1.SensorData{
			Metadata: &v1.SensorMetadata{
				TimeRequested: now,
				TimeReceived:  now,
			},
			Data: &v1.SensorData_Struct{Struct: s},
		})
	}
	return cf.Close()
}

func bsonMToStruct(m bson.M) (*structpb.Struct, error) {
	jsonBytes, err := bson.MarshalExtJSON(m, false, false)
	if err != nil {
		return nil, err
	}
	var plain map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &plain); err != nil {
		return nil, err
	}
	return structpb.NewStruct(plain)
}
