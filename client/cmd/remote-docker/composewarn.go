package main

// Compose's short volume syntax cannot carry our mount mode words (ADR 0042).

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/lhns/remote-docker/core/workspace"
)

// warnModes chains warnDroppedModes after compose's own pre-run, which has
// read the flags and .env by then.
func warnModes(sub *cobra.Command) {
	pre := sub.PersistentPreRunE
	sub.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if pre != nil {
			if err := pre(cmd, args); err != nil {
				return err
			}
		}
		if opts, err := projectOptions(sub); err == nil {
			warnDroppedModes(cmd.ErrOrStderr(), opts)
		}
		return nil
	}
}

// warnDroppedModes tells the person which of the project's short-syntax
// volumes carry a mode word compose is about to discard. It reads the files'
// raw YAML because LoadModel has already turned every short volume into the
// long form, options dropped. Includes and extends are not followed. A scan
// that cannot read the files says nothing: compose reports its own errors.
func warnDroppedModes(w io.Writer, opts *cli.ProjectOptions) {
	dir, err := opts.GetWorkingDir()
	if err != nil {
		return
	}
	details, err := opts.ReadConfigFiles(context.Background(), dir, opts)
	if err != nil {
		return
	}
	for _, f := range details.ConfigFiles {
		for _, line := range droppedModes(f.Content) {
			_, _ = fmt.Fprintln(w, line)
		}
	}
}

// droppedModes returns the warning for each short-syntax volume in one compose
// file that carries a mode word, as two lines each (diagnosis and fix). The
// fix names create_host_path: compose sends a bind with no advanced option as a
// plain Binds string and drops consistency there too, so only a bind that needs
// the Mount API (this one, or propagation) carries it to the daemon.
func droppedModes(content []byte) []string {
	var file struct {
		Services map[string]struct {
			Volumes []any `yaml:"volumes"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(content, &file) != nil {
		return nil
	}
	names := make([]string, 0, len(file.Services))
	for name := range file.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []string
	for _, name := range names {
		for _, v := range file.Services[name].Volumes {
			spec, ok := v.(string)
			if !ok {
				continue // long syntax carries consistency: itself
			}
			// Options follow the last colon and hold no slash, which a
			// target path or a Windows source (C:\x:/w) always does.
			i := strings.LastIndex(spec, ":")
			if i < 0 || strings.Contains(spec[i+1:], "/") {
				continue
			}
			var words []string
			for _, opt := range strings.Split(spec[i+1:], ",") {
				if workspace.IsModeWord(opt) {
					words = append(words, strings.TrimSpace(opt))
				}
			}
			if len(words) == 0 {
				continue
			}
			mode := strings.Join(words, ",")
			out = append(out,
				fmt.Sprintf("compose drops %q from %s (service %s), so it mounts without them", mode, spec[:i], name),
				"  fix: use the long syntax with consistency: "+mode+" and bind: {create_host_path: false}")
		}
	}
	return out
}
