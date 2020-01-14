package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/oneconcern/datamon/pkg/errors"

	"github.com/spf13/afero"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	iradix "github.com/hashicorp/go-immutable-radix"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseutil"
)

const (
	// Cache duration
	cacheYearLong                    = 365 * 24 * time.Hour
	dirLinkCount     uint32          = 2
	fileLinkCount    uint32          = 1
	rootPath                         = "/"
	firstINode       fuseops.InodeID = 1023
	dirDefaultMode                   = 0777 | os.ModeDir
	fileDefaultMode                  = 0666
	dirReadOnlyMode                  = 0555 | os.ModeDir
	fileReadOnlyMode                 = 0444
	defaultUID                       = 0
	defaultGID                       = 0
	dirInitialSize                   = 64
)

// ReadOnlyFS is the virtual read-only filesystem created on top of a bundle.
type ReadOnlyFS struct {
	mfs        *fuse.MountedFileSystem // The mounted filesystem
	fsInternal *readOnlyFsInternal     // The core of the filesystem
	server     fuse.Server             // Fuse server
}

// MutableFS is the virtual mutable filesystem created on top of a bundle.
type MutableFS struct {
	mfs        *fuse.MountedFileSystem // The mounted filesystem
	fsInternal *fsMutable              // The core of the filesystem
	server     fuse.Server             // Fuse server
}

func checkBundle(bundle *Bundle) error {
	if bundle == nil {
		return fmt.Errorf("bundle is nil")
	}
	if bundle.l == nil {
		return fmt.Errorf("logger is nil")
	}
	return nil
}

// NewReadOnlyFS creates a new instance of the datamon filesystem.
func NewReadOnlyFS(bundle *Bundle) (*ReadOnlyFS, error) {
	if err := checkBundle(bundle); err != nil {
		return nil, err
	}
	fs := &readOnlyFsInternal{
		fsCommon: fsCommon{
			bundle:     bundle,
			lookupTree: iradix.New(),
			l:          bundle.l.With(zap.String("repo", bundle.RepoID), zap.String("bundle", bundle.BundleID)),
		},
		readDirMap:   make(map[fuseops.InodeID][]fuseutil.Dirent),
		fsEntryStore: iradix.New(),
		fsDirStore:   iradix.New(),
	}

	// Extract the meta information needed.
	err := Publish(context.Background(), fs.bundle)
	if err != nil {
		fs.l.Error("Failed to publish bundle", zap.String("id", bundle.BundleID), zap.Error(err))
		return nil, err
	}
	// Populate the filesystem with medatata
	//
	// NOTE: this fetches the bundle metadata only and builds
	// an in-memory view of the file system structure.
	// This may lead to a large memory footprint for bundles
	// with many files (e.g. thousands)
	//
	// TODO: reduce memory footprint
	return fs.populateFS(bundle)
}

func localPath(consumable fmt.Stringer) (string, error) {
	fullPath := consumable.String()
	// assume consumable is built with storage/localfs
	parts := strings.Split(fullPath, "@")
	if len(parts) < 2 || parts[0] != "localfs" {
		return "", errors.New("bundle doesn't have localfs consumable store to provide local cache for mutable fs")
	}
	return parts[1], nil
}

// NewMutableFS creates a new instance of the datamon filesystem.
func NewMutableFS(bundle *Bundle) (*MutableFS, error) {
	if err := checkBundle(bundle); err != nil {
		return nil, err
	}
	pathToStaging, err := localPath(bundle.ConsumableStore)
	if err != nil {
		return nil, err
	}
	bundle.l.Info("mutable mount staging storage", zap.String("path", pathToStaging))

	l := bundle.l.With(zap.String("repo", bundle.RepoID))
	if bundle.BundleID != "" {
		l = l.With(zap.String("bundle", bundle.BundleID))
	}
	fs := &fsMutable{
		fsCommon: fsCommon{
			bundle:     bundle,
			lookupTree: iradix.New(),
			l:          l,
		},
		readDirMap:   make(map[fuseops.InodeID]map[fuseops.InodeID]*fuseutil.Dirent),
		iNodeStore:   iradix.New(),
		backingFiles: make(map[fuseops.InodeID]*afero.File),
		lock:         sync.Mutex{},
		iNodeGenerator: iNodeGenerator{
			lock:         sync.Mutex{},
			highestInode: firstINode,
			freeInodes:   make([]fuseops.InodeID, 0, 65536),
		},
		localCache: afero.NewBasePathFs(afero.NewOsFs(), pathToStaging),
	}
	err = fs.initRoot()
	if err != nil {
		return nil, err
	}
	return &MutableFS{
		mfs:        nil,
		fsInternal: fs,
		server:     fuseutil.NewFileSystemServer(fs),
	}, nil
}

func prepPath(path string) error {
	return os.MkdirAll(path, dirDefaultMode)
}

// MountReadOnly a ReadOnlyFS
func (dfs *ReadOnlyFS) MountReadOnly(path string) error {
	err := prepPath(path)
	if err != nil {
		return err
	}
	// Reminder: Options are OS specific
	// options := make(map[string]string)
	// options["allow_other"] = ""
	el, _ := zap.NewStdLogAt(dfs.fsInternal.l.
		With(zap.String("fuse", "read-only mount"), zap.String("mountpoint", path)), zapcore.ErrorLevel)
	dl, _ := zap.NewStdLogAt(dfs.fsInternal.l.
		With(zap.String("fuse-debug", "read-only mount"), zap.String("mountpoint", path)), zapcore.DebugLevel)
	mountCfg := &fuse.MountConfig{
		Subtype:     "datamon", // mount appears as "fuse.datamon"
		ReadOnly:    true,
		FSName:      dfs.fsInternal.bundle.RepoID,
		VolumeName:  dfs.fsInternal.bundle.BundleID, // NOTE: OSX only option
		ErrorLogger: el,
		DebugLogger: dl,
		// Options:     options,
	}
	dfs.mfs, err = fuse.Mount(path, dfs.server, mountCfg)
	if err == nil {
		dfs.fsInternal.l.Info("mounting", zap.String("mountpoint", path))
	}
	return err
}

// MountMutable mounts a MutableFS as mutable (read-write)
func (dfs *MutableFS) MountMutable(path string) error {
	err := prepPath(path)
	if err != nil {
		return err
	}
	el, _ := zap.NewStdLogAt(dfs.fsInternal.l.
		With(zap.String("fuse", "mutable mount"), zap.String("mountpoint", path)), zapcore.ErrorLevel)
	dl, _ := zap.NewStdLogAt(dfs.fsInternal.l.
		With(zap.String("fuse-debug", "mutable mount"), zap.String("mountpoint", path)), zapcore.DebugLevel)
	// TODO plumb additional mount options
	mountCfg := &fuse.MountConfig{
		Subtype:     "datamon-mutable",
		FSName:      dfs.fsInternal.bundle.RepoID,
		VolumeName:  dfs.fsInternal.bundle.BundleID,
		ErrorLogger: el,
		DebugLogger: dl,
	}
	dfs.mfs, err = fuse.Mount(path, dfs.server, mountCfg)
	if err == nil {
		dfs.fsInternal.l.Info("mounting", zap.String("mountpoint", path))
	}
	return err
}

// Unmount a ReadOnlyFS
func (dfs *ReadOnlyFS) Unmount(path string) error {
	dfs.fsInternal.l.Info("unmounting", zap.String("mountpoint", path))
	return fuse.Unmount(path)
}

// JoinMount blocks until a mounted file system has been unmounted.
// It does not return successfully until all ops read from the connection have been responded to
// (i.e. the file system server has finished processing all in-flight ops).
func (dfs *ReadOnlyFS) JoinMount(ctx context.Context) error {
	return dfs.mfs.Join(ctx)
}

// Unmount a MutableFS
func (dfs *MutableFS) Unmount(path string) error {
	// On unmount, walk the FS and create a bundle
	_ = dfs.fsInternal.Commit()
	//if err != nil {
	// dump the metadata to the local FS to manually recover.
	//}
	dfs.fsInternal.l.Info("unmounting", zap.String("mountpoint", path))
	return fuse.Unmount(path)
}

// JoinMount blocks until a mounted file system has been unmounted.
// It does not return successfully until all ops read from the connection have been responded to
// (i.e. the file system server has finished processing all in-flight ops).
func (dfs *MutableFS) JoinMount(ctx context.Context) error {
	return dfs.mfs.Join(ctx)
}
