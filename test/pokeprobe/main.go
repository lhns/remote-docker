// Command pokeprobe performs one minimal syscall against one path and says
// what happened. It is the other half of the ADR 0014 experiment: watchprobe
// reports which inotify events a container sees, and pokeprobe is what the
// workspace agent would do to produce them.
//
// Linux cannot inject a synthetic inotify event; fanotify(7) says so outright.
// The only mechanism available to anyone, Docker Desktop included, is to
// perform a real VFS operation and let the kernel emit the event. So the
// question is which real operation is cheap, non-destructive, and produces the
// event watchers key on.
//
// This binary answers it, one primitive per invocation, writing no bytes and
// changing nothing observable:
//
//	openclose  open(O_WRONLY) + close, never O_TRUNC
//	mtime      utimensat with atime=UTIME_OMIT and mtime set to its own
//	           current value
//	touch      utimensat with both times set -- the naive version, included
//	           as a control because it produces a DIFFERENT event
//	create     open(O_WRONLY|O_CREAT)
//	unlink     unlink
//	dirmtime   mtime, applied to the parent directory
//	stat       no poke at all; reports st_dev and st_ino
//
// Usage: pokeprobe <primitive> <path>
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: pokeprobe <primitive> <path>")
		os.Exit(2)
	}
	primitive, path := os.Args[1], os.Args[2]

	detail, err := poke(primitive, path)
	if err != nil {
		fmt.Printf("POKE %s %s error=%v\n", primitive, path, err)
		os.Exit(1)
	}
	fmt.Printf("POKE %s %s ok %s\n", primitive, path, detail)
}
