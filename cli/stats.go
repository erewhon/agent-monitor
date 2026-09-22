package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// runStats prints the outer tmux status-bar segment: a load sparkline,
// RAM and root-disk gauges, with tmux colour codes. A port of the former
// agent-monitor-stats shell script; history lives in the temp dir.
func runStats() error {
	load, err := loadAvg()
	if err != nil {
		return err
	}
	hist := appendHistory(filepath.Join(os.TempDir(), "agent-monitor-load-history"), load, 10)
	spark := sparkline(hist, float64(runtime.NumCPU()))
	memPct, _ := memUsedPct()
	diskPct, _ := diskUsedPct("/")
	fmt.Printf("#[fg=#b388ff]%s %.2f #[fg=#6677aa]│ #[fg=#6677aa]RAM #[fg=#9933ff]%s #[fg=#b84dff]%d%% #[fg=#6677aa]│ #[fg=#6677aa]/ #[fg=#9933ff]%s #[fg=#b84dff]%d%%",
		spark, load, barGauge(memPct, 6), memPct, barGauge(diskPct, 6), diskPct)
	return nil
}

func loadAvg() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("empty /proc/loadavg")
	}
	return strconv.ParseFloat(f[0], 64)
}

func appendHistory(path string, v float64, keep int) []float64 {
	var vals []float64
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if x, err := strconv.ParseFloat(strings.TrimSpace(sc.Text()), 64); err == nil {
				vals = append(vals, x)
			}
		}
		f.Close()
	}
	vals = append(vals, v)
	if len(vals) > keep {
		vals = vals[len(vals)-keep:]
	}
	var b strings.Builder
	for _, x := range vals {
		fmt.Fprintf(&b, "%.2f\n", x)
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
	return vals
}

// sparkline scales readings against max(readings, ncpu/8, 4) so typical
// loads show variation instead of flat-lining.
func sparkline(vals []float64, ncpu float64) string {
	chars := []rune("▁▂▃▄▅▆▇█")
	scale := ncpu / 8
	if scale < 4 {
		scale = 4
	}
	for _, v := range vals {
		if v > scale {
			scale = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		i := int(v / scale * 7)
		if i > 7 {
			i = 7
		}
		if i < 0 {
			i = 0
		}
		b.WriteRune(chars[i])
	}
	return b.String()
}

func barGauge(pct, n int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * n / 100
	return strings.Repeat("█", filled) + strings.Repeat("░", n-filled)
}

func memUsedPct() (int, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	var total, avail int64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("no MemTotal")
	}
	return int((total - avail) * 100 / total), nil
}

func diskUsedPct(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return 0, fmt.Errorf("zero-size filesystem")
	}
	return int((total - free) * 100 / total), nil
}
