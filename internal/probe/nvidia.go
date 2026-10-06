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

// parseNvidia reads the first GPU only. A multi-GPU rig is advertised by its
// first device, which is a known gap recorded in docs/known-misses.md.
func parseNvidia(out string) (nvidiaGPU, bool) {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	f := strings.Split(line, ",")
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
	return nvidiaGPU{
		Name: f[0], Driver: f[1],
		Util: util / 100, TempC: temp,
		MemoryTotalBytes: mib << 20,
	}, true
}
