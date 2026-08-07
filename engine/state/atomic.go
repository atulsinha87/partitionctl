package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// fileHooks are test seams for the crash-safety tests. They are nil in
// production and the compiler folds the checks away.
//
// They exist because "write temp, fsync, rename" can only be shown to be
// crash-safe by stopping the process between the write and the rename, and no
// portable way to SIGKILL a goroutine at a chosen instruction exists.
type fileHooks struct {
	// beforeRename runs after the temporary file is fully written and synced
	// but before it is renamed over the target. Returning an error simulates
	// the process dying at exactly that instant.
	beforeRename func(target, temp string) error

	// afterRename runs after a successful rename, before the directory is
	// synced.
	afterRename func(target string) error
}

// atomicWriter writes records so that a reader never observes a partial one.
//
// The discipline is: create a temp file in a sibling directory on the same
// filesystem, write, fsync the file, close, rename over the target, fsync the
// containing directory. Rename within a filesystem is atomic, so a crash leaves
// either the whole old record or the whole new one. The temp directory is
// separate from the record directories so that a temp file abandoned by a crash
// is never mistaken for a record during a scan.
type atomicWriter struct {
	tmpDir string
	hooks  *fileHooks
}

func (w atomicWriter) writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ioErr("create record directory", err)
	}
	if err := os.MkdirAll(w.tmpDir, 0o750); err != nil {
		return ioErr("create temp directory", err)
	}

	f, err := os.CreateTemp(w.tmpDir, "record-*.tmp")
	if err != nil {
		return ioErr("create temp record", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return ioErr("write temp record", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return ioErr("sync temp record", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return ioErr("close temp record", err)
	}

	if w.hooks != nil && w.hooks.beforeRename != nil {
		if err := w.hooks.beforeRename(path, tmp); err != nil {
			// Deliberately leave the temp file behind. A crash here would, and
			// the sweep on the next Open is what proves the leftover is inert.
			return err
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return ioErr("rename record into place", err)
	}

	if w.hooks != nil && w.hooks.afterRename != nil {
		if err := w.hooks.afterRename(path); err != nil {
			return err
		}
	}

	syncDir(dir)
	return nil
}

func (w atomicWriter) writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ioErr("encode record", err)
	}
	return w.writeFile(path, append(data, '\n'))
}

// sweepTmp removes temporary files abandoned by a crash. It runs when a store
// is opened. Failure is not fatal: a leftover temp file is inert, it is only
// untidy.
func (w atomicWriter) sweepTmp() {
	entries, err := os.ReadDir(w.tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			_ = os.Remove(filepath.Join(w.tmpDir, e.Name()))
		}
	}
}

// syncDir fsyncs a directory so that a rename is durable, not merely visible.
// Not every platform or filesystem supports it, and a failure there is not a
// correctness problem for the rename itself, so the error is dropped
// deliberately.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// readJSON reads and decodes a record. A missing file is [ErrNotFound].
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound.Detailf("%s", filepath.Base(path))
		}
		return ioErr("read record", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return ioErr("decode record "+filepath.Base(path), err)
	}
	return nil
}

// segmentSafe is the set of bytes allowed verbatim in a path segment.
func segmentSafe(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-', b == '_', b == '.':
		return true
	}
	return false
}

// segmentReadableMax caps the human-readable part of an encoded path segment.
const segmentReadableMax = 48

// encodeSegment turns an arbitrary identifier into a filesystem-safe path
// segment.
//
// It is a total, deterministic function: the same id always yields the same
// segment, which is what lets a single node's record be addressed and rewritten
// atomically without scanning. The readable prefix is for the operator reading
// a directory listing; the hash suffix is what carries the identity, so two ids
// that sanitize to the same prefix still get different files. Records also
// carry their own id, so the reader verifies the match and a hash collision
// surfaces as an error rather than as silent corruption.
//
// A leading dot is escaped, so an id can never produce a hidden file, and the
// empty id can never produce an empty segment.
func encodeSegment(id string) string {
	var b strings.Builder
	b.Grow(segmentReadableMax + 20)
	for i := 0; i < len(id) && b.Len() < segmentReadableMax; i++ {
		c := id[i]
		if segmentSafe(c) && !(i == 0 && c == '.') {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(id))
	return b.String() + "-" + hex.EncodeToString(sum[:8])
}
