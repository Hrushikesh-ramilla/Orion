package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	TasksIngestedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "total_tasks_ingested",
		Help: "The total number of tasks ingested into the system",
	})
	TasksCompletedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tasks_completed_total",
		Help: "The total number of tasks successfully completed",
	})
	TasksFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tasks_failed_total",
		Help: "The total number of tasks permanently failed",
	})
	TasksRetriedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tasks_retried_total",
		Help: "The total number of task retry attempts",
	})
	CurrentQueueSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "current_queue_size",
		Help: "The current number of tasks resting in the ready queue",
	})
	ActiveWorkers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "active_workers",
		Help: "The number of currently executing worker goroutines",
	})
	TaskLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "task_execution_latency_seconds",
		Help:    "Execution latency of completed tasks",
		Buckets: prometheus.DefBuckets,
	})
)
