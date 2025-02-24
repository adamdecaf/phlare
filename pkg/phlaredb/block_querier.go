package phlaredb

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/status"
	"github.com/google/pprof/profile"
	"github.com/google/uuid"
	"github.com/grafana/dskit/multierror"
	"github.com/oklog/ulid"
	"github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/samber/lo"
	"github.com/segmentio/parquet-go"
	"golang.org/x/exp/constraints"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"

	profilev1 "github.com/grafana/phlare/api/gen/proto/go/google/v1"
	ingestv1 "github.com/grafana/phlare/api/gen/proto/go/ingester/v1"
	typesv1 "github.com/grafana/phlare/api/gen/proto/go/types/v1"
	"github.com/grafana/phlare/pkg/iter"
	phlaremodel "github.com/grafana/phlare/pkg/model"
	phlareobjstore "github.com/grafana/phlare/pkg/objstore"
	phlarecontext "github.com/grafana/phlare/pkg/phlare/context"
	"github.com/grafana/phlare/pkg/phlaredb/block"
	query "github.com/grafana/phlare/pkg/phlaredb/query"
	schemav1 "github.com/grafana/phlare/pkg/phlaredb/schemas/v1"
	"github.com/grafana/phlare/pkg/phlaredb/tsdb/index"
)

type tableReader interface {
	open(ctx context.Context, bucketReader phlareobjstore.BucketReader) error
	io.Closer
}

type BlockQuerier struct {
	phlarectx context.Context
	logger    log.Logger

	bucketReader phlareobjstore.BucketReader

	queriers     []*singleBlockQuerier
	queriersLock sync.RWMutex
}

func NewBlockQuerier(phlarectx context.Context, bucketReader phlareobjstore.BucketReader) *BlockQuerier {
	return &BlockQuerier{
		phlarectx: contextWithBlockMetrics(phlarectx,
			newBlocksMetrics(
				phlarecontext.Registry(phlarectx),
			),
		),
		bucketReader: bucketReader,
	}
}

// generates meta.json by opening block
func (b *BlockQuerier) reconstructMetaFromBlock(ctx context.Context, ulid ulid.ULID) (metas *block.Meta, err error) {
	fakeMeta := block.NewMeta()
	fakeMeta.ULID = ulid

	q := newSingleBlockQuerierFromMeta(b.phlarectx, b.bucketReader, fakeMeta)
	defer q.Close()

	meta, err := q.reconstructMeta(ctx)
	if err != nil {
		return nil, err
	}
	return meta, nil
}

func (b *BlockQuerier) BlockMetas(ctx context.Context) (metas []*block.Meta, _ error) {
	var names []ulid.ULID
	if err := b.bucketReader.Iter(ctx, "", func(n string) error {
		ulid, ok := block.IsBlockDir(n)
		if !ok {
			return nil
		}
		names = append(names, ulid)
		return nil
	}); err != nil {
		return nil, err
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(16)
	metas = make([]*block.Meta, len(names))
	for pos := range names {
		func(pos int) {
			g.Go(func() error {
				path := filepath.Join(names[pos].String(), block.MetaFilename)
				metaReader, err := b.bucketReader.Get(ctx, path)
				if err != nil {
					if b.bucketReader.IsObjNotFoundErr(err) {
						level.Warn(b.logger).Log("msg", block.MetaFilename+" not found in block try to generate it", "block", names[pos].String())

						meta, err := b.reconstructMetaFromBlock(ctx, names[pos])
						if err != nil {
							level.Error(b.logger).Log("msg", "error generating meta for block", "block", names[pos].String(), "err", err)
							return nil
						}

						metas[pos] = meta
						return nil
					}

					level.Error(b.logger).Log("msg", "error reading block meta", "block", path, "err", err)
					return nil
				}

				metas[pos], err = block.Read(metaReader)
				if err != nil {
					level.Error(b.logger).Log("msg", "error parsing block meta", "block", path, "err", err)
					return nil
				}
				return nil
			})
		}(pos)
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// sort slice and make sure nils are last
	sort.Slice(metas, func(i, j int) bool {
		if metas[i] == nil {
			return false
		}
		if metas[j] == nil {
			return true
		}
		return metas[i].MinTime < metas[j].MinTime
	})

	// iterate from the end and cut of till the first non-nil
	var pos int
	for pos = len(metas) - 1; pos >= 0; pos-- {
		if metas[pos] != nil {
			break
		}
	}

	return metas[0 : pos+1], nil
}

// Sync gradually scans the available blocks. If there are any changes to the
// last run it will Open/Close new/no longer existing ones.
func (b *BlockQuerier) Sync(ctx context.Context) error {
	observedMetas, err := b.BlockMetas(ctx)
	if err != nil {
		return err
	}

	// hold write lock to queriers
	b.queriersLock.Lock()

	// build lookup map

	querierByULID := make(map[ulid.ULID]*singleBlockQuerier)

	for pos := range b.queriers {
		querierByULID[b.queriers[pos].meta.ULID] = b.queriers[pos]
	}

	// ensure queries has the right length
	lenQueriers := len(observedMetas)
	if cap(b.queriers) < lenQueriers {
		b.queriers = make([]*singleBlockQuerier, lenQueriers)
	} else {
		b.queriers = b.queriers[:lenQueriers]
	}

	for pos, m := range observedMetas {

		q, ok := querierByULID[m.ULID]
		if ok {
			b.queriers[pos] = q
			delete(querierByULID, m.ULID)
			continue
		}

		b.queriers[pos] = newSingleBlockQuerierFromMeta(b.phlarectx, b.bucketReader, m)
	}
	// ensure queriers are in ascending order.
	sort.Slice(b.queriers, func(i, j int) bool {
		return b.queriers[i].meta.MinTime < b.queriers[j].meta.MinTime
	})
	b.queriersLock.Unlock()

	// now close no longer available queries
	for _, q := range querierByULID {
		if err := q.Close(); err != nil {
			return err
		}
	}

	return nil
}

func (b *BlockQuerier) Close() error {
	b.queriersLock.Lock()
	defer b.queriersLock.Unlock()

	errs := multierror.New()
	for pos := range b.queriers {
		if err := b.queriers[pos].Close(); err != nil {
			errs.Add(err)
		}
	}
	return errs.Err()
}

type TableInfo struct {
	Rows      uint64
	RowGroups uint64
	Bytes     uint64
}

type BlockInfo struct {
	ID          ulid.ULID
	MinTime     model.Time
	MaxTime     model.Time
	Profiles    TableInfo
	Stacktraces TableInfo
	Locations   TableInfo
	Functions   TableInfo
	Mappings    TableInfo
	Strings     TableInfo
	Series      uint64
}

func (b *BlockQuerier) BlockInfo() []BlockInfo {
	result := make([]BlockInfo, len(b.queriers))
	return result
}

func (b *BlockQuerier) queriersFor(start, end model.Time) Queriers {
	b.queriersLock.RLock()
	defer b.queriersLock.RUnlock()

	result := make(Queriers, 0, len(b.queriers))
	for _, q := range b.queriers {
		if q.InRange(start, end) {
			result = append(result, q)
		}
	}
	return result
}

type minMax struct {
	min, max model.Time
}

func (mm *minMax) InRange(start, end model.Time) bool {
	return block.InRange(mm.min, mm.max, start, end)
}

type singleBlockQuerier struct {
	logger  log.Logger
	metrics *blocksMetrics

	bucketReader phlareobjstore.BucketReader
	meta         *block.Meta

	tables []tableReader

	openLock              sync.Mutex
	opened                bool
	tsBoundaryPerRowGroup []minMax
	index                 *index.Reader
	strings               inMemoryparquetReader[*schemav1.StoredString, *schemav1.StringPersister]
	functions             inMemoryparquetReader[*profilev1.Function, *schemav1.FunctionPersister]
	locations             inMemoryparquetReader[*profilev1.Location, *schemav1.LocationPersister]
	mappings              inMemoryparquetReader[*profilev1.Mapping, *schemav1.MappingPersister]
	stacktraces           parquetReader[*schemav1.Stacktrace, *schemav1.StacktracePersister]
	profiles              parquetReader[*schemav1.Profile, *schemav1.ProfilePersister]
}

func newSingleBlockQuerierFromMeta(phlarectx context.Context, bucketReader phlareobjstore.BucketReader, meta *block.Meta) *singleBlockQuerier {
	q := &singleBlockQuerier{
		logger:  phlarecontext.Logger(phlarectx),
		metrics: contextBlockMetrics(phlarectx),

		bucketReader: phlareobjstore.BucketReaderWithPrefix(bucketReader, meta.ULID.String()),
		meta:         meta,
	}
	q.tables = []tableReader{
		&q.strings,
		&q.mappings,
		&q.functions,
		&q.locations,
		&q.stacktraces,
		&q.profiles,
	}
	return q
}

func (b *singleBlockQuerier) Close() error {
	b.openLock.Lock()
	defer b.openLock.Unlock()
	errs := multierror.New()
	if b.index != nil {
		err := b.index.Close()
		b.index = nil
		if err != nil {
			errs.Add(err)
		}
	}

	for _, t := range b.tables {
		if err := t.Close(); err != nil {
			errs.Add(err)
		}
	}
	return errs.Err()
}

func (b *singleBlockQuerier) InRange(start, end model.Time) bool {
	return b.meta.InRange(start, end)
}

// reconstructMeta can regenerate a missing metadata file from the parquet structures
func (b *singleBlockQuerier) reconstructMeta(ctx context.Context) (*block.Meta, error) {
	tsBoundary, _, err := b.readTSBoundaries(ctx)
	if err != nil {
		return nil, err
	}

	profilesInfo := b.profiles.info()
	indexInfo := b.index.FileInfo()

	files := []block.File{
		indexInfo,
		profilesInfo,
		b.stacktraces.info(),
		b.locations.info(),
		b.mappings.info(),
		b.functions.info(),
		b.strings.info(),
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})

	return &block.Meta{
		ULID:    b.meta.ULID,
		MinTime: tsBoundary.min,
		MaxTime: tsBoundary.max,
		Version: block.MetaVersion1,
		Stats: block.BlockStats{
			NumProfiles: profilesInfo.Parquet.NumRows,
		},
		Files: files,
	}, nil
}

type mapPredicate[K constraints.Integer, V any] struct {
	min K
	max K
	m   map[K]V
}

func newMapPredicate[K constraints.Integer, V any](m map[K]V) query.Predicate {
	p := &mapPredicate[K, V]{
		m: m,
	}

	first := true
	for k := range m {
		if first || p.max < k {
			p.max = k
		}
		if first || p.min > k {
			p.min = k
		}
		first = false
	}

	return p
}

func (m *mapPredicate[K, V]) KeepColumnChunk(c parquet.ColumnChunk) bool {
	if ci := c.ColumnIndex(); ci != nil {
		for i := 0; i < ci.NumPages(); i++ {
			min := K(ci.MinValue(i).Int64())
			max := K(ci.MaxValue(i).Int64())
			if m.max >= min && m.min <= max {
				return true
			}
		}
		return false
	}

	return true
}

func (m *mapPredicate[K, V]) KeepPage(page parquet.Page) bool {
	if min, max, ok := page.Bounds(); ok {
		return m.max >= K(min.Int64()) && m.min <= K(max.Int64())
	}
	return true
}

func (m *mapPredicate[K, V]) KeepValue(v parquet.Value) bool {
	_, exists := m.m[K(v.Int64())]
	return exists
}

type labelsInfo struct {
	fp  model.Fingerprint
	lbs phlaremodel.Labels
}

func (b *singleBlockQuerier) forMatchingProfiles(ctx context.Context, matchers []*labels.Matcher, start, end model.Time,
	fn func(lbs phlaremodel.Labels, profile *schemav1.Profile, samples []schemav1.Sample) error,
) error {
	postings, err := PostingsForMatchers(b.index, nil, matchers...)
	if err != nil {
		return err
	}

	var (
		lbls         = make(phlaremodel.Labels, 0, 6)
		chks         = make([]index.ChunkMeta, 1)
		lblsPerIndex = make(map[uint32]labelsInfo)
	)

	// get all relevant labels/fingerprints
	for postings.Next() {
		fp, err := b.index.Series(postings.At(), &lbls, &chks)
		if err != nil {
			return err
		}
		if lblsExisting, exists := lblsPerIndex[chks[0].SeriesIndex]; exists {
			// Compare to check if there is a clash
			if phlaremodel.CompareLabelPairs(lbls, lblsExisting.lbs) != 0 {
				panic("label hash conflict")
			}
		} else {
			lblsPerIndex[chks[0].SeriesIndex] = labelsInfo{
				fp:  model.Fingerprint(fp),
				lbs: lbls,
			}
			lbls = make(phlaremodel.Labels, 0, 6)
		}
	}

	rowNums := query.NewJoinIterator(
		0,
		[]query.Iterator{
			b.profiles.columnIter(ctx, "SeriesIndex", newMapPredicate(lblsPerIndex), "SeriesIndex"),                              // get all profiles with matching seriesRef
			b.profiles.columnIter(ctx, "TimeNanos", query.NewIntBetweenPredicate(start.UnixNano(), end.UnixNano()), "TimeNanos"), // get all profiles within the time window
			b.profiles.columnIter(ctx, "ID", nil, "ID"),                                                                          // get all IDs
			// TODO: Provide option to ignore samples
			b.profiles.columnIter(ctx, "Samples.list.element.StacktraceID", nil, "StacktraceIDs"),
			b.profiles.columnIter(ctx, "Samples.list.element.Value", nil, "SampleValues"),
		},
		nil,
	)
	defer rowNums.Close()
	var (
		samples       reconstructSamples
		profile       schemav1.Profile
		schemaSamples []schemav1.Sample
		series        [][]parquet.Value
	)

	// retrieve the full profiles
	for rowNums.Next() {
		result := rowNums.At()

		series = result.Columns(series, "ID", "TimeNanos", "SeriesIndex")
		var err error
		profile.ID, err = uuid.FromBytes(series[0][0].ByteArray())
		if err != nil {
			return err
		}
		profile.TimeNanos = series[1][0].Int64()
		profile.SeriesIndex = series[2][0].Uint32()

		samples.buffer = result.Columns(samples.buffer, "StacktraceIDs", "SampleValues")

		labelsInfo, matched := lblsPerIndex[profile.SeriesIndex]
		if !matched {
			return nil
		}
		profile.SeriesFingerprint = labelsInfo.fp

		schemaSamples = samples.samples(schemaSamples)

		if err := fn(labelsInfo.lbs, &profile, schemaSamples); err != nil {
			return err
		}
	}

	return rowNums.Err()
}

type Profile interface {
	Timestamp() model.Time
	Fingerprint() model.Fingerprint
	Labels() phlaremodel.Labels
}

type Querier interface {
	InRange(start, end model.Time) bool
	SelectMatchingProfiles(ctx context.Context, params *ingestv1.SelectProfilesRequest) (iter.Iterator[Profile], error)
	MergeByStacktraces(ctx context.Context, rows iter.Iterator[Profile]) (*ingestv1.MergeProfilesStacktracesResult, error)
	MergeByLabels(ctx context.Context, rows iter.Iterator[Profile], by ...string) ([]*typesv1.Series, error)
	MergePprof(ctx context.Context, rows iter.Iterator[Profile]) (*profile.Profile, error)

	// Sorts profiles for retrieval.
	Sort([]Profile) []Profile
}

type BlockProfile struct {
	labels phlaremodel.Labels
	fp     model.Fingerprint
	ts     model.Time
	RowNum int64
}

func (p BlockProfile) RowNumber() int64 {
	return p.RowNum
}

func (p BlockProfile) Labels() phlaremodel.Labels {
	return p.labels
}

func (p BlockProfile) Timestamp() model.Time {
	return p.ts
}

func (p BlockProfile) Fingerprint() model.Fingerprint {
	return p.fp
}

func (b *singleBlockQuerier) SelectMatchingProfiles(ctx context.Context, params *ingestv1.SelectProfilesRequest) (iter.Iterator[Profile], error) {
	sp, ctx := opentracing.StartSpanFromContext(ctx, "SelectMatchingProfiles - Block")
	defer sp.Finish()
	if err := b.open(ctx); err != nil {
		return nil, err
	}
	matchers, err := parser.ParseMetricSelector(params.LabelSelector)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "failed to parse label selectors: "+err.Error())
	}
	matchers = append(matchers, phlaremodel.SelectorFromProfileType(params.Type))

	postings, err := PostingsForMatchers(b.index, nil, matchers...)
	if err != nil {
		return nil, err
	}

	var (
		lbls       = make(phlaremodel.Labels, 0, 6)
		chks       = make([]index.ChunkMeta, 1)
		lblsPerRef = make(map[int64]labelsInfo)
	)

	// get all relevant labels/fingerprints
	for postings.Next() {
		fp, err := b.index.Series(postings.At(), &lbls, &chks)
		if err != nil {
			return nil, err
		}
		if lblsExisting, exists := lblsPerRef[int64(chks[0].SeriesIndex)]; exists {
			// Compare to check if there is a clash
			if phlaremodel.CompareLabelPairs(lbls, lblsExisting.lbs) != 0 {
				panic("label hash conflict")
			}
		} else {
			lblsPerRef[int64(chks[0].SeriesIndex)] = labelsInfo{
				fp:  model.Fingerprint(fp),
				lbs: lbls,
			}
			lbls = make(phlaremodel.Labels, 0, 6)
		}
	}
	pIt := query.NewJoinIterator(
		0,
		[]query.Iterator{
			b.profiles.columnIter(ctx, "SeriesIndex", newMapPredicate(lblsPerRef), "SeriesIndex"),
			b.profiles.columnIter(ctx, "TimeNanos", query.NewIntBetweenPredicate(model.Time(params.Start).UnixNano(), model.Time(params.End).UnixNano()), "TimeNanos"),
		},
		nil,
	)
	iters := make([]iter.Iterator[Profile], 0, len(lblsPerRef))
	buf := make([][]parquet.Value, 2)
	defer pIt.Close()

	currSeriesIndex := int64(-1)
	var currentSeriesSlice []Profile
	for pIt.Next() {
		res := pIt.At()
		buf = res.Columns(buf, "SeriesIndex", "TimeNanos")
		seriesIndex := buf[0][0].Int64()
		if seriesIndex != currSeriesIndex {
			currSeriesIndex++
			if len(currentSeriesSlice) > 0 {
				iters = append(iters, iter.NewSliceIterator(currentSeriesSlice))
			}
			currentSeriesSlice = make([]Profile, 0, 100)
		}
		currentSeriesSlice = append(currentSeriesSlice, BlockProfile{
			labels: lblsPerRef[seriesIndex].lbs,
			fp:     lblsPerRef[seriesIndex].fp,
			ts:     model.TimeFromUnixNano(buf[1][0].Int64()),
			RowNum: res.RowNumber[0],
		})
	}
	if len(currentSeriesSlice) > 0 {
		iters = append(iters, iter.NewSliceIterator(currentSeriesSlice))
	}

	return iter.NewSortProfileIterator(iters), nil
}

func (b *singleBlockQuerier) Sort(in []Profile) []Profile {
	// Sort by RowNumber to avoid seeking back and forth in the file.
	sort.Slice(in, func(i, j int) bool {
		return in[i].(BlockProfile).RowNum < in[j].(BlockProfile).RowNum
	})
	return in
}

type reconstructSamples struct {
	buffer [][]parquet.Value
}

func (s *reconstructSamples) samples(samples []schemav1.Sample) []schemav1.Sample {
	if cap(samples) < len(s.buffer[0]) {
		samples = make([]schemav1.Sample, len(s.buffer[0]))
	}
	samples = samples[:len(s.buffer[0])]
	for pos := range samples {
		samples[pos].StacktraceID = s.buffer[0][pos].Uint64()
		samples[pos].Value = s.buffer[1][pos].Int64()
	}
	return samples
}

type uniqueIDs[T any] map[int64]T

func newUniqueIDs[T any]() uniqueIDs[T] {
	return uniqueIDs[T](make(map[int64]T))
}

func (m uniqueIDs[T]) iterator() iter.Iterator[int64] {
	ids := lo.Keys(m)
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return iter.NewSliceIterator(ids)
}

func (q *singleBlockQuerier) readTSBoundaries(ctx context.Context) (minMax, []minMax, error) {
	if err := q.open(ctx); err != nil {
		return minMax{}, nil, err
	}

	// find minTS and maxTS
	var columnTimeNanos *parquet.Column
	for _, c := range q.profiles.file.Root().Columns() {
		if c.Name() == "TimeNanos" {
			columnTimeNanos = c
			break
		}
	}
	if columnTimeNanos == nil {
		return minMax{}, nil, errors.New("'TimeNanos' column not found")
	}

	var (
		rowGroups             = q.profiles.file.RowGroups()
		tsBoundary            minMax
		tsBoundaryPerRowGroup = make([]minMax, len(rowGroups))
	)
	for idxRowGroup, rowGroup := range rowGroups {
		chunks := rowGroup.ColumnChunks()[columnTimeNanos.Index()]
		columnIndex := chunks.ColumnIndex()

		var min, max model.Time

		// determine the min/max across all pages
		for pageNum := 0; pageNum < columnIndex.NumPages(); pageNum++ {
			if current := model.TimeFromUnixNano(columnIndex.MinValue(pageNum).Int64()); pageNum == 0 || current < min {
				min = current
			}
			if current := model.TimeFromUnixNano(columnIndex.MaxValue(pageNum).Int64()); pageNum == 0 || current > max {
				max = current
			}
		}

		tsBoundaryPerRowGroup[idxRowGroup].min = min
		tsBoundaryPerRowGroup[idxRowGroup].max = max

		// determine the min/max across all row groups
		if idxRowGroup == 0 || min < tsBoundary.min {
			tsBoundary.min = min
		}
		if idxRowGroup == 0 || max > tsBoundary.max {
			tsBoundary.max = max
		}
	}

	return tsBoundary, tsBoundaryPerRowGroup, nil
}

func newByteSliceFromBucketReader(ctx context.Context, bucketReader phlareobjstore.BucketReader, path string) (index.RealByteSlice, error) {
	f, err := bucketReader.Get(ctx, path)
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	return index.RealByteSlice(data), nil
}

func (q *singleBlockQuerier) open(ctx context.Context) error {
	q.openLock.Lock()
	defer q.openLock.Unlock()

	// already open
	if q.opened {
		return nil
	}
	if err := q.openFiles(ctx); err != nil {
		return err
	}
	q.opened = true
	return nil
}

// openFiles opens the parquet and tsdb files so they are ready for usage.
func (q *singleBlockQuerier) openFiles(ctx context.Context) error {
	start := time.Now()
	sp, ctx := opentracing.StartSpanFromContext(ctx, "BlockQuerier - open")
	defer func() {
		q.metrics.blockOpeningLatency.Observe(time.Since(start).Seconds())
		sp.LogFields(
			otlog.String("block_ulid", q.meta.ULID.String()),
		)
		sp.Finish()
	}()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		// open tsdb index
		indexBytes, err := newByteSliceFromBucketReader(ctx, q.bucketReader, block.IndexFilename)
		if err != nil {
			return errors.Wrap(err, "error reading tsdb index")
		}

		q.index, err = index.NewReader(indexBytes)
		if err != nil {
			return errors.Wrap(err, "opening tsdb index")
		}
		return nil
	})

	// open parquet files
	for _, tableReader := range q.tables {
		tableReader := tableReader
		g.Go(func() error {
			if err := tableReader.open(contextWithBlockMetrics(ctx, q.metrics), q.bucketReader); err != nil {
				return err
			}
			return nil
		})
	}

	return g.Wait()
}

type parquetReader[M Models, P schemav1.PersisterName] struct {
	persister P
	file      *parquet.File
	reader    phlareobjstore.ReaderAt
	metrics   *blocksMetrics
}

func (r *parquetReader[M, P]) open(ctx context.Context, bucketReader phlareobjstore.BucketReader) error {
	r.metrics = contextBlockMetrics(ctx)
	filePath := r.persister.Name() + block.ParquetSuffix

	ra, err := bucketReader.ReaderAt(ctx, filePath)
	if err != nil {
		return errors.Wrapf(err, "opening file '%s'", filePath)
	}
	r.reader = ra

	// first try to open file, this is required otherwise OpenFile panics
	parquetFile, err := parquet.OpenFile(ra, ra.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return errors.Wrapf(err, "opening parquet file '%s'", filePath)
	}
	if parquetFile.NumRows() == 0 {
		return fmt.Errorf("error parquet file '%s' contains no rows", filePath)
	}

	// now open it for real
	r.file, err = parquet.OpenFile(ra, ra.Size())
	if err != nil {
		return errors.Wrapf(err, "opening parquet file '%s'", filePath)
	}

	return nil
}

func (r *parquetReader[M, P]) Close() error {
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}

func (r *parquetReader[M, P]) relPath() string {
	return r.persister.Name() + block.ParquetSuffix
}

func (r *parquetReader[M, P]) info() block.File {
	return block.File{
		Parquet: &block.ParquetFile{
			NumRows:      uint64(r.file.NumRows()),
			NumRowGroups: uint64(len(r.file.RowGroups())),
		},
		SizeBytes: uint64(r.file.Size()),
		RelPath:   r.relPath(),
	}
}

func (r *parquetReader[M, P]) columnIter(ctx context.Context, columnName string, predicate query.Predicate, alias string) query.Iterator {
	index, _ := query.GetColumnIndexByPath(r.file, columnName)
	if index == -1 {
		return query.NewErrIterator(fmt.Errorf("column '%s' not found in parquet file '%s'", columnName, r.relPath()))
	}
	ctx = query.AddMetricsToContext(ctx, r.metrics.query)
	return query.NewColumnIterator(ctx, r.file.RowGroups(), index, columnName, 1000, predicate, alias)
}

func repeatedColumnIter[T any](ctx context.Context, f *parquet.File, columnName string, rows iter.Iterator[T]) iter.Iterator[*query.RepeatedRow[T]] {
	index, _ := query.GetColumnIndexByPath(f, columnName)
	if index == -1 {
		return iter.NewErrIterator[*query.RepeatedRow[T]](fmt.Errorf("column '%s' not found in parquet file", columnName))
	}
	opentracing.SpanFromContext(ctx).SetTag("columnName", columnName)
	return query.NewRepeatedPageIterator(ctx, rows, f.RowGroups(), index, 1e4)
}

type retrieveRowIterator[M any] struct {
	idxRowGroup          int
	minRowNum, maxRowNum int64
	rowGroups            []parquet.RowGroup
	reader               *parquet.GenericReader[M]
	reconstruct          func(parquet.Row) (uint64, M, error)

	result         ResultWithRowNum[M]
	row            []M
	rowNumIterator iter.Iterator[int64]
	err            error
}

func (r *parquetReader[M, P]) retrieveRows(ctx context.Context, rowNumIterator iter.Iterator[int64]) iter.Iterator[ResultWithRowNum[M]] {
	return &retrieveRowIterator[M]{
		rowGroups:      r.file.RowGroups(),
		row:            make([]M, 1),
		rowNumIterator: rowNumIterator,
	}
}

func (i *retrieveRowIterator[M]) Err() error {
	return i.err
}

func (i *retrieveRowIterator[M]) nextRowGroup() error {
	if i.reader != nil {
		if err := i.reader.Close(); err != nil {
			return errors.Wrap(err, "closing row group")
		}
		i.idxRowGroup++
		if i.idxRowGroup >= len(i.rowGroups) {
			return io.EOF
		}
	}
	i.minRowNum = i.maxRowNum
	i.maxRowNum += i.rowGroups[i.idxRowGroup].NumRows()
	i.reader = parquet.NewGenericRowGroupReader[M](i.rowGroups[i.idxRowGroup])
	return nil
}

func (i *retrieveRowIterator[M]) At() ResultWithRowNum[M] {
	return i.result
}

func (i *retrieveRowIterator[M]) Next() bool {
	// get the next row num
	if !i.rowNumIterator.Next() {
		if err := i.rowNumIterator.Err(); err != nil {
			i.err = errors.Wrap(err, "error from row number iterator")
		}
		return false
	}
	rowNum := i.rowNumIterator.At()

	// ensure we initialise on first iteration
	if i.reader == nil {
		if err := i.nextRowGroup(); err != nil {
			i.err = errors.Wrap(err, "getting next row group")
			return false
		}
	}

	for {
		if rowNum < i.minRowNum {
			i.err = fmt.Errorf("row number selected %d is before current row number %d", rowNum, i.minRowNum)
			return false
		}
		if err := i.reader.SeekToRow(rowNum - i.minRowNum); err != nil {
			i.err = errors.Wrapf(err, "seek to row at rowNum=%d", rowNum)
			return false
		}
		_, err := i.reader.Read(i.row)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if err := i.nextRowGroup(); err != nil {
					if errors.Is(err, io.EOF) {
						continue
					}
					i.err = errors.Wrap(err, "getting next row group")
					return false
				}
				continue
			}
			i.err = errors.Wrapf(err, "reading row at rowNum=%d", rowNum)
			return false
		}
		break
	}

	i.result.RowNum = rowNum
	i.result.Result = i.row[0]
	return true
}

func (i *retrieveRowIterator[M]) Close() error {
	if i.reader != nil {
		return i.reader.Close()
	}
	return nil
}

type ResultWithRowNum[M any] struct {
	Result M
	RowNum int64
}

type inMemoryparquetReader[M Models, P schemav1.PersisterName] struct {
	persister P
	file      *parquet.File
	reader    phlareobjstore.ReaderAt
	cache     []M
}

func (r *inMemoryparquetReader[M, P]) open(ctx context.Context, bucketReader phlareobjstore.BucketReader) error {
	filePath := r.persister.Name() + block.ParquetSuffix

	ra, err := bucketReader.ReaderAt(ctx, filePath)
	if err != nil {
		return errors.Wrapf(err, "opening file '%s'", filePath)
	}
	r.reader = ra

	// first try to open file, this is required otherwise OpenFile panics
	parquetFile, err := parquet.OpenFile(ra, ra.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return errors.Wrapf(err, "opening parquet file '%s'", filePath)
	}
	if parquetFile.NumRows() == 0 {
		return fmt.Errorf("error parquet file '%s' contains no rows", filePath)
	}

	// now open it for real
	r.file, err = parquet.OpenFile(ra, ra.Size())
	if err != nil {
		return errors.Wrapf(err, "opening parquet file '%s'", filePath)
	}

	// read all rows into memory
	r.cache = make([]M, r.file.NumRows())
	var read int
	for _, rg := range r.file.RowGroups() {
		reader := parquet.NewGenericRowGroupReader[M](rg)
		for {
			n, err := reader.Read(r.cache[read:])
			read += n
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return errors.Wrapf(err, "reading row group from parquet file '%s'", filePath)
			}
		}
	}

	return nil
}

func (r *inMemoryparquetReader[M, P]) close() error {
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}

func (r *inMemoryparquetReader[M, P]) NumRows() int64 {
	return r.file.NumRows()
}

func (r *inMemoryparquetReader[M, P]) Close() error {
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}

func (r *inMemoryparquetReader[M, P]) relPath() string {
	return r.persister.Name() + block.ParquetSuffix
}

func (r *inMemoryparquetReader[M, P]) info() block.File {
	return block.File{
		Parquet: &block.ParquetFile{
			NumRows:      uint64(r.file.NumRows()),
			NumRowGroups: uint64(len(r.file.RowGroups())),
		},
		SizeBytes: uint64(r.file.Size()),
		RelPath:   r.relPath(),
	}
}

func (r *inMemoryparquetReader[M, P]) retrieveRows(ctx context.Context, rowNumIterator iter.Iterator[int64]) iter.Iterator[ResultWithRowNum[M]] {
	return &cacheIterator[M]{
		cache:          r.cache,
		rowNumIterator: rowNumIterator,
	}
}

type cacheIterator[M any] struct {
	cache          []M
	rowNumIterator iter.Iterator[int64]
}

func (c *cacheIterator[M]) Next() bool {
	if !c.rowNumIterator.Next() {
		return false
	}
	if c.rowNumIterator.At() >= int64(len(c.cache)) {
		return false
	}
	return true
}

func (c *cacheIterator[M]) At() ResultWithRowNum[M] {
	return ResultWithRowNum[M]{
		Result: c.cache[c.rowNumIterator.At()],
		RowNum: c.rowNumIterator.At(),
	}
}

func (c *cacheIterator[M]) Err() error {
	return nil
}

func (c *cacheIterator[M]) Close() error {
	return nil
}
