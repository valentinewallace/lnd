package monitoring

// PrometheusConfig is the set of configuration data that specifies
// the listening address of the Prometheus exporter.
type PrometheusConfig struct {
	// ListenAddr is the listening address that we should use to allow the
	// main Prometheus server to scrape our metrics.
	ListenAddr string `long:"listenaddr" description:"the interface we should listen on for prometheus"`
}
