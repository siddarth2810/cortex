package storegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/oklog/ulid/v2"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/extprom"

	"github.com/prometheus/prometheus/tsdb"

	"github.com/cortexproject/cortex/pkg/storage/bucket"
	"github.com/cortexproject/cortex/pkg/storage/tsdb/bucketindex"
	cortex_testutil "github.com/cortexproject/cortex/pkg/storage/tsdb/testutil"
)

func TestIgnoreDeletionMarkFilter_Filter(t *testing.T) {
	t.Parallel()
	testIgnoreDeletionMarkFilter(t, false)
}

func TestIgnoreDeletionMarkFilter_FilterWithBucketIndex(t *testing.T) {
	// parallel testing causes data race
	testIgnoreDeletionMarkFilter(t, true)
}

func testIgnoreDeletionMarkFilter(t *testing.T, bucketIndexEnabled bool) {
	// parallel testing causes data race
	const userID = "user-1"

	now := time.Now()
	ctx := context.Background()
	logger := log.NewNopLogger()

	// Create a bucket backed by filesystem.
	bkt, _ := cortex_testutil.PrepareFilesystemBucket(t)
	bkt = bucketindex.BucketWithGlobalMarkers(bkt)
	userBkt := bucket.NewUserBucketClient(userID, bkt, nil)

	shouldFetch := &metadata.DeletionMark{
		ID:           ulid.MustNew(1, nil),
		DeletionTime: now.Add(-15 * time.Hour).Unix(),
		Version:      1,
	}

	shouldIgnore := &metadata.DeletionMark{
		ID:           ulid.MustNew(2, nil),
		DeletionTime: now.Add(-60 * time.Hour).Unix(),
		Version:      1,
	}

	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(&shouldFetch))
	require.NoError(t, userBkt.Upload(ctx, path.Join(shouldFetch.ID.String(), metadata.DeletionMarkFilename), &buf))
	require.NoError(t, json.NewEncoder(&buf).Encode(&shouldIgnore))
	require.NoError(t, userBkt.Upload(ctx, path.Join(shouldIgnore.ID.String(), metadata.DeletionMarkFilename), &buf))
	require.NoError(t, userBkt.Upload(ctx, path.Join(ulid.MustNew(3, nil).String(), metadata.DeletionMarkFilename), bytes.NewBufferString("not a valid deletion-mark.json")))

	// Create the bucket index if required.
	var idx *bucketindex.Index
	if bucketIndexEnabled {
		var err error

		u := bucketindex.NewUpdater(bkt, userID, nil, logger)
		idx, _, _, err = u.UpdateIndex(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, bucketindex.WriteIndex(ctx, bkt, userID, nil, idx))
	}

	inputMetas := map[ulid.ULID]*metadata.Meta{
		ulid.MustNew(1, nil): {},
		ulid.MustNew(2, nil): {},
		ulid.MustNew(3, nil): {},
		ulid.MustNew(4, nil): {},
	}

	expectedMetas := map[ulid.ULID]*metadata.Meta{
		ulid.MustNew(1, nil): {},
		ulid.MustNew(3, nil): {},
		ulid.MustNew(4, nil): {},
	}

	expectedDeletionMarks := map[ulid.ULID]*metadata.DeletionMark{
		ulid.MustNew(1, nil): shouldFetch,
		ulid.MustNew(2, nil): shouldIgnore,
	}

	synced := extprom.NewTxGaugeVec(nil, prometheus.GaugeOpts{Name: "synced"}, []string{"state"})
	modified := extprom.NewTxGaugeVec(nil, prometheus.GaugeOpts{Name: "modified"}, []string{"state"})
	f := NewIgnoreDeletionMarkFilter(logger, objstore.WithNoopInstr(userBkt), 48*time.Hour, 32)

	if bucketIndexEnabled {
		require.NoError(t, f.FilterWithBucketIndex(ctx, inputMetas, idx, synced))
	} else {
		require.NoError(t, f.Filter(ctx, inputMetas, synced, modified))
	}

	assert.Equal(t, 1.0, promtest.ToFloat64(synced.WithLabelValues(block.MarkedForDeletionMeta)))
	assert.Equal(t, expectedMetas, inputMetas)
	assert.Equal(t, expectedDeletionMarks, f.DeletionMarkBlocks())
}

func TestIgnoreNonQueryableBlocksFilter(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ctx := context.Background()
	logger := log.NewNopLogger()

	inputMetas := map[ulid.ULID]*metadata.Meta{
		ulid.MustNew(1, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-2 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-0 * time.Hour).UnixMilli(),
			},
		},
		ulid.MustNew(2, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-4 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-2 * time.Hour).UnixMilli(),
			},
		},
		ulid.MustNew(3, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-6 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-4 * time.Hour).UnixMilli(),
			},
		},
		ulid.MustNew(4, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-8 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-6 * time.Hour).UnixMilli(),
			},
		},
	}

	expectedMetas := map[ulid.ULID]*metadata.Meta{
		ulid.MustNew(2, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-4 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-2 * time.Hour).UnixMilli(),
			},
		},
		ulid.MustNew(3, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-6 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-4 * time.Hour).UnixMilli(),
			},
		},
		ulid.MustNew(4, nil): {
			BlockMeta: tsdb.BlockMeta{
				MinTime: now.Add(-8 * time.Hour).UnixMilli(),
				MaxTime: now.Add(-6 * time.Hour).UnixMilli(),
			},
		},
	}

	synced := extprom.NewTxGaugeVec(nil, prometheus.GaugeOpts{Name: "synced"}, []string{"state"})
	modified := extprom.NewTxGaugeVec(nil, prometheus.GaugeOpts{Name: "modified"}, []string{"state"})

	f := NewIgnoreNonQueryableBlocksFilter(logger, 3*time.Hour)

	require.NoError(t, f.Filter(ctx, inputMetas, synced, modified))
	assert.Equal(t, expectedMetas, inputMetas)
}
