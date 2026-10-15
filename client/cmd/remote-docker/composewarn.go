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

// droppedModes returns the warning for each volume in one compose file whose
// mode words compose discards, as two lines each (diagnosis and fix): every
// short-syntax one, and a long-syntax bind that compose sends as a plain Binds
// string, which drops consistency too. Only a bind needing the Mount API
// carries it (compose's bindRequiresMountAPI, v5.6.0).
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
			if long, ok := v.(map[string]any); ok {
				out = append(out, droppedLong(name, long)...)
				continue
			}
			spec, ok := v.(string)
			if !ok {
				continue
			}
			// Options follow the last colon and hold no slash, which a
			// target path or a Windows source (C:\x:/w) always does.
			i := strings.LastIndex(spec, ":")
			if i < 0 || strings.Contains(spec[i+1:], "/") {
				continue
			}
			mode := modeWords(spec[i+1:])
			if mode == "" {
				continue
			}
			out = append(out,
				fmt.Sprintf("compose drops %q from %s (service %s), so it mounts without them", mode, spec[:i], name),
				"  fix: use the long syntax with consistency: "+mode+" and bind: {create_host_path: false}")
		}
	}
	return out
}

// droppedLong is droppedModes for one long-syntax volume.
func droppedLong(service string, v map[string]any) []string {
	consistency, _ := v["consistency"].(string)
	mode := modeWords(consistency)
	if v["type"] != "bind" || mode == "" {
		return nil
	}
	if bind, _ := v["bind"].(map[string]any); bind != nil {
		if create, ok := bind["create_host_path"].(bool); ok && !create {
			return nil
		}
		if bind["propagation"] != nil || bind["recursive"] != nil {
			return nil
		}
	}
	return []string{
		fmt.Sprintf("compose drops %q from %v:%v (service %s), so it mounts without them", mode, v["source"], v["target"], service),
		"  fix: add bind: {create_host_path: false} to that volume",
	}
}

// modeWords keeps the comma-separated words that are ours or Docker's.
func modeWords(list string) string {
	var words []string
	for _, opt := range strings.Split(list, ",") {
		if workspace.IsModeWord(opt) {
			words = append(words, strings.TrimSpace(opt))
		}
	}
	return strings.Join(words, ",")
}
