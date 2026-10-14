package session

// OurCommand renders one of our commands as the reader would type it, for a
// message. The binary may be renamed (to docker, for instance), which only the
// program's entry point can tell, so it replaces this; the default is the
// binary's own name.
var OurCommand = func(command string) string { return "remote-docker remote " + command }
