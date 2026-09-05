package nfsserve

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

var (
	modelSeed   = flag.Int64("nfs.seed", 0, "seed for TestModelRandomOperations; 0 draws one from the clock")
	modelSteps  = flag.Int("nfs.steps", 0, "operations for TestModelRandomOperations; 0 is the platform default")
	modelReplay = flag.Int("nfs.replay", -1, "run TestModelRandomOperations with -nfs.seed through this step, inclusive, and stop; the number is the one a failure names")
)

// defaultSteps is how many operations one run makes: enough to reach every
// pairing below several times. Windows gets a fifth: an RPC there costs
// 4-10 ms (go-billy's BoundOS runs EvalSymlinks on every operation, and
// inodeOf opens the file), which is 100 ms a step, and the budget is ten
// seconds. Measured 2026-09-05 on this project's development machine; the
// step count and the time are in the log line either way.
func defaultSteps() int {
	if runtime.GOOS == "windows" {
		return 80
	}
	return 400
}

const (
	// fullCheckEvery is how often the WHOLE tree is held against the shadow.
	// Every step checks what it touched; a whole-tree pass costs one RPC per
	// handle, listing and file, and on Windows go-billy's BoundOS runs
	// EvalSymlinks on every one of those (measured at 4-10 ms an RPC), so it
	// cannot be every step and stay under ten seconds.
	fullCheckEvery = 40

	// maxEntries is where the sequence starts leaning on removals.
	maxEntries = 30

	// historyLen is how many steps a failure repeats, so the operations that
	// led to it are in one place rather than scattered through the log.
	historyLen = 20
)

// A random sequence of operations against a share, checked against a shadow
// of what the tree should hold.
//
// The rules the single-purpose tests pin one at a time have to hold TOGETHER,
// after any order of operations, and that is where they fail silently: a
// fileid that survives a lookup but not the rename before it, a handle that
// resolves until the directory above it is renamed, a listing that agrees with
// the shadow until a name is reused. Each of those is a client marking an
// inode stale (ADR 0033) or a container reading a directory that is not the
// one on disk, with the server reporting no error at all. The seed is logged
// so a failure is replayed with -nfs.seed.
//
// A failure names a step, and -nfs.replay=N runs the seed through that step
// and no further, where -nfs.steps=N would stop one short of it. Every step
// logs one line, which Go prints only for a failing test, and a failure
// repeats the last historyLen of them. The sequence a seed produces depends
// on the platform, because the names drawn and the operations weighted differ
// on Windows, so a seed from CI replays on Linux only.
func TestModelRandomOperations(t *testing.T) {
	seed := *modelSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	steps := *modelSteps
	if steps == 0 {
		steps = defaultSteps()
	}
	if *modelReplay >= 0 {
		if *modelSeed == 0 {
			t.Fatal("-nfs.replay needs the -nfs.seed the failure printed")
		}
		steps = *modelReplay + 1
		t.Logf("replaying seed %d through step %d", seed, *modelReplay)
	} else {
		t.Logf("seed %d, %d steps", seed, steps)
	}

	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	client, root, err := mountRaw(t, serve(t, r), "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	target, err := nfsclient.NewTargetWithClient(client, rpc.AuthNull, root, "/cwd", 0)
	if err != nil {
		t.Fatal(err)
	}

	m := &model{
		t:       t,
		client:  client,
		target:  target,
		rng:     rand.New(rand.NewSource(seed)),
		entries: map[string]*modelEntry{"": {kind: kindDir}},
		touched: map[string]bool{},
	}
	m.note("", 0, root)

	start := time.Now()
	for step := range steps {
		op := m.chooseOp()
		m.step, m.desc = step, ""
		m.ops[op]++
		clear(m.touched)
		failedBefore := t.Failed()
		m.run(op)
		m.trace(op, failedBefore)
		if t.Failed() {
			m.fatalf("stopping after step %d (%s); replay with -nfs.seed=%d -nfs.replay=%d", step, opNames[op], seed, step)
		}
		if (step+1)%fullCheckEvery == 0 || step == steps-1 {
			m.checkAll()
		} else {
			m.checkTouched()
		}
		if t.Failed() {
			m.fatalf("the tree disagrees with the shadow after step %d (%s); replay with -nfs.seed=%d -nfs.replay=%d", step, opNames[op], seed, step)
		}
	}
	t.Logf("%d steps in %v, %d entries at the end, ops %v", steps, time.Since(start), len(m.entries), m.ops)
}

type entryKind int

const (
	kindFile entryKind = iota
	kindDir
	kindLink
)

var (
	kindTypes = map[entryKind]uint32{kindFile: nfsclient.NF3Reg, kindDir: nfsclient.NF3Dir, kindLink: nfsclient.NF3Lnk}
	kindNames = map[entryKind]string{kindFile: "file", kindDir: "dir", kindLink: "link"}
)

// typeName spells a wire type the way the shadow spells its kind, so the two
// sides of a diagnosis read alike.
func typeName(t uint32) string {
	for kind, wire := range kindTypes {
		if wire == t {
			return kindNames[kind]
		}
	}
	return fmt.Sprintf("type %d", t)
}

type modelEntry struct {
	kind    entryKind
	content []byte
	link    string // what a symlink points at

	// fileid is the number the wire reported the first time this entry was
	// seen; every later report has to repeat it. seen says whether it has.
	fileid uint64
	seen   bool

	// handles are what the server issued for this path, for as long as this
	// entry lives at it. A rename moves the entry and drops them: a handle
	// names a path in go-nfs, so the old ones are expected to go stale.
	// go-nfs's caching handler answers one path with one handle, so this
	// rarely holds more than one, and is capped so a check stays one RPC.
	handles [][]byte
}

type modelOp int

const (
	opCreate modelOp = iota
	opWrite
	opMkdir
	opRename
	opRemove
	opRmdir
	opSymlink
	opSetattr
	opLookup
	opReaddir
	opLink
	opCount
)

var opNames = [opCount]string{"CREATE", "WRITE", "MKDIR", "RENAME", "REMOVE", "RMDIR", "SYMLINK", "SETATTR", "LOOKUP", "READDIRPLUS", "LINK"}

type model struct {
	t      *testing.T
	client *rpc.Client
	target *nfsclient.Target
	rng    *rand.Rand
	step   int
	ops    [opCount]int

	// entries is keyed by share-relative path with forward slashes; "" is
	// the root.
	entries map[string]*modelEntry

	// touched is what the current step changed or asked about, and so what
	// is checked before the next one.
	touched map[string]bool

	// desc is what the current step decided to do, set once the choice is
	// made and before the wire is asked, so a step that fails mid-way still
	// names its arguments. history is the last historyLen trace lines.
	desc    string
	history []string
}

// trace logs the step as one line and keeps it for a failure to repeat.
// "ok" means the operation itself did not fail; a check that follows reports
// separately, under it.
func (m *model) trace(op modelOp, failedBefore bool) {
	desc := m.desc
	if desc == "" {
		desc = opNames[op] + " skipped, nothing to choose"
	}
	result := "ok"
	if m.t.Failed() && !failedBefore {
		result = "FAILED"
	}
	line := fmt.Sprintf("step %d: %s -> %s", m.step, desc, result)
	m.t.Log(line)
	m.history = append(m.history, line)
	if len(m.history) > historyLen {
		m.history = m.history[1:]
	}
}

// fatalf stops the run with the recent steps in one place above the reason.
func (m *model) fatalf(format string, args ...any) {
	m.t.Helper()
	m.t.Logf("the last %d steps:\n  %s", len(m.history), strings.Join(m.history, "\n  "))
	m.t.Fatalf(format, args...)
}

func (m *model) run(op modelOp) {
	switch op {
	case opCreate:
		m.create()
	case opWrite:
		m.write()
	case opMkdir:
		m.mkdir()
	case opRename:
		m.rename()
	case opRemove:
		m.remove()
	case opRmdir:
		m.rmdir()
	case opSymlink:
		m.symlink()
	case opSetattr:
		m.setattr()
	case opLookup:
		m.lookup()
	case opReaddir:
		m.readdir()
	case opLink:
		m.link()
	}
}

func (m *model) chooseOp() modelOp {
	weights := [opCount]int{
		opCreate: 18, opWrite: 12, opMkdir: 10, opRename: 12, opRemove: 8, opRmdir: 6,
		opSymlink: 5, opSetattr: 8, opLookup: 8, opReaddir: 6, opLink: 3,
	}
	if runtime.GOOS == "windows" {
		// os.Symlink needs a privilege an ordinary account does not hold, and
		// inodeOf opens the path without FILE_FLAG_OPEN_REPARSE_POINT, so a
		// link's identity there would be its target's: not a corner this test
		// can hold to the rule it checks.
		weights[opSymlink] = 0
	}
	if len(m.entries) > maxEntries {
		weights[opRemove] *= 5
		weights[opRmdir] *= 5
		weights[opCreate] = 1
		weights[opMkdir] = 1
	}
	total := 0
	for _, w := range weights {
		total += w
	}
	n := m.rng.Intn(total)
	for op, w := range weights {
		if n < w {
			return modelOp(op)
		}
		n -= w
	}
	panic("unreachable")
}

// wire is how the client library spells a path: "." for the root.
func wire(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func baseOf(p string) string {
	return p[strings.LastIndexByte(p, '/')+1:]
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// paths returns every path whose entry satisfies keep, sorted so a seed
// replays the same choices.
func (m *model) paths(keep func(p string, e *modelEntry) bool) []string {
	var out []string
	for p, e := range m.entries {
		if keep(p, e) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (m *model) pick(paths []string) (string, bool) {
	if len(paths) == 0 {
		return "", false
	}
	return paths[m.rng.Intn(len(paths))], true
}

func (m *model) dirs() []string {
	return m.paths(func(_ string, e *modelEntry) bool { return e.kind == kindDir })
}

func (m *model) files() []string {
	return m.paths(func(_ string, e *modelEntry) bool { return e.kind == kindFile })
}

func (m *model) children(dir string) []string {
	return m.paths(func(p string, _ *modelEntry) bool { return p != "" && parentOf(p) == dir })
}

func (m *model) touch(paths ...string) {
	for _, p := range paths {
		m.touched[p] = true
	}
}

// freshName draws a name not in dir. The awkward names are the ones a share
// meets in practice and a naive join gets wrong: a space, a non-ASCII letter,
// a name near the length limit. Unix adds what NTFS refuses, and a case twin
// that a case-insensitive filesystem would treat as the same file.
func (m *model) freshName(dir string) string {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "with space", "é"}
	if runtime.GOOS != "windows" {
		names = append(names, "A", "con", "nul", "a:b")
	}
	// The long name stays close to the root: a Windows path is limited to
	// 260 bytes where inodeOf opens it, and a temp dir already spends 70.
	if len(dir) < 60 {
		names = append(names, strings.Repeat("n", 100))
	}
	for range 50 {
		name := names[m.rng.Intn(len(names))]
		if _, taken := m.entries[joinPath(dir, name)]; !taken {
			return name
		}
	}
	return ""
}

// note records what the wire said about a path: the fileid, on first sight,
// and every handle it handed out.
func (m *model) note(p string, fileid uint64, fh []byte) {
	e := m.entries[p]
	if e == nil {
		m.t.Errorf("step %d: %q was reported by the server and is not in the shadow", m.step, p)
		return
	}
	if p != "" {
		if !e.seen {
			e.fileid, e.seen = fileid, true
		} else if e.fileid != fileid {
			m.t.Errorf("step %d: %q first reported fileid %#x and now %#x; the client marks the inode stale on that", m.step, p, e.fileid, fileid)
		}
	}
	if fh != nil && len(e.handles) < 2 && !slices.ContainsFunc(e.handles, func(h []byte) bool { return bytes.Equal(h, fh) }) {
		e.handles = append(e.handles, slices.Clone(fh))
	}
}

func (m *model) create() {
	dir, _ := m.pick(m.dirs())
	name := m.freshName(dir)
	if name == "" {
		return
	}
	p := joinPath(dir, name)
	m.desc = fmt.Sprintf("CREATE %q", p)
	m.touch(dir, p)
	fh, err := m.target.Create(p, 0o644)
	if err != nil {
		m.t.Errorf("step %d: CREATE %q: %v", m.step, p, err)
		return
	}
	m.entries[p] = &modelEntry{kind: kindFile}
	attr, err := m.target.GetAttr(fh)
	if err != nil {
		m.t.Errorf("step %d: GETATTR on the handle CREATE returned for %q: %v", m.step, p, err)
		return
	}
	m.note(p, attr.Fileid, fh)
	if content := m.randomBytes(); len(content) > 0 {
		m.desc += " then WRITE"
		m.writeAt(p, content)
	}
}

func (m *model) write() {
	p, ok := m.pick(m.files())
	if !ok {
		return
	}
	m.desc = fmt.Sprintf("WRITE %q", p)
	m.touch(p)
	m.writeAt(p, m.randomBytes())
}

// writeAt writes at offset zero, which is where the client library's File
// starts: a shorter write leaves the old tail in place.
func (m *model) writeAt(p string, data []byte) {
	m.desc += fmt.Sprintf(" %d bytes", len(data))
	f, err := m.target.OpenFile(p, 0o644)
	if err != nil {
		m.t.Errorf("step %d: open %q for writing: %v", m.step, p, err)
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		m.t.Errorf("step %d: WRITE %q: write %v, close %v", m.step, p, werr, cerr)
		return
	}
	e := m.entries[p]
	if len(e.content) < len(data) {
		e.content = append(e.content, make([]byte, len(data)-len(e.content))...)
	}
	copy(e.content, data)
}

func (m *model) randomBytes() []byte {
	b := make([]byte, m.rng.Intn(64))
	for i := range b {
		b[i] = byte(m.rng.Intn(256))
	}
	return b
}

func (m *model) mkdir() {
	dir, _ := m.pick(m.dirs())
	// Two levels below the root is enough to rename directories with
	// contents, and keeps the long name inside the path limit.
	if strings.Count(dir, "/") >= 1 {
		return
	}
	name := m.freshName(dir)
	if name == "" {
		return
	}
	p := joinPath(dir, name)
	m.desc = fmt.Sprintf("MKDIR %q", p)
	m.touch(dir, p)
	fh, err := m.target.Mkdir(p, 0o755)
	if err != nil {
		m.t.Errorf("step %d: MKDIR %q: %v", m.step, p, err)
		return
	}
	m.entries[p] = &modelEntry{kind: kindDir}
	attr, err := m.target.GetAttr(fh)
	if err != nil {
		m.t.Errorf("step %d: GETATTR on the handle MKDIR returned for %q: %v", m.step, p, err)
		return
	}
	m.note(p, attr.Fileid, fh)
}

func (m *model) rename() {
	src, ok := m.pick(m.paths(func(p string, _ *modelEntry) bool { return p != "" }))
	if !ok {
		return
	}
	srcEntry := m.entries[src]
	// A directory cannot move into itself, and stays within the depth limit.
	dstDir, ok := m.pick(m.paths(func(p string, e *modelEntry) bool {
		return e.kind == kindDir && p != src && !strings.HasPrefix(p, src+"/") &&
			(srcEntry.kind != kindDir || strings.Count(p, "/") < 1)
	}))
	if !ok {
		return
	}
	var name string
	over := false
	// A file may land on an existing file, which is what `mv` and every
	// atomic-save editor do; a directory onto a name is refused on Windows.
	if srcEntry.kind != kindDir && m.rng.Intn(3) == 0 {
		if p, ok := m.pick(m.paths(func(p string, e *modelEntry) bool {
			return parentOf(p) == dstDir && e.kind == kindFile && p != src
		})); ok {
			name, over = baseOf(p), true
		}
	}
	if name == "" {
		if name = m.freshName(dstDir); name == "" {
			return
		}
	}
	dst := joinPath(dstDir, name)
	m.desc = fmt.Sprintf("RENAME %s %q -> %q", kindNames[srcEntry.kind], src, dst)
	if over {
		m.desc += " over an existing file"
	}
	m.touch(parentOf(src), dstDir, dst)
	if err := m.target.Rename(src, dst); err != nil {
		m.t.Errorf("step %d: RENAME %q -> %q (over an existing file: %v): %v", m.step, src, dst, over, err)
		return
	}
	if over {
		delete(m.entries, dst)
	}
	// The subtree moves with its identities and without its handles.
	moved := map[string]*modelEntry{}
	for p, e := range m.entries {
		if p == src || strings.HasPrefix(p, src+"/") {
			e.handles = nil
			moved[dst+p[len(src):]] = e
			delete(m.entries, p)
		}
	}
	for p, e := range moved {
		m.entries[p] = e
		m.touch(p)
	}
}

func (m *model) remove() {
	p, ok := m.pick(m.paths(func(_ string, e *modelEntry) bool { return e.kind != kindDir }))
	if !ok {
		return
	}
	// A link names its target: whether a REMOVE of a link reaches the target
	// instead is the first thing a failure right after one has to answer.
	m.desc = fmt.Sprintf("REMOVE %s %q", kindNames[m.entries[p].kind], p)
	if e := m.entries[p]; e.kind == kindLink {
		m.desc += fmt.Sprintf(" (-> %q)", e.link)
	}
	m.touch(parentOf(p))
	if err := m.target.Remove(p); err != nil {
		m.t.Errorf("step %d: REMOVE %q: %v", m.step, p, err)
		return
	}
	delete(m.entries, p)
}

func (m *model) rmdir() {
	p, ok := m.pick(m.paths(func(p string, e *modelEntry) bool {
		return p != "" && e.kind == kindDir && len(m.children(p)) == 0
	}))
	if !ok {
		return
	}
	m.desc = fmt.Sprintf("RMDIR %q", p)
	m.touch(parentOf(p))
	if err := m.target.RmDir(p); err != nil {
		m.t.Errorf("step %d: RMDIR %q: %v", m.step, p, err)
		return
	}
	delete(m.entries, p)
}

func (m *model) symlink() {
	dir, _ := m.pick(m.dirs())
	name := m.freshName(dir)
	if name == "" {
		return
	}
	// Usually a sibling, sometimes nowhere: both are ordinary in a checkout.
	link := "nowhere"
	if sibling, ok := m.pick(m.paths(func(p string, e *modelEntry) bool {
		return parentOf(p) == dir && e.kind == kindFile
	})); ok && m.rng.Intn(4) != 0 {
		link = baseOf(sibling)
	}
	p := joinPath(dir, name)
	m.desc = fmt.Sprintf("SYMLINK %q -> %q", p, link)
	m.touch(dir, p)
	if err := m.target.Symlink(link, p); err != nil {
		m.t.Errorf("step %d: SYMLINK %q -> %q: %v", m.step, p, link, err)
		return
	}
	m.entries[p] = &modelEntry{kind: kindLink, link: link}
	attr, fh, err := m.target.Lookup(p)
	if err != nil {
		m.t.Errorf("step %d: LOOKUP of the new link %q: %v", m.step, p, err)
		return
	}
	m.note(p, attr.(*nfsclient.Fattr).Fileid, fh)
}

func (m *model) setattr() {
	p, ok := m.pick(m.files())
	if !ok {
		return
	}
	m.touch(p)
	e := m.entries[p]
	var sattr nfsclient.Sattr3
	if m.rng.Intn(2) == 0 {
		// Modes that keep the owner's write bit: a chmod that takes it away
		// sets the read-only attribute on Windows, and the next write fails.
		sattr.Mode = nfsclient.SetMode{SetIt: true, Mode: []uint32{0o644, 0o755, 0o600}[m.rng.Intn(3)]}
		m.desc = fmt.Sprintf("SETATTR %q mode %#o", p, sattr.Mode.Mode)
	} else {
		sattr.Size = nfsclient.SetSize{SetIt: true, Size: uint64(m.rng.Intn(len(e.content) + 8))}
		m.desc = fmt.Sprintf("SETATTR %q size %d", p, sattr.Size.Size)
	}
	if err := m.target.Setattr(p, sattr); err != nil {
		m.t.Errorf("step %d: SETATTR %q (%+v): %v", m.step, p, sattr, err)
		return
	}
	if sattr.Size.SetIt {
		size := int(sattr.Size.Size)
		if size <= len(e.content) {
			e.content = e.content[:size]
		} else {
			e.content = append(e.content, make([]byte, size-len(e.content))...)
		}
	}
}

func (m *model) lookup() {
	p, ok := m.pick(m.paths(func(p string, _ *modelEntry) bool { return p != "" }))
	if !ok {
		return
	}
	m.desc = fmt.Sprintf("LOOKUP %q", p)
	m.touch(p)
	attr, fh, err := m.target.Lookup(p)
	if err != nil {
		m.t.Errorf("step %d: LOOKUP %q: %v", m.step, p, err)
		return
	}
	m.note(p, attr.(*nfsclient.Fattr).Fileid, fh)
}

func (m *model) readdir() {
	dir, _ := m.pick(m.dirs())
	m.desc = fmt.Sprintf("READDIRPLUS %q", dir)
	m.touch(dir)
}

// link asks for a hard link, which the share refuses (link_test.go), and
// checks the refusal left nothing behind.
func (m *model) link() {
	file, ok := m.pick(m.files())
	if !ok {
		return
	}
	dir, _ := m.pick(m.dirs())
	name := m.freshName(dir)
	if name == "" {
		return
	}
	m.desc = fmt.Sprintf("LINK %q -> %q", file, joinPath(dir, name))
	m.touch(dir, file)
	_, fileFH, err := m.target.Lookup(file)
	if err != nil {
		m.t.Errorf("step %d: LOOKUP %q before LINK: %v", m.step, file, err)
		return
	}
	_, dirFH, err := m.target.Lookup(wire(dir))
	if err != nil {
		m.t.Errorf("step %d: LOOKUP %q before LINK: %v", m.step, dir, err)
		return
	}
	if status := rawLink(m.t, m.client, fileFH, dirFH, name); status != uint32(nfs.NFSStatusInval) {
		m.t.Errorf("step %d: LINK %q -> %q/%q returned %s, want NFS3ERR_INVAL", m.step, file, dir, name, statusName(status))
	}
	if _, _, err := m.target.Lookup(joinPath(dir, name)); err == nil {
		m.t.Errorf("step %d: after a refused LINK, %q exists", m.step, joinPath(dir, name))
	}
}

// checkTouched holds what the step touched against the shadow, and one
// handle chosen at random from the rest, so a handle that went stale is found
// within a few steps of the operation that broke it.
func (m *model) checkTouched() {
	for _, p := range m.paths(func(p string, _ *modelEntry) bool { return m.touched[p] }) {
		m.checkEntry(p)
	}
	if p, ok := m.pick(m.paths(func(p string, e *modelEntry) bool { return !m.touched[p] && len(e.handles) > 0 })); ok {
		m.checkHandles(p)
	}
}

func (m *model) checkAll() {
	for _, p := range m.paths(func(string, *modelEntry) bool { return true }) {
		m.checkEntry(p)
	}
}

// checkEntry holds one path against the shadow: its handles, and its listing,
// content or link target by kind.
func (m *model) checkEntry(p string) {
	m.checkHandles(p)
	e := m.entries[p]
	switch e.kind {
	case kindDir:
		m.checkListing(p)
	case kindFile:
		f, err := m.target.Open(p)
		if err != nil {
			m.t.Errorf("step %d: open %q: %v", m.step, p, err)
			return
		}
		got, err := io.ReadAll(f)
		if err != nil {
			m.t.Errorf("step %d: read %q: %v", m.step, p, err)
		} else if !bytes.Equal(got, e.content) {
			m.t.Errorf("step %d: %q holds %d bytes %x, the shadow says %d bytes %x", m.step, p, len(got), got, len(e.content), e.content)
		}
	case kindLink:
		f, err := m.target.Open(p)
		if err != nil {
			m.t.Errorf("step %d: open the link %q: %v", m.step, p, err)
			return
		}
		if got, err := f.Readlink(); err != nil || got != e.link {
			m.t.Errorf("step %d: READLINK %q = %q, %v; want %q", m.step, p, got, err, e.link)
		}
	}
}

// checkHandles asks GETATTR of every handle issued for a path that still
// exists, and holds the answer to the fileid first seen.
func (m *model) checkHandles(p string) {
	e := m.entries[p]
	for _, fh := range e.handles {
		attr, err := m.target.GetAttr(fh)
		if err != nil {
			m.t.Errorf("step %d: a handle for %q no longer resolves: %v", m.step, p, err)
			continue
		}
		if p != "" && attr.Fileid != e.fileid {
			m.t.Errorf("step %d: a handle for %q reports fileid %#x, first seen as %#x", m.step, p, attr.Fileid, e.fileid)
		}
	}
}

// checkListing compares one READDIRPLUS with the shadow, name by name, and
// records what it reported about each entry.
func (m *model) checkListing(dir string) {
	entries, err := m.target.ReadDirPlus(wire(dir))
	if err != nil {
		m.t.Errorf("step %d: READDIRPLUS %q: %v", m.step, dir, err)
		return
	}
	want := m.children(dir)
	var got []string
	for _, e := range entries {
		got = append(got, joinPath(dir, e.FileName))
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		m.t.Errorf("step %d: READDIRPLUS %q lists %q, the shadow holds %q", m.step, dir, got, want)
		m.describeDisagreement(got, want)
		return
	}
	for _, e := range entries {
		p := joinPath(dir, e.FileName)
		if !e.Attr.IsSet || !e.Handle.IsSet {
			m.t.Errorf("step %d: %q listed without attributes or a handle", m.step, p)
			continue
		}
		if e.FileId != e.Attr.Attr.Fileid {
			m.t.Errorf("step %d: %q listed with fileid %#x and attributes saying %#x", m.step, p, e.FileId, e.Attr.Attr.Fileid)
		}
		if want := kindTypes[m.entries[p].kind]; e.Attr.Attr.Type != want {
			m.t.Errorf("step %d: %q listed as type %d, the shadow says %d", m.step, p, e.Attr.Attr.Type, want)
		}
		m.note(p, e.FileId, e.Handle.FH)
	}
}

// describeDisagreement asks the server, by LOOKUP of the path, about every
// name the listing and the shadow disagree on and about every case twin among
// the names either holds, and prints each beside what the shadow says. A
// listing alone says the two differ; this says which file each side has,
// with its fileid, type and link target, which is what tells a REMOVE that
// took the wrong file from a shadow that recorded the wrong one.
func (m *model) describeDisagreement(got, want []string) {
	all := slices.Compact(slices.Sorted(slices.Values(slices.Concat(got, want))))
	for _, p := range all {
		twin := slices.ContainsFunc(all, func(q string) bool { return q != p && strings.EqualFold(q, p) })
		if slices.Contains(got, p) && slices.Contains(want, p) && !twin {
			continue
		}
		m.t.Logf("  %q: the shadow holds %s; the server says %s", p, m.shadowSays(p), m.serverSays(p))
	}
}

func (m *model) shadowSays(p string) string {
	e := m.entries[p]
	if e == nil {
		return "nothing"
	}
	s := fmt.Sprintf("%s fileid %#x", kindNames[e.kind], e.fileid)
	if e.kind == kindLink {
		s += fmt.Sprintf(" -> %q", e.link)
	}
	return s
}

func (m *model) serverSays(p string) string {
	attr, _, err := m.target.Lookup(p)
	if err != nil {
		return fmt.Sprintf("LOOKUP fails: %v", err)
	}
	fattr := attr.(*nfsclient.Fattr)
	s := fmt.Sprintf("%s fileid %#x", typeName(fattr.Type), fattr.Fileid)
	if fattr.Type != nfsclient.NF3Lnk {
		return s
	}
	f, err := m.target.Open(p)
	if err != nil {
		return s + fmt.Sprintf(", open fails: %v", err)
	}
	link, err := f.Readlink()
	if err != nil {
		return s + fmt.Sprintf(", READLINK fails: %v", err)
	}
	return s + fmt.Sprintf(" -> %q", link)
}
