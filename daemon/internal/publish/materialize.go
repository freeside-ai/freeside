package publish

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git object identity under the transport's enforced sha1 format
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

type materializationTreeEntry struct {
	mode string
	oid  string
	size int64
}

const (
	maxMaterializationEntries              = 100_000
	maxMaterializationBytes          int64 = 512 << 20
	maxTreeListingBytes              int64 = 64 << 20
	maxMaterializationPathBytes            = 4096
	maxMaterializationPathDepth            = 256
	maxMaterializationComponentBytes       = 255
	maxAttributeLineBytes                  = 64 << 10
)

type cappedMaterializationBuffer struct {
	// Keep this named: embedding bytes.Buffer promotes ReadFrom, which lets
	// io.Copy bypass the limiting Write method entirely.
	buffer    bytes.Buffer
	remaining int64
	exceeded  bool
}

var errMaterializationTreeListingLimit = errors.New("materialization tree listing limit exceeded")

// ErrMaterializationRefused marks a deterministic tree-shape, attribute, or
// resource-limit refusal discovered before workspace mutation. It retains the
// broader transport class for callers that only distinguish transport errors.
var ErrMaterializationRefused = fmt.Errorf("git tree materialization refused: %w", ErrGitTransport)

func (b *cappedMaterializationBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) <= b.remaining {
		n, err := b.buffer.Write(p)
		b.remaining -= int64(n)
		return n, err
	}

	allowed := int(max(b.remaining, 0))
	n, _ := b.buffer.Write(p[:allowed])
	b.remaining -= int64(n)
	b.exceeded = true
	return n, errMaterializationTreeListingLimit
}

func (b *cappedMaterializationBuffer) Bytes() []byte { return b.buffer.Bytes() }

type countingMaterializationWriter struct {
	w io.Writer
	n int64
}

type workingTreeEncodingDetector struct {
	line         []byte
	found        bool
	unsafe       bool
	overlongLine bool
}

func (d *workingTreeEncodingDetector) Write(p []byte) (int, error) {
	original := len(p)
	for len(p) > 0 {
		newline := bytes.IndexByte(p, '\n')
		fragment := p
		complete := false
		if newline >= 0 {
			fragment = p[:newline]
			p = p[newline+1:]
			complete = true
		} else {
			p = nil
		}
		if !d.overlongLine {
			if len(d.line)+len(fragment) > maxAttributeLineBytes {
				d.unsafe = true
				d.overlongLine = true
				d.line = d.line[:0]
			} else {
				d.line = append(d.line, fragment...)
			}
		}
		if complete {
			if !d.overlongLine && lineConfiguresWorkingTreeEncoding(d.line) {
				d.found = true
			}
			d.line = d.line[:0]
			d.overlongLine = false
		}
	}
	return original, nil
}

func (d *workingTreeEncodingDetector) rejects() bool {
	return d.unsafe || d.found || d.overlongLine || lineConfiguresWorkingTreeEncoding(d.line)
}

func lineConfiguresWorkingTreeEncoding(line []byte) bool {
	line = bytes.TrimLeft(bytes.TrimSuffix(line, []byte{'\r'}), " \t")
	if len(line) == 0 || line[0] == '#' {
		return false
	}
	fields, valid := materializationAttributeFields(line)
	if !valid {
		return true
	}
	for _, field := range fields {
		if len(field) == 0 || field[0] == '-' || field[0] == '!' {
			continue
		}
		name, _, _ := bytes.Cut(field, []byte{'='})
		if bytes.Equal(name, []byte("working-tree-encoding")) {
			return true
		}
	}
	return false
}

func materializationAttributeFields(line []byte) ([][]byte, bool) {
	if line[0] != '"' {
		fields := bytes.Fields(line)
		if len(fields) < 2 {
			return nil, true
		}
		return fields[1:], true
	}

	escaped := false
	for i := 1; i < len(line); i++ {
		switch {
		case escaped:
			escaped = false
		case line[i] == '\\':
			escaped = true
		case line[i] == '"':
			if i+1 < len(line) && line[i+1] != ' ' && line[i+1] != '\t' {
				return nil, false
			}
			return bytes.Fields(line[i+1:]), true
		}
	}
	return nil, false
}

func (w *countingMaterializationWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// materializeWorktree writes commitSHA's raw tree bytes into dir while
// preserving dir's own .git directory. Checkout and reset plumbing are
// deliberately excluded: both honor candidate-controlled .gitattributes, so
// Git can call transformed bytes clean while Ward's --no-filters proof
// correctly rejects them. This implementation stays independent from Ward's
// prover so the proof can detect producer defects instead of sharing them.
func materializeWorktree(ctx context.Context, r *netRunner, dir, commitSHA string) error {
	entries, err := validatedMaterializationTree(ctx, r, commitSHA)
	if err != nil {
		return err
	}

	// Validation above is read-only. From here on the checkout the caller has
	// already claimed is intentionally rebuilt; FetchBase owns failure cleanup
	// for its fresh checkout, and RetainWorktree owns its fresh destination.
	if err := clearMaterializationWorktree(dir); err != nil {
		return err
	}
	scratchEnv := r.env
	r.env = append(withoutGitIndexFile(r.env),
		"GIT_INDEX_FILE="+filepath.Join(dir, ".git", "index"))
	defer func() { r.env = scratchEnv }()
	if _, _, err := r.run(ctx, nil, "update-ref", "--no-deref", "HEAD", commitSHA); err != nil {
		return err
	}
	if _, _, err := r.run(ctx, nil, "read-tree", "--reset", commitSHA); err != nil {
		return err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open materialization root: %w", err)
	}
	defer func() { _ = root.Close() }()
	paths := make([]string, 0, len(entries))
	for treePath := range entries {
		paths = append(paths, treePath)
	}
	if err := readMaterializationBlobs(ctx, r, entries, paths, func(
		treePath string, entry materializationTreeEntry, blob io.Reader,
	) error {
		return writeMaterializationEntry(root, treePath, entry, blob)
	}); err != nil {
		return err
	}
	return verifyMaterialization(ctx, r, root, commitSHA, entries)
}

func validatedMaterializationTree(
	ctx context.Context, r *netRunner, commitSHA string,
) (map[string]materializationTreeEntry, error) {
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", commitSHA+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("commit %s is absent from checkout: %w", commitSHA, err)
	}
	if got := strings.TrimSpace(string(out)); got != commitSHA {
		return nil, fmt.Errorf("commit %s resolved to %s: %w", commitSHA, got, ErrMaterializationRefused)
	}
	entries, err := listMaterializationTree(ctx, r, commitSHA)
	if err != nil {
		return nil, err
	}
	if err := rejectMaterializationPrefixConflicts(entries); err != nil {
		return nil, err
	}
	if err := rejectMaterializationFilesystemCollisions(entries); err != nil {
		return nil, err
	}
	if err := rejectWorkingTreeEncodings(ctx, r, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func rejectWorkingTreeEncodings(
	ctx context.Context,
	r *netRunner,
	entries map[string]materializationTreeEntry,
) error {
	var paths []string
	for treePath := range entries {
		if !strings.EqualFold(path.Base(treePath), ".gitattributes") {
			continue
		}
		paths = append(paths, treePath)
	}
	return readMaterializationBlobs(ctx, r, entries, paths, func(
		treePath string, _ materializationTreeEntry, blob io.Reader,
	) error {
		detector := &workingTreeEncodingDetector{}
		if _, err := io.Copy(detector, blob); err != nil {
			return err
		}
		if detector.rejects() {
			return fmt.Errorf(
				"tree attributes at %q may configure working-tree-encoding: %w",
				treePath, ErrMaterializationRefused,
			)
		}
		return nil
	})
}

func readMaterializationBlobs(
	ctx context.Context,
	r *netRunner,
	entries map[string]materializationTreeEntry,
	paths []string,
	consume func(string, materializationTreeEntry, io.Reader) error,
) error {
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	return r.interact(ctx, func(stdin io.Writer, stdout io.Reader) error {
		requests := bufio.NewWriter(stdin)
		responses := bufio.NewReader(stdout)
		for _, treePath := range paths {
			entry := entries[treePath]
			if _, err := fmt.Fprintln(requests, entry.oid); err != nil {
				return err
			}
			if err := requests.Flush(); err != nil {
				return err
			}
			header, err := responses.ReadString('\n')
			if err != nil {
				return err
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[0] != entry.oid || fields[1] != "blob" {
				return fmt.Errorf("unexpected cat-file batch header for %s", treePath)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size != entry.size {
				return fmt.Errorf("cat-file batch size for %s does not match tree", treePath)
			}
			blob := &io.LimitedReader{R: responses, N: size}
			if err := consume(treePath, entry, blob); err != nil {
				return err
			}
			if blob.N != 0 {
				return fmt.Errorf("blob consumer left %d bytes for %s", blob.N, treePath)
			}
			delimiter, err := responses.ReadByte()
			if err != nil || delimiter != '\n' {
				return fmt.Errorf("invalid cat-file batch delimiter for %s", treePath)
			}
		}
		return nil
	}, "cat-file", "--batch")
}

func listMaterializationTree(
	ctx context.Context, r *netRunner, commitSHA string,
) (map[string]materializationTreeEntry, error) {
	listing := cappedMaterializationBuffer{remaining: maxTreeListingBytes}
	err := r.runTo(ctx, &listing, "ls-tree", "-r", "-l", "-z", "--full-tree", commitSHA)
	if listing.exceeded {
		return nil, fmt.Errorf("tree listing exceeds %d bytes: %w", maxTreeListingBytes, ErrMaterializationRefused)
	}
	if err != nil {
		return nil, err
	}
	entries := make(map[string]materializationTreeEntry)
	directories := map[string]struct{}{`.git`: {}, ".": {}}
	var totalBytes int64
	for _, record := range bytes.Split(listing.Bytes(), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("unparseable ls-tree record %q: %w", record, ErrMaterializationRefused)
		}
		treePath := string(pathBytes)
		if err := validateMaterializationTreePath(treePath); err != nil {
			return nil, err
		}
		if _, duplicate := entries[treePath]; duplicate {
			return nil, fmt.Errorf("tree repeats path %q: %w", treePath, ErrMaterializationRefused)
		}
		if err := accountMaterializationPath(
			treePath, len(entries), directories, maxMaterializationEntries,
		); err != nil {
			return nil, err
		}
		mode, objectType, oid := fields[0], fields[1], fields[2]
		if objectType != "blob" || (mode != "100644" && mode != "100755") {
			return nil, fmt.Errorf(
				"tree path %q has unsupported mode/type %s/%s: %w",
				treePath, mode, objectType, ErrMaterializationRefused,
			)
		}
		if !validCommitSHA(oid) {
			return nil, fmt.Errorf("tree path %q carries invalid object id %q: %w", treePath, oid, ErrMaterializationRefused)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("tree path %q carries invalid blob size %q: %w", treePath, fields[3], ErrMaterializationRefused)
		}
		if size > maxMaterializationBytes-totalBytes {
			return nil, fmt.Errorf("tree exceeds %d materialized bytes: %w", maxMaterializationBytes, ErrMaterializationRefused)
		}
		totalBytes += size
		entries[treePath] = materializationTreeEntry{mode: mode, oid: oid, size: size}
	}
	return entries, nil
}

func validateMaterializationTreePath(treePath string) error {
	if len(treePath) > maxMaterializationPathBytes {
		return fmt.Errorf(
			"tree path has %d bytes, exceeds %d: %w",
			len(treePath), maxMaterializationPathBytes, ErrMaterializationRefused,
		)
	}
	if depth := strings.Count(treePath, "/") + 1; depth > maxMaterializationPathDepth {
		return fmt.Errorf(
			"tree path has depth %d, exceeds %d: %w",
			depth, maxMaterializationPathDepth, ErrMaterializationRefused,
		)
	}
	if strings.ContainsAny(treePath, "\r\n") {
		return fmt.Errorf("tree path %q has a line break Ward cannot attest: %w", treePath, ErrMaterializationRefused)
	}
	for _, segment := range strings.Split(treePath, "/") {
		switch segment {
		case "", ".", "..":
			return fmt.Errorf("tree path %q has unsafe component %q: %w", treePath, segment, ErrMaterializationRefused)
		}
		if len(segment) > maxMaterializationComponentBytes {
			return fmt.Errorf(
				"tree path %q has a %d-byte component, exceeds %d: %w",
				treePath, len(segment), maxMaterializationComponentBytes, ErrMaterializationRefused,
			)
		}
		if importer.GitUnsafeComponent(segment) {
			return fmt.Errorf("tree path %q aliases the checkout git directory: %w", treePath, ErrMaterializationRefused)
		}
	}
	return nil
}

func accountMaterializationPath(
	treePath string,
	regularEntries int,
	directories map[string]struct{},
	limit int,
) error {
	for parent := path.Dir(treePath); parent != "." && parent != "/"; parent = path.Dir(parent) {
		directories[parent] = struct{}{}
	}
	if total := regularEntries + 1 + len(directories); total > limit {
		return fmt.Errorf("tree expands to more than %d files and directories: %w", limit, ErrMaterializationRefused)
	}
	return nil
}

func rejectMaterializationPrefixConflicts(entries map[string]materializationTreeEntry) error {
	for treePath := range entries {
		for parent := path.Dir(treePath); parent != "." && parent != "/"; parent = path.Dir(parent) {
			if _, conflict := entries[parent]; conflict {
				return fmt.Errorf("tree path %q is nested under entry %q: %w", treePath, parent, ErrMaterializationRefused)
			}
		}
	}
	return nil
}

func rejectMaterializationFilesystemCollisions(entries map[string]materializationTreeEntry) error {
	type foldNode struct {
		spelling  string
		ownerPath string
		filePath  string
		hasChild  bool
	}
	type foldEdge struct {
		parent    int
		component string
	}

	ordered := make([]string, 0, len(entries))
	for treePath := range entries {
		ordered = append(ordered, treePath)
	}
	sort.Strings(ordered)

	nodes := []foldNode{{}}
	edges := make(map[foldEdge]int, len(entries))
	refuse := func(first, second string) error {
		return fmt.Errorf(
			"tree paths %q and %q collide under checkout case/normalization folding: %w",
			first, second, ErrMaterializationRefused,
		)
	}
	for _, treePath := range ordered {
		parent := 0
		components := strings.Split(treePath, "/")
		for i, component := range components {
			if nodes[parent].filePath != "" {
				return refuse(nodes[parent].filePath, treePath)
			}
			edge := foldEdge{
				parent:    parent,
				component: importer.CheckoutFoldComponent(component),
			}
			child, exists := edges[edge]
			if !exists {
				child = len(nodes)
				edges[edge] = child
				nodes[parent].hasChild = true
				nodes = append(nodes, foldNode{spelling: component, ownerPath: treePath})
			} else if nodes[child].spelling != component {
				return refuse(nodes[child].ownerPath, treePath)
			}
			parent = child
			if i == len(components)-1 {
				if nodes[child].hasChild {
					return refuse(nodes[child].ownerPath, treePath)
				}
				nodes[child].filePath = treePath
			}
		}
	}
	return nil
}

func clearMaterializationWorktree(dir string) error {
	children, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read materialization checkout: %w", err)
	}
	for _, child := range children {
		if child.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, child.Name())); err != nil {
			return fmt.Errorf("clear materialization path %q: %w", child.Name(), err)
		}
	}
	return nil
}

func writeMaterializationEntry(
	root *os.Root,
	treePath string,
	entry materializationTreeEntry,
	blob io.Reader,
) error {
	if err := root.MkdirAll(filepath.FromSlash(path.Dir(treePath)), 0o700); err != nil {
		return fmt.Errorf("create materialization dir for %s: %w", treePath, err)
	}
	permissions := os.FileMode(0o644)
	if entry.mode == "100755" {
		permissions = 0o755
	}
	file, err := root.OpenFile(
		filepath.FromSlash(treePath), os.O_CREATE|os.O_EXCL|os.O_WRONLY, permissions,
	)
	if err != nil {
		return fmt.Errorf("materialize %s: %w", treePath, err)
	}
	written := &countingMaterializationWriter{w: file}
	_, streamErr := io.Copy(written, blob)
	closeErr := file.Close()
	if streamErr != nil {
		return streamErr
	}
	if closeErr != nil {
		return fmt.Errorf("close materialized %s: %w", treePath, closeErr)
	}
	if written.n != entry.size {
		return fmt.Errorf(
			"materialized path %s wrote %d bytes, tree reports %d: %w",
			treePath, written.n, entry.size, ErrGitTransport,
		)
	}
	return nil
}

func verifyMaterialization(
	ctx context.Context,
	r *netRunner,
	root *os.Root,
	commitSHA string,
	entries map[string]materializationTreeEntry,
) error {
	for treePath, entry := range entries {
		got, err := materializedObjectID(root, treePath, entry.mode)
		if err != nil {
			return fmt.Errorf("materialized path %s: %w: %w", treePath, err, ErrGitTransport)
		}
		if got != entry.oid {
			return fmt.Errorf(
				"materialized path %s holds %s, tree has %s: %w",
				treePath, got, entry.oid, ErrGitTransport,
			)
		}
	}
	if err := walkMaterializationStrays(root, entries); err != nil {
		return err
	}
	head, _, err := r.run(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(head)); got != commitSHA {
		return fmt.Errorf("materialized HEAD is %s, want %s: %w", got, commitSHA, ErrGitTransport)
	}
	return nil
}

func materializedObjectID(root *os.Root, treePath, mode string) (string, error) {
	rootPath := filepath.FromSlash(treePath)
	info, err := root.Lstat(rootPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("regular entry materialized as %s", info.Mode())
	}
	ownerExecutable := info.Mode().Perm()&0o100 != 0
	if wantExecutable := mode == "100755"; ownerExecutable != wantExecutable {
		return "", fmt.Errorf("mode %s entry materialized with permissions %s", mode, info.Mode().Perm())
	}
	file, err := root.Open(rootPath)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // read-only verification handle
	hash := sha1.New() //nolint:gosec // git object identity, not a cryptographic protection
	hash.Write([]byte("blob " + strconv.FormatInt(info.Size(), 10)))
	hash.Write([]byte{0})
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func walkMaterializationStrays(root *os.Root, entries map[string]materializationTreeEntry) error {
	legitimateDirs := map[string]struct{}{".": {}}
	for treePath := range entries {
		for parent := path.Dir(treePath); parent != "." && parent != "/"; parent = path.Dir(parent) {
			legitimateDirs[parent] = struct{}{}
		}
	}
	observed := make(map[string]struct{}, len(entries))
	var walk func(string) error
	walk = func(dir string) error {
		handle, err := root.Open(filepath.FromSlash(dir))
		if err != nil {
			return fmt.Errorf("open materialized directory %s: %w", dir, err)
		}
		children, readErr := handle.ReadDir(-1)
		closeErr := handle.Close()
		if readErr != nil {
			return fmt.Errorf("read materialized directory %s: %w", dir, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close materialized directory %s: %w", dir, closeErr)
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			treePath := child.Name()
			if dir != "." {
				treePath = dir + "/" + child.Name()
			}
			if treePath == ".git" {
				continue
			}
			if child.IsDir() {
				if _, ok := legitimateDirs[treePath]; !ok {
					return fmt.Errorf("materialized worktree has stray directory %s: %w", treePath, ErrGitTransport)
				}
				if err := walk(treePath); err != nil {
					return err
				}
				continue
			}
			if _, ok := entries[treePath]; !ok {
				return fmt.Errorf("materialized worktree has stray path %s: %w", treePath, ErrGitTransport)
			}
			observed[treePath] = struct{}{}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return err
	}
	// Exact-name membership matters on case- or Unicode-folding filesystems:
	// Lstat of both tree spellings can resolve to the same on-disk file. Ward's
	// expected-path proof distinguishes them, so detect the collapse here too.
	for treePath := range entries {
		if _, ok := observed[treePath]; !ok {
			return fmt.Errorf("materialized worktree is missing exact path %s: %w", treePath, ErrGitTransport)
		}
	}
	return nil
}
