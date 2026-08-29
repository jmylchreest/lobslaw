package compute

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Generated files accumulate without bound.
//
// Every image, video and speech file a turn produces lands in
// generated/ and stays there. Sessions have a retention pruner;
// artifacts had nothing, and a node that generates media steadily is a
// node that fills its disk — 19MB in thirteen files inside one
// afternoon on a test box, and a video is 1.5MB before anybody asks for
// a long one.
//
// A PERCENTAGE OF THE FILESYSTEM rather than a file count or a byte
// cap. The right number depends entirely on the volume it sits on: 500MB
// is nothing on a 4TB array and most of a container's scratch space, and
// an operator should not have to restate that as bytes for every
// deployment. A share of the disk means the same thing everywhere.
//
// AGE IS NOT THE PRIMARY AXIS, deliberately. Sessions prune by age
// because a transcript's value decays; a generated file's does not — a
// video from last week is as useful as one from this morning, and the
// one thing that reliably makes it a problem is the space it occupies.
// Age is available as a second condition for operators who want it.

// DefaultArtifactDiskPercent is the share of the filesystem generated
// artifacts may occupy before the oldest are swept.
//
// Five percent is small enough to be unobtrusive on any volume and
// large enough that a normal day of generation never reaches it. An
// operator who wants more says so.
const DefaultArtifactDiskPercent = 5.0

// ArtifactRetention bounds what generated/ may occupy.
type ArtifactRetention struct {
	// DiskPercent is the share of the filesystem the generated
	// directory may use, 0 < p <= 100. Zero takes the default;
	// negative disables sweeping entirely.
	DiskPercent float64

	// KeepDirs are subdirectories of the mount that are never swept.
	//
	// export/ is the one that matters: exporting a file is how a user
	// says "keep this", and a sweep that took those anyway would make
	// the gesture meaningless. See #200.
	KeepDirs []string

	Logger *slog.Logger
}

// sweepGenerated deletes the oldest generated files until they fit
// within the configured share of the filesystem.
//
// Best-effort and never fatal: the artifact that triggered this has
// already been written and delivered, and failing a generation because
// housekeeping could not run would trade a full disk for a lost result.
// Every failure is logged and the sweep gives up rather than guessing.
func sweepGenerated(root string, cfg ArtifactRetention) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	pct := cfg.DiskPercent
	switch {
	case pct < 0:
		return
	case pct == 0:
		pct = DefaultArtifactDiskPercent
	case pct > 100:
		pct = 100
	}

	dir := filepath.Join(root, generatedDir)
	total, err := filesystemBytes(dir)
	if err != nil {
		log.Warn("artifact: cannot size the filesystem; skipping the sweep",
			"dir", dir, "err", err)
		return
	}
	budget := int64(float64(total) * pct / 100)

	files, used, err := generatedFiles(dir)
	if err != nil {
		log.Warn("artifact: cannot walk generated files; skipping the sweep",
			"dir", dir, "err", err)
		return
	}
	if used <= budget {
		return
	}

	// Oldest first. Modification time rather than creation: a file
	// rewritten in place is a file somebody still cares about.
	sort.Slice(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })

	var freed int64
	var removed int
	for _, f := range files {
		if used-freed <= budget {
			break
		}
		if err := os.Remove(f.path); err != nil {
			log.Warn("artifact: sweep could not remove a file", "path", f.path, "err", err)
			continue
		}
		freed += f.size
		removed++
	}
	if removed > 0 {
		log.Info("artifact: swept generated files over the disk budget",
			"removed", removed, "freed_bytes", freed,
			"used_bytes", used-freed, "budget_bytes", budget,
			"disk_percent", pct)
	}
}

// generatedFile is one candidate, with the two facts the sweep needs.
type generatedFile struct {
	path    string
	size    int64
	modTime int64
}

// generatedFiles lists regular files under dir with their total size.
//
// Only the generated directory is walked. The sweep must never be able
// to reach the rest of the mount — the same tree carries the operator's
// workspace, inbound attachments and, once #200 lands, exports.
func generatedFiles(dir string) ([]generatedFile, int64, error) {
	var out []generatedFile
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A directory that does not exist yet is not an error: a
			// node that has generated nothing has nothing to sweep.
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, generatedFile{
			path: path, size: info.Size(), modTime: info.ModTime().UnixNano(),
		})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// filesystemBytes returns the size of the filesystem holding dir.
//
// The directory itself may not exist yet, so the walk climbs to the
// nearest ancestor that does: a budget is wanted before the first file
// is written, not after.
func filesystemBytes(dir string) (int64, error) {
	probe := dir
	for {
		var st syscall.Statfs_t
		if err := syscall.Statfs(probe, &st); err == nil {
			//nolint:unconvert,gosec // Bsize is int64 on some platforms and
			// uint32 on others; the conversion is required on one and a
			// no-op on the other.
			return int64(st.Blocks) * int64(st.Bsize), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, fmt.Errorf("artifact: no statfs-able ancestor of %q", dir)
		}
		probe = parent
	}
}
