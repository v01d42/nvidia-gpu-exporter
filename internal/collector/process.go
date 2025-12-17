package collector

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shirou/gopsutil/v4/process"
)

const (
	GPUProcessSubsystem   = "process"
	unknownProcessLabel   = "unknown"
	maxCommandLabelLength = 200
)

// GPUMetricsCollector manages Prometheus metrics for physical GPU resources.
type gpuProcessCollector struct {
	processGPUMem *prometheus.Desc
	logger        *slog.Logger
}

func init() {
	registerCollector("gpu_process", NewGPUProcessCollector)
}

func NewGPUProcessCollector(logger *slog.Logger) (Collector, error) {
	return &gpuProcessCollector{
		processGPUMem: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUProcessSubsystem, "gpu_memory"),
			"GPU process memory usage in bytes.",
			[]string{"hostname", "gpu_id", "pid", "process_name", "uid", "command"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *gpuProcessCollector) Update(ch chan<- prometheus.Metric) error {
	hostname := hostNameOrDefault(c.logger)

	usages, err := nvmlGPUProcessUsages(c.logger)
	if err != nil {
		if errors.Is(err, errGPUProcessInfoUnavailable) {
			c.logger.Debug("gpu process listing unavailable", "err", err)
			return nil
		}
		return fmt.Errorf("list gpu processes: %w", err)
	}
	if len(usages) == 0 {
		c.logger.Debug("no gpu processes reported")
		return nil
	}

	metaCache := make(map[uint]processMetadata)

	for _, usage := range usages {
		meta, ok := metaCache[usage.pid]
		if !ok {
			var metaErr error
			meta, metaErr = collectProcessInfo(usage.pid, "")
			if metaErr != nil {
				c.logger.Debug("failed to collect host process info", "pid", usage.pid, "err", metaErr)
				continue
			}
			metaCache[usage.pid] = meta
		}

		labels := []string{
			hostname,
			strconv.FormatUint(uint64(usage.gpu), 10),
			strconv.FormatUint(uint64(usage.pid), 10),
			meta.name,
			meta.uid,
			meta.command,
		}

		ch <- prometheus.MustNewConstMetric(
			c.processGPUMem,
			prometheus.GaugeValue,
			sanitizeBytes(int64(usage.memBytes)),
			labels...,
		)
	}

	return nil
}

var (
	errGPUProcessInfoUnavailable = errors.New("gpu process list unavailable")
)

type processMetadata struct {
	name    string
	uid     string
	command string
}

type gpuProcessUsage struct {
	gpu      uint
	pid      uint
	memBytes uint64
}

func nvmlGPUProcessUsages(logger *slog.Logger) ([]gpuProcessUsage, error) {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, wrapNVMLAvailabilityError("nvml init", ret)
	}
	defer func() {
		if shutdownRet := nvml.Shutdown(); shutdownRet != nvml.SUCCESS {
			logger.Debug("failed to shutdown nvml", "err", nvml.ErrorString(shutdownRet))
		}
	}()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, wrapNVMLAvailabilityError("nvml device count", ret)
	}

	usages := make([]gpuProcessUsage, 0)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device handle (index=%d): %s", i, nvml.ErrorString(ret))
		}

		if err := appendNVMLProcessUsages(&usages, device.GetComputeRunningProcesses, "compute", i, logger); err != nil {
			return nil, err
		}
		if err := appendNVMLProcessUsages(&usages, device.GetGraphicsRunningProcesses, "graphics", i, logger); err != nil {
			return nil, err
		}
	}

	sort.Slice(usages, func(i, j int) bool {
		if usages[i].gpu == usages[j].gpu {
			return usages[i].pid < usages[j].pid
		}
		return usages[i].gpu < usages[j].gpu
	})

	return usages, nil
}

type nvmlProcessGetter func() ([]nvml.ProcessInfo, nvml.Return)

func appendNVMLProcessUsages(dst *[]gpuProcessUsage, getter nvmlProcessGetter, typ string, gpuIndex int, logger *slog.Logger) error {
	processes, ret := getter()
	switch ret {
	case nvml.SUCCESS:
		for _, info := range processes {
			if info.Pid == 0 {
				continue
			}
			*dst = append(*dst, gpuProcessUsage{
				gpu:      uint(gpuIndex),
				pid:      uint(info.Pid),
				memBytes: info.UsedGpuMemory,
			})
		}
		return nil
	case nvml.ERROR_NOT_SUPPORTED, nvml.ERROR_NO_PERMISSION, nvml.ERROR_NOT_FOUND:
		logger.Debug("nvml process info unavailable", "gpu_index", gpuIndex, "type", typ, "err", nvml.ErrorString(ret))
		return nil
	default:
		return fmt.Errorf("nvml %s running processes (gpu=%d): %s", typ, gpuIndex, nvml.ErrorString(ret))
	}
}

func wrapNVMLAvailabilityError(op string, ret nvml.Return) error {
	switch ret {
	case nvml.ERROR_UNINITIALIZED,
		nvml.ERROR_LIBRARY_NOT_FOUND,
		nvml.ERROR_DRIVER_NOT_LOADED,
		nvml.ERROR_NOT_SUPPORTED,
		nvml.ERROR_NO_PERMISSION,
		nvml.ERROR_UNKNOWN:
		return fmt.Errorf("%s: %w (%s)", op, errGPUProcessInfoUnavailable, nvml.ErrorString(ret))
	default:
		return fmt.Errorf("%s: %s", op, nvml.ErrorString(ret))
	}
}

func collectProcessInfo(pid uint, fallbackName string) (processMetadata, error) {
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return processMetadata{}, err
	}

	name, err := proc.Name()
	if err != nil {
		name = ""
	}

	uid := unknownProcessLabel
	if uids, err := proc.Uids(); err == nil && len(uids) > 0 {
		uid = strconv.FormatInt(int64(uids[0]), 10)
	}

	cmdline, err := proc.Cmdline()
	if err != nil || strings.TrimSpace(cmdline) == "" {
		if args, err := proc.CmdlineSlice(); err == nil && len(args) > 0 {
			cmdline = strings.Join(args, " ")
		}
	}

	meta := processMetadata{
		name:    firstNonEmpty(name, fallbackName, unknownProcessLabel),
		uid:     firstNonEmpty(uid, unknownProcessLabel),
		command: firstNonEmpty(cmdline, name, fallbackName, unknownProcessLabel),
	}
	if limit := maxCommandLabelLength; len(meta.command) > limit {
		runes := []rune(meta.command)
		if len(runes) > limit {
			meta.command = string(runes[:limit])
		} else {
			meta.command = meta.command[:limit]
		}
	}

	return meta, nil
}

func firstNonEmpty(values ...string) string {
	for _, val := range values {
		if strings.TrimSpace(val) != "" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

func sanitizeBytes(value int64) float64 {
	if value <= 0 {
		return 0
	}
	return float64(value)
}
