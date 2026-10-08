// seal_results writes SHA256SUMS for a results directory assembled outside
// cmd/evidence, such as the GCP studies, so it is held to the same rule as
// every other result: any edit after sealing fails validation.
package main

import (
	"fmt"
	"os"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: seal_results DIR")
		os.Exit(2)
	}
	if err := evidence.WriteManifest(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("sealed", os.Args[1])
}
