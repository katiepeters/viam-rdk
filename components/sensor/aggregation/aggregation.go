// Package aggregation implements an aggregation sensor that runs MongoDB aggregation
// pipelines over captured tabular data and returns results via Readings().
package aggregation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Model is the resource model for the aggregation sensor builtin.
var Model = resource.NewModel("rdk", "builtin", "aggregation")

// Config is the configuration for the aggregation sensor.
type Config struct {
	// SourceComponent is the short name of the component whose captured readings to aggregate.
	SourceComponent string `json:"source"`
	// SourceMethod is the method name to aggregate (e.g. "Readings").
	SourceMethod string `json:"method"`
	// Stages is the list of MongoDB aggregation pipeline stages (after the auto-prepended $match).
	Stages []map[string]interface{} `json:"stages,omitempty"`
}

// Validate ensures the config is well-formed.
func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	if cfg.SourceComponent == "" {
		return nil, nil, fmt.Errorf("source is required")
	}
	if cfg.SourceMethod == "" {
		return nil, nil, fmt.Errorf("method is required")
	}
	return nil, nil, nil
}

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newSensor,
	})
}

// Sensor is the aggregation sensor resource.
type Sensor struct {
	resource.Named
	resource.AlwaysRebuild

	cfg        Config
	mu         sync.Mutex
	collection *mongo.Collection
	lastCallAt time.Time
	createdAt  time.Time
	logger     logging.Logger
}

func newSensor(
	_ context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	return &Sensor{
		Named:     conf.ResourceName().AsNamed(),
		cfg:       *cfg,
		createdAt: time.Now(),
		logger:    logger,
	}, nil
}

// SourceComponent returns the configured source component name.
func (s *Sensor) SourceComponent() string { return s.cfg.SourceComponent }

// SourceMethod returns the configured source method name.
func (s *Sensor) SourceMethod() string { return s.cfg.SourceMethod }

// TabularObserver returns a function that the data manager wires to the source component's
// collector. Each captured reading is written to this sensor's mongo collection.
func (s *Sensor) TabularObserver() func(ctx context.Context, doc data.TabularDataBson) {
	return func(ctx context.Context, doc data.TabularDataBson) {
		s.mu.Lock()
		coll := s.collection
		s.mu.Unlock()
		if coll == nil {
			return
		}
		if _, err := coll.InsertOne(ctx, doc); err != nil {
			s.logger.Warnw("aggregation sensor: failed to insert reading", "error", err)
		}
	}
}

// SetCollection injects the mongo collection for this sensor's aggregations.
// Called by the data manager once mongod is ready.
func (s *Sensor) SetCollection(coll *mongo.Collection) {
	s.mu.Lock()
	s.collection = coll
	s.mu.Unlock()
}

// Readings runs the configured aggregation pipeline over all readings captured since
// the last call (using time.Now() - lastCallAt as the window) and returns the results.
// On the first call the window extends back to sensor creation time.
func (s *Sensor) Readings(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	s.mu.Lock()
	coll := s.collection
	now := time.Now()
	windowStart := s.lastCallAt
	if windowStart.IsZero() {
		windowStart = s.createdAt
	}
	s.lastCallAt = now
	s.mu.Unlock()

	if coll == nil {
		return map[string]interface{}{}, nil
	}

	pipeline, err := buildPipeline(windowStart, s.cfg.Stages)
	if err != nil {
		return nil, fmt.Errorf("aggregation sensor: build pipeline: %w", err)
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregation sensor: aggregate: %w", err)
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var results []map[string]interface{}
	for cursor.Next(ctx) {
		var raw bson.M
		if err := cursor.Decode(&raw); err != nil {
			return nil, fmt.Errorf("aggregation sensor: decode: %w", err)
		}
		// Round-trip through extended JSON to get plain Go types.
		jsonBytes, err := bson.MarshalExtJSON(raw, false, false)
		if err != nil {
			continue
		}
		var plain map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &plain); err != nil {
			continue
		}
		results = append(results, plain)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("aggregation sensor: cursor: %w", err)
	}
	if results == nil {
		results = []map[string]interface{}{}
	}
	return map[string]interface{}{"results": results}, nil
}

// Close implements resource.Resource.
func (s *Sensor) Close(_ context.Context) error {
	return nil
}

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
