package main

import (
	"strings"
	"testing"
)

func TestDroppedModes(t *testing.T) {
	const body = `
services:
  claude:
    volumes:
      - ./workspace:/workspace:read=cached,write=back
      - ./a:/a:cached
      - ./b:/b:ro,delegated
      - 'C:\Users\alice\src:/w:read=cached'
      - ./plain:/p
      - ./ro:/r:ro
      - data:/d
      - data:/e:ro
      - ./c:/c:ro,owner=1000.2000
      - /only-target
      - type: bind
        source: ./workspace
        target: /long
        consistency: read=cached,write=back
`
	got := droppedModes([]byte(body))
	want := []string{
		`compose drops "read=cached,write=back" from ./workspace:/workspace (service claude), so it mounts without them`,
		`  fix: use the long syntax with consistency: read=cached,write=back and bind: {create_host_path: false}`,
		`compose drops "cached" from ./a:/a (service claude), so it mounts without them`,
		`  fix: use the long syntax with consistency: cached and bind: {create_host_path: false}`,
		`compose drops "delegated" from ./b:/b (service claude), so it mounts without them`,
		`  fix: use the long syntax with consistency: delegated and bind: {create_host_path: false}`,
		`compose drops "read=cached" from C:\Users\alice\src:/w (service claude), so it mounts without them`,
		`  fix: use the long syntax with consistency: read=cached and bind: {create_host_path: false}`,
		`compose drops "owner=1000.2000" from ./c:/c (service claude), so it mounts without them`,
		`  fix: use the long syntax with consistency: owner=1000.2000 and bind: {create_host_path: false}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDroppedModesQuietOnUnreadable(t *testing.T) {
	if got := droppedModes([]byte("services: [")); got != nil {
		t.Errorf("got %v, want nothing", got)
	}
}
