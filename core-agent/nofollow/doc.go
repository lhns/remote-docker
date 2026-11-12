// Package nofollow resolves a path below a trusted directory without following
// a symbolic link in any component of it. Linux only; see open_linux.go.
//
// For paths an account controls: it is root inside its own daemon's filesystem
// and can put a link anywhere in it, so a path joined onto /proc/<pid>/root is
// not contained by being joined.
package nofollow
