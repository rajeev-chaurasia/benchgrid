package main

import (
	"flag"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"time"
)

// stress is the noise source for the noise run: bursts of every core spinning,
// separated by idle gaps, both of random length from a fixed seed so two runs
// of the harness inject the same pattern. Bursty rather than constant,
// because constant load is what a preflight check catches, and the question
// is what gets through after it.
func stress(args []string) {
	fs := flag.NewFlagSet("stress", flag.ExitOnError)
	seed := fs.Uint64("seed", 1, "rng seed")
	fs.Parse(args)
	rng := rand.New(rand.NewPCG(*seed, 0))
	var on atomic.Bool
	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			x := uint64(1)
			for {
				if on.Load() {
					for j := 0; j < 1e5; j++ {
						x = x*6364136223846793005 + 1442695040888963407
					}
				} else {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}
	for {
		on.Store(true)
		time.Sleep(time.Duration(300+rng.IntN(1200)) * time.Millisecond)
		on.Store(false)
		time.Sleep(time.Duration(300+rng.IntN(1200)) * time.Millisecond)
	}
}
