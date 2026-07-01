// Package pipelinemanager owns the mongod process and aggregation pipeline workers
// for machine-level pipeline configuration.
package pipelinemanager

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"go.mongodb.org/mongo-driver/mongo"
	goutils "go.viam.com/utils"

	"go.viam.com/rdk/config"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/utils"
)

// API is the resource API for the internal pipeline manager service.
var API = resource.NewAPI("rdk", "service", "pipeline_manager")

// InternalServiceName is the name of the pipeline manager in the resource graph.
var InternalServiceName = resource.NewName(API, "builtin")

// PipelineManager owns the mongod process, lazy collections, and pipeline workers
// for all machine-level aggregation pipelines. It is registered as an internal
// service in the robot's resource graph so the data manager can retrieve it via
// resource.Dependencies and wire tabular observers to its collections.
type PipelineManager struct {
	mu          sync.Mutex
	lazyCols    map[string]*lazyCollection // key: collectionKey(component, method)
	workers     []*pipelineWorker
	bgCancel    context.CancelFunc
	mongoClient *mongo.Client
	mongodProc  *mongodProcess
	captureDir  string
	logger      logging.Logger
}

// New creates a new PipelineManager.
func New(logger logging.Logger) *PipelineManager {
	return &PipelineManager{
		logger:     logger,
		captureDir: filepath.Join(utils.ViamDotDir, "capture"),
	}
}

// Name implements resource.Resource.
func (pm *PipelineManager) Name() resource.Name { return InternalServiceName }

// Reconfigure implements resource.Resource. Pipeline configuration arrives via
// local_robot.go calling UpdatePipelines; this method is a no-op.
func (pm *PipelineManager) Reconfigure(_ context.Context, _ resource.Dependencies, _ resource.Config) error {
	return nil
}

// DoCommand implements resource.Resource (unused).
func (pm *PipelineManager) DoCommand(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

// Status implements resource.Resource (unused).
func (pm *PipelineManager) Status(_ context.Context) (map[string]interface{}, error) {
	return nil, nil
}

// Close implements resource.Resource.
func (pm *PipelineManager) Close(ctx context.Context) error {
	pm.teardown(ctx)
	return nil
}

// SetCaptureDir updates the directory where pipeline results are written.
// Called by the data manager during its Reconfigure.
func (pm *PipelineManager) SetCaptureDir(dir string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.captureDir = dir
}

// ObserverFor returns a TabularDataBson observer for the given (component, method) pair,
// or nil if no pipeline targets it. Satisfies capture.PipelineObservers.
func (pm *PipelineManager) ObserverFor(component, method string) func(ctx context.Context, doc data.TabularDataBson) {
	pm.mu.Lock()
	lc, ok := pm.lazyCols[collectionKey(component, method)]
	pm.mu.Unlock()
	if !ok {
		return nil
	}
	return func(ctx context.Context, doc data.TabularDataBson) {
		coll := lc.get()
		if coll == nil {
			return
		}
		if _, err := coll.InsertOne(ctx, doc); err != nil {
			pm.logger.Warnw("pipeline: failed to insert reading", "error", err)
		}
	}
}

// UpdatePipelines tears down any running pipeline infrastructure and starts fresh
// with the provided configuration. Called by local_robot.go when the machine-level
// pipeline config changes.
func (pm *PipelineManager) UpdatePipelines(pipelines []config.PipelineConfig) {
	pm.teardown(context.Background())

	pm.mu.Lock()
	pm.lazyCols = nil
	pm.mu.Unlock()

	if len(pipelines) == 0 {
		return
	}

	type methodGroup struct {
		component, method string
		cfgs              []config.PipelineConfig
	}
	grouped := make(map[string]*methodGroup)
	for _, p := range pipelines {
		key := collectionKey(p.Component, p.Method)
		if _, ok := grouped[key]; !ok {
			grouped[key] = &methodGroup{component: p.Component, method: p.Method}
		}
		grouped[key].cfgs = append(grouped[key].cfgs, p)
	}

	lazyCols := make(map[string]*lazyCollection, len(grouped))
	for key := range grouped {
		lazyCols[key] = &lazyCollection{}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	pm.mu.Lock()
	pm.lazyCols = lazyCols
	pm.bgCancel = bgCancel
	pm.mu.Unlock()

	methods := make([]*methodGroup, 0, len(grouped))
	for _, m := range grouped {
		methods = append(methods, m)
	}
	lazySnap := lazyCols

	go func() {
		binPath := mongodBinPath()
		dataDir := mongodDataDir()

		if err := ensureMongodBinary(bgCtx, binPath, pm.logger); err != nil {
			if bgCtx.Err() == nil {
				pm.logger.Warnf("pipeline: %v", err)
			}
			return
		}
		if bgCtx.Err() != nil {
			return
		}

		proc, client, err := launchMongod(bgCtx, binPath, dataDir, pm.logger)
		if err != nil {
			if bgCtx.Err() == nil {
				pm.logger.Warnf("pipeline: %v", err)
			}
			return
		}

		pm.mu.Lock()
		if bgCtx.Err() != nil {
			pm.mu.Unlock()
			goutils.UncheckedError(client.Disconnect(context.Background()))
			proc.stop()
			return
		}
		pm.mongodProc = proc
		pm.mongoClient = client
		pm.mu.Unlock()

		db := client.Database("pipelines")
		var workers []*pipelineWorker
		for i, m := range methods {
			key := collectionKey(m.component, m.method)
			coll := db.Collection(fmt.Sprintf("c%d", i))
			lazySnap[key].set(coll)
			tracker := newPipelineProgressTracker(len(m.cfgs))

			pm.mu.Lock()
			captureDir := pm.captureDir
			pm.mu.Unlock()

			w := startPipelineWorkers(bgCtx, m.cfgs, coll, tracker, captureDir, pm.logger)
			workers = append(workers, w...)
		}

		pm.mu.Lock()
		if bgCtx.Err() == nil {
			pm.workers = append(pm.workers, workers...)
		} else {
			stopPipelineWorkers(workers)
		}
		pm.mu.Unlock()
	}()
}

func (pm *PipelineManager) teardown(ctx context.Context) {
	pm.mu.Lock()
	bgCancel := pm.bgCancel
	workers := pm.workers
	mongoClient := pm.mongoClient
	mongodProc := pm.mongodProc
	pm.bgCancel = nil
	pm.workers = nil
	pm.mongoClient = nil
	pm.mongodProc = nil
	pm.mu.Unlock()

	if bgCancel != nil {
		bgCancel()
	}
	stopPipelineWorkers(workers)
	if mongoClient != nil {
		goutils.UncheckedError(mongoClient.Disconnect(ctx))
	}
	if mongodProc != nil {
		mongodProc.stop()
	}
}

func collectionKey(component, method string) string {
	return component + "/" + method
}
