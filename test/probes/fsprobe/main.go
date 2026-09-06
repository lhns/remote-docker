// Command fsprobe runs one fixed sequence of filesystem operations against
// one directory and prints a transcript of what each did, one line per
// operation, so the same binary run against a native bind mount and against
// an NFS share can be diffed line by line.
//
// It runs inside a Linux container, as root, and is built like the other
// probes: CGO_ENABLED=0 GOOS=linux go build. Anything needing a second
// process (a lock held by somebody else, a mapping written from outside,
// concurrent appenders) re-executes this binary in the hidden `--child` mode.
//
// Usage: fsprobe [--group NAME[,NAME...]] DIR
//
// Every group runs in DIR/fsprobe/<group>, created fresh and removed at the
// end. Each step prints
//
//	<group>/<step>: <op> -> <result> [<stat>]
//
// where <result> is `ok`, `ok:<value>` for a step that produces data, or the
// errno name (`E<number>` when unnamed). A step that panics prints
// `-> PANIC:<message>` and the run continues. Nothing printed contains DIR, an
// inode number, a device or a timestamp: stat.go normalises those, so two
// transcripts differ only where the filesystems behave differently. A refused
// operation is a transcript line, not a failure; the exit code is non-zero
// only when the program itself is broken.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) > 2 && os.Args[1] == "--child" {
		os.Exit(childMain(os.Args[2], os.Args[3:]))
	}

	groups := flag.String("group", "", "comma-separated group names (default: all)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: fsprobe [--group NAME[,NAME...]] DIR")
		os.Exit(2)
	}

	var names []string
	if *groups != "" {
		names = strings.Split(*groups, ",")
	}
	if err := run(flag.Arg(0), names, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fsprobe:", err)
		os.Exit(1)
	}
}
