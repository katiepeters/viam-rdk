# Component Pipelines

Runs MongoDB aggregation pipelines against live sensor capture data on a schedule.
Results are written as capture files and sync to the cloud normally.

## Layout

- `mongod.go` — download `~/.viam/bin/mongod` on first use, start/stop the process
- `pipeline.go` — `lazyCollection`, `pipelineProgressTracker`, workers, deletion logic
- `capture.go` — mongod lifecycle + per-method lazy collections on `Capture`; observer wiring
- `../../data/registry.go` — `CollectorParams.TabularObserver` callback (no mongo import in data pkg)
- `../../data/collector.go` — `maybeNotifyTabularObserver` calls the observer after each tabular write
- `../../data_manager.go` — `DataCaptureConfig.Pipelines []CaptureMethodPipeline` and struct def

## Config shape

Pipelines are nested inside a capture method (not a top-level service field):

```json
"capture_methods": [
  {
    "method": "Readings",
    "capture_frequency_hz": 0.5,
    "pipelines": [
      { "name": "every-5", "schedule": "5m", "stages": [{"$sort": {"time_received": 1}}, ...] }
    ]
  }
]
```

## Data flow

```
Capture tick → collector.writeCaptureResults → maybeNotifyTabularObserver → lazyCollection.get() → mongo.InsertOne
                                             ↓
                               .capture file (synced & deleted by sync service)

Pipeline ticker → mongo.Aggregate (prepended $match on window) → writePipelineResults
               → tracker.record(runTime) → mongo.DeleteMany(time_received < safeDeleteBefore)
```

## Lifecycle

1. `Capture.reconfigurePipelines` runs **before** `newCollectors`.
2. Lazy collections are created immediately; observers attach to them and silently drop readings
   until mongod is ready (collection is nil).
3. A background goroutine downloads `~/.viam/bin/mongod` if absent, starts mongod on port 27018,
   waits for it to accept connections, sets each `lazyCollection`, then starts pipeline workers.
4. Torn down in `Close` in this order:
   - Cancel background goroutine; stop pipeline workers (stop reads/deletes)
   - Flush + close collectors (stops observer writes)
   - Disconnect pipeline mongo client; stop mongod process
   - Disconnect cloud mongo client

## Deletion invariant

A reading with `time_received = T` is safe to delete once every pipeline that shares its collection
has run at least once with `runTime > T` (i.e., T was in or before the most recent window).
`pipelineProgressTracker.safeDeleteBefore()` returns `min(lastRunTime across all pipelines)`.
No deletion happens until all pipelines have recorded at least one run.

## Conventions

- `*mongo.Collection` is the right type for collection parameters in signatures.
- `schedule` doubles as the lookback window for the prepended `$match`.
- `TabularDataBson` (defined in `data/collector.go`) is the document schema in the collection.
- Aggregation stages in config are `[]map[string]interface{}` — converted per stage via JSON round-trip in `buildPipeline`.
- Mongod binary lives at `~/.viam/bin/mongod`; its presence there means viam owns it.
- Mongod data dir is `~/.viam/mongod-data/`; mongod log is `~/.viam/mongod.log`.
- Readings captured before mongod is ready are silently dropped (acceptable for a POC).
