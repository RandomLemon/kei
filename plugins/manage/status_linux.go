//go:build linux

package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 主机硬件采集来源（Linux）。插件不读配置/密钥以外的业务文件，此处只读内核暴露的
// 只读虚拟文件系统：CPU/负载/内存来自 /proc，显卡来自 /sys/class/drm 与
// /proc/driver/nvidia。
const (
	procCPUInfo      = "/proc/cpuinfo"
	procStatPath     = "/proc/stat"
	procLoadavgPath  = "/proc/loadavg"
	procMeminfoPath  = "/proc/meminfo"
	procMountsPath   = "/proc/mounts"
	drmClassDir      = "/sys/class/drm"
	nvidiaGPUDirPath = "/proc/driver/nvidia/gpus"
	// cpuSampleGap 是 CPU 使用率的两次 /proc/stat 采样间隔。
	cpuSampleGap = 200 * time.Millisecond
)

// hostStatus 采集 Linux 主机硬件快照。单项不可读时对应字段留零值，
// 由 formatHostInfo 渲染为「不可用」，因此正常路径不返回错误。
func hostStatus(ctx context.Context) (hostInfo, error) {
	var h hostInfo
	h.cpuUsage = -1

	if data, err := os.ReadFile(procCPUInfo); err == nil {
		h.cpuModel, h.cpuPhysical, h.cpuLogical = parseCPUInfo(string(data))
	}
	if l1, l5, l15, err := readLoadavg(); err == nil {
		h.load1, h.load5, h.load15 = l1, l5, l15
	}
	if usage, err := cpuUsage(ctx); err == nil {
		h.cpuUsage = usage
	}
	h.memTotal, h.memAvailable, h.swapTotal, h.swapFree = readMeminfo()
	h.disks = collectDisks()
	h.gpus = collectGPUs()
	return h, nil
}

// parseCPUInfo 从 /proc/cpuinfo 解析型号、物理核心数与逻辑处理器数。
//
// 物理核心数优先取 "cpu cores" × "physical id" 去重后的插槽数；缺少该字段的
// 架构（如部分 ARM）退化为 0。
func parseCPUInfo(content string) (model string, physical, logical int) {
	cores := 0
	sockets := map[string]struct{}{}
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "processor":
			logical++
		case "model name":
			if model == "" {
				model = val
			}
		case "cpu cores":
			if cores == 0 {
				cores, _ = strconv.Atoi(val)
			}
		case "physical id":
			sockets[val] = struct{}{}
		}
	}
	switch {
	case cores > 0 && len(sockets) > 0:
		physical = cores * len(sockets)
	case cores > 0:
		physical = cores
	}
	return model, physical, logical
}

// parseLoadavg 从 /proc/loadavg 解析 1/5/15 分钟平均负载。
func parseLoadavg(content string) (l1, l5, l15 float64, err error) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return 0, 0, 0, errors.New("loadavg: 字段不足")
	}
	vals := make([]float64, 3)
	for i := range vals {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return 0, 0, 0, err
		}
		vals[i] = v
	}
	return vals[0], vals[1], vals[2], nil
}

// parseStatCPU 从 /proc/stat 的汇总 cpu 行解析 (idle, total)。
//
// idle 含 idle + iowait；total 为各状态时长之和（单位 jiffies）。
func parseStatCPU(content string) (idle, total uint64, err error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		for i, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, err
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return idle, total, nil
	}
	return 0, 0, errors.New("stat: 缺少 cpu 行")
}

// cpuUsage 通过两次采样 /proc/stat 计算整体 CPU 使用率（0..1）。
func cpuUsage(ctx context.Context) (float64, error) {
	idle1, total1, err := readStatCPU()
	if err != nil {
		return 0, err
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(cpuSampleGap):
	}
	idle2, total2, err := readStatCPU()
	if err != nil {
		return 0, err
	}
	if total2 <= total1 {
		return 0, errors.New("stat: 采样区间内无 CPU 时间")
	}
	usage := 1 - float64(idle2-idle1)/float64(total2-total1)
	if usage < 0 {
		usage = 0
	}
	if usage > 1 {
		usage = 1
	}
	return usage, nil
}

func readStatCPU() (idle, total uint64, err error) {
	data, err := os.ReadFile(procStatPath)
	if err != nil {
		return 0, 0, err
	}
	return parseStatCPU(string(data))
}

func readLoadavg() (l1, l5, l15 float64, err error) {
	data, err := os.ReadFile(procLoadavgPath)
	if err != nil {
		return 0, 0, 0, err
	}
	return parseLoadavg(string(data))
}

// parseMeminfo 从 /proc/meminfo 解析内存与交换总量/可用量（字节）。
//
// MemAvailable 缺失时（旧内核）退化为 MemFree + Buffers + Cached。
func parseMeminfo(content string) (total, available, swapTotal, swapFree uint64) {
	vals := map[string]uint64{}
	for _, line := range strings.Split(content, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			n *= 1024
		}
		vals[strings.TrimSpace(key)] = n
	}
	available = vals["MemAvailable"]
	if available == 0 {
		available = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	return vals["MemTotal"], available, vals["SwapTotal"], vals["SwapFree"]
}

func readMeminfo() (total, available, swapTotal, swapFree uint64) {
	data, err := os.ReadFile(procMeminfoPath)
	if err != nil {
		return 0, 0, 0, 0
	}
	return parseMeminfo(string(data))
}

// mountEntry 是 /proc/mounts 的一行。
type mountEntry struct{ device, mountpoint, fstype string }

// mountEscaper 还原 /proc/mounts 中的八进制转义（空格、制表符等）。
var mountEscaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// parseMounts 解析 /proc/mounts 内容。
func parseMounts(content string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		out = append(out, mountEntry{
			device:     mountEscaper.Replace(fields[0]),
			mountpoint: mountEscaper.Replace(fields[1]),
			fstype:     fields[2],
		})
	}
	return out
}

// collectDisks 汇总所有块设备（device 以 /dev/ 开头，或挂载点为 /）的占用情况。
//
// 同一设备的多处挂载合并为一行：主挂载点取最短路径，其余计入 extraMounts。
// statfs 失败的挂载点被跳过。
func collectDisks() []diskInfo {
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return nil
	}
	type group struct {
		fstype      string
		primary     string
		extraMounts int
	}
	groups := map[string]*group{}
	var order []string
	for _, m := range parseMounts(string(data)) {
		if !strings.HasPrefix(m.device, "/dev/") && m.mountpoint != "/" {
			continue
		}
		g, ok := groups[m.device]
		if !ok {
			groups[m.device] = &group{fstype: m.fstype, primary: m.mountpoint}
			order = append(order, m.device)
			continue
		}
		if len(m.mountpoint) < len(g.primary) {
			g.primary = m.mountpoint
		}
		g.extraMounts++
	}

	var disks []diskInfo
	for _, dev := range order {
		g := groups[dev]
		total, used, avail, err := diskUsage(g.primary)
		if err != nil {
			continue
		}
		disks = append(disks, diskInfo{
			device:      dev,
			fstype:      g.fstype,
			mountpoint:  g.primary,
			extraMounts: g.extraMounts,
			total:       total,
			used:        used,
			available:   avail,
		})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].mountpoint < disks[j].mountpoint })
	return disks
}

// diskUsage 用 statfs 计算路径所在文件系统的总量/已用/可用字节。
//
// 口径与 df 一致：used = Blocks - Bfree，available = Bavail。
func diskUsage(path string) (total, used, available uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	bsize := uint64(st.Bsize)
	total = uint64(st.Blocks) * bsize
	available = uint64(st.Bavail) * bsize
	free := uint64(st.Bfree) * bsize
	if free > total {
		free = total
	}
	used = total - free
	return total, used, available, nil
}

// pciVendors 是常见显卡 PCI 厂商 ID 到厂商名的映射（用于命名取不到型号的显卡）。
var pciVendors = map[string]string{
	"0x10de": "NVIDIA",
	"0x1002": "AMD",
	"0x1022": "AMD",
	"0x8086": "Intel",
	"0x106b": "Apple",
	"0x13b5": "ARM",
	"0x1af4": "Red Hat (virtio)",
	"0x1b36": "Red Hat (QXL)",
	"0x1234": "Bochs",
	"0x15ad": "VMware",
	"0x1414": "Microsoft",
}

// collectGPUs 枚举 /sys/class/drm 下的 card* 设备。
//
// 型号优先取 /proc/driver/nvidia/gpus/<bdf>/information 的 Model（NVIDIA 专有接口），
// 否则退化到 PCI 厂商名；取不到型号与厂商时显示「未知厂商」。
func collectGPUs() []gpuInfo {
	entries, err := os.ReadDir(drmClassDir)
	if err != nil {
		return nil
	}
	models := nvidiaModels()
	var out []gpuInfo
	for _, e := range entries {
		if !isCardDir(e.Name()) {
			continue
		}
		devDir := filepath.Join(drmClassDir, e.Name(), "device")
		vendor := readTrim(filepath.Join(devDir, "vendor"))
		device := readTrim(filepath.Join(devDir, "device"))
		if vendor == "" {
			vendor = "未知"
		}
		if device == "" {
			device = "未知"
		}
		name := models[pciSlotName(devDir)]
		if name == "" {
			name = pciVendors[vendor]
		}
		if name == "" {
			name = "未知厂商"
		}
		out = append(out, gpuInfo{name: name, vendorID: vendor, deviceID: device})
	}
	return out
}

// isCardDir 判断 DRM 目录名是否是主设备（card0、card1…），排除 card0-DP-1 等连接器。
func isCardDir(name string) bool {
	rest, ok := strings.CutPrefix(name, "card")
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// nvidiaModels 读取 /proc/driver/nvidia/gpus/<bdf>/information，返回 BDF → 型号。
func nvidiaModels() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(nvidiaGPUDirPath)
	if err != nil {
		return out
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(nvidiaGPUDirPath, e.Name(), "information"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, val, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(key) == "Model" {
				if model := strings.TrimSpace(val); model != "" {
					out[e.Name()] = model
				}
				break
			}
		}
	}
	return out
}

// pciSlotName 从设备的 uevent 读取 PCI BDF 地址（如 0000:64:00.0）。
func pciSlotName(devDir string) string {
	data, err := os.ReadFile(filepath.Join(devDir, "uevent"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "PCI_SLOT_NAME="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// readTrim 读取文件并去除首尾空白，失败返回空串。
func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
