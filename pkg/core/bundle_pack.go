// Copyright © 2018 One Concern

package core

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"go.uber.org/zap"

	"github.com/oneconcern/datamon/pkg/storage"

	"gopkg.in/yaml.v2"

	"github.com/oneconcern/datamon/pkg/cafs"
	"github.com/oneconcern/datamon/pkg/model"
)

const (
	defaultBundleEntriesPerFile = 1000
	fileUploadsPerFlush         = 4
)

type filePacked struct {
	hash      string
	name      string
	keys      []byte
	size      uint64
	duplicate bool
	idx       int
	snapshot  time.Time
}

func filePacked2BundleEntry(packedFile filePacked) model.BundleEntry {
	return model.BundleEntry{
		Hash:         packedFile.hash,
		NameWithPath: packedFile.name,
		FileMode:     0, // #TODO: #35 file mode support
		Size:         packedFile.size,
		Timestamp:    packedFile.snapshot,
	}
}

type uploadBundleChans struct {
	// recv data from goroutines about uploaded files
	filePacked chan<- filePacked
	error      chan<- errorHit
	// signal file upload goroutines done by writing to this channel
	doneOk             chan<- struct{}
	concurrencyControl <-chan struct{}
}

func uploadBundleEntriesFileList(ctx context.Context, bundle *Bundle, fileList []model.BundleEntry) error {
	buffer, err := yaml.Marshal(model.BundleEntries{
		BundleEntries: fileList,
	})
	if err != nil {
		return err
	}
	msCRC, ok := bundle.MetaStore().(storage.StoreCRC)
	archivePathToBundleFileList := model.GetArchivePathToBundleFileList(
		bundle.RepoID,
		bundle.BundleID,
		bundle.BundleDescriptor.BundleEntriesFileCount)
	if ok {
		crc := crc32.Checksum(buffer, crc32.MakeTable(crc32.Castagnoli))
		bundle.l.Debug("uploadBundleEntriesFileList calling MetaStore.PutCRC",
			zap.String("archive path", archivePathToBundleFileList),
			zap.Int("BundleEntriesFileCount", int(bundle.BundleDescriptor.BundleEntriesFileCount)),
		)
		err = msCRC.PutCRC(ctx,
			archivePathToBundleFileList,
			bytes.NewReader(buffer), storage.NoOverWrite, crc)
	} else {
		bundle.l.Debug("uploadBundleEntriesFileList calling MetaStore.Put",
			zap.String("archive path", archivePathToBundleFileList),
			zap.Int("BundleEntriesFileCount", int(bundle.BundleDescriptor.BundleEntriesFileCount)),
		)
		err = bundle.MetaStore().Put(ctx,
			archivePathToBundleFileList,
			bytes.NewReader(buffer), storage.NoOverWrite)
	}
	if err != nil {
		return err
	}
	bundle.BundleDescriptor.BundleEntriesFileCount++
	return nil
}

func (b *Bundle) skipFile(file string) bool {
	exist, err := b.ConsumableStore.Has(context.Background(), file)
	if err != nil {
		b.l.Error("could not check if file exists",
			zap.String("file", file),
			zap.String("repo", b.RepoID),
			zap.String("bundleID", b.BundleID))
		exist = true // Code will decide later how to handle this file
	}
	return model.IsGeneratedFile(file) || (b.SkipOnError && !exist)
}

func uploadBundleFile(
	ctx context.Context,
	file string,
	cafsArchive cafs.Fs,
	fileReader io.Reader,
	chans uploadBundleChans,
	fileIdx int,
	logger *zap.Logger,
) {

	defer func() {
		<-chans.concurrencyControl
	}()
	logger.Debug("putting file in cafs",
		zap.String("filename", file),
	)

	putRes, e := cafsArchive.Put(ctx, fileReader)
	if e != nil {
		chans.error <- errorHit{
			error: e,
			file:  file,
		}
		return
	}

	chans.filePacked <- filePacked{
		hash:      putRes.Key.String(),
		keys:      putRes.Keys,
		name:      file,
		size:      uint64(putRes.Written),
		duplicate: putRes.Found,
		idx:       fileIdx,
	}
	logger.Debug("sent file packed result",
		zap.Int("idx", fileIdx),
	)
}

func uploadBundleFiles(
	ctx context.Context,
	bundle *Bundle,
	files []string,
	cafsArchive cafs.Fs,
	chans uploadBundleChans) {
	concurrencyControl := make(chan struct{}, bundle.concurrentFileUploads)
	chans.concurrencyControl = concurrencyControl
	for fileIdx, file := range files {
		// Check to see if the file is to be skipped.
		if bundle.skipFile(file) {
			bundle.l.Info("skipping file",
				zap.String("file", file),
				zap.String("repo", bundle.RepoID),
				zap.String("bundleID", bundle.BundleID),
			)
			continue
		}
		fileReader, err := bundle.ConsumableStore.Get(ctx, file)
		if err != nil {
			if bundle.SkipOnError {
				bundle.l.Info("skipping file",
					zap.String("file", file),
					zap.String("repo", bundle.RepoID),
					zap.String("bundleID", bundle.BundleID),
					zap.Error(err),
				)
				continue
			}
			chans.error <- errorHit{
				error: err,
				file:  file,
			}
			break
		}
		concurrencyControl <- struct{}{}
		bundle.l.Debug("kicking off upload file",
			zap.Int("idx", fileIdx),
		)
		if bundle.MetricsEnabled() {
			bundle.m.Volume.Bundles.Inc("Upload")
		}
		go uploadBundleFile(ctx, file, cafsArchive, fileReader, chans,
			fileIdx, bundle.l)
	}
	bundle.l.Debug("awaiting last uploads to complete",
		zap.Int("max possible remaining uploads", cap(concurrencyControl)),
	)
	/* once the buffered channel semaphore is filled with sentinel entries,
	 * all `uploadBundleFile` goroutines have exited.
	 */
	for i := 0; i < cap(concurrencyControl); i++ {
		concurrencyControl <- struct{}{}
	}
	bundle.l.Debug("upload threads finished. sending doneOk event.")
	chans.doneOk <- struct{}{}
}

func uploadBundle(ctx context.Context, bundle *Bundle, bundleEntriesPerFile uint, getKeys func() ([]string, error), opts ...Option) error {
	settings := defaultSettings()
	for _, apply := range opts {
		apply(&settings)
	}

	// Walk the entire tree
	// TODO: #53 handle large file count
	if getKeys == nil {
		getKeys = func() ([]string, error) {
			return bundle.ConsumableStore.Keys(context.Background())
		}
	}
	files, err := getKeys()
	if err != nil {
		return err
	}
	cafsArchive, err := cafs.New(
		cafs.LeafSize(bundle.BundleDescriptor.LeafSize),
		cafs.Backend(bundle.BlobStore()),
		cafs.ConcurrentFlushes(bundle.concurrentFileUploads/fileUploadsPerFlush),
		cafs.LeafTruncation(bundle.BundleDescriptor.Version < 1),
		cafs.Logger(bundle.l),
		cafs.WithMetrics(bundle.MetricsEnabled()),
		cafs.WithRetry(bundle.Retry),
	)
	if err != nil {
		return err
	}

	// Upload the files and the bundle list
	if bundle.BundleID == "" {
		err = bundle.InitializeBundleID()
		if err != nil {
			return err
		}
	}

	filePackedC := make(chan filePacked)
	errorC := make(chan errorHit)
	doneOkC := make(chan struct{})

	go uploadBundleFiles(ctx, bundle, files, cafsArchive, uploadBundleChans{
		filePacked: filePackedC,
		error:      errorC,
		doneOk:     doneOkC,
	})

	if settings.profilingEnabled {
		if err = writeMemProfile(opts...); err != nil {
			return err
		}
	}

	var (
		numFilePackedRes   int
		numFileListUploads int
		totalSize          uint64
	)

	fileList := make([]model.BundleEntry, 0, bundleEntriesPerFile)

	t0 := time.Now()
	defer func() {
		if bundle.MetricsEnabled() {
			bundle.m.Volume.IO.BundleFiles(int64(numFilePackedRes), int64(numFileListUploads), "Upload")
			bundle.m.Volume.IO.IORecord(t0, "Upload")(int64(totalSize), err)
		}
	}()

	for {
		var gotDoneSignal bool
		select {
		case f := <-filePackedC:
			numFilePackedRes++
			bundle.l.Debug("Uploaded file",
				zap.String("name", f.name),
				zap.Bool("duplicate", f.duplicate),
				zap.String("key", f.hash),
				zap.Int("num keys", len(f.keys)),
				zap.Int("idx", f.idx),
			)
			totalSize += f.size
			fileList = append(fileList, filePacked2BundleEntry(f))
			// Write the bundle entry file if reached max or the last one
			if len(fileList) == int(bundleEntriesPerFile) {
				bundle.l.Debug("Uploading filelist (max entries reached)")
				err = uploadBundleEntriesFileList(ctx, bundle, fileList)
				if err != nil {
					bundle.l.Error("Bundle upload failed.  Failed to upload bundle entries list.",
						zap.Error(err),
					)
					return err
				}
				numFileListUploads++
				fileList = fileList[:0]
			}
		case e := <-errorC:
			bundle.l.Error("Bundle upload failed. Failed to upload file",
				zap.Error(e.error),
				zap.String("file", e.file),
			)
			return e.error
		case <-doneOkC:
			bundle.l.Debug("Got upload done signal")
			gotDoneSignal = true
		}
		if gotDoneSignal {
			break
		}
	}
	if len(fileList) != 0 {
		bundle.l.Debug("Uploading filelist (final)")
		err = uploadBundleEntriesFileList(ctx, bundle, fileList)
		if err != nil {
			bundle.l.Error("Bundle upload failed.  Failed to upload bundle entries list.",
				zap.Error(err),
			)
			return err
		}
		numFileListUploads++
	}
	bundle.l.Info("uploaded filelists",
		zap.Int("actual number uploads attempted", numFileListUploads),
		zap.Int("approx expected number of uploads", maxInt(numFilePackedRes/int(bundleEntriesPerFile), 1)),
	)

	err = uploadBundleDescriptor(ctx, bundle)
	if err != nil {
		return err
	}
	bundle.l.Info("Uploaded bundle id",
		zap.String("BundleID", bundle.BundleID),
	)
	return nil
}

func validateBundle(bundle *Bundle) bool {
	if bundle.BundleDescriptor.Deduplication == "" {
		bundle.l.Error("failed to validate bundle, Deduplication scheme not set")
		return false
	}
	return true
}

func uploadBundleDescriptor(ctx context.Context, bundle *Bundle) error {
	if !validateBundle(bundle) {
		return fmt.Errorf("failed to validate bundle")
	}
	buffer, err := yaml.Marshal(bundle.BundleDescriptor)
	if err != nil {
		return err
	}
	msCRC, ok := bundle.MetaStore().(storage.StoreCRC)
	if ok {
		crc := crc32.Checksum(buffer, crc32.MakeTable(crc32.Castagnoli))
		err = msCRC.PutCRC(ctx,
			model.GetArchivePathToBundle(bundle.RepoID, bundle.BundleID),
			bytes.NewReader(buffer), storage.NoOverWrite, crc)

	} else {
		err = bundle.MetaStore().Put(ctx,
			model.GetArchivePathToBundle(bundle.RepoID, bundle.BundleID),
			bytes.NewReader(buffer), storage.NoOverWrite)
	}
	if err != nil {
		return err
	}
	return nil
}
