package main

// Prints what it can see under /data and /data/inner once a second. Used to
// determine exactly which host-side mount events reach a running container.

import (
	"fmt"
	"os"
	"time"
)

func list(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "ERR"
	}
	if len(entries) == 0 {
		return "<empty>"
	}
	names := ""
	for _, e := range entries {
		names += e.Name() + ","
	}
	return names
}

func main() {
	for i := 0; i < 60; i++ {
		fmt.Printf("tick=%d data=%s inner=%s\n", i, list("/data"), list("/data/inner"))
		os.Stdout.Sync()
		time.Sleep(time.Second)
	}
}
