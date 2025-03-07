// Package cloud implements a service to grab gRPC connections to talk to
// a cloud service that manages robots.
package cloud

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"go.viam.com/utils"
	"go.viam.com/utils/rpc"

	"go.viam.com/rdk/config"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

const (
	// SubtypeName is a constant that identifies the internal cloud connection resource
	// subtype string.
	SubtypeName               = "cloud_connection"
	connectTimeout            = 5 * time.Second
	connectTimeoutBehindProxy = time.Minute
)

// API is the fully qualified API for the internal cloud connection service.
var API = resource.APINamespaceRDKInternal.WithServiceType(SubtypeName)

// InternalServiceName is used to refer to/depend on this service internally.
var InternalServiceName = resource.NewName(API, "builtin")

// A ConnectionService supplies connections to a cloud service managing robots. Each
// connection should be closed when its not be used anymore.
type ConnectionService interface {
	resource.Resource
	AcquireConnection(ctx context.Context) (string, rpc.ClientConn, error)
	AcquireConnectionAPIKey(ctx context.Context, apiKey, apiKeyID string) (string, rpc.ClientConn, error)
}

// NewCloudConnectionService makes a new cloud connection service to get gRPC connections
// to a cloud service managing robots.
func NewCloudConnectionService(cfg *config.Cloud, conn rpc.ClientConn, logger logging.Logger) ConnectionService {
	if cfg == nil || cfg.AppAddress == "" {
		return &cloudManagedService{
			Named: InternalServiceName.AsNamed(),
		}
	}

	cm := &cloudManagedService{
		Named:    InternalServiceName.AsNamed(),
		conn:     conn,
		managed:  true,
		dialer:   rpc.NewCachedDialer(),
		cloudCfg: *cfg,
		logger:   logger,
	}

	return cm
}

type cloudManagedService struct {
	resource.Named
	// we assume the config is immutable for the lifetime of the process
	resource.TriviallyReconfigurable

	conn rpc.ClientConn

	managed  bool
	cloudCfg config.Cloud
	logger   logging.Logger

	dialerMu sync.RWMutex
	dialer   rpc.Dialer
}

// AcquireConnection returns the connection provided to `NewCloudConnectionService` regardless of the state of the `cloudManagedService`.
// This means that if `Close` has been called on the `cloudManagedService`, `AcquireConnection` can still return an open connection.
func (cm *cloudManagedService) AcquireConnection(ctx context.Context) (string, rpc.ClientConn, error) {
	if cm.conn == nil {
		return "", nil, ErrNotCloudManaged
	}

	return cm.cloudCfg.ID, cm.conn, nil
}

func (cm *cloudManagedService) AcquireConnectionAPIKey(ctx context.Context,
	apiKey, apiKeyID string,
) (string, rpc.ClientConn, error) {
	cm.dialerMu.RLock()
	defer cm.dialerMu.RUnlock()
	if !cm.managed {
		return "", nil, ErrNotCloudManaged
	}
	if cm.dialer == nil {
		return "", nil, errors.New("service closed")
	}

	ctx = rpc.ContextWithDialer(ctx, cm.dialer)
	timeout := connectTimeout
	// When environment indicates we are behind a proxy, bump timeout. Network
	// operations tend to take longer when behind a proxy.
	if os.Getenv(rpc.SocksProxyEnvVar) != "" {
		timeout = connectTimeoutBehindProxy
	}
	timeOutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := config.CreateNewGRPCClientWithAPIKey(timeOutCtx, &cm.cloudCfg, apiKey, apiKeyID, cm.logger)
	return cm.cloudCfg.ID, conn, err
}

func (cm *cloudManagedService) Close(ctx context.Context) error {
	cm.dialerMu.Lock()
	defer cm.dialerMu.Unlock()

	if cm.dialer != nil {
		utils.UncheckedError(cm.dialer.Close())
		cm.dialer = nil
	}

	return nil
}

// ErrNotCloudManaged is returned if a connection is requested but the robot is not
// yet cloud managed.
var ErrNotCloudManaged = errors.New("this robot is not cloud managed")
