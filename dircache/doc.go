// Package dircache keeps a local cache of a directory tree coherent in both
// directions: prefetch what the consumer reads, within a budget, invalidate
// what changes here, and carry the consumer's writes back.
//
// That sentence names no transport and no storage, which is the test for what
// belongs here: both are behind Store (ADR 0044). The module depends on
// nothing, this repository included; types.go declares what it needs and a
// caller converts (client/internal/session/cacheobserver.go).
//
// The cache may be incomplete at every moment. A file it does not hold is
// served from the tree underneath, so a budget, a running walk or an exclusion
// makes a share slower and never wrong.
package dircache
