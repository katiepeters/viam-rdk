package capture

import (
	"context"

	"go.viam.com/rdk/data"
)

// PipelineObservers provides per-(component, method) tabular observer callbacks.
// Implemented by robot/pipelinemanager.PipelineManager.
type PipelineObservers interface {
	ObserverFor(component, method string) func(ctx context.Context, doc data.TabularDataBson)
}
