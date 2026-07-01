package capture

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"

	"go.viam.com/rdk/data"
)

// aggregationSensorDep is the interface capture uses to detect and wire aggregation sensors
// found in the data manager's weak dependencies. Implemented by aggregation.Sensor.
type aggregationSensorDep interface {
	SourceComponent() string
	SourceMethod() string
	TabularObserver() func(ctx context.Context, doc data.TabularDataBson)
	SetCollection(coll *mongo.Collection)
}
