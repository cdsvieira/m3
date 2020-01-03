// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package fs

import (
	"fmt"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/persist"
	"github.com/m3db/m3/src/dbnode/persist/fs"
	"github.com/m3db/m3/src/dbnode/storage/block"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/bootstrapper"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/storage/index/convert"
	"github.com/m3db/m3/src/dbnode/storage/series"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/m3ninx/doc"
	"github.com/m3db/m3/src/m3ninx/index/segment"
	"github.com/m3db/m3/src/x/checked"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/instrument"
	"github.com/m3db/m3/src/x/pool"
	xsync "github.com/m3db/m3/src/x/sync"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

type runType int

const (
	bootstrapDataRunType runType = iota
	bootstrapIndexRunType
)

type newDataFileSetReaderFn func(
	bytesPool pool.CheckedBytesPool,
	opts fs.Options,
) (fs.DataFileSetReader, error)

type fileSystemSource struct {
	opts              Options
	fsopts            fs.Options
	log               *zap.Logger
	idPool            ident.Pool
	newReaderFn       newDataFileSetReaderFn
	newReaderPoolOpts bootstrapper.NewReaderPoolOptions
	dataProcessors    xsync.WorkerPool
	indexProcessors   xsync.WorkerPool
	persistManager    *bootstrapper.SharedPersistManager
	metrics           fileSystemSourceMetrics
}

type fileSystemSourceMetrics struct {
	persistedIndexBlocksRead  tally.Counter
	persistedIndexBlocksWrite tally.Counter
}

func newFileSystemSource(opts Options) bootstrap.Source {
	iopts := opts.InstrumentOptions()
	scope := iopts.MetricsScope().SubScope("fs-bootstrapper")
	iopts = iopts.SetMetricsScope(scope)
	opts = opts.SetInstrumentOptions(iopts)

	dataProcessors := xsync.NewWorkerPool(opts.BoostrapDataNumProcessors())
	dataProcessors.Init()
	indexProcessors := xsync.NewWorkerPool(opts.BoostrapIndexNumProcessors())
	indexProcessors.Init()

	s := &fileSystemSource{
		opts:            opts,
		fsopts:          opts.FilesystemOptions(),
		log:             iopts.Logger().With(zap.String("bootstrapper", "filesystem")),
		idPool:          opts.IdentifierPool(),
		newReaderFn:     fs.NewReader,
		dataProcessors:  dataProcessors,
		indexProcessors: indexProcessors,
		persistManager: &bootstrapper.SharedPersistManager{
			Mgr: opts.PersistManager(),
		},
		metrics: fileSystemSourceMetrics{
			persistedIndexBlocksRead:  scope.Counter("persist-index-blocks-read"),
			persistedIndexBlocksWrite: scope.Counter("persist-index-blocks-write"),
		},
	}
	s.newReaderPoolOpts.Alloc = s.newReader

	return s
}

func (s *fileSystemSource) AvailableData(
	md namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
	runOpts bootstrap.RunOptions,
) (result.ShardTimeRanges, error) {
	return s.availability(md, shardsTimeRanges)
}

func (s *fileSystemSource) AvailableIndex(
	md namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
	runOpts bootstrap.RunOptions,
) (result.ShardTimeRanges, error) {
	return s.availability(md, shardsTimeRanges)
}

func (s *fileSystemSource) Read(
	namespaces bootstrap.Namespaces,
) (bootstrap.NamespaceResults, error) {
	results := bootstrap.NamespaceResults{
		Results: bootstrap.NewNamespaceResultsMap(bootstrap.NamespaceResultsMapOptions{}),
	}

	// NB(r): Perform all data bootstrapping first then index bootstrapping
	// to more clearly deliniate which process is slower than the other.
	nowFn := s.opts.ResultOptions().ClockOptions().NowFn()
	start := nowFn()
	s.log.Info("bootstrapping time series data start")
	for _, elem := range namespaces.Namespaces.Iter() {
		namespace := elem.Value()
		md := namespace.Metadata

		r, err := s.read(bootstrapDataRunType, md, namespace.DataAccumulator,
			namespace.DataRunOptions.ShardTimeRanges,
			namespace.DataRunOptions.RunOptions)
		if err != nil {
			return bootstrap.NamespaceResults{}, err
		}

		results.Results.Set(md.ID(), bootstrap.NamespaceResult{
			Metadata:   md,
			Shards:     namespace.Shards,
			DataResult: r.data,
		})
	}
	s.log.Info("bootstrapping time series data success",
		zap.Duration("took", nowFn().Sub(start)))

	start = nowFn()
	s.log.Info("bootstrapping index metadata start")
	for _, elem := range namespaces.Namespaces.Iter() {
		namespace := elem.Value()
		md := namespace.Metadata
		if !md.Options().IndexOptions().Enabled() {
			// Not bootstrapping for index.
			s.log.Info("bootstrapping for namespace disabled by options",
				zap.String("ns", md.ID().String()))
			continue
		}

		r, err := s.read(bootstrapIndexRunType, md, namespace.DataAccumulator,
			namespace.IndexRunOptions.ShardTimeRanges,
			namespace.IndexRunOptions.RunOptions)
		if err != nil {
			return bootstrap.NamespaceResults{}, err
		}

		result, ok := results.Results.Get(md.ID())
		if !ok {
			err = fmt.Errorf("missing expected result for namespace: %s",
				md.ID().String())
			return bootstrap.NamespaceResults{}, err
		}

		result.IndexResult = r.index

		results.Results.Set(md.ID(), result)
	}
	s.log.Info("bootstrapping index metadata success",
		zap.Stringer("took", nowFn().Sub(start)))

	return results, nil
}

func (s *fileSystemSource) availability(
	md namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
) (result.ShardTimeRanges, error) {
	result := make(map[uint32]xtime.Ranges, len(shardsTimeRanges))
	for shard, ranges := range shardsTimeRanges {
		result[shard] = s.shardAvailability(md.ID(), shard, ranges)
	}
	return result, nil
}

func (s *fileSystemSource) shardAvailability(
	namespace ident.ID,
	shard uint32,
	targetRangesForShard xtime.Ranges,
) xtime.Ranges {
	if targetRangesForShard.IsEmpty() {
		return xtime.Ranges{}
	}

	readInfoFilesResults := fs.ReadInfoFiles(s.fsopts.FilePathPrefix(),
		namespace, shard, s.fsopts.InfoReaderBufferSize(), s.fsopts.DecodingOptions())

	var tr xtime.Ranges
	for i := 0; i < len(readInfoFilesResults); i++ {
		result := readInfoFilesResults[i]
		if err := result.Err.Error(); err != nil {
			s.log.Error("unable to read info files in shardAvailability",
				zap.Uint32("shard", shard),
				zap.Stringer("namespace", namespace),
				zap.Error(err),
				zap.Any("targetRangesForShard", targetRangesForShard),
				zap.String("filepath", result.Err.Filepath()),
			)
			continue
		}
		info := result.Info
		t := xtime.FromNanoseconds(info.BlockStart)
		w := time.Duration(info.BlockSize)
		currRange := xtime.Range{Start: t, End: t.Add(w)}
		if targetRangesForShard.Overlaps(currRange) {
			tr = tr.AddRange(currRange)
		}
	}
	return tr
}

func (s *fileSystemSource) bootstrapFromReaders(
	run runType,
	ns namespace.Metadata,
	accumulator bootstrap.NamespaceDataAccumulator,
	runOpts bootstrap.RunOptions,
	readerPool *bootstrapper.ReaderPool,
	readersCh <-chan bootstrapper.TimeWindowReaders,
) *runResult {
	var (
		runResult  = newRunResult()
		resultOpts = s.opts.ResultOptions()
		wg         sync.WaitGroup
		processors xsync.WorkerPool
	)

	switch run {
	case bootstrapDataRunType:
		processors = s.dataProcessors
	case bootstrapIndexRunType:
		processors = s.indexProcessors
	default:
		panic(fmt.Errorf("unrecognized run type: %d", run))
	}

	for timeWindowReaders := range readersCh {
		timeWindowReaders := timeWindowReaders
		wg.Add(1)
		processors.Go(func() {
			s.loadShardReadersDataIntoShardResult(run, ns, accumulator,
				runOpts, runResult, resultOpts, timeWindowReaders, readerPool)
			wg.Done()
		})
	}
	wg.Wait()

	return runResult
}

// markRunResultErrorsAndUnfulfilled checks the list of times that had errors and makes
// sure that we don't return any blocks or bloom filters for them. In addition,
// it looks at any remaining (unfulfilled) ranges and makes sure they're marked
// as unfulfilled.
func (s *fileSystemSource) markRunResultErrorsAndUnfulfilled(
	runResult *runResult,
	requestedRanges result.ShardTimeRanges,
	remainingRanges result.ShardTimeRanges,
	timesWithErrors []time.Time,
) {
	// NB(xichen): this is the exceptional case where we encountered errors due to files
	// being corrupted, which should be fairly rare so we can live with the overhead. We
	// experimented with adding the series to a temporary map and only adding the temporary map
	// to the final result but adding series to large map with string keys is expensive, and
	// the current implementation saves the extra overhead of merging temporary map with the
	// final result.
	if len(timesWithErrors) > 0 {
		timesWithErrorsString := make([]string, len(timesWithErrors))
		for i := range timesWithErrors {
			timesWithErrorsString[i] = timesWithErrors[i].String()
		}
		s.log.Info("encounted errors for range",
			zap.String("requestedRanges", requestedRanges.SummaryString()),
			zap.Strings("timesWithErrors", timesWithErrorsString))
	}

	if !remainingRanges.IsEmpty() {
		runResult.Lock()
		for _, unfulfilled := range []result.ShardTimeRanges{
			runResult.data.Unfulfilled(),
			runResult.index.Unfulfilled(),
		} {
			unfulfilled.AddRanges(remainingRanges)
		}
		runResult.Unlock()
	}
}

func (s *fileSystemSource) loadShardReadersDataIntoShardResult(
	run runType,
	ns namespace.Metadata,
	accumulator bootstrap.NamespaceDataAccumulator,
	runOpts bootstrap.RunOptions,
	runResult *runResult,
	ropts result.Options,
	timeWindowReaders bootstrapper.TimeWindowReaders,
	readerPool *bootstrapper.ReaderPool,
) {
	var (
		blockPool                  = ropts.DatabaseBlockOptions().DatabaseBlockPool()
		seriesCachePolicy          = ropts.SeriesCachePolicy()
		indexBlockDocumentsBuilder segment.DocumentsBuilder
		timesWithErrors            []time.Time
		nsCtx                      = namespace.NewContextFrom(ns)
		docsPool                   = s.opts.DocumentArrayPool()
		batch                      = docsPool.Get()
	)
	defer func() {
		docsPool.Put(batch)
	}()

	requestedRanges := timeWindowReaders.Ranges
	remainingRanges := requestedRanges.Copy()
	shardReaders := timeWindowReaders.Readers
	for shard, shardReaders := range shardReaders {
		shard := uint32(shard)
		readers := shardReaders.Readers

		for _, r := range readers {
			var (
				timeRange = r.Range()
				start     = timeRange.Start
				blockSize = ns.Options().RetentionOptions().BlockSize()
				err       error
			)
			switch run {
			case bootstrapDataRunType:
				// Pass, since nothing to do.
			case bootstrapIndexRunType:
				indexBlockDocumentsBuilder, err = runResult.getOrAddDocumentsBuilder(start, ns, ropts)
			default:
				// Unreachable unless an internal method calls with a run type casted from int.
				panic(fmt.Errorf("invalid run type: %d", run))
			}

			flushBatch := bootstrapper.CreateFlushBatchFn(&runResult.RWMutex, batch, indexBlockDocumentsBuilder)
			numEntries := r.Entries()
			for i := 0; err == nil && i < numEntries; i++ {
				switch run {
				case bootstrapDataRunType:
					err = s.readNextEntryAndRecordBlock(nsCtx, accumulator, shard, r,
						runResult, start, blockSize, blockPool, seriesCachePolicy)
				case bootstrapIndexRunType:
					// We can just read the entry and index if performing an index run.
					err = s.readNextEntryAndMaybeIndex(r, batch, flushBatch)
				default:
					// Unreachable unless an internal method calls with a run type casted from int.
					panic(fmt.Errorf("invalid run type: %d", run))
				}
			}
			// NB(bodu): Only flush if we've experienced no errors up to this point.
			if err == nil {
				if len(batch) > 0 {
					err = flushBatch()
				}
			}

			if err == nil {
				// Validate the read results.
				var validateErr error
				switch run {
				case bootstrapDataRunType:
					if seriesCachePolicy == series.CacheAll {
						validateErr = r.Validate()
					} else {
						err = fmt.Errorf("invalid series cache policy: %s", seriesCachePolicy.String())
					}
				case bootstrapIndexRunType:
					validateErr = r.ValidateMetadata()
				default:
					// Unreachable unless an internal method calls with a run type casted from int.
					panic(fmt.Errorf("invalid run type: %d", run))
				}
				if validateErr != nil {
					err = fmt.Errorf("data validation failed: %v", validateErr)
				}
			}

			if err == nil && run == bootstrapIndexRunType {
				// Mark index block as fulfilled.
				fulfilled := result.ShardTimeRanges{
					shard: xtime.Ranges{}.AddRange(timeRange),
				}
				err = runResult.index.IndexResults().MarkFulfilled(start, fulfilled,
					ns.Options().IndexOptions())
			}

			if err == nil {
				remainingRanges.Subtract(result.ShardTimeRanges{
					shard: xtime.Ranges{}.AddRange(timeRange),
				})
			} else {
				s.log.Error(err.Error())
				timesWithErrors = append(timesWithErrors, timeRange.Start)
			}
		}
	}

	var (
		shouldPersist = s.shouldPersist(runOpts)
		noneRemaining = remainingRanges.IsEmpty()
	)
	if run == bootstrapIndexRunType && shouldPersist && noneRemaining {
		err := bootstrapper.PersistBootstrapIndexSegment(
			ns,
			requestedRanges,
			runResult.index.IndexResults(),
			s.persistManager,
			s.opts.ResultOptions(),
		)
		if err != nil {
			iopts := s.opts.ResultOptions().InstrumentOptions()
			instrument.EmitAndLogInvariantViolation(iopts, func(l *zap.Logger) {
				l.Error("persist fs index bootstrap failed",
					zap.Stringer("namespace", ns.ID()),
					zap.Stringer("requestedRanges", requestedRanges),
					zap.Error(err))
			})
		}
	}

	// Return readers to pool.
	for _, shardReaders := range shardReaders {
		for _, r := range shardReaders.Readers {
			if err := r.Close(); err == nil {
				readerPool.Put(r)
			}
		}
	}

	s.markRunResultErrorsAndUnfulfilled(runResult, requestedRanges,
		remainingRanges, timesWithErrors)
}

func (s *fileSystemSource) readNextEntryAndRecordBlock(
	nsCtx namespace.Context,
	accumulator bootstrap.NamespaceDataAccumulator,
	shardID uint32,
	r fs.DataFileSetReader,
	runResult *runResult,
	blockStart time.Time,
	blockSize time.Duration,
	blockPool block.DatabaseBlockPool,
	seriesCachePolicy series.CachePolicy,
) error {
	var (
		seriesBlock = blockPool.Get()
		id          ident.ID
		tagsIter    ident.TagIterator
		data        checked.Bytes
		err         error
	)

	defer func() {
		// Can finalize the ID and tags always.
		if id != nil {
			id.Finalize()
		}
		if tagsIter != nil {
			tagsIter.Close()
		}
	}()

	switch seriesCachePolicy {
	case series.CacheAll:
		id, tagsIter, data, _, err = r.Read()
	default:
		err = fmt.Errorf("invalid series cache policy: %s", seriesCachePolicy.String())
	}
	if err != nil {
		return fmt.Errorf("error reading data file: %v", err)
	}

	ref, err := accumulator.CheckoutSeriesWithLock(shardID, id, tagsIter)
	if err != nil {
		return fmt.Errorf("unable to checkout series: %v", err)
	}

	seg := ts.NewSegment(data, nil, ts.FinalizeHead)
	seriesBlock.Reset(blockStart, blockSize, seg, nsCtx)
	if err := ref.Series.LoadBlock(seriesBlock, series.WarmWrite); err != nil {
		return fmt.Errorf("unable to load block: %v", err)
	}

	return nil
}

func (s *fileSystemSource) readNextEntryAndMaybeIndex(
	r fs.DataFileSetReader,
	batch []doc.Document,
	flushBatch func() error,
) error {
	// If performing index run, then simply read the metadata and add to segment.
	id, tagsIter, _, _, err := r.ReadMetadata()
	if err != nil {
		return err
	}

	d, err := convert.FromMetricIter(id, tagsIter)
	// Finalize the ID and tags.
	id.Finalize()
	tagsIter.Close()
	if err != nil {
		return err
	}

	batch = append(batch, d)

	if len(batch) >= documentArrayPoolCapacity {
		return flushBatch()
	}

	return nil
}

func (s *fileSystemSource) read(
	run runType,
	md namespace.Metadata,
	accumulator bootstrap.NamespaceDataAccumulator,
	shardsTimeRanges result.ShardTimeRanges,
	runOpts bootstrap.RunOptions,
) (*runResult, error) {
	var (
		seriesCachePolicy = s.opts.ResultOptions().SeriesCachePolicy()
		res               *runResult
	)
	if shardsTimeRanges.IsEmpty() {
		return newRunResult(), nil
	}

	setOrMergeResult := func(newResult *runResult) {
		if newResult == nil {
			return
		}
		if res == nil {
			res = newResult
		} else {
			res = res.mergedResult(newResult)
		}
	}

	if run == bootstrapDataRunType {
		if seriesCachePolicy != series.CacheAll {
			// Unless we're caching all series (or all series metadata) in memory, we
			// return just the availability of the files we have.
			return s.bootstrapDataRunResultFromAvailability(md, shardsTimeRanges), nil
		}
	}

	if run == bootstrapIndexRunType {
		// NB(r): First read all the FSTs and add to runResult index results,
		// subtract the shard + time ranges from what we intend to bootstrap
		// for those we found.
		r, err := s.bootstrapFromIndexPersistedBlocks(md,
			shardsTimeRanges)
		if err != nil {
			s.log.Warn("filesystem bootstrapped failed to read persisted index blocks")
		} else {
			// We may have less we need to read
			shardsTimeRanges = shardsTimeRanges.Copy()
			shardsTimeRanges.Subtract(r.fulfilled)
			// Set or merge result.
			setOrMergeResult(r.result)
		}
	}

	// Create a reader pool once per bootstrap as we don't really want to
	// allocate and keep around readers outside of the bootstrapping process,
	// hence why its created on demand each time.
	readerPool := bootstrapper.NewReaderPool(s.newReaderPoolOpts)
	readersCh := make(chan bootstrapper.TimeWindowReaders)
	shouldPersistIndexBootstrap := run == bootstrapIndexRunType && s.shouldPersist(runOpts)
	var blockSize time.Duration
	switch run {
	case bootstrapDataRunType:
		blockSize = md.Options().RetentionOptions().BlockSize()
	case bootstrapIndexRunType:
		blockSize = md.Options().IndexOptions().BlockSize()
	default:
		panic(fmt.Errorf("unrecognized run type: %d", run))
	}
	runtimeOpts := s.opts.RuntimeOptionsManager().Get()
	go bootstrapper.EnqueueReaders(md, runOpts, runtimeOpts, s.fsopts, shardsTimeRanges,
		readerPool, readersCh, shouldPersistIndexBootstrap, blockSize, s.log)
	bootstrapFromDataReadersResult := s.bootstrapFromReaders(run, md,
		accumulator, runOpts, readerPool, readersCh)

	// Merge any existing results if necessary.
	setOrMergeResult(bootstrapFromDataReadersResult)

	return res, nil
}

func (s *fileSystemSource) newReader() (fs.DataFileSetReader, error) {
	bytesPool := s.opts.ResultOptions().DatabaseBlockOptions().BytesPool()
	return s.newReaderFn(bytesPool, s.fsopts)
}

func (s *fileSystemSource) bootstrapDataRunResultFromAvailability(
	md namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
) *runResult {
	runResult := newRunResult()
	unfulfilled := runResult.data.Unfulfilled()
	for shard, ranges := range shardsTimeRanges {
		if ranges.IsEmpty() {
			continue
		}
		availability := s.shardAvailability(md.ID(), shard, ranges)
		remaining := ranges.RemoveRanges(availability)
		if !remaining.IsEmpty() {
			unfulfilled.AddRanges(result.ShardTimeRanges{
				shard: remaining,
			})
		}
	}
	runResult.data.SetUnfulfilled(unfulfilled)
	return runResult
}

type bootstrapFromIndexPersistedBlocksResult struct {
	fulfilled result.ShardTimeRanges
	result    *runResult
}

func (s *fileSystemSource) bootstrapFromIndexPersistedBlocks(
	ns namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
) (bootstrapFromIndexPersistedBlocksResult, error) {
	res := bootstrapFromIndexPersistedBlocksResult{
		fulfilled: result.ShardTimeRanges{},
	}

	indexBlockSize := ns.Options().IndexOptions().BlockSize()
	infoFiles := fs.ReadIndexInfoFiles(s.fsopts.FilePathPrefix(), ns.ID(),
		s.fsopts.InfoReaderBufferSize())

	for _, infoFile := range infoFiles {
		if err := infoFile.Err.Error(); err != nil {
			s.log.Error("unable to read index info file",
				zap.Stringer("namespace", ns.ID()),
				zap.Error(err),
				zap.Stringer("shardsTimeRanges", shardsTimeRanges),
				zap.String("filepath", infoFile.Err.Filepath()),
			)
			continue
		}

		info := infoFile.Info
		indexBlockStart := xtime.UnixNano(info.BlockStart).ToTime()
		indexBlockRange := xtime.Range{
			Start: indexBlockStart,
			End:   indexBlockStart.Add(indexBlockSize),
		}
		willFulfill := result.ShardTimeRanges{}
		for _, shard := range info.Shards {
			tr, ok := shardsTimeRanges[shard]
			if !ok {
				// No ranges match for this shard.
				continue
			}

			iter := tr.Iter()
			for iter.Next() {
				curr := iter.Value()
				intersection, intersects := curr.Intersect(indexBlockRange)
				if !intersects {
					continue
				}
				willFulfill[shard] = willFulfill[shard].AddRange(intersection)
			}
		}

		if willFulfill.IsEmpty() {
			// No matching shard/time ranges with this block.
			continue
		}

		segments, err := fs.ReadIndexSegments(fs.ReadIndexSegmentsOptions{
			ReaderOptions: fs.IndexReaderOpenOptions{
				Identifier:  infoFile.ID,
				FileSetType: persist.FileSetFlushType,
			},
			FilesystemOptions: s.fsopts,
		})
		if err != nil {
			s.log.Error("unable to read segments from index fileset",
				zap.Stringer("namespace", ns.ID()),
				zap.Error(err),
				zap.Time("blockStart", indexBlockStart),
				zap.Int("volumeIndex", infoFile.ID.VolumeIndex),
			)
			continue
		}

		// Track success.
		s.metrics.persistedIndexBlocksRead.Inc(1)

		// Record result.
		if res.result == nil {
			res.result = newRunResult()
		}
		segmentsFulfilled := willFulfill
		indexBlock := result.NewIndexBlock(indexBlockStart, segments,
			segmentsFulfilled)
		// NB(r): Don't need to call MarkFulfilled on the IndexResults here
		// as we've already passed the ranges fulfilled to the block that
		// we place in the IndexResuts with the call to Add(...).
		res.result.index.Add(indexBlock, nil)
		res.fulfilled.AddRanges(segmentsFulfilled)
	}

	return res, nil
}

func (s *fileSystemSource) shouldPersist(runOpts bootstrap.RunOptions) bool {
	persistConfig := runOpts.PersistConfig()
	return persistConfig.Enabled && persistConfig.FileSetType == persist.FileSetFlushType
}

type runResult struct {
	sync.RWMutex
	data  result.DataBootstrapResult
	index result.IndexBootstrapResult
}

func newRunResult() *runResult {
	return &runResult{
		data:  result.NewDataBootstrapResult(),
		index: result.NewIndexBootstrapResult(),
	}
}

func (r *runResult) getOrAddDocumentsBuilder(
	start time.Time,
	ns namespace.Metadata,
	ropts result.Options,
) (segment.DocumentsBuilder, error) {
	// Only called once per shard so ok to acquire write lock immediately.
	r.Lock()
	defer r.Unlock()

	indexResults := r.index.IndexResults()
	indexBlockDocumentsBuilder, err := indexResults.GetOrAddDocumentsBuilder(start,
		ns.Options().IndexOptions(), ropts)
	return indexBlockDocumentsBuilder, err
}

func (r *runResult) mergedResult(other *runResult) *runResult {
	return &runResult{
		data:  result.MergedDataBootstrapResult(r.data, other.data),
		index: result.MergedIndexBootstrapResult(r.index, other.index),
	}
}