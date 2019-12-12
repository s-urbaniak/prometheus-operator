package prometheus

import (
	v1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/tools/cache"
)

var (
	descThanosSpecReplicas = prometheus.NewDesc(
		"prometheus_operator_spec_replicas",
		"Number of expected replicas for the object.",
		[]string{
			"namespace",
			"name",
		}, nil,
	)
)

type thanosRulerCollector struct {
	store cache.Store
}

func NewThanosRulerCollector(s cache.Store) *thanosRulerCollector {
	return &thanosRulerCollector{store: s}
}

// Describe implements the prometheus.Collector interface.
func (c *thanosRulerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descThanosSpecReplicas
}

// Collect implements the prometheus.Collector interface.
func (c *thanosRulerCollector) Collect(ch chan<- prometheus.Metric) {
	for _, p := range c.store.List() {
		c.collectThanos(ch, p.(*v1.Prometheus))
	}
}

func (c *thanosRulerCollector) collectThanos(ch chan<- prometheus.Metric, p *v1.Prometheus) {
	replicas := float64(minReplicas)
	if p.Spec.Replicas != nil {
		replicas = float64(*p.Spec.Replicas)
	}
	ch <- prometheus.MustNewConstMetric(descThanosSpecReplicas, prometheus.GaugeValue, replicas, p.Namespace, p.Name)
}
