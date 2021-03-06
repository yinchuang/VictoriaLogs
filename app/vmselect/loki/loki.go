package loki

import (
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaLogs/app/vmselect/netstorage"
	"github.com/VictoriaMetrics/VictoriaLogs/app/vmselect/querier"
	"github.com/VictoriaMetrics/VictoriaLogs/app/vmselect/searchutils"
	"github.com/VictoriaMetrics/VictoriaLogs/app/vmselect/websocket"
	"github.com/VictoriaMetrics/VictoriaLogs/lib/logql"
	"github.com/VictoriaMetrics/VictoriaLogs/lib/storage"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmselect/bufferedwriter"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/auth"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/metrics"
	"github.com/valyala/fastjson/fastfloat"
	"github.com/valyala/quicktemplate"
)

var (
	latencyOffset = flag.Duration("search.latencyOffset", time.Second*30, "The time when data points become visible in query results after the collection. "+
		"Too small value can result in incomplete last points for query results")
	maxQueryLen = flagutil.NewBytes("search.maxQueryLen", 16*1024, "The maximum search query length in bytes")
	maxLookback = flag.Duration("search.maxLookback", 0, "Synonim to -search.lookback-delta from Prometheus. "+
		"The value is dynamically detected from interval between time series datapoints if not set. It can be overridden on per-query basis via max_lookback arg. "+
		"See also '-search.maxStalenessInterval' flag, which has the same meaining due to historical reasons")
	maxStalenessInterval = flag.Duration("search.maxStalenessInterval", 0, "The maximum interval for staleness calculations. "+
		"By default it is automatically calculated from the median interval between samples. This flag could be useful for tuning "+
		"Prometheus data model closer to Influx-style data model. See https://prometheus.io/docs/prometheus/latest/querying/basics/#staleness for details. "+
		"See also '-search.maxLookback' flag, which has the same meaning due to historical reasons")
	selectNodes = flagutil.NewArray("selectNode", "Addresses of vmselect nodes; usage: -selectNode=vmselect-host1:8481 -selectNode=vmselect-host2:8481")
)

// Default step used if not set.
const defaultStep = 5 * 60 * 1000

// FederateHandler implements /federate . See https://prometheus.io/docs/prometheus/latest/federation/
func FederateHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse request form values: %w", err)
	}
	matches := r.Form["match[]"]
	if len(matches) == 0 {
		return fmt.Errorf("missing `match[]` arg")
	}
	lookbackDelta, err := getMaxLookback(r)
	if err != nil {
		return err
	}
	if lookbackDelta <= 0 {
		lookbackDelta = defaultStep
	}
	start, err := searchutils.GetTime(r, "start", ct-lookbackDelta)
	if err != nil {
		return err
	}
	end, err := searchutils.GetTime(r, "end", ct)
	if err != nil {
		return err
	}
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	if start >= end {
		start = end - defaultStep
	}
	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return err
	}
	sq := &storage.SearchQuery{
		AccountID:    at.AccountID,
		ProjectID:    at.ProjectID,
		MinTimestamp: start,
		MaxTimestamp: end,
		TagFilterss:  tagFilterss,
	}
	rss, isPartial, err := netstorage.ProcessSearchQuery(at, sq, 2, deadline)
	if err != nil {
		return fmt.Errorf("cannot fetch data for %q: %w", sq, err)
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		rss.Cancel()
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}

	w.Header().Set("Content-Type", "text/plain")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	err = rss.RunParallel(func(rs *netstorage.Result, workerID uint) error {
		if err := bw.Error(); err != nil {
			return err
		}
		bb := quicktemplate.AcquireByteBuffer()
		WriteFederate(bb, rs)
		_, err := bw.Write(bb.B)
		quicktemplate.ReleaseByteBuffer(bb)
		return err
	})
	if err != nil {
		return fmt.Errorf("error during data fetching: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	federateDuration.UpdateDuration(startTime)
	return nil
}

var federateDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/federate"}`)

// ExportNativeHandler exports data in native format from /api/v1/export/native.
func ExportNativeHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse request form values: %w", err)
	}
	matches := r.Form["match[]"]
	if len(matches) == 0 {
		// Maintain backwards compatibility
		match := r.FormValue("match")
		if len(match) == 0 {
			return fmt.Errorf("missing `match[]` arg")
		}
		matches = []string{match}
	}
	start, err := searchutils.GetTime(r, "start", 0)
	if err != nil {
		return err
	}
	end, err := searchutils.GetTime(r, "end", ct)
	if err != nil {
		return err
	}
	deadline := searchutils.GetDeadlineForExport(r, startTime)
	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return err
	}
	sq := &storage.SearchQuery{
		AccountID:    at.AccountID,
		ProjectID:    at.ProjectID,
		MinTimestamp: start,
		MaxTimestamp: end,
		TagFilterss:  tagFilterss,
	}
	w.Header().Set("Content-Type", "VictoriaMetrics/native")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)

	// Marshal tr
	trBuf := make([]byte, 0, 16)
	trBuf = encoding.MarshalInt64(trBuf, start)
	trBuf = encoding.MarshalInt64(trBuf, end)
	_, _ = bw.Write(trBuf)

	// Marshal native blocks.
	isPartial, err := netstorage.ExportBlocks(at, sq, deadline, func(mn *storage.MetricName, b *storage.Block, tr storage.TimeRange) error {
		if err := bw.Error(); err != nil {
			return err
		}
		dstBuf := bbPool.Get()
		tmpBuf := bbPool.Get()
		dst := dstBuf.B
		tmp := tmpBuf.B

		// Marshal mn
		tmp = mn.MarshalNoAccountIDProjectID(tmp[:0])
		dst = encoding.MarshalUint32(dst, uint32(len(tmp)))
		dst = append(dst, tmp...)

		// Marshal b
		tmp = b.MarshalPortable(tmp[:0])
		dst = encoding.MarshalUint32(dst, uint32(len(tmp)))
		dst = append(dst, tmp...)

		tmpBuf.B = tmp
		bbPool.Put(tmpBuf)

		_, err := bw.Write(dst)

		dstBuf.B = dst
		bbPool.Put(dstBuf)
		return err
	})
	if err == nil && isPartial && searchutils.GetDenyPartialResponse(r) {
		err = fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}
	if err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	exportNativeDuration.UpdateDuration(startTime)
	return nil
}

var exportNativeDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/export/native"}`)

var bbPool bytesutil.ByteBufferPool

// ExportHandler exports data in raw format from /api/v1/export.
func ExportHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse request form values: %w", err)
	}
	matches := r.Form["match[]"]
	if len(matches) == 0 {
		// Maintain backwards compatibility
		match := r.FormValue("match")
		if len(match) == 0 {
			return fmt.Errorf("missing `match[]` arg")
		}
		matches = []string{match}
	}
	start, err := searchutils.GetTime(r, "start", 0)
	if err != nil {
		return err
	}
	end, err := searchutils.GetTime(r, "end", ct)
	if err != nil {
		return err
	}
	format := r.FormValue("format")
	maxRowsPerLine := int(fastfloat.ParseInt64BestEffort(r.FormValue("max_rows_per_line")))
	reduceMemUsage := searchutils.GetBool(r, "reduce_mem_usage")
	deadline := searchutils.GetDeadlineForExport(r, startTime)
	if start >= end {
		end = start + defaultStep
	}
	if err := exportHandler(at, w, r, matches, start, end, format, maxRowsPerLine, reduceMemUsage, deadline); err != nil {
		return fmt.Errorf("error when exporting data for queries=%q on the time range (start=%d, end=%d): %w", matches, start, end, err)
	}
	exportDuration.UpdateDuration(startTime)
	return nil
}

var exportDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/export"}`)

func exportHandler(at *auth.Token, w http.ResponseWriter, r *http.Request, matches []string, start, end int64,
	format string, maxRowsPerLine int, reduceMemUsage bool, deadline searchutils.Deadline) error {
	writeResponseFunc := WriteExportStdResponse
	writeLineFunc := func(xb *exportBlock, resultsCh chan<- *quicktemplate.ByteBuffer) {
		bb := quicktemplate.AcquireByteBuffer()
		WriteExportJSONLine(bb, xb)
		resultsCh <- bb
	}
	contentType := "application/stream+json"
	if format == "prometheus" {
		contentType = "text/plain"
		writeLineFunc = func(xb *exportBlock, resultsCh chan<- *quicktemplate.ByteBuffer) {
			bb := quicktemplate.AcquireByteBuffer()
			WriteExportPrometheusLine(bb, xb)
			resultsCh <- bb
		}
	} else if format == "promapi" {
		writeResponseFunc = WriteExportPromAPIResponse
		writeLineFunc = func(xb *exportBlock, resultsCh chan<- *quicktemplate.ByteBuffer) {
			bb := quicktemplate.AcquireByteBuffer()
			WriteExportPromAPILine(bb, xb)
			resultsCh <- bb
		}
	}
	if maxRowsPerLine > 0 {
		writeLineFuncOrig := writeLineFunc
		writeLineFunc = func(xb *exportBlock, resultsCh chan<- *quicktemplate.ByteBuffer) {
			valuesOrig := xb.datas
			timestampsOrig := xb.timestamps
			values := valuesOrig
			timestamps := timestampsOrig
			for len(values) > 0 {
				var valuesChunk [][]byte
				var timestampsChunk []int64
				if len(values) > maxRowsPerLine {
					valuesChunk = values[:maxRowsPerLine]
					timestampsChunk = timestamps[:maxRowsPerLine]
					values = values[maxRowsPerLine:]
					timestamps = timestamps[maxRowsPerLine:]
				} else {
					valuesChunk = values
					timestampsChunk = timestamps
					values = nil
					timestamps = nil
				}
				xb.datas = valuesChunk
				xb.timestamps = timestampsChunk
				writeLineFuncOrig(xb, resultsCh)
			}
			xb.datas = valuesOrig
			xb.timestamps = timestampsOrig
		}
	}

	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return err
	}
	sq := &storage.SearchQuery{
		AccountID:    at.AccountID,
		ProjectID:    at.ProjectID,
		MinTimestamp: start,
		MaxTimestamp: end,
		TagFilterss:  tagFilterss,
	}
	w.Header().Set("Content-Type", contentType)
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)

	resultsCh := make(chan *quicktemplate.ByteBuffer, runtime.GOMAXPROCS(-1))
	doneCh := make(chan error)
	if !reduceMemUsage {
		rss, isPartial, err := netstorage.ProcessSearchQuery(at, sq, 2, deadline)
		if err != nil {
			return fmt.Errorf("cannot fetch data for %q: %w", sq, err)
		}
		if isPartial && searchutils.GetDenyPartialResponse(r) {
			rss.Cancel()
			return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
		}
		go func() {
			err := rss.RunParallel(func(rs *netstorage.Result, workerID uint) error {
				if err := bw.Error(); err != nil {
					return err
				}
				xb := exportBlockPool.Get().(*exportBlock)
				xb.mn = &rs.MetricName
				xb.timestamps = rs.Timestamps
				xb.values = rs.Values
				xb.datas = rs.Datas
				writeLineFunc(xb, resultsCh)
				xb.reset()
				exportBlockPool.Put(xb)
				return nil
			})
			close(resultsCh)
			doneCh <- err
		}()
	} else {
		go func() {
			isPartial, err := netstorage.ExportBlocks(at, sq, deadline, func(mn *storage.MetricName, b *storage.Block, tr storage.TimeRange) error {
				if err := bw.Error(); err != nil {
					return err
				}
				if err := b.UnmarshalData(true); err != nil {
					return fmt.Errorf("cannot unmarshal block during export: %s", err)
				}
				xb := exportBlockPool.Get().(*exportBlock)
				xb.mn = mn
				xb.timestamps, xb.datas = b.AppendRowsWithTimeRangeFilter(xb.timestamps[:0], xb.datas[:0], tr)
				for i := 0; i < len(xb.timestamps); i++ {
					xb.values = append(xb.values, 1)
				}
				if len(xb.timestamps) > 0 {
					writeLineFunc(xb, resultsCh)
				}
				xb.reset()
				exportBlockPool.Put(xb)
				return nil
			})
			if err == nil && isPartial && searchutils.GetDenyPartialResponse(r) {
				err = fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
			}
			close(resultsCh)
			doneCh <- err
		}()
	}

	// writeResponseFunc must consume all the data from resultsCh.
	writeResponseFunc(bw, resultsCh)
	if err := bw.Flush(); err != nil {
		return err
	}
	err = <-doneCh
	if err != nil {
		return fmt.Errorf("error during data fetching: %w", err)
	}
	return nil
}

type exportBlock struct {
	mn         *storage.MetricName
	timestamps []int64
	values     []float64
	datas      [][]byte
}

func (xb *exportBlock) reset() {
	xb.mn = nil
	xb.timestamps = xb.timestamps[:0]
	xb.values = xb.values[:0]
	xb.datas = xb.datas[:0]
}

var exportBlockPool = &sync.Pool{
	New: func() interface{} {
		return &exportBlock{}
	},
}

// DeleteHandler processes /api/v1/admin/tsdb/delete_series prometheus API request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#delete-series
func DeleteHandler(startTime time.Time, at *auth.Token, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse request form values: %w", err)
	}
	if r.FormValue("start") != "" || r.FormValue("end") != "" {
		return fmt.Errorf("start and end aren't supported. Remove these args from the query in order to delete all the matching metrics")
	}
	matches := r.Form["match[]"]
	if len(matches) == 0 {
		return fmt.Errorf("missing `match[]` arg")
	}
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return err
	}
	sq := &storage.SearchQuery{
		AccountID:   at.AccountID,
		ProjectID:   at.ProjectID,
		TagFilterss: tagFilterss,
	}
	deletedCount, err := netstorage.DeleteSeries(at, sq, deadline)
	if err != nil {
		return fmt.Errorf("cannot delete time series matching %q: %w", matches, err)
	}
	if deletedCount > 0 {
		// Reset rollup result cache on all the vmselect nodes,
		// since the cache may contain deleted data.
		// TODO: reset only cache for (account, project)
		resetRollupResultCaches()
	}
	deleteDuration.UpdateDuration(startTime)
	return nil
}

var deleteDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/admin/tsdb/delete_series"}`)

func resetRollupResultCaches() {
	if len(*selectNodes) == 0 {
		logger.Panicf("BUG: missing -selectNode flag")
	}
	for _, selectNode := range *selectNodes {
		callURL := fmt.Sprintf("http://%s/internal/resetRollupResultCache", selectNode)
		resp, err := httpClient.Get(callURL)
		if err != nil {
			logger.Errorf("error when accessing %q: %s", callURL, err)
			resetRollupResultCacheErrors.Inc()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			logger.Errorf("unexpected status code at %q; got %d; want %d", callURL, resp.StatusCode, http.StatusOK)
			resetRollupResultCacheErrors.Inc()
			continue
		}
		_ = resp.Body.Close()
	}
	resetRollupResultCacheCalls.Inc()
}

var (
	resetRollupResultCacheErrors = metrics.NewCounter("vm_reset_rollup_result_cache_errors_total")
	resetRollupResultCacheCalls  = metrics.NewCounter("vm_reset_rollup_result_cache_calls_total")
)

var httpClient = &http.Client{
	Timeout: time.Second * 5,
}

// LabelValuesHandler processes /api/v1/label/<labelName>/values request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#querying-label-values
func LabelValuesHandler(startTime time.Time, at *auth.Token, labelName string, w http.ResponseWriter, r *http.Request) error {
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse form values: %w", err)
	}
	var labelValues []string
	var isPartial bool
	if len(r.Form["match[]"]) == 0 && len(r.Form["start"]) == 0 && len(r.Form["end"]) == 0 {
		var err error
		labelValues, isPartial, err = netstorage.GetLabelValues(at, labelName, deadline)
		if err != nil {
			return fmt.Errorf(`cannot obtain label values for %q: %w`, labelName, err)
		}
	} else {
		// Extended functionality that allows filtering by label filters and time range
		// i.e. /api/v1/label/foo/values?match[]=foobar{baz="abc"}&start=...&end=...
		// is equivalent to `label_values(foobar{baz="abc"}, foo)` call on the selected
		// time range in Grafana templating.
		matches := r.Form["match[]"]
		if len(matches) == 0 {
			matches = []string{fmt.Sprintf("{%s!=''}", labelName)}
		}
		ct := startTime.UnixNano() / 1e6
		end, err := searchutils.GetTime(r, "end", ct)
		if err != nil {
			return err
		}
		start, err := searchutils.GetTime(r, "start", end-defaultStep)
		if err != nil {
			return err
		}
		labelValues, isPartial, err = labelValuesWithMatches(at, labelName, matches, start, end, deadline)
		if err != nil {
			return fmt.Errorf("cannot obtain label values for %q, match[]=%q, start=%d, end=%d: %w", labelName, matches, start, end, err)
		}
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}

	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	WriteLabelValuesResponse(bw, labelValues)
	if err := bw.Flush(); err != nil {
		return err
	}
	labelValuesDuration.UpdateDuration(startTime)
	return nil
}

func labelValuesWithMatches(at *auth.Token, labelName string, matches []string, start, end int64, deadline searchutils.Deadline) ([]string, bool, error) {
	if len(matches) == 0 {
		logger.Panicf("BUG: matches must be non-empty")
	}
	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return nil, false, err
	}

	// Add `labelName!=''` tag filter in order to filter out series without the labelName.
	// There is no need in adding `__name__!=''` filter, since all the time series should
	// already have non-empty name.
	if labelName != "__name__" {
		key := []byte(labelName)
		for i, tfs := range tagFilterss {
			tagFilterss[i] = append(tfs, storage.TagFilter{
				Key:        key,
				IsNegative: true,
			})
		}
	}
	if start >= end {
		end = start + defaultStep
	}
	sq := &storage.SearchQuery{
		AccountID:    at.AccountID,
		ProjectID:    at.ProjectID,
		MinTimestamp: start,
		MaxTimestamp: end,
		TagFilterss:  tagFilterss,
	}
	rss, isPartial, err := netstorage.ProcessSearchQuery(at, sq, 0, deadline)
	if err != nil {
		return nil, false, fmt.Errorf("cannot fetch data for %q: %w", sq, err)
	}

	m := make(map[string]struct{})
	var mLock sync.Mutex
	err = rss.RunParallel(func(rs *netstorage.Result, workerID uint) error {
		labelValue := rs.MetricName.GetTagValue(labelName)
		if len(labelValue) == 0 {
			return nil
		}
		mLock.Lock()
		m[string(labelValue)] = struct{}{}
		mLock.Unlock()
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("error when data fetching: %w", err)
	}

	labelValues := make([]string, 0, len(m))
	for labelValue := range m {
		labelValues = append(labelValues, labelValue)
	}
	sort.Strings(labelValues)
	return labelValues, isPartial, nil
}

var labelValuesDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/label/{}/values"}`)

// LabelsCountHandler processes /api/v1/labels/count request.
func LabelsCountHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	labelEntries, isPartial, err := netstorage.GetLabelEntries(at, deadline)
	if err != nil {
		return fmt.Errorf(`cannot obtain label entries: %w`, err)
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}
	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	WriteLabelsCountResponse(bw, labelEntries)
	if err := bw.Flush(); err != nil {
		return err
	}
	labelsCountDuration.UpdateDuration(startTime)
	return nil
}

var labelsCountDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/labels/count"}`)

const secsPerDay = 3600 * 24

// TSDBStatusHandler processes /api/v1/status/tsdb request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#tsdb-stats
func TSDBStatusHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse form values: %w", err)
	}
	date := fasttime.UnixDate()
	dateStr := r.FormValue("date")
	if len(dateStr) > 0 {
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return fmt.Errorf("cannot parse `date` arg %q: %w", dateStr, err)
		}
		date = uint64(t.Unix()) / secsPerDay
	}
	topN := 10
	topNStr := r.FormValue("topN")
	if len(topNStr) > 0 {
		n, err := strconv.Atoi(topNStr)
		if err != nil {
			return fmt.Errorf("cannot parse `topN` arg %q: %w", topNStr, err)
		}
		if n <= 0 {
			n = 1
		}
		if n > 1000 {
			n = 1000
		}
		topN = n
	}
	status, isPartial, err := netstorage.GetTSDBStatusForDate(at, deadline, date, topN)
	if err != nil {
		return fmt.Errorf(`cannot obtain tsdb status for date=%d, topN=%d: %w`, date, topN, err)
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}
	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	WriteTSDBStatusResponse(bw, status)
	if err := bw.Flush(); err != nil {
		return err
	}
	tsdbStatusDuration.UpdateDuration(startTime)
	return nil
}

var tsdbStatusDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/status/tsdb"}`)

// LabelsHandler processes /api/v1/labels request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#getting-label-names
func LabelsHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse form values: %w", err)
	}
	var labels []string
	var isPartial bool
	if len(r.Form["match[]"]) == 0 && len(r.Form["start"]) == 0 && len(r.Form["end"]) == 0 {
		var err error
		labels, isPartial, err = netstorage.GetLabels(at, deadline)
		if err != nil {
			return fmt.Errorf("cannot obtain labels: %w", err)
		}
	} else {
		// Extended functionality that allows filtering by label filters and time range
		// i.e. /api/v1/labels?match[]=foobar{baz="abc"}&start=...&end=...
		matches := r.Form["match[]"]
		if len(matches) == 0 {
			matches = []string{"{__name__!=''}"}
		}
		ct := startTime.UnixNano() / 1e6
		end, err := searchutils.GetTime(r, "end", ct)
		if err != nil {
			return err
		}
		start, err := searchutils.GetTime(r, "start", end-defaultStep)
		if err != nil {
			return err
		}
		labels, isPartial, err = labelsWithMatches(at, matches, start, end, deadline)
		if err != nil {
			return fmt.Errorf("cannot obtain labels for match[]=%q, start=%d, end=%d: %w", matches, start, end, err)
		}
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}

	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	WriteLabelsResponse(bw, labels)
	if err := bw.Flush(); err != nil {
		return err
	}
	labelsDuration.UpdateDuration(startTime)
	return nil
}

func labelsWithMatches(at *auth.Token, matches []string, start, end int64, deadline searchutils.Deadline) ([]string, bool, error) {
	if len(matches) == 0 {
		logger.Panicf("BUG: matches must be non-empty")
	}
	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return nil, false, err
	}
	if start >= end {
		end = start + defaultStep
	}
	sq := &storage.SearchQuery{
		AccountID:    at.AccountID,
		ProjectID:    at.ProjectID,
		MinTimestamp: start,
		MaxTimestamp: end,
		TagFilterss:  tagFilterss,
	}
	rss, isPartial, err := netstorage.ProcessSearchQuery(at, sq, 0, deadline)
	if err != nil {
		return nil, false, fmt.Errorf("cannot fetch data for %q: %w", sq, err)
	}

	m := make(map[string]struct{})
	var mLock sync.Mutex
	err = rss.RunParallel(func(rs *netstorage.Result, workerID uint) error {
		mLock.Lock()
		tags := rs.MetricName.Tags
		for i := range tags {
			t := &tags[i]
			m[string(t.Key)] = struct{}{}
		}
		m["__name__"] = struct{}{}
		mLock.Unlock()
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("error when data fetching: %w", err)
	}

	labels := make([]string, 0, len(m))
	for label := range m {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels, isPartial, nil
}

var labelsDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/labels"}`)

// SeriesCountHandler processes /api/v1/series/count request.
func SeriesCountHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	n, isPartial, err := netstorage.GetSeriesCount(at, deadline)
	if err != nil {
		return fmt.Errorf("cannot obtain series count: %w", err)
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}

	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	WriteSeriesCountResponse(bw, n)
	if err := bw.Flush(); err != nil {
		return err
	}
	seriesCountDuration.UpdateDuration(startTime)
	return nil
}

var seriesCountDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/series/count"}`)

// SeriesHandler processes /api/v1/series request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#finding-series-by-label-matchers
func SeriesHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("cannot parse form values: %w", err)
	}
	matches := r.Form["match[]"]
	if len(matches) == 0 {
		return fmt.Errorf("missing `match[]` arg")
	}
	end, err := searchutils.GetTime(r, "end", ct)
	if err != nil {
		return err
	}
	// Do not set start to searchutils.minTimeMsecs by default as Prometheus does,
	// since this leads to fetching and scanning all the data from the storage,
	// which can take a lot of time for big storages.
	// It is better setting start as end-defaultStep by default.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/91
	start, err := searchutils.GetTime(r, "start", end-defaultStep)
	if err != nil {
		return err
	}
	deadline := searchutils.GetDeadlineForQuery(r, startTime)

	tagFilterss, err := getTagFilterssFromMatches(matches)
	if err != nil {
		return err
	}
	if start >= end {
		end = start + defaultStep
	}
	sq := &storage.SearchQuery{
		AccountID:    at.AccountID,
		ProjectID:    at.ProjectID,
		MinTimestamp: start,
		MaxTimestamp: end,
		TagFilterss:  tagFilterss,
	}
	rss, isPartial, err := netstorage.ProcessSearchQuery(at, sq, 0, deadline)
	if err != nil {
		return fmt.Errorf("cannot fetch data for %q: %w", sq, err)
	}
	if isPartial && searchutils.GetDenyPartialResponse(r) {
		rss.Cancel()
		return fmt.Errorf("cannot return full response, since some of vmstorage nodes are unavailable")
	}

	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)
	resultsCh := make(chan *quicktemplate.ByteBuffer)
	doneCh := make(chan error)
	go func() {
		err := rss.RunParallel(func(rs *netstorage.Result, workerID uint) error {
			if err := bw.Error(); err != nil {
				return err
			}
			bb := quicktemplate.AcquireByteBuffer()
			writemetricNameObject(bb, &rs.MetricName)
			resultsCh <- bb
			return nil
		})
		close(resultsCh)
		doneCh <- err
	}()
	// WriteSeriesResponse must consume all the data from resultsCh.
	WriteSeriesResponse(bw, resultsCh)
	if err := bw.Flush(); err != nil {
		return err
	}
	err = <-doneCh
	if err != nil {
		return fmt.Errorf("error during data fetching: %w", err)
	}
	seriesDuration.UpdateDuration(startTime)
	return nil
}

var seriesDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/series"}`)

// QueryHandler processes /api/v1/query request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#instant-queries
func QueryHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	query := r.FormValue("query")
	if len(query) == 0 {
		return fmt.Errorf("missing `query` arg")
	}
	start, err := searchutils.GetTime(r, "time", ct)
	if err != nil {
		return err
	}
	lookbackDelta, err := getMaxLookback(r)
	if err != nil {
		return err
	}
	step, err := searchutils.GetDuration(r, "step", lookbackDelta)
	if err != nil {
		return err
	}
	if step <= 0 {
		step = defaultStep
	}
	limit, err := searchutils.GetInt64(r, "limit", defaultLimit)
	if err != nil {
		return err
	}
	forward := searchutils.GetString(r, "direction", "backward") == "forward"

	deadline := searchutils.GetDeadlineForQuery(r, startTime)

	if len(query) > maxQueryLen.N {
		return fmt.Errorf("too long query; got %d bytes; mustn't exceed `-search.maxQueryLen=%d` bytes", len(query), maxQueryLen.N)
	}
	if childQuery, windowStr, offsetStr := querier.IsMetricSelectorWithRollup(query); childQuery != "" {
		window, err := parsePositiveDuration(windowStr, step)
		if err != nil {
			return fmt.Errorf("cannot parse window: %w", err)
		}
		offset, err := parseDuration(offsetStr, step)
		if err != nil {
			return fmt.Errorf("cannot parse offset: %w", err)
		}
		start -= offset
		end := start
		start = end - window
		if err := exportHandler(at, w, r, []string{childQuery}, start, end, "promapi", 0, false, deadline); err != nil {
			return fmt.Errorf("error when exporting data for query=%q on the time range (start=%d, end=%d): %w", childQuery, start, end, err)
		}
		queryDuration.UpdateDuration(startTime)
		return nil
	}
	if childQuery, windowStr, stepStr, offsetStr := querier.IsRollup(query); childQuery != "" {
		newStep, err := parsePositiveDuration(stepStr, step)
		if err != nil {
			return fmt.Errorf("cannot parse step: %w", err)
		}
		if newStep > 0 {
			step = newStep
		}
		window, err := parsePositiveDuration(windowStr, step)
		if err != nil {
			return fmt.Errorf("cannot parse window: %w", err)
		}
		offset, err := parseDuration(offsetStr, step)
		if err != nil {
			return fmt.Errorf("cannot parse offset: %w", err)
		}
		start -= offset
		end := start
		start = end - window

		w.Header().Set("Content-Type", "application/json")
		if _, err := queryRangeHandler(startTime, at, w, childQuery, start, end, step, limit, forward, r, ct, false, nil); err != nil {
			return fmt.Errorf("error when executing query=%q on the time range (start=%d, end=%d, step=%d): %w", childQuery, start, end, step, err)
		}

		queryDuration.UpdateDuration(startTime)
		return nil
	}

	queryOffset := getLatencyOffsetMilliseconds()
	if !searchutils.GetBool(r, "nocache") && ct-start < queryOffset && start-ct < queryOffset {
		// Adjust start time only if `nocache` arg isn't set.
		// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/241
		startPrev := start
		start = ct - queryOffset
		queryOffset = startPrev - start
	} else {
		queryOffset = 0
	}
	ec := querier.EvalConfig{
		AuthToken:        at,
		Start:            start,
		End:              start,
		Step:             step,
		QuotedRemoteAddr: httpserver.GetQuotedRemoteAddr(r),
		Deadline:         deadline,
		LookbackDelta:    lookbackDelta,

		DenyPartialResponse: searchutils.GetDenyPartialResponse(r),
	}
	result, e, err := querier.Exec(&ec, query, true)
	if err != nil {
		return fmt.Errorf("error when executing query=%q for (time=%d, step=%d): %w", query, start, step, err)
	}
	if queryOffset > 0 {
		for i := range result {
			timestamps := result[i].Timestamps
			for j := range timestamps {
				timestamps[j] += queryOffset
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)

	switch e.(type) {
	case *logql.BinaryOpExpr, *logql.MetricExpr:
		WriteStreamsQueryResponse(bw, result)
	default:
		WriteVectorQueryResponse(bw, result)
	}

	if err := bw.Flush(); err != nil {
		return err
	}
	queryDuration.UpdateDuration(startTime)
	return nil
}

var queryDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/query"}`)

func parseDuration(s string, step int64) (int64, error) {
	if len(s) == 0 {
		return 0, nil
	}
	return logql.DurationValue(s, step)
}

func parsePositiveDuration(s string, step int64) (int64, error) {
	if len(s) == 0 {
		return 0, nil
	}
	return logql.PositiveDurationValue(s, step)
}

// Default limit used if not set.
const defaultLimit = 1000

// QueryRangeHandler processes /api/v1/query_range request.
//
// See https://prometheus.io/docs/prometheus/latest/querying/api/#range-queries
func QueryRangeHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	query := r.FormValue("query")
	if len(query) == 0 {
		return fmt.Errorf("missing `query` arg")
	}
	start, err := searchutils.GetTime(r, "start", ct-defaultStep)
	if err != nil {
		return err
	}
	end, err := searchutils.GetTime(r, "end", ct)
	if err != nil {
		return err
	}
	step, err := searchutils.GetDuration(r, "step", defaultStep)
	if err != nil {
		return err
	}
	limit, err := searchutils.GetInt64(r, "limit", defaultLimit)
	if err != nil {
		return err
	}
	forward := searchutils.GetString(r, "direction", "backward") == "forward"

	w.Header().Set("Content-Type", "application/json")

	if _, err := queryRangeHandler(startTime, at, w, query, start, end, step, limit, forward, r, ct, false, nil); err != nil {
		return fmt.Errorf("error when executing query=%q on the time range (start=%d, end=%d, step=%d): %w", query, start, end, step, err)
	}
	queryRangeDuration.UpdateDuration(startTime)
	return nil
}

func TailHandler(startTime time.Time, at *auth.Token, w http.ResponseWriter, r *http.Request) error {
	ct := startTime.UnixNano() / 1e6
	query := r.FormValue("query")
	if len(query) == 0 {
		return fmt.Errorf("missing `query` arg")
	}
	start, err := searchutils.GetTime(r, "start", ct-defaultStep)
	if err != nil {
		return err
	}
	limit, err := searchutils.GetInt64(r, "limit", defaultLimit)
	if err != nil {
		return err
	}

	conn, err := websocket.TryUpgrade(w, r)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastTs, end int64
	filter := make(map[uint64]int64)

	for ; true; <-ticker.C {
		startTime = time.Now()
		end = startTime.UnixNano() / 1e6
		result, err := queryRangeHandler(startTime, at, conn, query, start, end, 60, limit, false, r, end, true, filter)
		if err != nil {
			return fmt.Errorf("error when executing query=%q on the time range (start=%d, end=%d, limit=%d): %w", query, start, end, limit, err)
		}
		for _, rs := range result {
			lastTs = rs.Timestamps[len(rs.Timestamps)-1]
			if lastTs > start {
				start = lastTs
			}
			filter[rs.MetricNameHash] = lastTs
			limit -= int64(len(rs.Timestamps))
		}
		for hashKey, lastTs := range filter {
			if end-lastTs > defaultStep {
				delete(filter, hashKey)
			}
		}
		if limit <= 0 {
			break
		}
	}
	return nil
}

func queryRangeHandler(startTime time.Time, at *auth.Token, w io.Writer, query string, start, end, step, limit int64,
	forward bool, r *http.Request, ct int64, tail bool, filter map[uint64]int64) ([]netstorage.Result, error) {
	deadline := searchutils.GetDeadlineForQuery(r, startTime)
	mayCache := !searchutils.GetBool(r, "nocache")
	lookbackDelta, err := getMaxLookback(r)
	if err != nil {
		return nil, err
	}

	// Validate input args.
	if len(query) > maxQueryLen.N {
		return nil, fmt.Errorf("too long query; got %d bytes; mustn't exceed `-search.maxQueryLen=%d` bytes", len(query), maxQueryLen.N)
	}
	if start > end {
		end = start + defaultStep
	}
	if err := querier.ValidateMaxPointsPerTimeseries(start, end, step); err != nil {
		return nil, err
	}
	if mayCache {
		start, end = querier.AdjustStartEnd(start, end, step)
	}

	ec := querier.EvalConfig{
		AuthToken:        at,
		Start:            start,
		End:              end,
		Step:             step,
		Limit:            limit,
		Forward:          forward,
		QuotedRemoteAddr: httpserver.GetQuotedRemoteAddr(r),
		Deadline:         deadline,
		MayCache:         mayCache,
		LookbackDelta:    lookbackDelta,

		DenyPartialResponse: searchutils.GetDenyPartialResponse(r),
	}
	result, e, err := querier.Exec(&ec, query, false)
	if err != nil {
		return nil, fmt.Errorf("cannot execute query: %w", err)
	}

	bw := bufferedwriter.Get(w)
	defer bufferedwriter.Put(bw)

	switch e.(type) {
	case *logql.BinaryOpExpr, *logql.MetricExpr:
		// Remove NaN values as Prometheus does.
		// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/153
		result = removeFilteredValuesAndTimeseries(result, filter)

		if tail {
			if len(result) > 0 {
				WriteTailQueryRangeResponse(bw, result)
			}
		} else {
			WriteStreamsQueryRangeResponse(bw, result)
		}
	default:
		queryOffset := getLatencyOffsetMilliseconds()
		if ct-queryOffset < end {
			result = adjustLastPoints(result, ct-queryOffset, ct+step)
		}

		// Remove NaN values as Prometheus does.
		// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/153
		result = removeFilteredValuesAndTimeseries(result, filter)

		WriteVectorQueryRangeResponse(bw, result)
	}

	if err := bw.Flush(); err != nil {
		return result, err
	}
	return result, nil
}

func removeFilteredValuesAndTimeseries(tss []netstorage.Result, filter map[uint64]int64) []netstorage.Result {
	var lastTs int64
	var filtered bool
	dst := tss[:0]
	for i := range tss {
		ts := &tss[i]
		hasNaNs := false
		for _, v := range ts.Values {
			if math.IsNaN(v) {
				hasNaNs = true
				break
			}
		}
		if filter != nil {
			lastTs, filtered = filter[ts.MetricNameHash]
		}
		if !hasNaNs && !filtered {
			// Fast path: nothing to remove.
			if len(ts.Values) > 0 {
				dst = append(dst, *ts)
			}
			continue
		}

		// Slow path: remove NaNs.
		srcDatas := ts.Datas
		srcTimestamps := ts.Timestamps
		dstDatas := ts.Datas[:0]
		dstValues := ts.Values[:0]
		dstTimestamps := ts.Timestamps[:0]
		for j, v := range ts.Values {
			if math.IsNaN(v) || srcTimestamps[j] <= lastTs {
				continue
			}
			if srcDatas != nil {
				dstDatas = append(dstDatas, srcDatas[j])
			}
			dstValues = append(dstValues, v)
			dstTimestamps = append(dstTimestamps, srcTimestamps[j])
		}
		ts.Datas = dstDatas
		ts.Values = dstValues
		ts.Timestamps = dstTimestamps
		if len(ts.Values) > 0 {
			dst = append(dst, *ts)
		}
	}
	return dst
}

var queryRangeDuration = metrics.NewSummary(`vm_request_duration_seconds{path="/api/v1/query_range"}`)

var nan = math.NaN()

// adjustLastPoints substitutes the last point values on the time range (start..end]
// with the previous point values, since these points may contain incomplete values.
func adjustLastPoints(tss []netstorage.Result, start, end int64) []netstorage.Result {
	for i := range tss {
		ts := &tss[i]
		values := ts.Values
		timestamps := ts.Timestamps
		j := len(timestamps) - 1
		if j >= 0 && timestamps[j] > end {
			// It looks like the `offset` is used in the query, which shifts time range beyond the `end`.
			// Leave such a time series as is, since it is unclear which points may be incomplete in it.
			// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/625
			continue
		}
		for j >= 0 && timestamps[j] > start {
			j--
		}
		j++
		lastValue := nan
		if j > 0 {
			lastValue = values[j-1]
		}
		for j < len(timestamps) && timestamps[j] <= end {
			values[j] = lastValue
			j++
		}
	}
	return tss
}

func getMaxLookback(r *http.Request) (int64, error) {
	d := maxLookback.Milliseconds()
	if d == 0 {
		d = maxStalenessInterval.Milliseconds()
	}
	return searchutils.GetDuration(r, "max_lookback", d)
}

func getTagFilterssFromMatches(matches []string) ([][]storage.TagFilter, error) {
	tagFilterss := make([][]storage.TagFilter, 0, len(matches))
	for _, match := range matches {
		tagFilters, err := querier.ParseMetricSelector(match)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q: %w", match, err)
		}
		tagFilterss = append(tagFilterss, tagFilters)
	}
	return tagFilterss, nil
}

func getLatencyOffsetMilliseconds() int64 {
	d := latencyOffset.Milliseconds()
	if d <= 1000 {
		d = 1000
	}
	return d
}
