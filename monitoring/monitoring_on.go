// +build monitoring

package monitoring

import (
	"fmt"
	"net/http"

	"google.golang.org/grpc"

	"github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Enabled signifies whether the monitoring tag was specified when building lnd
// and whether to automatically export Prometheus metrics.
const Enabled = true

// Start registers gRPC metrics and launches the Prometheus exporter on the
// specified address.
func Start(grpcServer *grpc.Server, listenAddr string) {
	if listenAddr == "" {
		listenAddr = "localhost:8989"
	}
	grpc_prometheus.Register(grpcServer)
	http.Handle("/metrics", promhttp.Handler())
	fmt.Println(http.ListenAndServe(listenAddr, nil))
}
