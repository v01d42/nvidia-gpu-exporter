package collector

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	dcgm "github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

const (
	GPUMetricsSubsystem = "metrics"
)

var gpuMetricFields = []dcgm.Short{
	dcgm.DCGM_FI_DEV_FB_FREE,
	dcgm.DCGM_FI_DEV_FB_USED,
	dcgm.DCGM_FI_DEV_FB_TOTAL,
	dcgm.DCGM_FI_DEV_GPU_TEMP,
	dcgm.DCGM_FI_DEV_GPU_UTIL,
}

// GPUMetricsCollector manages Prometheus metrics for physical GPU resources and
// node‑level CPU / memory usage.
type gpuMetricsCollector struct {
	gpuFreeMemory  *prometheus.Desc
	gpuUsedMemory  *prometheus.Desc
	gpuTotalMemory *prometheus.Desc
	gpuTemperature *prometheus.Desc
	gpuUtilization *prometheus.Desc
	CPUUtilization *prometheus.Desc
	memUtilization *prometheus.Desc
	logger         *slog.Logger
}

func init() {
	registerCollector("gpu_metrics", NewGPUMetricsCollector)
}

func NewGPUMetricsCollector(logger *slog.Logger) (Collector, error) {
	return &gpuMetricsCollector{
		gpuFreeMemory: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "free_memory"),
			"GPU free memory in bytes.",
			[]string{"hostname", "gpu_id", "gpu_name"}, nil,
		),
		gpuUsedMemory: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "used_memory"),
			"GPU used memory in bytes.",
			[]string{"hostname", "gpu_id", "gpu_name"}, nil,
		),
		gpuTotalMemory: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "total_memory"),
			"GPU total memory in bytes.",
			[]string{"hostname", "gpu_id", "gpu_name"}, nil,
		),
		gpuTemperature: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "temperature"),
			"GPU temperature in Celsius.",
			[]string{"hostname", "gpu_id", "gpu_name"}, nil,
		),
		gpuUtilization: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "gpu_utilization"),
			"GPU utilization percentage.",
			[]string{"hostname", "gpu_id", "gpu_name"}, nil,
		),
		CPUUtilization: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "cpu_utilization"),
			"Node total CPU utilization percentage.",
			[]string{"hostname"}, nil,
		),
		memUtilization: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUMetricsSubsystem, "memory_utilization"),
			"Node total memory utilization percentage.",
			[]string{"hostname"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *gpuMetricsCollector) Update(ch chan<- prometheus.Metric) error {
	hostname := hostNameOrDefault(c.logger)
	cleanup, err := dcgm.Init(dcgm.Embedded)
	if err != nil {
		return fmt.Errorf("failed to initialize DCGM: %w", err)
	}
	defer cleanup()

	gpus, err := dcgm.GetSupportedDevices()
	if err != nil {
		return fmt.Errorf("failed to list supported GPUs: %w", err)
	}
	if len(gpus) == 0 {
		c.logger.Warn("DCGM did not report any GPUs on this node")
		return nil
	}

	for _, gpuID := range gpus {
		deviceInfo, err := dcgm.GetDeviceInfo(gpuID)
		if err != nil {
			c.logger.Warn("failed to query DCGM device info", "gpu_id", gpuID, "err", err)
			continue
		}

		fieldValues, err := c.collectFieldValues(gpuID, gpuMetricFields)
		if err != nil {
			c.logger.Warn("failed to collect DCGM field values", "gpu_id", gpuID, "err", err)
			continue
		}

		labels := []string{
			hostname,
			strconv.FormatUint(uint64(gpuID), 10),
			gpuDisplayName(deviceInfo),
		}

		if val, ok := fieldValues[dcgm.DCGM_FI_DEV_FB_FREE]; ok {
			ch <- prometheus.MustNewConstMetric(c.gpuFreeMemory, prometheus.GaugeValue, mibToBytes(val.Int64()), labels...)
		}
		if val, ok := fieldValues[dcgm.DCGM_FI_DEV_FB_USED]; ok {
			ch <- prometheus.MustNewConstMetric(c.gpuUsedMemory, prometheus.GaugeValue, mibToBytes(val.Int64()), labels...)
		}
		if val, ok := fieldValues[dcgm.DCGM_FI_DEV_FB_TOTAL]; ok {
			ch <- prometheus.MustNewConstMetric(c.gpuTotalMemory, prometheus.GaugeValue, mibToBytes(val.Int64()), labels...)
		}
		if val, ok := fieldValues[dcgm.DCGM_FI_DEV_GPU_TEMP]; ok {
			ch <- prometheus.MustNewConstMetric(c.gpuTemperature, prometheus.GaugeValue, float64(val.Int64()), labels...)
		}
		if val, ok := fieldValues[dcgm.DCGM_FI_DEV_GPU_UTIL]; ok {
			ch <- prometheus.MustNewConstMetric(c.gpuUtilization, prometheus.GaugeValue, float64(val.Int64()), labels...)
		}
	}

	// Node‑level CPU / memory utilization. We treat failures here as non‑fatal
	// and log them, since GPU metrics are still valuable on their own.
	if cpuPercent, err := readCPUPercent(); err != nil {
		c.logger.Debug("failed to read node cpu utilization", "err", err)
	} else {
		ch <- prometheus.MustNewConstMetric(
			c.CPUUtilization,
			prometheus.GaugeValue,
			cpuPercent,
			hostname,
		)
	}

	if memPercent, err := readMemPercent(); err != nil {
		c.logger.Debug("failed to read node memory utilization", "err", err)
	} else {
		ch <- prometheus.MustNewConstMetric(
			c.memUtilization,
			prometheus.GaugeValue,
			memPercent,
			hostname,
		)
	}

	return nil
}

func (c *gpuMetricsCollector) collectFieldValues(gpuID uint, fields []dcgm.Short) (map[dcgm.Short]dcgm.FieldValue_v1, error) {
	suffix := time.Now().UnixNano()
	fieldsGroup, err := dcgm.FieldGroupCreate(fmt.Sprintf("gpu-metrics-fields-%d-%d", gpuID, suffix), fields)
	if err != nil {
		return nil, fmt.Errorf("create field group: %w", err)
	}
	defer func() {
		if destroyErr := dcgm.FieldGroupDestroy(fieldsGroup); destroyErr != nil {
			c.logger.Debug("failed to destroy DCGM field group", "gpu_id", gpuID, "err", destroyErr)
		}
	}()

	group, err := dcgm.WatchFields(gpuID, fieldsGroup, fmt.Sprintf("gpu-metrics-watch-%d-%d", gpuID, suffix))
	if err != nil {
		return nil, fmt.Errorf("watch fields: %w", err)
	}
	defer func() {
		if destroyErr := dcgm.DestroyGroup(group); destroyErr != nil {
			c.logger.Debug("failed to destroy DCGM group", "gpu_id", gpuID, "err", destroyErr)
		}
	}()

	values, err := dcgm.GetLatestValuesForFields(gpuID, fields)
	if err != nil {
		return nil, fmt.Errorf("get latest values: %w", err)
	}

	result := make(map[dcgm.Short]dcgm.FieldValue_v1, len(values))
	for _, value := range values {
		if value.Status != dcgm.DCGM_ST_OK {
			continue
		}
		result[value.FieldID] = value
	}

	return result, nil
}

func hostNameOrDefault(logger *slog.Logger) string {
	hostname, err := os.Hostname()
	if err != nil {
		if logger != nil {
			logger.Warn("failed to determine hostname", "err", err)
		}
		return "unknown"
	}

	if nodeName := os.Getenv("NODE_NAME"); nodeName != "" {
		return nodeName
	}

	return hostname
}

func gpuDisplayName(info dcgm.Device) string {
	if info.Identifiers.Model != "" {
		return info.Identifiers.Model
	}
	if info.Identifiers.Brand != "" {
		return info.Identifiers.Brand
	}
	return fmt.Sprintf("gpu-%d", info.GPU)
}

func mibToBytes(value int64) float64 {
	const bytesInMiB = 1024 * 1024
	return float64(value) * bytesInMiB
}

// readCPUPercent returns the total CPU utilization percentage for the node.
// It uses gopsutil's instantaneous percentage (interval=0) aggregated over all
// CPUs, which is sufficient for exporter‑style monitoring.
func readCPUPercent() (float64, error) {
	percentages, err := cpu.Percent(0, false)
	if err != nil {
		return 0, err
	}
	if len(percentages) == 0 {
		return 0, fmt.Errorf("cpu percent unavailable")
	}
	return percentages[0], nil
}

// readMemPercent returns the total memory utilization percentage for the
// node, based on gopsutil's VirtualMemory stats.
func readMemPercent() (float64, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return vm.UsedPercent, nil
}
