// +build !monitoring

package monitoring

import (
	"google.golang.org/grpc"
)

// Enabled specifies that lnd was not build with the monitoring tag so gRPC
// metrics should not be exported automatically.
const Enabled = false

// Start is required for lnd to compile so that Prometheus metric exporting can
// be hidden behind a build tag.
func Start(_ *grpc.Server, _ string) {}
