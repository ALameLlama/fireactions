package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	namespace = "fireactions"
)

var (
	metricCleanIdleVMs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "clean_idle_vms", Namespace: namespace,
		Help: "Number of clean idle virtual machines",
	}, []string{"profile"})
	metricClaimedVMs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "claimed_vms", Namespace: namespace,
		Help: "Number of claimed virtual machines",
	}, []string{"profile"})
	metricActiveEnvironments = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "active_environments", Namespace: namespace,
		Help: "Number of owned environments by profile",
	}, []string{"profile"})
	metricVMAcquisition = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "vm_acquisition_seconds", Namespace: namespace,
		Help:    "Time spent acquiring a virtual machine",
		Buckets: prometheus.DefBuckets,
	}, []string{"profile"})
	metricVMBoot = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "vm_boot_seconds", Namespace: namespace,
		Help:    "Time spent booting a virtual machine",
		Buckets: prometheus.DefBuckets,
	}, []string{"profile"})
	metricGuestReadiness = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "guest_readiness_seconds", Namespace: namespace,
		Help:    "Time spent waiting for the guest agent to become ready",
		Buckets: prometheus.DefBuckets,
	}, []string{"profile"})
	metricOperations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "operations_total", Namespace: namespace,
		Help: "Environment operations by profile, operation, and outcome",
	}, []string{"profile", "operation", "outcome"})
	metricCleanupFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cleanup_failures_total", Namespace: namespace,
		Help: "Environment cleanup attempts that failed",
	}, []string{"profile"})
)

// refreshMetrics must be called without machinesMu held.
func (p *Pool) refreshMetrics() {
	idle, claimed := 0, 0
	p.machinesMu.Lock()
	for _, machine := range p.machines {
		switch machine.Metadata().State {
		case "idle":
			idle++
		case "claimed":
			claimed++
		}
	}
	defer p.machinesMu.Unlock()
	metricCleanIdleVMs.WithLabelValues(p.config.Name).Set(float64(idle))
	metricClaimedVMs.WithLabelValues(p.config.Name).Set(float64(claimed))
}
