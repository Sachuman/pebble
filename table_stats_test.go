// Copyright 2020 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/manifest"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/sstable/colblk"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func TestTableStats(t *testing.T) {
	// loadedInfo is protected by d.mu.
	var loadedInfo *TableStatsInfo
	opts := &Options{
		Comparer:                    testkeys.Comparer,
		DisableAutomaticCompactions: true,
		FormatMajorVersion:          FormatMinSupported,
		FS:                          vfs.NewMem(),
		EventListener: &EventListener{
			TableStatsLoaded: func(info TableStatsInfo) {
				loadedInfo = &info
			},
		},
		Logger: testLogger{t},
	}

	d, err := Open("", opts)
	require.NoError(t, err)
	defer func() {
		if d != nil {
			require.NoError(t, closeAllSnapshots(d))
			require.NoError(t, d.Close())
		}
	}()

	datadriven.RunTest(t, "testdata/table_stats", func(t *testing.T, td *datadriven.TestData) string {
		switch td.Cmd {
		case "disable":
			d.mu.Lock()
			d.opts.DisableTableStats = true
			d.mu.Unlock()
			return ""

		case "enable":
			d.mu.Lock()
			d.opts.DisableTableStats = false
			d.maybeCollectTableStatsLocked()
			d.mu.Unlock()
			return ""

		case "define":
			require.NoError(t, closeAllSnapshots(d))
			require.NoError(t, d.Close())
			loadedInfo = nil

			d, err = runDBDefineCmd(td, opts)
			if err != nil {
				return err.Error()
			}
			d.mu.Lock()
			s := d.mu.versions.currentVersion().String()
			d.mu.Unlock()
			return s

		case "reopen":
			require.NoError(t, d.Close())
			loadedInfo = nil

			// Open using existing file system.
			d, err = Open("", opts)
			require.NoError(t, err)
			return ""

		case "batch":
			b := d.NewBatch()
			if err := runBatchDefineCmd(td, b); err != nil {
				return err.Error()
			}
			b.Commit(nil)
			return ""

		case "flush":
			if err := d.Flush(); err != nil {
				return err.Error()
			}

			d.mu.Lock()
			s := d.mu.versions.currentVersion().String()
			d.mu.Unlock()
			return s

		case "ingest":
			if err = runBuildCmd(td, d, d.opts.FS); err != nil {
				return err.Error()
			}
			if err = runIngestCmd(td, d, d.opts.FS); err != nil {
				return err.Error()
			}
			d.mu.Lock()
			s := d.mu.versions.currentVersion().String()
			d.mu.Unlock()
			return s

		case "metric":
			m := d.Metrics()
			// TODO(jackson): Make a generalized command that uses reflection to
			// pull out arbitrary Metrics fields.
			var buf bytes.Buffer
			for _, arg := range td.CmdArgs {
				switch arg.String() {
				case "keys.missized-tombstones-count":
					fmt.Fprintf(&buf, "%s: %d", arg.String(), m.Keys.MissizedTombstonesCount)
				default:
					return fmt.Sprintf("unrecognized metric %s", arg)
				}
			}
			return buf.String()

		case "lsm":
			d.mu.Lock()
			s := d.mu.versions.currentVersion().String()
			d.mu.Unlock()
			return s

		case "build":
			if err := runBuildCmd(td, d, d.opts.FS); err != nil {
				return err.Error()
			}
			return ""

		case "ingest-and-excise":
			if err := runIngestAndExciseCmd(td, d); err != nil {
				return err.Error()
			}
			// Wait for a possible flush.
			d.mu.Lock()
			for d.mu.compact.flushing {
				d.mu.compact.cond.Wait()
			}
			d.mu.Unlock()
			return ""

		case "wait-pending-table-stats":
			return runTableStatsCmd(td, d)

		case "wait-loaded-initial":
			d.mu.Lock()
			for d.mu.tableStats.loading || !d.mu.tableStats.loadedInitial {
				d.mu.tableStats.cond.Wait()
			}
			s := loadedInfo.String()
			d.mu.Unlock()
			return s

		case "compact":
			if err := runCompactCmd(td, d); err != nil {
				return err.Error()
			}
			d.mu.Lock()
			// Disable the "dynamic base level" code for this test.
			d.mu.versions.picker.forceBaseLevel1()
			s := d.mu.versions.currentVersion().String()
			d.mu.Unlock()
			return s

		case "metadata-stats":
			// Prints some metadata about some sstable which is currently in the
			// latest version.
			return runMetadataCommand(t, td, d)

		case "properties":
			return runSSTablePropertiesCmd(t, td, d)

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func TestTableRangeDeletionIter(t *testing.T) {
	var m *manifest.TableMetadata
	cmp := testkeys.Comparer
	keySchema := colblk.DefaultKeySchema(cmp, 16)
	fs := vfs.NewMem()
	datadriven.RunTest(t, "testdata/table_stats_deletion_iter", func(t *testing.T, td *datadriven.TestData) string {
		switch cmd := td.Cmd; cmd {
		case "build":
			f, err := fs.Create("tmp.sst", vfs.WriteCategoryUnspecified)
			if err != nil {
				return err.Error()
			}
			w := sstable.NewRawWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
				Comparer:    cmp,
				KeySchema:   &keySchema,
				TableFormat: sstable.TableFormatMax,
			})
			m = &manifest.TableMetadata{}
			for _, line := range strings.Split(td.Input, "\n") {
				err = w.EncodeSpan(keyspan.ParseSpan(line))
				if err != nil {
					return err.Error()
				}
			}
			if err = w.Close(); err != nil {
				return err.Error()
			}
			meta, err := w.Metadata()
			if err != nil {
				return err.Error()
			}
			if meta.HasPointKeys {
				m.ExtendPointKeyBounds(cmp.Compare, meta.SmallestPoint, meta.LargestPoint)
			}
			if meta.HasRangeDelKeys {
				m.ExtendPointKeyBounds(cmp.Compare, meta.SmallestRangeDel, meta.LargestRangeDel)
			}
			if meta.HasRangeKeys {
				m.ExtendRangeKeyBounds(cmp.Compare, meta.SmallestRangeKey, meta.LargestRangeKey)
			}
			return m.DebugString(cmp.FormatKey, false /* verbose */)
		case "spans":
			f, err := fs.Open("tmp.sst")
			if err != nil {
				return err.Error()
			}
			var r *sstable.Reader
			readable, err := sstable.NewSimpleReadable(f)
			if err != nil {
				return err.Error()
			}
			r, err = sstable.NewReader(context.Background(), readable, sstable.ReaderOptions{
				Comparer:   cmp,
				KeySchemas: sstable.KeySchemas{keySchema.Name: &keySchema},
			})
			if err != nil {
				return errors.CombineErrors(err, readable.Close()).Error()
			}
			defer r.Close()
			iter, err := newCombinedDeletionKeyspanIter(context.Background(), cmp, r, m, sstable.NoReadEnv)
			if err != nil {
				return err.Error()
			}
			defer iter.Close()
			var buf bytes.Buffer
			s, err := iter.First()
			for ; s != nil; s, err = iter.Next() {
				buf.WriteString(s.String() + "\n")
			}
			if err != nil {
				return err.Error()
			}
			if buf.Len() == 0 {
				return "(none)"
			}
			return buf.String()
		default:
			return fmt.Sprintf("unknown command: %s", cmd)
		}
	})
}
