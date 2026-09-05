//go:build !linux

package union

// mountedAt cannot be answered off Linux, where none of this runs, and answers
// false rather than "the path exists": a union that never mounted looks exactly
// like one that did, and a stub saying otherwise would make it look alive here.
func mountedAt(string) bool { return false }
