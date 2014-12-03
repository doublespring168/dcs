package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Debian/dcs/cmd/dcs-web/common"
	"github.com/Debian/dcs/cmd/dcs-web/search"
	dcsregexp "github.com/Debian/dcs/regexp"
	"github.com/Debian/dcs/stringpool"
	"github.com/Debian/dcs/varz"
	"github.com/influxdb/influxdb-go"
	"hash/fnv"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	queryResultsPath = flag.String("query_results_path",
		"/tmp/qr/",
		"TODO")
	influxDBHost = flag.String("influx_db_host",
		"",
		"host:port of the InfluxDB to store time series in")
	influxDBDatabase = flag.String("influx_db_database",
		"dcs",
		"InfluxDB database name")
	influxDBUsername = flag.String("influx_db_username",
		"root",
		"InfluxDB username")
	influxDBPassword = flag.String("influx_db_password",
		"root",
		"InfluxDB password")

	perPackagePathRe = regexp.MustCompile(`^/perpackage-results/([^/]+)/` +
		strconv.Itoa(resultsPerPackage) + `/page_([0-9]+).json$`)
)

const (
	// NB: All of these constants needs to match those in static/instant.js.
	packagesPerPage   = 5
	resultsPerPackage = 2
)

// TODO: make this type satisfy obsoletableEvent
// TODO: get rid of this type — replace all occurences with a more specific
// version, e.g. Error, ProgressUpdate. Then, strip all fields except “Type”
// and make those use Result as an anonymous struct.
type Result struct {
	// This is set to “result” to distinguish the message type on the client.
	// Additionally, it serves as an indicator for whether the result is
	// initialized or whether this is the nil value.
	Type string

	dcsregexp.Match

	Package string

	FilesProcessed int
	FilesTotal     int
}

type Error struct {
	// This is set to “error” to distinguish the message type on the client.
	Type string

	// Currently only “backendunavailable”
	ErrorType string
}

type ProgressUpdate struct {
	Type           string
	QueryId        string
	FilesProcessed int
	FilesTotal     int
	Results        int
}

func (p *ProgressUpdate) EventType() string {
	return p.Type
}

func (p *ProgressUpdate) ObsoletedBy(newEvent *obsoletableEvent) bool {
	return (*newEvent).EventType() == p.Type
}

type ByRanking []Result

func (s ByRanking) Len() int {
	return len(s)
}

func (s ByRanking) Less(i, j int) bool {
	if s[i].Ranking == s[j].Ranking {
		// On a tie, we use the path to make the order of results stable over
		// multiple queries (which can have different results depending on
		// which index backend reacts quicker).
		return s[i].Path > s[j].Path
	}
	return s[i].Ranking > s[j].Ranking
}

func (s ByRanking) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type resultPointer struct {
	backendidx int
	ranking    float32
	offset     int64
	length     int64

	// Used as a tie-breaker when sorting by ranking to guarantee stable
	// results, independent of the order in which the results are returned from
	// source backends.
	pathHash uint64

	// Used for per-package results. Points into a stringpool.StringPool
	packageName *string
}

type pointerByRanking []resultPointer

func (s pointerByRanking) Len() int {
	return len(s)
}

func (s pointerByRanking) Less(i, j int) bool {
	if s[i].ranking == s[j].ranking {
		return s[i].pathHash > s[j].pathHash
	}
	return s[i].ranking > s[j].ranking
}

func (s pointerByRanking) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type queryState struct {
	started  time.Time
	events   []event
	newEvent *sync.Cond
	done     bool
	query    string

	results  [10]Result
	resultMu *sync.Mutex

	filesTotal     []int
	filesProcessed []int
	filesMu        *sync.Mutex

	resultPages int
	numResults  int

	// One file per backend, containing JSON-serialized results. When writing,
	// we keep the offsets, so that we can later sort the pointers and write
	// the resulting files.
	tempFiles      []*os.File
	packagePool    *stringpool.StringPool
	resultPointers []resultPointer

	allPackages       map[string]bool
	allPackagesSorted []string
	allPackagesMu     *sync.Mutex

	FirstPathRank float32
}

var (
	state   = make(map[string]queryState)
	stateMu sync.Mutex
)

func queryBackend(queryid string, backend string, backendidx int, sourceQuery []byte) {
	// When exiting this function, check that all results were processed. If
	// not, the backend query must have failed for some reason. Send a progress
	// update to prevent the query from running forever.
	defer func() {
		filesTotal := state[queryid].filesTotal[backendidx]

		if state[queryid].filesProcessed[backendidx] == filesTotal {
			return
		}

		if filesTotal == -1 {
			filesTotal = 0
		}

		// TODO: use a more specific type (progressupdate)
		storeProgress(queryid, backendidx, Result{
			Type:           "progress",
			FilesProcessed: filesTotal,
			FilesTotal:     filesTotal})

		addEventMarshal(queryid, &Error{
			Type:      "error",
			ErrorType: "backendunavailable",
		})
	}()

	// TODO: switch in the config
	log.Printf("[%s] [src:%s] connecting...\n", queryid, backend)
	conn, err := net.DialTimeout("tcp", strings.Replace(backend, "28082", "26082", -1), 5*time.Second)
	if err != nil {
		log.Printf("[%s] [src:%s] Connection failed: %v\n", queryid, backend, err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write(sourceQuery); err != nil {
		log.Printf("[%s] [src:%s] could not send query: %v\n", queryid, backend, err)
		return
	}
	decoder := json.NewDecoder(conn)
	r := Result{Type: "result"}
	for !state[queryid].done {
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if err := decoder.Decode(&r); err != nil {
			if err == io.EOF {
				return
			} else {
				log.Printf("[%s] [src:%s] Error decoding result stream: %v\n", queryid, backend, err)
				return
			}
		}
		if r.Type == "result" {
			storeResult(queryid, backendidx, r)
		} else if r.Type == "progress" {
			storeProgress(queryid, backendidx, r)
		}
		// The source backend sends back results without type, so the default is “result”.
		r.Type = "result"
	}
}

func maybeStartQuery(queryid, src, query string) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	querystate, running := state[queryid]
	// XXX: Starting a new query while there may still be clients reading that
	// query is not a great idea. Best fix may be to make getEvent() use a
	// querystate instead of the string identifier.
	if !running || time.Since(querystate.started) > 30*time.Minute {
		// See if we can garbage-collect old queries.
		if !running && len(state) >= 10 {
			log.Printf("Trying to garbage collect queries (currently %d)\n", len(state))
			for queryid, s := range state {
				if len(state) < 10 {
					break
				}
				if !s.done {
					continue
				}
				delete(state, queryid)
			}
			log.Printf("Garbage collection done. %d queries remaining", len(state))
		}
		backends := strings.Split(*common.SourceBackends, ",")
		state[queryid] = queryState{
			started:        time.Now(),
			query:          query,
			newEvent:       sync.NewCond(&sync.Mutex{}),
			resultMu:       &sync.Mutex{},
			filesTotal:     make([]int, len(backends)),
			filesProcessed: make([]int, len(backends)),
			filesMu:        &sync.Mutex{},
			tempFiles:      make([]*os.File, len(backends)),
			allPackages:    make(map[string]bool),
			allPackagesMu:  &sync.Mutex{},
			packagePool:    stringpool.NewStringPool(),
		}

		var err error
		dir := filepath.Join(*queryResultsPath, queryid)
		if err := os.MkdirAll(dir, os.FileMode(0755)); err != nil {
			log.Printf("[%s] could not create %q: %v\n", queryid, dir, err)
			failQuery(queryid)
			return false
		}

		// TODO: it’d be so much better if we would correctly handle ESPACE errors
		// in the code below (and above), but for that we need to carefully test it.
		ensureEnoughSpaceAvailable()

		for i := 0; i < len(backends); i++ {
			state[queryid].filesTotal[i] = -1
			path := filepath.Join(dir, fmt.Sprintf("unsorted_%d.json", i))
			state[queryid].tempFiles[i], err = os.Create(path)
			if err != nil {
				log.Printf("[%s] could not create %q: %v\n", queryid, path, err)
				failQuery(queryid)
				return false
			}
		}
		log.Printf("initial results = %v\n", state[queryid])

		// Rewrite the query into a query for source backends.
		fakeUrl, err := url.Parse("?" + query)
		if err != nil {
			log.Fatal(err)
		}
		rewritten := search.RewriteQuery(*fakeUrl)
		type streamingRequest struct {
			Query string
			URL   string
		}
		request := streamingRequest{
			Query: rewritten.Query().Get("q"),
			URL:   rewritten.String(),
		}
		log.Printf("[%s] querying for %q\n", queryid, request.Query)
		sourceQuery, err := json.Marshal(&request)
		if err != nil {
			log.Fatal(err)
		}

		for idx, backend := range backends {
			go queryBackend(queryid, backend, idx, sourceQuery)
		}
		return false
	}

	return true
}

func QueryzHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if cancel := r.PostFormValue("cancel"); cancel != "" {
		addEventMarshal(cancel, &Error{
			Type:      "error",
			ErrorType: "cancelled",
		})
		finishQuery(cancel)
		http.Redirect(w, r, "/queryz", http.StatusFound)
		return
	}

	type queryStats struct {
		Searchterm     string
		QueryId        string
		NumEvents      int
		NumResults     int
		NumResultPages int
		NumPackages    int
		Done           bool
		Started        time.Time
		Duration       time.Duration
		FilesTotal     []int
		FilesProcessed []int
	}
	stateMu.Lock()
	stats := make([]queryStats, len(state))
	idx := 0
	for queryid, s := range state {
		stats[idx] = queryStats{
			Searchterm:     s.query,
			QueryId:        queryid,
			NumEvents:      len(s.events),
			Done:           s.done,
			Started:        s.started,
			Duration:       time.Since(s.started),
			NumResults:     len(s.resultPointers),
			NumPackages:    len(s.allPackages),
			NumResultPages: s.resultPages,
			FilesTotal:     s.filesTotal,
			FilesProcessed: s.filesProcessed,
		}
		if stats[idx].NumResults == 0 && stats[idx].Done {
			stats[idx].NumResults = s.numResults
		}
		idx++
	}
	stateMu.Unlock()
	if err := common.Templates.ExecuteTemplate(w, "queryz.html", map[string]interface{}{
		"queries": stats,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// Caller needs to hold s.clientsMu
func sendPaginationUpdate(queryid string, s queryState) {
	type Pagination struct {
		// Set to “pagination”.
		Type        string
		QueryId     string
		ResultPages int
	}

	if s.resultPages > 0 {
		addEventMarshal(queryid, &Pagination{
			Type:        "pagination",
			QueryId:     queryid,
			ResultPages: s.resultPages,
		})
	}
}

func storeResult(queryid string, backendidx int, result Result) {
	result.Type = "result"

	result.Package = result.Path[:strings.Index(result.Path, "_")]

	// Without acquiring a lock, just check if we need to consider this result
	// for the top 10 at all.
	s := state[queryid]

	if s.FirstPathRank > 0 {
		// Now store the combined ranking of PathRanking (pre) and Ranking (post).
		// We add the values because they are both percentages.
		// To make the Ranking (post) less significant, we multiply it with
		// 1/10 * FirstPathRank. We used to use maxPathRanking here, but
		// requiring that means delaying the search until all results are
		// there. Instead, FirstPathRank is a good enough approximation (but
		// different enough for each query that we can’t hardcode it).
		result.Ranking = result.PathRank + ((s.FirstPathRank * 0.1) * result.Ranking)
	} else {
		s.FirstPathRank = result.PathRank
	}

	worst := s.results[9]
	if result.Ranking > worst.Ranking {
		s.resultMu.Lock()

		// TODO: find the first s.result[] for the same package. then check again if the result is worthy of replacing that per-package result
		// TODO: probably change the data structure so that we can do this more easily and also keep N results per package.

		combined := append(s.results[:], result)
		sort.Sort(ByRanking(combined))
		copy(s.results[:], combined[:10])
		state[queryid] = s
		s.resultMu.Unlock()

		// The result entered the top 10, so send it to the client(s) for
		// immediate display.
		addEventMarshal(queryid, &result)
	}

	tmpOffset, err := state[queryid].tempFiles[backendidx].Seek(0, os.SEEK_CUR)
	if err != nil {
		log.Printf("[%s] could not seek: %v\n", queryid, err)
		failQuery(queryid)
		return
	}

	if err := json.NewEncoder(s.tempFiles[backendidx]).Encode(result); err != nil {
		log.Printf("[%s] could not write %v: %v\n", queryid, result, err)
		failQuery(queryid)
		return
	}

	offsetAfterWriting, err := state[queryid].tempFiles[backendidx].Seek(0, os.SEEK_CUR)
	if err != nil {
		log.Printf("[%s] could not seek: %v\n", queryid, err)
		failQuery(queryid)
		return
	}

	h := fnv.New64()
	io.WriteString(h, result.Path)

	stateMu.Lock()
	s = state[queryid]
	s.resultPointers = append(s.resultPointers, resultPointer{
		backendidx:  backendidx,
		ranking:     result.Ranking,
		offset:      tmpOffset,
		length:      offsetAfterWriting - tmpOffset,
		pathHash:    h.Sum64(),
		packageName: s.packagePool.Get(result.Package)})
	s.allPackages[result.Package] = true
	s.numResults++
	state[queryid] = s
	stateMu.Unlock()
}

func failQuery(queryid string) {
	varz.Increment("failed-queries")
	addEventMarshal(queryid, &Error{
		Type:      "error",
		ErrorType: "failed",
	})
	finishQuery(queryid)
}

func finishQuery(queryid string) {
	log.Printf("[%s] done, closing all client channels.\n", queryid)
	stateMu.Lock()
	s := state[queryid]
	for _, f := range s.tempFiles {
		f.Close()
	}
	state[queryid] = s
	stateMu.Unlock()
	addEvent(queryid, []byte{}, nil)

	if *influxDBHost != "" {
		go func() {
			db, err := influxdb.NewClient(&influxdb.ClientConfig{
				Host:     *influxDBHost,
				Database: *influxDBDatabase,
				Username: *influxDBUsername,
				Password: *influxDBPassword,
			})
			if err != nil {
				log.Printf("Cannot log query-finished timeseries: %v\n", err)
				return
			}

			var seriesBatch []*influxdb.Series
			series := influxdb.Series{
				Name:    "query-finished.int-dcsi-web",
				Columns: []string{"queryid", "searchterm", "milliseconds", "results"},
				Points: [][]interface{}{
					[]interface{}{
						queryid,
						state[queryid].query,
						time.Since(state[queryid].started) / time.Millisecond,
						state[queryid].numResults,
					},
				},
			}
			seriesBatch = append(seriesBatch, &series)

			if err := db.WriteSeries(seriesBatch); err != nil {
				log.Printf("Cannot log query-finished timeseries: %v\n", err)
				return
			}
		}()
	}
}

type ByModTime []os.FileInfo

func (s ByModTime) Len() int {
	return len(s)
}

func (s ByModTime) Less(i, j int) bool {
	return s[i].ModTime().Before(s[j].ModTime())
}

func (s ByModTime) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func availableBytes(path string) uint64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		log.Fatal("Could not stat filesystem for %q: %v\n", path, err)
	}
	log.Printf("Available bytes on %q: %d\n", path, stat.Bavail*uint64(stat.Bsize))
	return stat.Bavail * uint64(stat.Bsize)
}

func ensureEnoughSpaceAvailable() {
	headroom := uint64(2 * 1024 * 1024 * 1024)
	if availableBytes(*queryResultsPath) >= headroom {
		return
	}

	log.Printf("Deleting an old query...\n")
	dir, err := os.Open(*queryResultsPath)
	if err != nil {
		log.Fatal(err)
	}
	defer dir.Close()
	infos, err := dir.Readdir(-1)
	if err != nil {
		log.Fatal(err)
	}
	sort.Sort(ByModTime(infos))
	for _, info := range infos {
		if !info.IsDir() {
			continue
		}
		log.Printf("Removing query results for %q to make enough space\n", info.Name())
		if err := os.RemoveAll(filepath.Join(*queryResultsPath, info.Name())); err != nil {
			log.Fatal(err)
		}
		if availableBytes(*queryResultsPath) >= headroom {
			break
		}
	}
}

func createFromPointers(queryid string, name string, pointers []resultPointer) error {
	log.Printf("[%s] writing %q\n", queryid, name)
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte("[")); err != nil {
		return err
	}
	for idx, pointer := range pointers {
		src := state[queryid].tempFiles[pointer.backendidx]
		if _, err := src.Seek(pointer.offset, os.SEEK_SET); err != nil {
			return err
		}
		if idx > 0 {
			if _, err := f.Write([]byte(",")); err != nil {
				return err
			}
		}
		if _, err := io.CopyN(f, src, pointer.length); err != nil {
			return err
		}
	}
	if _, err := f.Write([]byte("]\n")); err != nil {
		return err
	}
	return nil
}

func writeToDisk(queryid string) error {
	// Get the slice with results and unset it on the state so that processing can continue.
	stateMu.Lock()
	s := state[queryid]
	pointers := s.resultPointers
	if len(pointers) == 0 {
		log.Printf("[%s] not writing, no results.\n", queryid)
		stateMu.Unlock()
		return nil
	}
	s.resultPointers = nil
	idx := 0
	packages := make([]string, len(s.allPackages))
	// TODO: sort by ranking as soon as we store the best ranking with each package. (at the moment it’s first result, first stored)
	for pkg, _ := range s.allPackages {
		packages[idx] = pkg
		idx++
	}
	s.allPackagesSorted = packages
	state[queryid] = s
	stateMu.Unlock()

	log.Printf("[%s] writing, %d results.\n", queryid, len(pointers))
	log.Printf("[%s] packages: %v\n", queryid, packages)

	sort.Sort(pointerByRanking(pointers))

	resultsPerPage := 10
	dir := filepath.Join(*queryResultsPath, queryid)
	if err := os.MkdirAll(dir, os.FileMode(0755)); err != nil {
		return err
	}

	// TODO: it’d be so much better if we would correctly handle ESPACE errors
	// in the code below (and above), but for that we need to carefully test it.
	ensureEnoughSpaceAvailable()

	f, err := os.Create(filepath.Join(dir, "packages.json"))
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(struct{ Packages []string }{packages}); err != nil {
		return err
	}
	f.Close()

	pages := int(math.Ceil(float64(len(pointers)) / float64(resultsPerPage)))
	for page := 0; page < pages; page++ {
		start := page * resultsPerPage
		end := (page + 1) * resultsPerPage
		if end > len(pointers) {
			end = len(pointers)
		}

		name := filepath.Join(dir, fmt.Sprintf("page_%d.json", page))
		if err := createFromPointers(queryid, name, pointers[start:end]); err != nil {
			return err
		}
	}

	// Now save the results into their package-specific files.
	bypkg := make(map[string][]resultPointer)
	for _, pointer := range pointers {
		pkgresults := bypkg[*pointer.packageName]
		if len(pkgresults) >= resultsPerPackage {
			continue
		}
		pkgresults = append(pkgresults, pointer)
		bypkg[*pointer.packageName] = pkgresults
	}

	for pkg, pkgresults := range bypkg {
		name := filepath.Join(dir, fmt.Sprintf("pkg_%s.json", pkg))
		if err := createFromPointers(queryid, name, pkgresults); err != nil {
			return err
		}
	}

	stateMu.Lock()
	s = state[queryid]
	s.resultPages = pages
	state[queryid] = s
	stateMu.Unlock()

	sendPaginationUpdate(queryid, s)
	return nil
}

func storeProgress(queryid string, backendidx int, progress Result) {
	backends := strings.Split(*common.SourceBackends, ",")
	s := state[queryid]
	s.filesMu.Lock()
	s.filesTotal[backendidx] = progress.FilesTotal
	s.filesMu.Unlock()
	allSet := true
	for i := 0; i < len(backends); i++ {
		if s.filesTotal[i] == -1 {
			log.Printf("total number for backend %d missing\n", i)
			allSet = false
			break
		}
	}

	s.filesMu.Lock()
	s.filesProcessed[backendidx] = progress.FilesProcessed
	s.filesMu.Unlock()

	filesProcessed := 0
	for _, processed := range s.filesProcessed {
		filesProcessed += processed
	}
	filesTotal := 0
	for _, total := range s.filesTotal {
		filesTotal += total
	}

	if allSet && filesProcessed == filesTotal {
		log.Printf("[%s] [src:%d] query done on all backends, writing to disk.\n", queryid, backendidx)
		if err := writeToDisk(queryid); err != nil {
			log.Printf("[%s] writeToDisk() failed: %v\n", queryid)
			failQuery(queryid)
		}
	}

	if allSet {
		log.Printf("[%s] [src:%d] (sending) progress: %d of %d\n", queryid, backendidx, progress.FilesProcessed, progress.FilesTotal)
		addEventMarshal(queryid, &ProgressUpdate{
			Type:           progress.Type,
			QueryId:        queryid,
			FilesProcessed: filesProcessed,
			FilesTotal:     filesTotal,
			Results:        s.numResults,
		})
		if filesProcessed == filesTotal {
			finishQuery(queryid)
		}
	} else {
		log.Printf("[%s] [src:%d] progress: %d of %d\n", queryid, backendidx, progress.FilesProcessed, progress.FilesTotal)
	}
}

func PerPackageResultsHandler(w http.ResponseWriter, r *http.Request) {
	matches := perPackagePathRe.FindStringSubmatch(r.URL.Path)
	if matches == nil || len(matches) != 3 {
		// TODO: what about non-js clients?
		// While this just serves index.html, the javascript part of index.html
		// realizes the path starts with /perpackage-results/ and starts the
		// search, then requests the specified page on search completion.
		http.ServeFile(w, r, filepath.Join(*staticPath, "index.html"))
		return
	}
	queryid := matches[1]
	pagenr, err := strconv.Atoi(matches[2])
	if err != nil {
		log.Fatal("Could not convert %q into a number: %v\n", matches[2], err)
	}
	s, ok := state[queryid]
	if !ok {
		http.Error(w, "No such query.", http.StatusNotFound)
		return
	}
	if !s.done {
		started := time.Now()
		for time.Since(started) < 60*time.Second {
			if state[queryid].done {
				s = state[queryid]
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !s.done {
			log.Printf("[%s] query not yet finished, cannot produce per-package results\n", queryid)
			http.Error(w, "Query not finished yet.", http.StatusInternalServerError)
			return
		}
	}

	pages := int(math.Ceil(float64(len(s.allPackagesSorted)) / float64(packagesPerPage)))
	if pagenr >= pages {
		log.Printf("[%s] page %d not found (total %d pages)\n", queryid, pagenr, pages)
		http.Error(w, "No such page.", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Advise the client to cache the results for one hour. This needs to match
	// the nginx configuration for serving static files (the not-per-package
	// results are served directly by nginx).
	utc := time.Now().UTC()
	cacheSince := utc.Format(http.TimeFormat)
	cacheUntil := utc.Add(1 * time.Hour).Format(http.TimeFormat)
	w.Header().Set("Cache-Control", "max-age=3600, public")
	w.Header().Set("Last-Modified", cacheSince)
	w.Header().Set("Expires", cacheUntil)

	log.Printf("[%s] Computing per-package results for page %d\n", queryid, pagenr)
	dir := filepath.Join(*queryResultsPath, queryid)

	start := pagenr * packagesPerPage
	end := (pagenr + 1) * packagesPerPage
	if end > len(s.allPackagesSorted) {
		end = len(s.allPackagesSorted)
	}

	// We concatenate a JSON reply that essentially contains multiple JSON
	// files by directly writing to a buffer in order to avoid
	// decoding/encoding the same data. We cannot write directly to the
	// ResponseWriter because we may still need to use http.Error(), which must
	// be called before sending any content.
	//
	// Perhaps a better way would be to use HTTP2 and send multiple files to
	// the client.
	var buffer bytes.Buffer
	buffer.Write([]byte("["))

	for _, pkg := range s.allPackagesSorted[start:end] {
		if buffer.Len() == 1 {
			fmt.Fprintf(&buffer, `{"Package": "%s", "Results":`, pkg)
		} else {
			fmt.Fprintf(&buffer, `,{"Package": "%s", "Results":`, pkg)
		}
		f, err := os.Open(filepath.Join(dir, "pkg_"+pkg+".json"))
		if err != nil {
			http.Error(w, fmt.Sprintf("Could not open %q: %v", "pkg_"+pkg+".json", err), http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(&buffer, f); err != nil {
			http.Error(w, fmt.Sprintf("Could not read %q: %v", "pkg_"+pkg+".json", err), http.StatusInternalServerError)
			return
		}
		f.Close()
		fmt.Fprintf(&buffer, `}`)
	}

	buffer.Write([]byte("]"))
	if _, err := io.Copy(w, &buffer); err != nil {
		log.Printf("[%s] Could not send response: %v\n", queryid, err)
	}
}
