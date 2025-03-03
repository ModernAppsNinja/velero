/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kopia

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/vmware-tanzu/velero/pkg/repository/udmrepo"
	"github.com/vmware-tanzu/velero/pkg/uploader"
	"github.com/vmware-tanzu/velero/pkg/util/logging"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/pkg/errors"
)

// All function mainly used to make testing more convenient
var applyRetentionPolicyFunc = policy.ApplyRetentionPolicy
var saveSnapshotFunc = snapshot.SaveSnapshot
var loadSnapshotFunc = snapshot.LoadSnapshot
var listSnapshotsFunc = snapshot.ListSnapshots
var filesystemEntryFunc = snapshotfs.FilesystemEntryFromIDWithPath
var restoreEntryFunc = restore.Entry

// SnapshotUploader which mainly used for UT test that could overwrite Upload interface
type SnapshotUploader interface {
	Upload(
		ctx context.Context,
		source fs.Entry,
		policyTree *policy.Tree,
		sourceInfo snapshot.SourceInfo,
		previousManifests ...*snapshot.Manifest,
	) (*snapshot.Manifest, error)
}

func newOptionalInt(b int) *policy.OptionalInt {
	ob := policy.OptionalInt(b)
	return &ob
}

func newOptionalBool(b bool) *policy.OptionalBool {
	ob := policy.OptionalBool(b)
	return &ob
}

// setupDefaultPolicy set default policy for kopia
func setupDefaultPolicy() *policy.Tree {
	defaultPolicy := *policy.DefaultPolicy

	defaultPolicy.RetentionPolicy.KeepLatest = newOptionalInt(math.MaxInt32)
	defaultPolicy.CompressionPolicy.CompressorName = "none"
	defaultPolicy.UploadPolicy.MaxParallelFileReads = newOptionalInt(runtime.NumCPU())
	defaultPolicy.UploadPolicy.ParallelUploadAboveSize = nil
	defaultPolicy.SchedulingPolicy.Manual = true
	defaultPolicy.ErrorHandlingPolicy.IgnoreUnknownTypes = newOptionalBool(true)

	return policy.BuildTree(nil, &defaultPolicy)
}

// Backup backup specific sourcePath and update progress
func Backup(ctx context.Context, fsUploader SnapshotUploader, repoWriter repo.RepositoryWriter, sourcePath string, realSource string,
	forceFull bool, parentSnapshot string, tags map[string]string, log logrus.FieldLogger) (*uploader.SnapshotInfo, bool, error) {
	if fsUploader == nil {
		return nil, false, errors.New("get empty kopia uploader")
	}
	dir, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, false, errors.Wrapf(err, "Invalid source path '%s'", sourcePath)
	}

	// to be consistent with restic when backup empty dir returns one error for upper logic handle
	dirs, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, errors.Wrapf(err, "Unable to read dir in path %s", dir)
	} else if len(dirs) == 0 {
		return nil, true, nil
	}

	dir = filepath.Clean(dir)

	sourceInfo := snapshot.SourceInfo{
		UserName: udmrepo.GetRepoUser(),
		Host:     udmrepo.GetRepoDomain(),
		Path:     filepath.Clean(realSource),
	}
	if sourceInfo.Path == "" {
		sourceInfo.Path = dir
	}

	rootDir, err := getLocalFSEntry(dir)
	if err != nil {
		return nil, false, errors.Wrap(err, "Unable to get local filesystem entry")
	}

	kopiaCtx := logging.SetupKopiaLog(ctx, log)
	snapID, snapshotSize, err := SnapshotSource(kopiaCtx, repoWriter, fsUploader, sourceInfo, rootDir, forceFull, parentSnapshot, tags, log, "Kopia Uploader")
	if err != nil {
		return nil, false, err
	}

	snapshotInfo := &uploader.SnapshotInfo{
		ID:   snapID,
		Size: snapshotSize,
	}

	return snapshotInfo, false, nil
}

func getLocalFSEntry(path0 string) (fs.Entry, error) {
	path, err := resolveSymlink(path0)
	if err != nil {
		return nil, errors.Wrap(err, "resolveSymlink")
	}

	e, err := localfs.NewEntry(path)
	if err != nil {
		return nil, errors.Wrap(err, "can't get local fs entry")
	}

	return e, nil
}

// resolveSymlink returns the path name after the evaluation of any symbolic links
func resolveSymlink(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", errors.Wrap(err, "stat")
	}

	if (st.Mode() & os.ModeSymlink) == 0 {
		return path, nil
	}

	return filepath.EvalSymlinks(path)
}

// SnapshotSource which setup policy for snapshot, upload snapshot, update progress
func SnapshotSource(
	ctx context.Context,
	rep repo.RepositoryWriter,
	u SnapshotUploader,
	sourceInfo snapshot.SourceInfo,
	rootDir fs.Entry,
	forceFull bool,
	parentSnapshot string,
	snapshotTags map[string]string,
	log logrus.FieldLogger,
	description string,
) (string, int64, error) {
	log.Info("Start to snapshot...")
	snapshotStartTime := time.Now()

	var previous []*snapshot.Manifest
	if !forceFull {
		if parentSnapshot != "" {
			log.Infof("Using provided parent snapshot %s", parentSnapshot)

			mani, err := loadSnapshotFunc(ctx, rep, manifest.ID(parentSnapshot))
			if err != nil {
				return "", 0, errors.Wrapf(err, "Failed to load previous snapshot %v from kopia", parentSnapshot)
			}

			previous = append(previous, mani)
		} else {
			log.Infof("Searching for parent snapshot")

			pre, err := findPreviousSnapshotManifest(ctx, rep, sourceInfo, snapshotTags, nil, log)
			if err != nil {
				return "", 0, errors.Wrapf(err, "Failed to find previous kopia snapshot manifests for si %v", sourceInfo)
			}

			previous = pre
		}
	} else {
		log.Info("Forcing full snapshot")
	}

	for i := range previous {
		log.Infof("Using parent snapshot %s, start time %v, end time %v, description %s", previous[i].ID, previous[i].StartTime.ToTime(), previous[i].EndTime.ToTime(), previous[i].Description)
	}

	policyTree := setupDefaultPolicy()

	manifest, err := u.Upload(ctx, rootDir, policyTree, sourceInfo, previous...)
	if err != nil {
		return "", 0, errors.Wrapf(err, "Failed to upload the kopia snapshot for si %v", sourceInfo)
	}

	manifest.Tags = snapshotTags

	manifest.Description = description

	if _, err = saveSnapshotFunc(ctx, rep, manifest); err != nil {
		return "", 0, errors.Wrapf(err, "Failed to save kopia manifest %v", manifest.ID)
	}
	_, err = applyRetentionPolicyFunc(ctx, rep, sourceInfo, true)
	if err != nil {
		return "", 0, errors.Wrapf(err, "Failed to apply kopia retention policy for si %v", sourceInfo)
	}
	if err = rep.Flush(ctx); err != nil {
		return "", 0, errors.Wrapf(err, "Failed to flush kopia repository")
	}
	log.Infof("Created snapshot with root %v and ID %v in %v", manifest.RootObjectID(), manifest.ID, time.Since(snapshotStartTime).Truncate(time.Second))
	return reportSnapshotStatus(manifest, policyTree)
}

func reportSnapshotStatus(manifest *snapshot.Manifest, policyTree *policy.Tree) (string, int64, error) {
	manifestID := manifest.ID
	snapSize := manifest.Stats.TotalFileSize

	var errs []string
	if ds := manifest.RootEntry.DirSummary; ds != nil {
		for _, ent := range ds.FailedEntries {
			policy := policyTree.DefinedPolicy()
			if !(policy != nil && bool(*policy.ErrorHandlingPolicy.IgnoreUnknownTypes) && strings.Contains(ent.Error, fs.ErrUnknown.Error())) {
				errs = append(errs, fmt.Sprintf("Error when processing %v: %v", ent.EntryPath, ent.Error))
			}
		}
	}

	if len(errs) != 0 {
		return "", 0, errors.New(strings.Join(errs, "\n"))
	}

	return string(manifestID), snapSize, nil
}

// findPreviousSnapshotManifest returns the list of previous snapshots for a given source, including
// last complete snapshot following it.
func findPreviousSnapshotManifest(ctx context.Context, rep repo.Repository, sourceInfo snapshot.SourceInfo, snapshotTags map[string]string, noLaterThan *fs.UTCTimestamp, log logrus.FieldLogger) ([]*snapshot.Manifest, error) {
	man, err := listSnapshotsFunc(ctx, rep, sourceInfo)
	if err != nil {
		return nil, err
	}

	var previousComplete *snapshot.Manifest
	var result []*snapshot.Manifest

	for _, p := range man {
		log.Debugf("Found one snapshot %s, start time %v, incomplete %s, tags %v", p.ID, p.StartTime.ToTime(), p.IncompleteReason, p.Tags)

		requester, found := p.Tags[uploader.SnapshotRequesterTag]
		if !found {
			continue
		}

		if requester != snapshotTags[uploader.SnapshotRequesterTag] {
			continue
		}

		uploaderName, found := p.Tags[uploader.SnapshotUploaderTag]
		if !found {
			continue
		}

		if uploaderName != snapshotTags[uploader.SnapshotUploaderTag] {
			continue
		}

		if noLaterThan != nil && p.StartTime.After(*noLaterThan) {
			continue
		}

		if p.IncompleteReason == "" && (previousComplete == nil || p.StartTime.After(previousComplete.StartTime)) {
			previousComplete = p
		}
	}

	if previousComplete != nil {
		result = append(result, previousComplete)
	}

	return result, nil
}

// Restore restore specific sourcePath with given snapshotID and update progress
func Restore(ctx context.Context, rep repo.RepositoryWriter, progress *Progress, snapshotID, dest string, log logrus.FieldLogger, cancleCh chan struct{}) (int64, int32, error) {
	log.Info("Start to restore...")

	kopiaCtx := logging.SetupKopiaLog(ctx, log)

	snapshot, err := snapshot.LoadSnapshot(kopiaCtx, rep, manifest.ID(snapshotID))
	if err != nil {
		return 0, 0, errors.Wrapf(err, "Unable to load snapshot %v", snapshotID)
	}

	log.Infof("Restore from snapshot %s, description %s, created time %v, tags %v", snapshotID, snapshot.Description, snapshot.EndTime.ToTime(), snapshot.Tags)

	rootEntry, err := filesystemEntryFunc(kopiaCtx, rep, snapshotID, false)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "Unable to get filesystem entry for snapshot %v", snapshotID)
	}

	path, err := filepath.Abs(dest)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "Unable to resolve path %v", dest)
	}

	output := &restore.FilesystemOutput{
		TargetPath:             path,
		OverwriteDirectories:   true,
		OverwriteFiles:         true,
		OverwriteSymlinks:      true,
		IgnorePermissionErrors: true,
	}

	err = output.Init(ctx)
	if err != nil {
		return 0, 0, errors.Wrap(err, "error to init output")
	}

	stat, err := restoreEntryFunc(kopiaCtx, rep, output, rootEntry, restore.Options{
		Parallel:               runtime.NumCPU(),
		RestoreDirEntryAtDepth: math.MaxInt32,
		Cancel:                 cancleCh,
		ProgressCallback: func(ctx context.Context, stats restore.Stats) {
			progress.ProgressBytes(stats.RestoredTotalFileSize, stats.EnqueuedTotalFileSize)
		},
	})

	if err != nil {
		return 0, 0, errors.Wrapf(err, "Failed to copy snapshot data to the target")
	}
	return stat.RestoredTotalFileSize, stat.RestoredFileCount, nil
}
