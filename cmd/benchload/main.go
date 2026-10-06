// benchload is the benchmark the evidence runs measure: a fixed amount of
// SHA-256 work with knobs for every way a real benchmark misbehaves. It is
// deliberately boring, because the subject of every measurement here is the
// harness around it, not the workload.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	rounds := flag.Int("rounds", 20000, "sha256 rounds over a 1 KiB block")
	sleep := flag.Duration("sleep", 0, "sleep after the work, for runs that must last")
	crash := flag.Bool("crash", false, "die by a signal after the work")
	exit := flag.Int("exit", 0, "exit with this code after the work")
	hang := flag.Bool("hang", false, "ignore SIGTERM and never finish")
	orphan := flag.Bool("orphan", false, "leave a child running that ignores SIGTERM")
	flag.Parse()

	if *hang {
		signal.Ignore(syscall.SIGTERM)
	}
	if *orphan {
		child := exec.Command(os.Args[0], "-hang")
		child.Start()
	}

	block := make([]byte, 1024)
	start := time.Now()
	var sum [32]byte
	for i := 0; i < *rounds; i++ {
		sum = sha256.Sum256(block)
		block[0] = sum[0]
	}
	elapsed := time.Since(start)
	fmt.Printf("BENCHGRID_METRIC throughput %.3f\n", float64(*rounds)/elapsed.Seconds())
	fmt.Printf("BENCHGRID_METRIC work_ns %d\n", elapsed.Nanoseconds())

	if *hang {
		// A bare select{} would be detected as a deadlock and exit 2.
		for {
			time.Sleep(time.Hour)
		}
	}
	time.Sleep(*sleep)
	if *crash {
		// The Go runtime turns a SIGSEGV sent by kill into exit status 2, which
		// would look like an ordinary failure. SIGKILL cannot be intercepted.
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Second)
	}
	os.Exit(*exit)
}
