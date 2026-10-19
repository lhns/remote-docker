// Package prune decides what the agent removes from a daemon: images nothing
// has used for a while, and stopped containers when asked (ADR 0056). It is
// pure: listings, the record of when each image was last in use, and the time
// go in; what to remove comes out. Running docker is dockercli's job.
package prune

import (
	"slices"
	"strings"
	"time"
)

// Image is an image as a daemon lists it.
type Image struct {
	ID      string
	Tags    []string // repo:tag
	Digests []string // repo@sha256:...
	Size    int64
}

// Refs is what to remove to remove the image: every tag, or every digest
// reference for an image with no tag, or its id. Never `rmi -f`: without it
// Docker refuses an image a stopped container still uses, which is the whole
// safety of removing by reference.
func (i Image) Refs() []string {
	switch {
	case len(i.Tags) > 0:
		return i.Tags
	case len(i.Digests) > 0:
		return i.Digests
	}
	return []string{i.ID}
}

// Container is a container as a daemon lists it.
type Container struct {
	ID      string
	Name    string
	ImageID string
	// Status is the daemon's State.Status: created, running, paused,
	// restarting, removing, exited or dead.
	Status   string
	Created  time.Time
	Finished time.Time // zero for a container that never ran
}

// Stopped is a container that runs nothing and will not by itself.
func (c Container) Stopped() bool {
	return c.Status == "exited" || c.Status == "dead" || c.Status == "created"
}

// Since is when a stopped container stopped: its finish time, or its creation
// for one that never started.
func (c Container) Since() time.Time {
	if c.Finished.IsZero() || c.Finished.Year() <= 1 {
		return c.Created
	}
	return c.Finished
}

// Resolve names the image a reference, as typed in `docker run`, means: a tag
// with or without :latest, a digest reference, or an id in full or by prefix.
func Resolve(images []Image, name string) (string, bool) {
	name = strings.TrimPrefix(name, "docker.io/library/")
	name = strings.TrimPrefix(name, "docker.io/")
	withLatest := name
	if !strings.ContainsAny(name, ":@") {
		withLatest = name + ":latest"
	}
	bare := strings.TrimPrefix(name, "sha256:")
	for _, img := range images {
		for _, t := range img.Tags {
			t = strings.TrimPrefix(t, "docker.io/library/")
			if t == name || t == withLatest {
				return img.ID, true
			}
		}
		if slices.Contains(img.Digests, name) {
			return img.ID, true
		}
	}
	if len(bare) >= 4 && isHex(bare) {
		for _, img := range images {
			if strings.HasPrefix(strings.TrimPrefix(img.ID, "sha256:"), bare) {
				return img.ID, true
			}
		}
	}
	return "", false
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Used is every image a container references, running or not.
func Used(containers []Container) map[string]bool {
	used := map[string]bool{}
	for _, c := range containers {
		used[c.ImageID] = true
	}
	return used
}

// Candidate is an image that may go, and when it was last in use.
type Candidate struct {
	Image Image
	Seen  time.Time
}

// unused lists the images no container references and the caller did not
// protect, oldest last-seen first.
func unused(rec *Record, images []Image, containers []Container, protected map[string]bool, now time.Time) []Candidate {
	used := Used(containers)
	var out []Candidate
	for _, img := range images {
		if used[img.ID] || protected[img.ID] {
			continue
		}
		out = append(out, Candidate{Image: img, Seen: rec.LastSeen(img.ID, now)})
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return a.Seen.Compare(b.Seen) })
	return out
}

// AgeImages is every unused image last in use longer ago than after.
func AgeImages(rec *Record, images []Image, containers []Container, protected map[string]bool, after time.Duration, now time.Time) []Candidate {
	return slices.DeleteFunc(unused(rec, images, containers, protected, now), func(c Candidate) bool {
		return now.Sub(c.Seen) <= after
	})
}

// PressureImages is every unused image not seen within floor, oldest first. An
// image just pulled for a build in progress is inside the floor, and stays.
func PressureImages(rec *Record, images []Image, containers []Container, protected map[string]bool, floor time.Duration, now time.Time) []Candidate {
	return slices.DeleteFunc(unused(rec, images, containers, protected, now), func(c Candidate) bool {
		return now.Sub(c.Seen) < floor
	})
}

// AgeContainers is every stopped container that stopped longer ago than after.
// A container named rd-* is the agent's own and is never one.
func AgeContainers(containers []Container, after time.Duration, now time.Time) []Container {
	var out []Container
	for _, c := range containers {
		if c.Stopped() && !strings.HasPrefix(strings.TrimPrefix(c.Name, "/"), "rd-") && now.Sub(c.Since()) > after {
			out = append(out, c)
		}
	}
	return out
}
