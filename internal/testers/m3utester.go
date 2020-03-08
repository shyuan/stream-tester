package testers

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/m3u8"
	"github.com/livepeer/stream-tester/internal/utils"
	"github.com/livepeer/stream-tester/internal/utils/uhttp"
	"github.com/livepeer/stream-tester/model"
	"golang.org/x/net/http2"
)

// HTTPTimeout http timeout downloading manifests/segments
const HTTPTimeout = 16 * time.Second

var httpClient = &http.Client{
	// Transport: &http2.Transport{TLSClientConfig: tlsConfig},
	// Transport: &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: false}},
	// Transport: &http2.Transport{AllowHTTP: true},
	Timeout: HTTPTimeout,
}

var http2Client = &http.Client{
	// Transport: &http2.Transport{TLSClientConfig: tlsConfig},
	// Transport: &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: false}},
	// Transport: &http2.Transport{AllowHTTP: true},
	Transport: &http2.Transport{},
	Timeout:   HTTPTimeout,
}

var wowzaSessionRE *regexp.Regexp = regexp.MustCompile(`_(w\d+)_`)
var wowzaBandwidthRE *regexp.Regexp = regexp.MustCompile(`_b(\d+)\.`)
var mistSessionRE *regexp.Regexp = regexp.MustCompile(`(\?sessId=\d+)`)

// m3utester tests one stream, reading all the media streams
type m3utester struct {
	initialURL       *url.URL
	downloads        map[string]*mediaDownloader
	downloadsKeys    []string // first should be source
	mu               sync.RWMutex
	started          bool
	finished         bool
	wowzaMode        bool
	mistMode         bool
	infiniteMode     bool
	save             bool
	startTime        time.Time
	done             <-chan struct{} // signals to stop
	sentTimesMap     *utils.SyncedTimesMap
	segmentsMatcher  *segmentsMatcher
	downloadResults  fullDownloadResultsMap
	fullResultsCh    chan fullDownloadResult
	dm               sync.Mutex
	savePlayList     *m3u8.MasterPlaylist
	savePlayListName string
	saveDirName      string
	cancel           context.CancelFunc
	shouldSkip       [][]string
	// gaps             int
}

type fullDownloadResultsMap map[string]*fullDownloadResults

type fullDownloadResults struct {
	results           []downloadResult
	mediaPlaylistName string
	resolution        string
}

type fullDownloadResult struct {
	downloadResult
	mediaPlaylistName string
	resolution        string
}

type downloadResult struct {
	status             string
	bytes              int
	try                int
	videoParseError    error
	startTime          time.Duration
	duration           time.Duration
	appTime            time.Time
	timeAtFirstPlace   time.Time
	downloadCompetedAt time.Time
	name               string
	seqNo              uint64
	mySeqNo            uint64
	resolution         string
	keyFrames          int
}

func (r *downloadResult) String() string {
	// return fmt.Sprintf("%10s %14s seq %3d: mySeq %3d time %7s duration %7s size %7d bytes appearance time %s (%d)",
	// r.resolution, r.name, r.seqNo, r.mySeqNo, r.startTime, r.duration, r.bytes, r.appTime, r.appTime.UnixNano())
	return fmt.Sprintf("%10s %14s seq %3d: time %7s duration %7s size %7d bytes appearance time %s (%d)",
		r.resolution, r.name, r.seqNo, r.startTime, r.duration, r.bytes, r.appTime, r.appTime.UnixNano())
}

func (r *downloadResult) String2() string {
	// return fmt.Sprintf("%10s %14s seq %3d: mySeq %3d time %7s duration %7s size %7d bytes appearance at first place time %s (%d)",
	// 	r.resolution, r.name, r.seqNo, r.mySeqNo, r.startTime, r.duration, r.bytes, r.timeAtFirstPlace, r.timeAtFirstPlace.UnixNano())
	// return fmt.Sprintf("%10s %20s seq %3d: time %7s duration %7s size %7d bytes at first %s (%d)",
	// 	r.resolution, r.name, r.seqNo, r.startTime, r.duration, r.bytes, r.timeAtFirstPlace, r.timeAtFirstPlace.UnixNano())
	return fmt.Sprintf("%10s %20s seq %3d: time %7s duration %7s first %s app %s",
		r.resolution, r.name, r.seqNo, r.startTime, r.duration, r.timeAtFirstPlace.Format(printTimeFormat), r.appTime.Format(printTimeFormat))
}

// const printTimeFormat = "2006-01-02T15:04:05"
const printTimeFormat = "2006-01-02T15:04:05.999999999"

type downloadResultsBySeq []*downloadResult

func (p downloadResultsBySeq) Len() int { return len(p) }
func (p downloadResultsBySeq) Less(i, j int) bool {
	// return p[i].seqNo < p[j].seqNo
	// return p[i].mySeqNo < p[j].mySeqNo
	return p[i].appTime.Before(p[j].appTime)
}
func (p downloadResultsBySeq) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
func (p downloadResultsBySeq) findBySeqNo(seqNo uint64) *downloadResult {
	for _, seg := range p {
		if seg.seqNo == seqNo {
			return seg
		}
	}
	return nil
}
func (p downloadResultsBySeq) findByMySeqNo(seqNo uint64) *downloadResult {
	for _, seg := range p {
		if seg.mySeqNo == seqNo {
			return seg
		}
	}
	return nil
}

// newM3UTester ...
func newM3UTester(ctx context.Context, done <-chan struct{}, sentTimesMap *utils.SyncedTimesMap, wowzaMode, mistMode, infiniteMode, save bool, sm *segmentsMatcher,
	shouldSkip [][]string) *m3utester {

	t := &m3utester{
		downloads:       make(map[string]*mediaDownloader),
		done:            done,
		sentTimesMap:    sentTimesMap,
		wowzaMode:       wowzaMode,
		mistMode:        mistMode,
		infiniteMode:    infiniteMode,
		save:            save,
		segmentsMatcher: sm,
		shouldSkip:      shouldSkip,
		downloadResults: make(map[string]*fullDownloadResults),
		fullResultsCh:   make(chan fullDownloadResult, 16),
	}
	if ctx != nil {
		ct, cancel := context.WithCancel(ctx)
		t.done = ct.Done()
		t.cancel = cancel
	}
	if save {
		t.savePlayList = m3u8.NewMasterPlaylist()
	}
	return t
}

func (mt *m3utester) IsFinished() bool {
	return mt.finished
}

func (mt *m3utester) Stop() {
	if mt.cancel != nil {
		mt.cancel()
	}
}

func (mt *m3utester) Start(u string) {
	purl, err := url.Parse(u)
	if err != nil {
		glog.Fatal(err)
	}
	mt.initialURL = purl
	if mt.save {
		up := strings.Split(u, "/")
		upl := len(up)
		mt.saveDirName = ""
		mt.savePlayListName = up[upl-1]
		if upl > 1 {
			if up[upl-2] == "stream" {
				mt.saveDirName = strings.Split(up[upl-1], ".")[0] + "/"
			} else if mt.wowzaMode || mt.mistMode {
				mt.saveDirName = up[upl-2]
			}
		}
		if mt.saveDirName != "" {
			mt.savePlayListName = path.Join(mt.saveDirName, mt.savePlayListName)
			if _, err := os.Stat(mt.saveDirName); os.IsNotExist(err) {
				os.Mkdir(mt.saveDirName, 0755)
			}
		}
		glog.Infof("Save dir name: '%s', save playlist name %s", mt.saveDirName, mt.savePlayListName)
	}
	go mt.downloadLoop()
	go mt.workerLoop()
	// if mt.infiniteMode {
	// 	go mt.anaylysePrintingLoop()
	// }
}

/*
func (mt *m3utester) anaylysePrintingLoop() string {
	for {
		time.Sleep(30 * time.Second)
		if !mt.startTime.IsZero() {
			mt.dm.Lock()
			a, _ := analyzeDownloads(mt.downloadResults, false, false)
			mt.dm.Unlock()
			glog.Infof("Analysis from start %s:\n%s", time.Since(mt.startTime), a)
		}
	}
}
*/

func sortByResolution(results map[string]*fullDownloadResults) []string {
	r := make([]string, 0, len(results))
	return r
}

func containsString(ss []string, stf string) bool {
	for _, s := range ss {
		if s == stf {
			return true
		}
	}
	return false
}

func (fdr fullDownloadResultsMap) getResolutions() []string {
	res := make([]string, 0)
	for _, dr := range fdr {
		if !containsString(res, dr.resolution) {
			res = append(res, dr.resolution)
		}
	}
	return res
}

func (fdr fullDownloadResultsMap) byResolution(resolution string) []*fullDownloadResults {
	res := make([]*fullDownloadResults, 0)
	for _, dr := range fdr {
		if dr.resolution == resolution {
			res = append(res, dr)
		}
	}
	return res
}

/*
  Should consider 10.867s and 10.866s to be equal
*/
func isTimeEqual(t1, t2 time.Duration) bool {
	diff := t1 - t2
	if diff < 0 {
		diff *= -1
	}
	// 1000000
	// return diff <= time.Millisecond
	return diff <= 1*time.Second
}

func isTimeEqualM(t1, t2 time.Duration) bool {
	diff := t1 - t2
	if diff < 0 {
		diff *= -1
	}
	// 1000000
	return diff <= 100*time.Millisecond
}

func isTimeEqualT(t1, t2 time.Time) bool {
	diff := t1.Sub(t2)
	if diff < 0 {
		diff *= -1
	}
	// 1000000
	return diff <= 10*time.Millisecond
}

func isTimeEqualTD(t1, t2 time.Time, d time.Duration) bool {
	diff := t1.Sub(t2)
	if diff < 0 {
		diff *= -1
	}
	return diff <= d
}

func absTimeTiff(t1, t2 time.Duration) time.Duration {
	diff := t1 - t2
	if diff < 0 {
		diff *= -1
	}
	return diff
}

func absTimeTiffT(t1, t2 time.Time) time.Duration {
	diff := t1.Sub(t2)
	if diff < 0 {
		diff *= -1
	}
	return diff
}

/*
func analyzeDownloads(downloadResults fullDownloadResultsMap, short, streamEnded bool) (string, int) {
	res := ""
	resolutions := downloadResults.getResolutions()
	byRes := make(map[string]downloadResultsBySeq)
	short = false
	var gaps int
	for _, resolution := range resolutions {
		results := make(downloadResultsBySeq, 0)
		fresults := downloadResults.byResolution(resolution)
		for _, rs := range fresults {
			for _, r := range rs.results {
				rl := r
				results = append(results, &rl)
			}
		}
		sort.Sort(results)
		byRes[resolution] = results
		res += fmt.Sprintf("=== For resolution %s:\n", resolution)
		allGood := "=== All is good! ===\n"
		if len(results) == 0 {
			res += "No segments!!!!\n"
			continue
		}
		if !short {
			res += fmt.Sprintf("==== Results sorted:\n")
			var tillNext time.Duration
			var problem string
			for i, r := range results {
				problem = ""
				tillNext = 0
				if i < len(results)-1 {
					ns := results[i+1]
					tillNext = ns.startTime - r.startTime
					if tillNext > 0 && !isTimeEqualM(r.duration, tillNext) {
						problem = fmt.Sprintf(" ===> possible gap - to big time difference %s", r.duration-tillNext)
					}
				}
				res += fmt.Sprintf("%10s %14s seq %3d: mySeq %3d time %s duration %s till next %s appearance time %s %s\n",
					resolution, r.name, r.seqNo, r.mySeqNo, r.startTime, r.duration, tillNext, r.appTime, problem)
			}
		}
		if results[0].seqNo > 1 {
			res += fmt.Sprintf("Segments start from %d\n", results[0].seqNo)
		}
		var lastSeq, lastRSeq uint64
		var lastStartTime time.Duration
		var lastFileName string
		for _, seg := range results {
			if seg.mySeqNo != lastSeq+1 {
				if seg.mySeqNo > lastSeq {
					res += fmt.Sprintf("Gap in sequence - file %s with seqNo %d mySeq %d (start time %s), previous seqNo is %d mySeq %d (start time %s)\n",
						seg.name, seg.seqNo, seg.mySeqNo, seg.startTime, lastSeq, lastRSeq, lastStartTime)
					allGood = ""
					gaps++
				} else if seg.mySeqNo == lastSeq {
					if seg.startTime != lastStartTime {
						res += fmt.Sprintf("Media stream switched, but corresponding segments have different time stamp: file %s with seqNo %d (start time %s), previous file %s seqNo is %d (start time %s)\n",
							seg.name, seg.seqNo, seg.startTime, lastFileName, lastSeq, lastStartTime)
						allGood = ""
					}
				} else if seg.mySeqNo < lastSeq {
					res += fmt.Sprintf("Very strange problem - seq is less than previous: file %s with seqNo %d (start time %s), previous seqNo is %d (start time %s)\n", seg.name, seg.seqNo, seg.startTime, lastSeq, lastStartTime)
					allGood = ""
				}
			}
			lastSeq = seg.mySeqNo
			lastRSeq = seg.seqNo
			lastStartTime = seg.startTime
			lastFileName = seg.name
		}
		res += allGood
	}
	// now check timestamps alignments in different renditions
	lastTimeDiffs := make(map[string]map[string]time.Duration)
	oneStep := make(map[string]map[string]bool)
	for resolution, resRes := range byRes {
		for i, seg := range resRes {
			for sresolution, resRes2 := range byRes {
				if sresolution == resolution {
					continue
				}
				if _, has := oneStep[resolution]; !has {
					oneStep[resolution] = make(map[string]bool)
				}
				if _, has := lastTimeDiffs[resolution]; !has {
					lastTimeDiffs[resolution] = make(map[string]time.Duration)
				}
				// altSeg := resRes2.findBySeqNo(seg.seqNo)
				altSeg := resRes2.findByMySeqNo(seg.mySeqNo)
				if altSeg == nil {
					if streamEnded || i < len(resRes)-4 {
						res += fmt.Sprintf("Segment %10s seqNo %3d mySeq %3d doesn't have corresponding segment in %10s\n", resolution, seg.seqNo, seg.mySeqNo, sresolution)
					}
				} else {
					if !isTimeEqual(seg.startTime, altSeg.startTime) {
						altSegM := resRes2.findBySeqNo(seg.seqNo - 1)
						altSegP := resRes2.findBySeqNo(seg.seqNo + 1)
						altSegM2 := resRes2.findBySeqNo(seg.seqNo - 2)
						altSegP2 := resRes2.findBySeqNo(seg.seqNo + 2)
						diff := seg.startTime - altSeg.startTime
						lastDiff := lastTimeDiffs[resolution][sresolution]
						if diff != lastDiff {
							// if !oneStep[resolution][sresolution] {
							res += fmt.Sprintf("Segment %10s seqNo %5d mySeq %3d has time %s but segment %10s seqNo %5d mySeq %3d has time %s diff %s\n",
								resolution, seg.seqNo, seg.mySeqNo, seg.startTime, sresolution, altSeg.seqNo, altSeg.mySeqNo, altSeg.startTime, seg.startTime-altSeg.startTime)
							// }
							if altSegM != nil && isTimeEqual(seg.startTime, altSegM.startTime) {
								if !oneStep[resolution][sresolution] {
									res += fmt.Sprintf("Stream %10s is one step behind stream %10s\n", resolution, sresolution)
									oneStep[resolution][sresolution] = true
									// break
								}
							}
							if altSegP != nil && isTimeEqual(seg.startTime, altSegP.startTime) {
								if !oneStep[resolution][sresolution] {
									res += fmt.Sprintf("Stream %10s is one step ahead stream %10s\n", resolution, sresolution)
									oneStep[resolution][sresolution] = true
									// break
								}
							}
							if altSegM2 != nil && isTimeEqual(seg.startTime, altSegM2.startTime) {
								if !oneStep[resolution][sresolution] {
									res += fmt.Sprintf("Stream %10s is two steps behind stream %10s\n", resolution, sresolution)
									oneStep[resolution][sresolution] = true
									// break
								}
							}
							if altSegP2 != nil && isTimeEqual(seg.startTime, altSegP2.startTime) {
								if !oneStep[resolution][sresolution] {
									res += fmt.Sprintf("Stream %10s is two steps ahead stream %10s\n", resolution, sresolution)
									oneStep[resolution][sresolution] = true
									// break
								}
							}
							// res += fmt.Sprintf("%d - %d\n", seg.startTime, altSeg.startTime)
							lastTimeDiffs[resolution][sresolution] = diff
						}
					}
				}
			}
		}
	}
	return res, gaps
}

type fullDownloadResultsArray []*fullDownloadResults

func (p fullDownloadResultsArray) Len() int { return len(p) }
func (p fullDownloadResultsArray) Less(i, j int) bool {
	ms1 := wowzaBandwidthRE.FindStringSubmatch(p[i].mediaPlaylistName)
	ms2 := wowzaBandwidthRE.FindStringSubmatch(p[j].mediaPlaylistName)
	// glog.Infof("name1 %s name2 %d res %+v res2 %+v", p[i].mediaPlaylistName, p[j].mediaPlaylistName, ms1, ms2)
	b1, _ := strconv.Atoi(ms1[1])
	b2, _ := strconv.Atoi(ms2[1])
	return b1 < b2
}
func (p fullDownloadResultsArray) Swap(i, j int) { p[i], p[j] = p[j], p[i] }

func sortByBandwidth(results map[string]*fullDownloadResults) fullDownloadResultsArray {
	res := make(fullDownloadResultsArray, 0, len(results))
	for _, r := range results {
		res = append(res, r)
	}
	sort.Sort(res)
	return res
}

func (mt *m3utester) DownloadStatsFormatted() string {
	mt.dm.Lock()
	defer mt.dm.Unlock()
	res := fmt.Sprintf("Has %d media playlists:\n", len(mt.downloadResults))
	sortedResults := sortByBandwidth(mt.downloadResults)
	for _, cdr := range sortedResults {
		res += fmt.Sprintf("Media playlist %25s resolution %10s segments %4d\n", cdr.mediaPlaylistName, cdr.resolution, len(cdr.results))
	}
	for _, cdr := range sortedResults {
		res += fmt.Sprintf("Media playlist %s:\n", cdr.mediaPlaylistName)
		for _, dr := range cdr.results {
			res += fmt.Sprintf("%s %s seqNo=%3d start time %s duration %s appearance time %s\n", cdr.resolution, dr.name, dr.seqNo, dr.startTime, dr.duration, dr.appTime)
			// startTime       time.Duration
			// duration        time.Duration
			// name            string
			// seqNo           uint64
		}
	}
	// res += "\nAnalysis:\n"
	// res += analyzeDownloads(mt.downloadResults)
	return res
}

func (mt *m3utester) AnalyzeFormatted(short bool) string {
	res := "\nAnalysis:\n"
	mt.dm.Lock()
	res1, _ := analyzeDownloads(mt.downloadResults, short, false)
	res += res1
	mt.dm.Unlock()
	return res
}
*/

// GetFIrstSegmentTime return timestamp of first frame of first segment.
// Second returned value is true if already found.
func (mt *m3utester) GetFIrstSegmentTime() (time.Duration, bool) {
	mt.mu.RLock()
	defer mt.mu.RUnlock()
	for _, md := range mt.downloads {
		if md.firstSegmentParsed {
			return md.firstSegmentTime, true
		}
	}
	return 0, false
}
func (mt *m3utester) statsSeparate() []*downloadStats {
	mt.mu.RLock()
	rs := make([]*downloadStats, 0, len(mt.downloads))
	for _, key := range mt.downloadsKeys {
		md := mt.downloads[key]
		md.mu.Lock()
		st := md.stats.clone()
		rs = append(rs, st)
		md.mu.Unlock()
	}
	mt.mu.RUnlock()
	return rs
}

func (mt *m3utester) stats() downloadStats {
	stats := downloadStats{
		errors: make(map[string]int),
	}
	mt.mu.RLock()
	for i, d := range mt.downloads {
		glog.V(model.DEBUG).Infof("==> for media stream %s succ %d fail %d", i, d.stats.success, d.stats.fail)
		d.mu.Lock()
		stats.bytes += d.stats.bytes
		stats.success += d.stats.success
		stats.fail += d.stats.fail
		if d.source {
			stats.keyframes = d.stats.keyframes
		}
		for e, en := range d.stats.errors {
			stats.errors[e] = stats.errors[e] + en
		}
		d.mu.Unlock()
	}
	mt.mu.RUnlock()
	// mt.dm.Lock()
	// _, gaps := analyzeDownloads(mt.downloadResults, true, false)
	// stats.gaps = gaps
	// mt.dm.Unlock()
	return stats
}

/*
func (mt *m3utester) StatsFormatted() string {
	mt.mu.RLock()
	keys := getSortedKeys(mt.downloads)
	r := ""
	for _, u := range keys {
		d := mt.downloads[u]
		r += fmt.Sprintf("Stats for %s\n", u)
		r += d.statsFormatted()
	}
	mt.mu.RUnlock()
	return r
}
*/

func (mt *m3utester) workerLoop() {
	for {
		select {
		case <-mt.done:
			return
		case fr := <-mt.fullResultsCh:
			mt.dm.Lock()
			if _, has := mt.downloadResults[fr.mediaPlaylistName]; !has {
				mt.downloadResults[fr.mediaPlaylistName] = &fullDownloadResults{resolution: fr.resolution, mediaPlaylistName: fr.mediaPlaylistName}
			}
			// turn off for now
			// r := mt.downloadResults[fr.mediaPlaylistName]
			// r.results = append(r.results, fr.downloadResult)
			mt.dm.Unlock()
			if mt.save {
				err := ioutil.WriteFile(mt.savePlayListName, mt.savePlayList.Encode().Bytes(), 0644)
				if err != nil {
					glog.Fatal(err)
				}
			}
		}
	}
}

func (mt *m3utester) downloadLoop() {
	surl := mt.initialURL.String()
	// loops := 0
	// var gotPlaylistWaitingForEnd bool
	var gotPlaylist bool
	if mt.infiniteMode {
		glog.Infof("Waiting for playlist %s", surl)
	}
	mistMediaStreams := make(map[string]string) // maps clean urls to urls with session

	for {
		select {
		case <-mt.done:
			return
		default:
		}
		/*
			if gotPlaylistWaitingForEnd {
				time.Sleep(2 * time.Second)
				loops++
				if loops%2 == 0 {
					if glog.V(model.DEBUG) {
						fmt.Println(mt.StatsFormatted())
					}
				}
				continue
			}
		*/
		resp, err := httpClient.Do(uhttp.GetRequest(surl))
		if err != nil {
			glog.Infof("===== get error getting master playlist %s: %v", surl, err)
			time.Sleep(2 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			glog.Infof("===== status error getting master playlist %s: %v (%s) body: %s", surl, resp.StatusCode, resp.Status, string(b))
			time.Sleep(2 * time.Second)
			continue
		}
		b, err := ioutil.ReadAll(resp.Body)
		// err = mpl.DecodeFrom(resp.Body, true)
		mpl := m3u8.NewMasterPlaylist()
		err = mpl.Decode(*bytes.NewBuffer(b), true)
		resp.Body.Close()
		if err != nil {
			glog.Info("===== error getting master playlist: ", err)
			// glog.Error(err)
			time.Sleep(2 * time.Second)
			continue
		}
		glog.V(model.VVERBOSE).Infof("Got master playlist with %d variants (%s):", len(mpl.Variants), surl)
		glog.V(model.VVERBOSE).Info(mpl)
		// glog.Infof("Got master playlist with %d variants (%s):", len(mpl.Variants), surl)
		// glog.Info(mpl)
		if !mt.wowzaMode || len(mpl.Variants) > model.ProfilesNum {
			if mt.infiniteMode {
				if !gotPlaylist {
					glog.Infof("Got playlist for %s with %d variants", surl, len(mpl.Variants))
					mt.startTime = time.Now()
					gotPlaylist = true
				}
			}
			// if len(mpl.Variants) > 1 && !gotPlaylistWaitingForEnd {
			// gotPlaylistWaitingForEnd = true
			for i, variant := range mpl.Variants {
				// glog.Infof("Variant URI: %s", variant.URI)
				if mt.wowzaMode {
					// remove Wowza's session id from URL
					variant.URI = wowzaSessionRE.ReplaceAllString(variant.URI, "_")
				}
				if mt.mistMode {
					vURIClean := mistSessionRE.ReplaceAllString(variant.URI, "")
					if firstURI, has := mistMediaStreams[vURIClean]; has {
						variant.URI = firstURI
					} else {
						mistMediaStreams[vURIClean] = variant.URI
					}
				}
				pvrui, err := url.Parse(variant.URI)
				if err != nil {
					glog.Error(err)
					panic(err)
				}
				// glog.Infof("Parsed uri: %+v", pvrui, pvrui.IsAbs)
				if !pvrui.IsAbs() {
					pvrui = mt.initialURL.ResolveReference(pvrui)
				}
				// glog.Info(pvrui)
				mediaURL := pvrui.String()
				// Wowza changes media manifests on each fetch, so indentifying streams by
				// bandwitdh and
				// variantID := strconv.Itoa(variant.Bandwidth) + variant.Resolution
				mt.mu.Lock()
				if _, ok := mt.downloads[mediaURL]; !ok {
					var shouldSkip []string
					if len(mt.shouldSkip) > i {
						shouldSkip = mt.shouldSkip[i]
					}
					md := newMediaDownloader(variant.URI, mediaURL, variant.Resolution, mt.done, mt.sentTimesMap, mt.wowzaMode, mt.save,
						mt.fullResultsCh, mt.saveDirName, mt.segmentsMatcher, shouldSkip)
					mt.downloads[mediaURL] = md
					// md.source = strings.Contains(mediaURL, "source")
					md.source = i == 0
					md.stats.source = md.source
					if mt.save {
						mt.savePlayList.Append(variant.URI, md.savePlayList, variant.VariantParams)
					}
					if len(mt.downloadsKeys) > 0 && md.source {
						panic(fmt.Sprintf("Source stream should be first, instead found %s", mt.downloadsKeys[0]))
					}
					mt.downloadsKeys = append(mt.downloadsKeys, mediaURL)
				}
				mt.mu.Unlock()
			}
			// glog.Infof("Processed playlist with %d variant, not checking anymore", len(mpl.Variants))
			// return
		}
		// }
		// glog.Info(string(b))
		time.Sleep(2 * time.Second)
		/*
			loops++
			if loops%2 == 0 {
				if glog.V(model.DEBUG) {
					fmt.Println(mt.StatsFormatted())
				}
			}
		*/
	}
}

type downloadStats struct {
	success int
	fail    int
	retries int
	// gaps      int
	keyframes  int
	bytes      int64
	errors     map[string]int
	resolution string
	source     bool
}

type downloadTask struct {
	baseURL  *url.URL
	url      string
	seqNo    uint64
	title    string
	duration float64
	mySeqNo  uint64
	appTime  time.Time
}

func (ds *downloadStats) formatForConsole() string {
	r := fmt.Sprintf(`Success: %7d
`, ds.success)
	return r
}

// mediaDownloader downloads all the segments from one media stream
// (it constanly reloads manifest, and downloads any segments found in manifest)
type mediaDownloader struct {
	name               string // usually medial playlist relative name
	resolution         string
	u                  *url.URL
	stats              downloadStats
	downTasks          chan downloadTask
	mu                 sync.Mutex
	firstSegmentParsed bool
	firstSegmentTime   time.Duration
	firstSegmentTimes  sortedTimes     // PTSs of first segments
	done               <-chan struct{} // signals to stop
	sentTimesMap       *utils.SyncedTimesMap
	latencies          []time.Duration // latencies stored as segments get downloaded
	latenciesPerStream []time.Duration // here index is seqNo, so if segment is failed download then value will be zero
	source             bool
	wowzaMode          bool
	shouldSkip         []string
	saveSegmentsToDisk bool
	savePlayList       *m3u8.MediaPlaylist
	savePlayListName   string
	saveDir            string
	livepeerNameSchema bool
	fullResultsCh      chan fullDownloadResult
	segmentsMatcher    *segmentsMatcher
	lastKeyFramesPTSs  sortedTimes
	// downloadedSegments []string // for debugging
}

func newMediaDownloader(name, u, resolution string, done <-chan struct{}, sentTimesMap *utils.SyncedTimesMap, wowzaMode, save bool, frc chan fullDownloadResult,
	baseSaveDir string, sm *segmentsMatcher, shouldSkip []string) *mediaDownloader {
	pu, err := url.Parse(u)
	if err != nil {
		glog.Fatal(err)
	}
	md := &mediaDownloader{
		name:            name,
		u:               pu,
		resolution:      resolution,
		segmentsMatcher: sm,
		shouldSkip:      shouldSkip,
		downTasks:       make(chan downloadTask, 256),
		stats: downloadStats{
			errors:     make(map[string]int),
			resolution: resolution,
		},
		done:               done,
		sentTimesMap:       sentTimesMap,
		wowzaMode:          wowzaMode,
		saveSegmentsToDisk: save,
		fullResultsCh:      frc,
	}
	if save {
		mpl, err := m3u8.NewMediaPlaylist(10000, 10000)
		mpl.MediaType = m3u8.VOD
		mpl.Live = false
		if err != nil {
			panic(err)
		}
		md.savePlayList = mpl
		// md.savePlayListName = up[upl-1]
		md.saveDir = baseSaveDir
		// md.savePlayListName = path.Join(baseSaveDir, up[upl-1])
		if strings.Contains(name, baseSaveDir) {
			md.savePlayListName = name
		} else {
			base, _ := path.Split(md.savePlayListName)
			md.savePlayListName = path.Join(baseSaveDir, name)
			if base != "" {
				md.saveDir = path.Join(baseSaveDir, base)
			}
		}
		glog.V(model.DEBUG).Infof("Media stream %s (%s) save dir %s palylist name %s", name, resolution, md.saveDir, md.savePlayListName)
		up := strings.Split(u, "/")
		upl := len(up)
		if upl > 2 && up[upl-3] == "stream" {
			md.livepeerNameSchema = true
			// dirName = strings.Split(up[upl-1], ".")[0] + "/"
		}
		base, _ := path.Split(md.savePlayListName)
		if base != "" {
			if _, err := os.Stat(base); os.IsNotExist(err) {
				os.Mkdir(base, 0755)
			}
		}
		// if dirName != "" {
		// 	if _, err := os.Stat(dirName); os.IsNotExist(err) {
		// 		os.Mkdir(path.Join(baseSaveDir, dirName), 0755)
		// 	}
		// }
		// md.savePlayListName = path.Join(baseSaveDir, dirName, md.savePlayListName)
	}
	// md.saveSegmentsToDisk = true
	go md.downloadLoop()
	go md.workerLoop()
	return md
}

func (md *mediaDownloader) statsFormatted() string {
	res := fmt.Sprintf("Downloaded: %5d\nFailed:     %5d\nRetries:   %5d\n", md.stats.success, md.stats.fail, md.stats.retries)
	et := 0
	for _, e := range md.stats.errors {
		et += e
	}
	res += fmt.Sprintf("Errors: (%d total)\n", et)
	for en, ec := range md.stats.errors {
		res += fmt.Sprintf("Error %s: %d\n", en, ec)
	}
	return res
}

func (md *mediaDownloader) downloadSegment(task *downloadTask, res chan downloadResult) {
	purl, err := url.Parse(task.url)
	if err != nil {
		glog.Fatal(err)
	}
	fsurl := task.url
	if !purl.IsAbs() {
		fsurl = md.u.ResolveReference(purl).String()
	}
	try := 0
	for {
		glog.V(model.DEBUG).Infof("Downloading segment seqNo=%d url=%s try=%d", task.seqNo, fsurl, try)
		resp, err := httpClient.Do(uhttp.GetRequest(fsurl))
		if err != nil {
			glog.Errorf("Error downloading %s: %v", fsurl, err)
			if try < 4 {
				try++
				continue
			}
			res <- downloadResult{status: err.Error(), try: try}
			return
		}
		b, err := ioutil.ReadAll(resp.Body)
		now := time.Now()
		if err != nil {
			glog.Errorf("Error downloading reading body %s: %v", fsurl, err)
			if try < 4 {
				try++
				continue
			}
			res <- downloadResult{status: err.Error(), try: try}
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			glog.Errorf("Error status downloading segment %s result status %s", fsurl, resp.Status)
			if try < 8 {
				try++
				time.Sleep(time.Second)
				continue
			}
			res <- downloadResult{status: resp.Status, try: try}
			return
		}
		fsttim, dur, keyFrames, skeyFrames, verr := utils.GetVideoStartTimeDurFrames(b)
		if verr != nil {
			msg := fmt.Sprintf("Error parsing video data %s result status %s video data len %d err %v", fsurl, resp.Status, len(b), err)
			glog.Error(msg)
			if model.FailHardOnBadSegments {
				fname := fmt.Sprintf("bad_video_%s_%d.ts", md.name, task.seqNo)
				ioutil.WriteFile(fname, b, 0644)
				glog.Infof("Wrote bad segment to '%s'", fname)
				panic(err)
			}
		} else {
			// add keys
			md.mu.Lock()
			for _, tm := range skeyFrames {
				md.lastKeyFramesPTSs = append(md.lastKeyFramesPTSs, tm)
			}
			sort.Sort(md.lastKeyFramesPTSs)
			if len(md.lastKeyFramesPTSs) > 32 {
				md.lastKeyFramesPTSs = md.lastKeyFramesPTSs[len(md.lastKeyFramesPTSs)-32:]
			}
			md.mu.Unlock()
		}
		glog.V(model.DEBUG).Infof("Download %s result: %s len %d timeStart %s segment duration %s keyframes %d (%+v)",
			fsurl, resp.Status, len(b), fsttim, dur, keyFrames, skeyFrames)
		if !md.firstSegmentParsed && task.seqNo == 0 {
			// fst, err := utils.GetVideoStartTime(b)
			// if err != nil {
			// 	glog.Fatal(err)
			// }
			md.firstSegmentTime = fsttim
			md.firstSegmentParsed = true
		}
		if len(md.firstSegmentTimes) < 32 {
			md.mu.Lock()
			md.firstSegmentTimes = append(md.firstSegmentTimes, fsttim)
			sort.Sort(md.firstSegmentTimes)
			md.mu.Unlock()
		}
		if md.segmentsMatcher != nil {
			// fsttim, dur, err := utils.GetVideoStartTimeAndDur(b)
			// if err != nil {
			// 	if err != io.EOF {
			// 		glog.Fatal(err)
			// 	}
			// } else {
			if verr == nil {
				latency, speedRatio, merr := md.segmentsMatcher.matchSegment(fsttim, dur, now)
				src := "    source"
				if !md.source {
					src = "transcoded"
				}
				glog.V(model.DEBUG).Infof("== downloaded %s seqNo %d start time %s lat %s now %s speed ratio %v merr %v", src, task.seqNo, fsttim, latency, now, speedRatio, merr)
				if merr == nil {
					md.mu.Lock()
					md.latencies = append(md.latencies, latency)
					for len(md.latenciesPerStream) <= int(task.seqNo) {
						md.latenciesPerStream = append(md.latenciesPerStream, 0)
					}
					md.latenciesPerStream[task.seqNo] = latency
					glog.V(model.VVERBOSE).Infof("lat: %+v", md.latenciesPerStream)
					md.mu.Unlock()
				}
				if merr != nil {
					panic(merr)
				}
				/*
					var st time.Time
					var has bool
					if st, has = md.sentTimesMap.GetTime(fsttim, fsurl); has {
						latency = now.Sub(st)
						md.latencies = append(md.latencies, latency)
						md.mu.Lock()
						for len(md.latenciesPerStream) <= int(task.seqNo) {
							md.latenciesPerStream = append(md.latenciesPerStream, 0)
						}
						md.latenciesPerStream[task.seqNo] = latency
						md.mu.Unlock()
					}
					latency2, speedRatio, merr := md.segmentsMatcher.matchSegment(fsttim, dur, now)
					glog.Infof("== downloaded %s seqNo %d start time %s lat %s mlat %s now %s lat found %v sr %v merr %v",
						src, task.seqNo, fsttim, latency, latency2, now, has, speedRatio, merr)
				*/
			}
		}

		if md.saveSegmentsToDisk {
			seg := new(m3u8.MediaSegment)
			seg.URI = task.url
			seg.SeqId = task.seqNo
			seg.Duration = task.duration
			seg.Title = task.title
			// md.savePlayList.AppendSegment(seg)
			md.mu.Lock()
			md.savePlayList.InsertSegment(task.seqNo, seg)
			md.mu.Unlock()

			// glog.Infof("url: %s", task.url)
			upts := strings.Split(fsurl, "/")
			// fn := upts[len(upts)-2] + "-" + path.Base(task.url)
			ind := len(upts) - 2
			fn := path.Base(task.url)
			// if ind < 0 {
			if !md.livepeerNameSchema {
				// ind = 0
				// fn = upts[0]
				for i, n := range upts {
					if n == md.saveDir {
						fn = strings.Join(upts[i+1:], "/")
						break
					}
				}
			} else {
				// fn := fmt.Sprintf("%s-%05d.ts", upts[ind], task.seqNo)
				fn = fmt.Sprintf("%s-%s", upts[ind], fn)
			}
			glog.V(model.INSANE).Infof("Saving segment url=%s fsurl=%s saveDir=%s fn=%s livepeerNameSchema=%v", task.url, fsurl, md.saveDir, fn, md.livepeerNameSchema)
			// playListFileName := md.name
			// if md.wowzaMode {
			// 	dn := upts[len(upts)-2]
			// 	if _, err := os.Stat(dn); os.IsNotExist(err) {
			// 		os.Mkdir(dn, 0755)
			// 	}
			// 	fn = path.Join(dn, upts[len(upts)-1])
			// 	playListFileName = path.Join(dn, playListFileName)
			// }
			err = ioutil.WriteFile(path.Join(md.saveDir, fn), b, 0644)
			if err != nil {
				glog.Fatal(err)
			}
			md.mu.Lock()
			err = ioutil.WriteFile(md.savePlayListName, md.savePlayList.Encode().Bytes(), 0644)
			md.mu.Unlock()
			if err != nil {
				glog.Fatal(err)
			}
			glog.V(model.DEBUG).Infof("Segment %s saved to %s", seg.URI, path.Join(md.saveDir, fn))
		}
		res <- downloadResult{status: resp.Status, bytes: len(b), try: try, name: task.url, seqNo: task.seqNo,
			videoParseError: verr, startTime: fsttim, duration: dur, mySeqNo: task.mySeqNo, appTime: task.appTime, keyFrames: keyFrames}
		return
	}
}

func (md *mediaDownloader) workerLoop() {
	// seen := newStringRing(128 * 1024)
	resultsCahn := make(chan downloadResult, 32) // http status or excpetion
	for {
		select {
		case <-md.done:
			return
		case res := <-resultsCahn:
			md.mu.Lock()
			glog.V(model.VERBOSE).Infof("Got result %+v", res)
			glog.V(model.DEBUG).Infof("Got download result seqNo=%d start time=%s dur=%s keys=%d res=%s name=%s", res.seqNo,
				res.startTime, res.duration, res.keyFrames, md.resolution, res.name)
			md.stats.retries += res.try
			if res.status == "200 OK" {
				md.stats.success++
				md.stats.keyframes += res.keyFrames
				md.stats.bytes += int64(res.bytes)
				md.fullResultsCh <- fullDownloadResult{downloadResult: res, mediaPlaylistName: md.name, resolution: md.resolution}
				// md.downloadedSegments = append(md.downloadedSegments, res.name)
			} else {
				md.stats.fail++
				md.stats.errors[res.status] = md.stats.errors[res.status] + 1
			}
			md.mu.Unlock()
		case task := <-md.downTasks:
			// if seen.Contains(task.url) {
			// 	continue
			// }
			glog.V(model.VERBOSE).Infof("Got task to download: seqNo=%d url=%s", task.seqNo, task.url)
			// seen.Add(task.url)
			go md.downloadSegment(&task, resultsCahn)
		}
	}
}

func (md *mediaDownloader) downloadLoop() {
	surl := md.u.String()
	gotManifest := false
	var mySeqNo uint64
	seen := newStringRing(128 * 1024)
	for {
		select {
		case <-md.done:
			return
		default:
		}
		resp, err := httpClient.Do(uhttp.GetRequest(surl))
		if err != nil {
			glog.Error(err)
			time.Sleep(1 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				glog.Infof("Media playlist %s resolution %s status %v: %v", surl, md.resolution, resp.StatusCode, resp.Status)
			} else {
				glog.Infof("Media playlist %s resolution %s status %v: %v", surl, md.resolution, resp.StatusCode, resp.Status)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		b, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			glog.Infof("Media playlist %s resolution %s mpl read error %v", surl, md.resolution, err)
			time.Sleep(time.Second)
			continue
		}
		// if !md.source {
		// 	fmt.Println("-----################")
		// 	fmt.Println(string(b))
		// }
		// glog.Infoln("-----################")
		// glog.Infoln(string(b))
		// err = mpl.DecodeFrom(resp.Body, true)
		pl, err := m3u8.NewMediaPlaylist(100, 100)
		if err != nil {
			glog.Fatal(err)
		}
		err = pl.Decode(*bytes.NewBuffer(b), true)
		// err = pl.DecodeFrom(resp.Body, true)
		// resp.Body.Close()
		if err != nil {
			glog.Fatal(err)
		}
		glog.V(model.INSANE).Infof("Got media playlist %s with %d (really %d) segments of url %s:", md.resolution, len(pl.Segments), countSegments(pl), surl)
		glog.V(model.INSANE).Info(pl)
		if !gotManifest && md.saveSegmentsToDisk {
			md.savePlayList.TargetDuration = pl.TargetDuration
			md.savePlayList.SeqNo = pl.SeqNo
			gotManifest = true
		}
		// for i := len(pl.Segments) - 1; i >= 0; i-- {
		now := time.Now()
		for i, segment := range pl.Segments {
			// segment := pl.Segments[i]
			if segment != nil {
				// glog.Infof("Segment: %+v", *segment)
				if md.wowzaMode {
					// remove Wowza's session id from URL
					segment.URI = wowzaSessionRE.ReplaceAllString(segment.URI, "_")
				}
				if seen.Contains(segment.URI) {
					continue
				}
				if len(md.shouldSkip) > 0 {
					curi := mistSessionRE.ReplaceAllString(segment.URI, "")
					if utils.StringsSliceContains(md.shouldSkip, curi) {
						seen.Add(segment.URI)
						mySeqNo++
						continue
					}
				}
				seen.Add(segment.URI)
				mySeqNo++
				seqNo := pl.SeqNo + uint64(i)
				// attempt to parse seqNo from file name
				_, fn := path.Split(segment.URI)
				ext := path.Ext(fn)
				fn = strings.TrimSuffix(fn, ext)
				parsedSeq, err := strconv.ParseUint(fn, 10, 64)
				if err == nil {
					seqNo = parsedSeq
				}
				md.downTasks <- downloadTask{url: segment.URI, seqNo: seqNo, title: segment.Title, duration: segment.Duration, mySeqNo: mySeqNo, appTime: now}
				now = now.Add(time.Millisecond)
				// glog.V(model.VERBOSE).Infof("segment %s is of length %f seqId=%d", segment.URI, segment.Duration, segment.SeqId)
			}
		}
		delay := 1 * time.Second
		if md.sentTimesMap != nil || md.segmentsMatcher != nil {
			delay = 100 * time.Millisecond
		}
		time.Sleep(delay)
	}
}

func countSegments(mpl *m3u8.MediaPlaylist) int {
	var res int
	for _, seg := range mpl.Segments {
		if seg != nil {
			res++
		}
	}
	return res
}

/*
func getSortedKeys(data map[string]*mediaDownloader) []string {
	res := make(sort.StringSlice, 0, len(data))
	for k := range data {
		res = append(res, k)
	}
	res.Sort()
	return res
}
*/

type sortedTimes []time.Duration

func (p sortedTimes) Len() int { return len(p) }
func (p sortedTimes) Less(i, j int) bool {
	return p[i] < p[j]
}
func (p sortedTimes) Swap(i, j int) { p[i], p[j] = p[j], p[i] }

func (ds *downloadStats) clone() *downloadStats {
	st := *ds
	st.errors = make(map[string]int)
	for k, v := range ds.errors {
		st.errors[k] = v
	}
	return &st
}
