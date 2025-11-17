package collector

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shirou/gopsutil/v4/cpu"
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
	processCPU    *prometheus.Desc
	processMem    *prometheus.Desc
	logger        *slog.Logger
	cpuSamples    map[uint]cpuSample
	systemSample  systemCPUSample
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
		processCPU: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUProcessSubsystem, "cpu"),
			"Process CPU usage percentage.",
			[]string{"hostname", "gpu_id", "pid", "process_name", "uid", "command"}, nil,
		),
		processMem: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, GPUProcessSubsystem, "memory"),
			"Process memory usage percentage.",
			[]string{"hostname", "gpu_id", "pid", "process_name", "uid", "command"}, nil,
		),
		logger:     logger,
		cpuSamples: make(map[uint]cpuSample),
	}, nil
}

func (c *gpuProcessCollector) Update(ch chan<- prometheus.Metric) error {
	hostname := hostNameOrDefault(c.logger)

	pids, err := nvmlProcessPIDs(c.logger)
	if err != nil {
		if errors.Is(err, errGPUProcessInfoUnavailable) {
			c.logger.Debug("gpu process listing unavailable", "err", err)
			return nil
		}
		return fmt.Errorf("list gpu processes: %w", err)
	}
	if len(pids) == 0 {
		c.logger.Debug("no gpu processes reported")
		c.resetCPUSamples()
		return nil
	}

	totalSeconds, err := readTotalCPUSeconds()
	if err != nil {
		return fmt.Errorf("read system cpu seconds: %w", err)
	}
	systemDelta, hasSystemDelta := c.updateSystemSample(totalSeconds)
	numCPU := runtime.NumCPU()

	cleanup, err := dcgm.Init(dcgm.Embedded)
	if err != nil {
		return fmt.Errorf("init dcgm: %w", err)
	}
	defer cleanup()

	group, err := dcgm.WatchPidFields()
	if err != nil {
		return fmt.Errorf("watch pid fields: %w", err)
	}
	defer func() {
		if destroyErr := dcgm.DestroyGroup(group); destroyErr != nil {
			c.logger.Debug("failed to destroy pid group", "err", destroyErr)
		}
	}()

	for _, pid := range pids {
		infos, err := dcgm.GetProcessInfo(group, pid)
		if err != nil {
			if shouldIgnoreProcessError(err) {
				continue
			}
			c.logger.Debug("failed to get process info", "pid", pid, "err", err)
			continue
		}

		if len(infos) == 0 {
			continue
		}

		meta, memPercent, procCPUSeconds, err := collectProcessInfo(pid, infos[0].Name)
		if err != nil {
			c.logger.Debug("failed to collect host process info", "pid", pid, "err", err)
			continue
		}

		cpuPercent := c.processCPUPercent(pid, procCPUSeconds, systemDelta, hasSystemDelta, numCPU)
		if cpuPercent < 0 {
			cpuPercent = 0
		}
		if memPercent < 0 {
			memPercent = 0
		}

		for _, info := range infos {
			labels := []string{
				hostname,
				strconv.FormatUint(uint64(info.GPU), 10),
				strconv.FormatUint(uint64(info.PID), 10),
				meta.name,
				meta.uid,
				meta.command,
			}

			ch <- prometheus.MustNewConstMetric(
				c.processGPUMem,
				prometheus.GaugeValue,
				sanitizeBytes(info.Memory.GlobalUsed),
				labels...,
			)

			ch <- prometheus.MustNewConstMetric(
				c.processCPU,
				prometheus.GaugeValue,
				cpuPercent,
				labels...,
			)

			ch <- prometheus.MustNewConstMetric(
				c.processMem,
				prometheus.GaugeValue,
				memPercent,
				labels...,
			)
		}
	}

	c.pruneCPUSamples(pids)

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

type cpuSample struct {
	cpuSeconds float64
}

type systemCPUSample struct {
	totalSeconds float64
	initialized  bool
}

func nvmlProcessPIDs(logger *slog.Logger) ([]uint, error) {
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

	pidSet := make(map[uint]struct{})
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device handle (index=%d): %s", i, nvml.ErrorString(ret))
		}

		if err := addNVMLProcessInfo(pidSet, device.GetComputeRunningProcesses, "compute", i, logger); err != nil {
			return nil, err
		}
		if err := addNVMLProcessInfo(pidSet, device.GetGraphicsRunningProcesses, "graphics", i, logger); err != nil {
			return nil, err
		}
	}

	result := make([]uint, 0, len(pidSet))
	for pid := range pidSet {
		result = append(result, pid)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

type nvmlProcessGetter func() ([]nvml.ProcessInfo, nvml.Return)

func addNVMLProcessInfo(pidSet map[uint]struct{}, getter nvmlProcessGetter, typ string, gpuIndex int, logger *slog.Logger) error {
	processes, ret := getter()
	switch ret {
	case nvml.SUCCESS:
		for _, info := range processes {
			if info.Pid == 0 {
				continue
			}
			pidSet[uint(info.Pid)] = struct{}{}
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

func collectProcessInfo(pid uint, fallbackName string) (processMetadata, float64, float64, error) {
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return processMetadata{}, 0, 0, err
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

	memPercent, err := proc.MemoryPercent()
	if err != nil {
		return processMetadata{}, 0, 0, err
	}

	times, err := proc.Times()
	if err != nil {
		return processMetadata{}, 0, 0, err
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

	cpuSeconds := times.User + times.System

	return meta, float64(memPercent), cpuSeconds, nil
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
	if value <= 0 || dcgm.IsInt64Blank(value) {
		return 0
	}
	return float64(value)
}

func (c *gpuProcessCollector) processCPUPercent(pid uint, procSeconds float64, systemDelta float64, hasSystemDelta bool, numCPU int) float64 {
	prev, ok := c.cpuSamples[pid]
	c.cpuSamples[pid] = cpuSample{cpuSeconds: procSeconds}
	if !ok || !hasSystemDelta || systemDelta <= 0 {
		return 0
	}
	if procSeconds <= prev.cpuSeconds {
		return 0
	}
	processDelta := procSeconds - prev.cpuSeconds
	return (processDelta / systemDelta) * float64(numCPU) * 100
}

func (c *gpuProcessCollector) pruneCPUSamples(active []uint) {
	if len(c.cpuSamples) == 0 {
		return
	}
	activeSet := make(map[uint]struct{}, len(active))
	for _, pid := range active {
		activeSet[pid] = struct{}{}
	}
	for pid := range c.cpuSamples {
		if _, ok := activeSet[pid]; !ok {
			delete(c.cpuSamples, pid)
		}
	}
}

func (c *gpuProcessCollector) resetCPUSamples() {
	for pid := range c.cpuSamples {
		delete(c.cpuSamples, pid)
	}
	c.systemSample = systemCPUSample{}
}

func (c *gpuProcessCollector) updateSystemSample(total float64) (float64, bool) {
	if !c.systemSample.initialized {
		c.systemSample = systemCPUSample{totalSeconds: total, initialized: true}
		return 0, false
	}
	var delta float64
	if total >= c.systemSample.totalSeconds {
		delta = total - c.systemSample.totalSeconds
	}
	c.systemSample.totalSeconds = total
	if delta <= 0 {
		return 0, false
	}
	return delta, true
}

func readTotalCPUSeconds() (float64, error) {
	timesStats, err := cpu.Times(false)
	if err != nil {
		return 0, err
	}
	if len(timesStats) == 0 {
		return 0, fmt.Errorf("cpu times unavailable")
	}
	ts := timesStats[0]
	return ts.User + ts.System + ts.Nice + ts.Irq + ts.Softirq +
		ts.Steal + ts.Idle + ts.Iowait + ts.Guest + ts.GuestNice, nil
}

func shouldIgnoreProcessError(err error) bool {
	var dcgmErr *dcgm.Error
	if !errors.As(err, &dcgmErr) {
		return false
	}

	switch int(dcgmErr.Code) {
	case dcgm.DCGM_ST_NO_DATA,
		dcgm.DCGM_ST_NOT_WATCHED,
		dcgm.DCGM_ST_BADPARAM,
		dcgm.DCGM_ST_NO_PERMISSION:
		return true
	default:
		return false
	}
}
