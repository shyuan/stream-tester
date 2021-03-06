package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/message"
)

const (
	SHORT    = 4
	DEBUG    = 5
	VERBOSE  = 6
	VVERBOSE = 7
	INSANE   = 12
	INSANE2  = 14
)

// IProduction is set to true in the proceess of building Docker container
var IProduction = ""

// Production is set to true in the Docker container
var Production = false

// ErroNotFound stream not found
var ErroNotFound = errors.New("Stream not found")

func init() {
	if IProduction == "true" {
		Production = true
	}
}

// Version version
// content of this constant will be set at build time,
// using -ldflags, using output of the `git describe` command.
var Version = "undefined"

// AppName used `User-Agent` string
var AppName = "stream-tester"

// ProfilesNum number of transcoding profiles
var ProfilesNum = 2

// FailHardOnBadSegments if true then panic if can't parse downloaded segments
var FailHardOnBadSegments bool

// ExitCode exit code
var ExitCode int

// InfinitePuller interface
type InfinitePuller interface {
	// Start blocks
	Start()
}

// Streamer2 interface
type Streamer2 interface {
	StartStreaming(sourceFileName string, rtmpIngestURL, mediaURL string, waitForTarget, timeToStream time.Duration)
	// StartPulling pull arbitrary HLS stream and report found errors
	StartPulling(mediaURL string)
}

// Streamer interface
type Streamer interface {
	StartStreams(sourceFileName, bhost, rtmpPort, mhost, mediaPort string, simStreams, repeat uint, streamDuration time.Duration,
		notFinal, measureLatency, noBar bool, groupStartBy int, startDelayBetweenGroups, waitForTarget time.Duration) (string, error)
	Stats(basedManifestID string) (*Stats, error)
	// StatsFormatted() string
	// DownStatsFormatted() string
	// AnalyzeFormatted(short bool) string
	Done() <-chan struct{}
	Stop() // Stop active streams
	Cancel()
}

// Latencies contains latencies
type Latencies struct {
	Avg time.Duration `json:"avg"`
	P50 time.Duration `json:"p_50"`
	P95 time.Duration `json:"p_95"`
	P99 time.Duration `json:"p_99"`
}

// Stats represents global test statistics
type Stats struct {
	RTMPActiveStreams              int               `json:"rtmp_active_streams"` // number of active RTMP streams
	RTMPstreams                    int               `json:"rtmp_streams"`        // number of RTMP streams
	MediaStreams                   int               `json:"media_streams"`       // number of media streams
	TotalSegmentsToSend            int               `json:"total_segments_to_send"`
	SentSegments                   int               `json:"sent_segments"`
	DownloadedSegments             int               `json:"downloaded_segments"`
	DownloadedSourceSegments       int               `json:"downloaded_source_segments"`
	DownloadedTranscodedSegments   int               `json:"downloaded_transcoded_segments"`
	ShouldHaveDownloadedSegments   int               `json:"should_have_downloaded_segments"`
	FailedToDownloadSegments       int               `json:"failed_to_download_segments"`
	BytesDownloaded                int64             `json:"bytes_downloaded"`
	Retries                        int               `json:"retries"`
	SuccessRate                    float64           `json:"success_rate"` // DownloadedSegments/profilesNum*SentSegments
	SentKeyFrames                  int               `json:"sent_key_frames"`
	DownloadedKeyFrames            int               `json:"downloaded_key_frames"`
	SuccessRate2                   float64           `json:"success_rate2"` // DownloadedKeyFrames/profilesNum*SentKeyFrames
	ConnectionLost                 int               `json:"connection_lost"`
	Finished                       bool              `json:"finished"`
	ProfilesNum                    int               `json:"profiles_num"`
	SourceLatencies                Latencies         `json:"source_latencies"`
	TranscodedLatencies            Latencies         `json:"transcoded_latencies"`
	RawSourceLatencies             []time.Duration   `json:"raw_source_latencies"`
	RawTranscodedLatencies         []time.Duration   `json:"raw_transcoded_latencies"`
	RawTranscodeLatenciesPerStream [][]time.Duration `json:"raw_transcode_latencies_per_stream"`
	WowzaMode                      bool              `json:"wowza_mode"`
	StartTime                      time.Time         `json:"start_time"`
	Errors                         map[string]int    `json:"errors"`
}

// REST requests

// StartStreamsReq start streams request
type StartStreamsReq struct {
	FileName        string `json:"file_name"`
	Host            string `json:"host"`
	MHost           string `json:"media_host"`
	RTMP            uint16 `json:"rtmp"`
	Media           uint16 `json:"media"`
	Repeat          uint   `json:"repeat"`
	Simultaneous    uint   `json:"simultaneous"`
	Time            string `json:"time"`
	ProfilesNum     int    `json:"profiles_num"`
	DoNotClearStats bool   `json:"do_not_clear_stats"`
	MeasureLatency  bool   `json:"measure_latency"`
	HTTPIngest      bool   `json:"http_ingest"`
	Lapi            bool   `json:"lapi"`    // Use Livepeer API to create stream
	Mist            bool   `json:"mist"`    // Streaming into the Mist server
	Presets         string `json:"presets"` // Transcoding profiles to use with Livepeer API
}

// StartStreamsRes start streams response
type StartStreamsRes struct {
	Success        bool   `json:"success"`
	BaseManifestID string `json:"base_manifest_id"`
}

// FormatForConsole formats stats to be shown in console
func (st *Stats) FormatForConsole() string {
	p := message.NewPrinter(message.MatchLanguage("en"))
	r := p.Sprintf(`
Number of RTMP streams:                       %7d
Number of media streams:                      %7d
Started ago                                   %7s
Total number of segments to be sent:          %7d
Total number of segments sent to broadcaster: %7d
Total number of segments read back:           %7d
Total number of segments should read back:    %7d
Number of retries:                            %7d
Success rate:                                     %9.5f%%
Lost connection to broadcaster:               %7d
Source latencies:                             %s
Transcoded latencies:                         %s
Sent key frames:                              %7d
Downloaded key frames:                        %7d
Downloaded source segments:                   %7d
Downloaded transcoded segments:               %7d
Success rate 2:                                   %9.5f%%
Bytes dowloaded:                         %12d`, st.RTMPstreams, st.MediaStreams, time.Now().Sub(st.StartTime), st.TotalSegmentsToSend, st.SentSegments, st.DownloadedSegments,
		st.ShouldHaveDownloadedSegments, st.Retries, st.SuccessRate, st.ConnectionLost, st.SourceLatencies.String(), st.TranscodedLatencies.String(),
		st.SentKeyFrames, st.DownloadedKeyFrames, st.DownloadedSourceSegments, st.DownloadedTranscodedSegments, st.SuccessRate2, st.BytesDownloaded)
	if len(st.Errors) > 0 {
		r += "\n"
		// r = fmt.Sprintf("%s\nErrors: %+v\n", r, st.Errors)
	}
	/* disable temporarily
	if len(st.RawTranscodeLatenciesPerStream) > 0 {
		r += "\nTranscode latencies per stream:\n"
		for _, rtl := range st.RawTranscodeLatenciesPerStream {
			r += formatLatenciesSlice(rtl) + "\n"
		}
	}
	*/
	return r
}

// FormatErrorsForConsole prints only .Errors
func (st *Stats) FormatErrorsForConsole() string {
	if len(st.Errors) == 0 {
		return ""
	}
	r := make([]string, 0, len(st.Errors))
	ss := make(sort.StringSlice, 0, len(st.Errors))
	for k := range st.Errors {
		ss = append(ss, k)
	}
	ss.Sort()
	for _, k := range ss {
		r = append(r, fmt.Sprintf("  %s: %d", strings.TrimSpace(k), st.Errors[k]))
	}
	return "Errors:\n" + strings.Join(r, "\n")
}

func (ls *Latencies) String() string {
	r := fmt.Sprintf(`{Average: %s, P50: %s, P95: %s, P99: %s}`, ls.Avg, ls.P50, ls.P95, ls.P99)
	return r
}

func formatLatenciesSlice(lat []time.Duration) string {
	var buf []string
	if len(lat) < 17 {
		return fmt.Sprintf("%+v", lat)
	}
	for i := 0; i < 8; i++ {
		buf = append(buf, fmt.Sprintf("%s", lat[i]))
	}
	buf = append(buf, "...")
	for i := len(lat) - 9; i < len(lat); i++ {
		buf = append(buf, fmt.Sprintf("%s", lat[i]))
	}
	return "[" + strings.Join(buf, " ") + "]"
}
