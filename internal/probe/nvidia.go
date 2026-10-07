package probe

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type nvidiaGPU struct {
	Name             string
	Driver           string
	Util             float64
	TempC            float64
	MemoryTotalBytes int64
	Count            int
}

const nvidiaQuery = "--query-gpu=name,driver_version,utilization.gpu,temperature.gpu,memory.total"

func queryNvidia(ctx context.Context) (nvidiaGPU, bool) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nvidiaGPU{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", nvidiaQuery, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nvidiaGPU{}, false
	}
	return parseNvidia(string(out))
}

// parseNvidia reads every GPU. The rig advertises the first device's model
// and driver, the smallest memory of any device, and the count, because a
// spec that needs 16 GB per device is not satisfied by one 24 GB device and
// one 8 GB device. Utilization and temperature are the highest of any device,
// because the gate exists to refuse a rig where anything is busy or hot.
func parseNvidia(out string) (nvidiaGPU, bool) {
	var g nvidiaGPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 5 {
			return nvidiaGPU{}, false
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		util, err1 := strconv.ParseFloat(f[2], 64)
		temp, err2 := strconv.ParseFloat(f[3], 64)
		mib, err3 := strconv.ParseInt(f[4], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return nvidiaGPU{}, false
		}
		mem := mib << 20
		if g.Count == 0 {
			g = nvidiaGPU{Name: f[0], Driver: f[1], Util: util / 100, TempC: temp, MemoryTotalBytes: mem}
		}
		g.Count++
		g.Util = max(g.Util, util/100)
		g.TempC = max(g.TempC, temp)
		g.MemoryTotalBytes = min(g.MemoryTotalBytes, mem)
	}
	return g, g.Count > 0
}
