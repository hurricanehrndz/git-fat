package fatrepo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstub"
)

// Object is a fat object as git sees it: a path whose blob is a stub.
type Object struct {
	// Path is repo-relative with / separators, as git reports it.
	Path string
	// SHA and Size are decoded from the stub.
	SHA  string
	Size int64

	// mode and blob are the entry git records for the stub.
	mode, blob string
}

// entry is a path and the mode and blob git records for it.
type entry struct {
	path, mode, blob string
}

// gitlinkMode marks a submodule commit, which is not a blob in this repo.
const gitlinkMode = "160000"

// IndexObjects returns the fat objects among the index's stage-0 entries.
func (r *Repo) IndexObjects(ctx context.Context) ([]Object, error) {
	top, err := r.Toplevel(ctx)
	if err != nil {
		return nil, err
	}
	out, err := gitBytes(ctx, top, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var entries []entry
	for _, rec := range splitZ(out) {
		// <mode> <blob> <stage>\t<path>
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("git ls-files: unexpected record %q", rec)
		}
		if f[2] == "0" && f[0] != gitlinkMode {
			entries = append(entries, entry{path, f[0], f[1]})
		}
	}
	return r.fatObjects(ctx, top, entries)
}

// NewObjects returns the fat objects that HEAD adds or changes relative to
// ref, or with cached set, that the index does. Python counted only added
// paths, so a fat file whose content changed escaped fscheck-new and
// pull-new; modifications and type changes count here.
func (r *Repo) NewObjects(ctx context.Context, ref string, cached bool) ([]Object, error) {
	top, err := r.Toplevel(ctx)
	if err != nil {
		return nil, err
	}
	base, err := git(ctx, top, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("unknown ref %q", ref)
	}
	// Rename detection is off, so a renamed fat file shows as added and
	// every record carries exactly one path.
	args := []string{"diff-tree", "-r", "-z", "--no-renames", "--diff-filter=ACMRT", base, "HEAD"}
	if cached {
		args = []string{"diff-index", "--cached", "-r", "-z", "--no-renames", "--diff-filter=ACMRT", base}
	}
	out, err := gitBytes(ctx, top, nil, args...)
	if err != nil {
		return nil, err
	}
	recs := splitZ(out)
	var entries []entry
	for i := 0; i+1 < len(recs); i += 2 {
		// :<old mode> <new mode> <old blob> <new blob> <status>, then the path
		f := strings.Fields(recs[i])
		if len(f) != 5 || !strings.HasPrefix(f[0], ":") {
			return nil, fmt.Errorf("git %s: unexpected record %q", args[0], recs[i])
		}
		if f[1] != gitlinkMode {
			entries = append(entries, entry{recs[i+1], f[1], f[3]})
		}
	}
	return r.fatObjects(ctx, top, entries)
}

// HeadObjects returns the fat objects HEAD's tree records at the given
// repo-relative paths, as Python's pull looked them up. A path that is not a
// fat object in HEAD is warned about, naming cmd, and skipped.
func (r *Repo) HeadObjects(ctx context.Context, cmd string, paths []string) ([]Object, error) {
	if len(paths) == 0 {
		// ls-tree would list the whole tree.
		return nil, nil
	}
	top, err := r.Toplevel(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]string{"--literal-pathspecs", "ls-tree", "-r", "-z", "--full-tree", "HEAD", "--"}, paths...)
	out, err := gitBytes(ctx, top, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []entry
	for _, rec := range splitZ(out) {
		// <mode> <type> <blob>\t<path>
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected record %q", rec)
		}
		if f[1] == "blob" {
			entries = append(entries, entry{path, f[0], f[2]})
		}
	}
	objs, err := r.fatObjects(ctx, top, entries)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, o := range objs {
		found[o.Path] = true
	}
	for _, p := range paths {
		if !found[p] {
			r.logf(false, "%s: %s is not a fat object", cmd, p)
		}
	}
	return objs, nil
}

// ResolvePaths turns file arguments, relative to Dir and possibly naming
// directories, into the repo-relative paths of the tracked files they match.
// A missing argument is warned about, naming cmd, as Python did; none
// existing is an error. Untracked files match nothing.
func (r *Repo) ResolvePaths(ctx context.Context, cmd string, args []string) ([]string, error) {
	var valid []string
	for _, a := range args {
		p := a
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.Dir, p)
		}
		if _, err := os.Lstat(p); err != nil {
			r.logf(false, "%s: %s not found, is given path valid?", cmd, a)
			continue
		}
		valid = append(valid, a)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("%s: no valid file paths given", cmd)
	}
	// Literal pathspecs, so a file named "*.fat" matches only itself.
	args = append([]string{"--literal-pathspecs", "ls-files", "--full-name", "-z", "--"}, valid...)
	out, err := gitBytes(ctx, r.Dir, nil, args...)
	if err != nil {
		return nil, err
	}
	return splitZ(out), nil
}

// fatObjects returns the entries whose blob is a stub, in entry order. It
// sizes every blob with one cat-file --batch-check and reads only the
// stub-sized ones with one cat-file --batch, so no git process runs per file.
//
// A fat object is a blob of exactly fatstub.Len bytes that decodes, the rule
// the filters use. Python's is_fatblob read the first 74 bytes, so it also
// took longer blobs that merely start with a stub; that difference is
// deliberate, since smudge passes such blobs through as content.
func (r *Repo) fatObjects(ctx context.Context, top string, entries []entry) ([]Object, error) {
	var in bytes.Buffer
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.blob] {
			seen[e.blob] = true
			in.WriteString(e.blob + "\n")
		}
	}
	if in.Len() == 0 {
		return nil, nil
	}
	sized, err := gitBytes(ctx, top, in.Bytes(), "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	in.Reset()
	stubLen := strconv.Itoa(fatstub.Len)
	for line := range strings.Lines(string(sized)) {
		// <blob> <type> <size>, or <blob> missing
		if f := strings.Fields(line); len(f) == 3 && f[1] == "blob" && f[2] == stubLen {
			in.WriteString(f[0] + "\n")
		}
	}
	if in.Len() == 0 {
		return nil, nil
	}
	contents, err := gitBytes(ctx, top, in.Bytes(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	stubs, err := readBatch(contents)
	if err != nil {
		return nil, err
	}
	var objs []Object
	for _, e := range entries {
		if o, ok := stubs[e.blob]; ok {
			o.Path, o.mode, o.blob = e.path, e.mode, e.blob
			objs = append(objs, o)
		}
	}
	return objs, nil
}

// readBatch parses cat-file --batch output and returns the blobs that are
// stubs, decoded and keyed by blob name.
func readBatch(out []byte) (map[string]Object, error) {
	stubs := map[string]Object{}
	br := bufio.NewReader(bytes.NewReader(out))
	for {
		header, err := br.ReadString('\n')
		if err == io.EOF && header == "" {
			return stubs, nil
		}
		f := strings.Fields(header)
		if err != nil || len(f) != 3 {
			return nil, fmt.Errorf("git cat-file --batch: unexpected header %q", header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch: unexpected header %q", header)
		}
		// The content is followed by a newline.
		body := make([]byte, size+1)
		if _, err = io.ReadFull(br, body); err != nil {
			return nil, fmt.Errorf("git cat-file --batch: %w", err)
		}
		body = body[:size]
		if !fatstub.IsStub(body) {
			continue
		}
		if sha, n, derr := fatstub.Decode(body); derr == nil {
			stubs[f[0]] = Object{SHA: sha, Size: n}
		}
	}
}

// splitZ splits NUL-terminated git output into its records.
func splitZ(out []byte) []string {
	s := strings.TrimSuffix(string(out), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
