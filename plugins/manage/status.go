package manage

import (
	"fmt"
	"strings"
)

// hostInfo 是 /manage status 采集到的主机硬件快照。
//
// 采集实现按操作系统分文件：Linux 见 status_linux.go（读 /proc、/sys），
// 其它平台见 status_other.go（返回不支持）。本文件只定义数据结构与渲染，
// 与操作系统无关，因此可在任意平台编译与测试。
type hostInfo struct {
	cpuModel     string  // CPU 型号，未知为空
	cpuPhysical  int     // 物理核心数，未知为 0
	cpuLogical   int     // 逻辑处理器数，未知为 0
	cpuUsage     float64 // 整体使用率 0..1，负值表示不可用
	load1        float64 // 1 分钟平均负载
	load5        float64 // 5 分钟平均负载
	load15       float64 // 15 分钟平均负载
	memTotal     uint64
	memAvailable uint64
	swapTotal    uint64
	swapFree     uint64
	disks        []diskInfo
	gpus         []gpuInfo
}

// diskInfo 描述一个块设备的占用情况。
//
// 同一设备的多处挂载（例如 btrfs 子卷）合并为一行：mountpoint 是最短的挂载点，
// extraMounts 是其余挂载点数量。
type diskInfo struct {
	device      string
	fstype      string
	mountpoint  string
	extraMounts int
	total       uint64
	used        uint64
	available   uint64
}

// gpuInfo 描述一块 DRM 显卡。
type gpuInfo struct {
	name     string // 型号或厂商名，取不到时为「未知厂商」
	vendorID string // PCI 厂商 ID，形如 0x10de
	deviceID string // PCI 设备 ID
}

// formatHostInfo 把主机快照渲染为 /manage status 的多行文本。
func formatHostInfo(h hostInfo) string {
	var b strings.Builder
	b.WriteString("主机状态：")

	model := h.cpuModel
	if model == "" {
		model = "未知型号"
	}
	switch {
	case h.cpuPhysical > 0 && h.cpuLogical > 0:
		fmt.Fprintf(&b, "\nCPU：%s（%d 核 %d 线程）", model, h.cpuPhysical, h.cpuLogical)
	case h.cpuLogical > 0:
		fmt.Fprintf(&b, "\nCPU：%s（%d 逻辑处理器）", model, h.cpuLogical)
	default:
		fmt.Fprintf(&b, "\nCPU：%s", model)
	}
	if h.cpuUsage >= 0 {
		fmt.Fprintf(&b, "\nCPU 使用率：%.1f%% · 负载 %.2f / %.2f / %.2f（1/5/15 分钟）",
			h.cpuUsage*100, h.load1, h.load5, h.load15)
	} else {
		fmt.Fprintf(&b, "\nCPU 使用率：不可用 · 负载 %.2f / %.2f / %.2f（1/5/15 分钟）",
			h.load1, h.load5, h.load15)
	}

	if h.memTotal > 0 {
		used := h.memTotal - min(h.memAvailable, h.memTotal)
		fmt.Fprintf(&b, "\n内存：已用 %s / %s（%.1f%%）· 可用 %s",
			formatBytes(used), formatBytes(h.memTotal), percent(used, h.memTotal), formatBytes(h.memAvailable))
	} else {
		b.WriteString("\n内存：不可用")
	}
	if h.swapTotal > 0 {
		used := h.swapTotal - min(h.swapFree, h.swapTotal)
		fmt.Fprintf(&b, "\n交换：已用 %s / %s（%.1f%%）",
			formatBytes(used), formatBytes(h.swapTotal), percent(used, h.swapTotal))
	} else {
		b.WriteString("\n交换：无")
	}

	if len(h.disks) == 0 {
		b.WriteString("\n磁盘：无可用信息")
	} else {
		b.WriteString("\n磁盘：")
		for _, d := range h.disks {
			fmt.Fprintf(&b, "\n- %s（%s）已用 %s / %s（%.1f%%）· 挂载 %s",
				d.device, d.fstype, formatBytes(d.used), formatBytes(d.total),
				percent(d.used, d.used+d.available), d.mountpoint)
			if d.extraMounts > 0 {
				fmt.Fprintf(&b, " 等 %d 处", d.extraMounts+1)
			}
		}
	}

	if len(h.gpus) == 0 {
		b.WriteString("\nGPU：未检测到")
	} else {
		b.WriteString("\nGPU：")
		for _, g := range h.gpus {
			fmt.Fprintf(&b, "\n- %s（%s:%s）", g.name, g.vendorID, g.deviceID)
		}
	}

	return b.String()
}

// formatBytes 把字节数格式化为 1 位小数的 KiB/MiB/GiB/TiB/PiB。
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// percent 返回 part 占 total 的百分比（0..100），total 为 0 时返回 0。
func percent(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
