package store_test

import (
	"flag"
	"log"
	"os"
	"strconv"
	"testing"
)

// storeTestParallelCap is the most t.Parallel tests this package runs at
// once by default. Every store test drives modernc.org/sqlite, whose libc
// (modernc.org/libc v1.73.4: mem.go on darwin, mem_musl.go on linux)
// serializes each malloc and free under one process-wide mutex, so the
// package gets slower as parallelism rises. Measured on a loaded 10-core
// Mac: -parallel 10 took 114.7 s wall, 5 took 76.5 s, 3 took 54.9 s; under
// GOMAXPROCS=4 (CI-like), 4 took 53.1 s and 2 took 47.6 s. Concurrent
// in-memory migration chains peaked at 2 goroutines. Issue #2744 has the
// full record.
const storeTestParallelCap = 2

// TestMain lowers the default -test.parallel (GOMAXPROCS) to
// storeTestParallelCap and then runs every test. It never raises the
// value, so a GOMAXPROCS below the cap keeps Go's default, and it leaves a
// -parallel given on the command line alone, whatever its value. flag.Visit
// detects that case because it visits only flags that were set, which a
// comparison with the default could not tell apart from an explicit
// -parallel equal to GOMAXPROCS.
func TestMain(m *testing.M) {
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.parallel" {
			explicit = true
		}
	})
	if !explicit {
		parallel := flag.Lookup("test.parallel").Value
		current, err := strconv.Atoi(parallel.String())
		if err != nil {
			log.Fatalf("read test.parallel: %v", err)
		}
		if current > storeTestParallelCap {
			if err := parallel.Set(strconv.Itoa(storeTestParallelCap)); err != nil {
				log.Fatalf("cap test.parallel: %v", err)
			}
		}
	}
	os.Exit(m.Run())
}
