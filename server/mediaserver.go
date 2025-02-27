/*
Package server is the place we integrate the Livepeer node with the LPMS media server.
*/
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livepeer/go-livepeer/drivers"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/pm"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	lpmscore "github.com/livepeer/lpms/core"
	ffmpeg "github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/segmenter"
	"github.com/livepeer/lpms/stream"
	"github.com/livepeer/lpms/vidplayer"
	"github.com/livepeer/m3u8"
	"github.com/patrickmn/go-cache"
)

var errAlreadyExists = errors.New("StreamAlreadyExists")
var errStorage = errors.New("ErrStorage")
var errDiscovery = errors.New("ErrDiscovery")
var errNoOrchs = errors.New("ErrNoOrchs")
var errUnknownStream = errors.New("ErrUnknownStream")
var errMismatchedParams = errors.New("Mismatched type for stream params")

const HLSWaitInterval = time.Second
const HLSBufferCap = uint(43200) //12 hrs assuming 1s segment
const HLSBufferWindow = uint(5)
const StreamKeyBytes = 6

const SegLen = 2 * time.Second
const BroadcastRetry = 15 * time.Second

var BroadcastJobVideoProfiles = []ffmpeg.VideoProfile{ffmpeg.P240p30fps4x3, ffmpeg.P360p30fps16x9}

var AuthWebhookURL string

// For HTTP push watchdog
var httpPushTimeout = 1 * time.Minute
var httpPushResetTimer = func() (context.Context, context.CancelFunc) {
	sleepDur := time.Duration(int64(float64(httpPushTimeout) * 0.9))
	return context.WithTimeout(context.Background(), sleepDur)
}

type rtmpConnection struct {
	mid             core.ManifestID
	nonce           uint64
	stream          stream.RTMPVideoStream
	pl              core.PlaylistManager
	profile         *ffmpeg.VideoProfile
	params          *core.StreamParameters
	sessManager     *BroadcastSessionsManager
	lastUsed        time.Time
	sourceBytes     uint64
	transcodedBytes uint64
}

type LivepeerServer struct {
	RTMPSegmenter           lpmscore.RTMPSegmenter
	LPMS                    *lpmscore.LPMS
	LivepeerNode            *core.LivepeerNode
	HTTPMux                 *http.ServeMux
	ExposeCurrentManifest   bool
	recordingsAuthResponses *cache.Cache

	// Thread sensitive fields. All accesses to the
	// following fields should be protected by `connectionLock`
	rtmpConnections   map[core.ManifestID]*rtmpConnection
	internalManifests map[core.ManifestID]core.ManifestID
	lastHLSStreamID   core.StreamID
	lastManifestID    core.ManifestID
	connectionLock    *sync.RWMutex
}

type authWebhookResponse struct {
	ManifestID           string   `json:"manifestID"`
	StreamKey            string   `json:"streamKey"`
	Presets              []string `json:"presets"`
	ObjectStore          string   `json:"objectStore"`
	RecordObjectStore    string   `json:"recordObjectStore"`
	RecordObjectStoreURL string   `json:"recordObjectStoreUrl"`
	Profiles             []struct {
		Name    string `json:"name"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Bitrate int    `json:"bitrate"`
		FPS     uint   `json:"fps"`
		FPSDen  uint   `json:"fpsDen"`
		Profile string `json:"profile"`
		GOP     string `json:"gop"`
	} `json:"profiles"`
	PreviousSessions []string `json:"previousSessions"`
}

func NewLivepeerServer(rtmpAddr string, lpNode *core.LivepeerNode, httpIngest bool, transcodingOptions string) (*LivepeerServer, error) {
	opts := lpmscore.LPMSOpts{
		RtmpAddr:     rtmpAddr,
		RtmpDisabled: true,
		WorkDir:      lpNode.WorkDir,
		HttpMux:      http.NewServeMux(),
	}
	switch lpNode.NodeType {
	case core.BroadcasterNode:
		opts.RtmpDisabled = false

		if transcodingOptions != "" {
			var profiles []ffmpeg.VideoProfile
			content, err := ioutil.ReadFile(transcodingOptions)
			if err == nil && len(content) > 0 {
				stubResp := &authWebhookResponse{}
				err = json.Unmarshal(content, &stubResp.Profiles)
				if err != nil {
					return nil, err
				}
				profiles, err = jsonProfileToVideoProfile(stubResp)
				if err != nil {
					return nil, err
				}
			} else {
				// check the built-in profiles
				profiles = parsePresets(strings.Split(transcodingOptions, ","))
			}
			if len(profiles) <= 0 {
				return nil, fmt.Errorf("No transcoding profiles found")
			}
			BroadcastJobVideoProfiles = profiles
		}
	}
	server := lpmscore.New(&opts)
	ls := &LivepeerServer{RTMPSegmenter: server, LPMS: server, LivepeerNode: lpNode, HTTPMux: opts.HttpMux, connectionLock: &sync.RWMutex{},
		rtmpConnections:         make(map[core.ManifestID]*rtmpConnection),
		internalManifests:       make(map[core.ManifestID]core.ManifestID),
		recordingsAuthResponses: cache.New(time.Hour, 2*time.Hour),
	}
	if lpNode.NodeType == core.BroadcasterNode && httpIngest {
		opts.HttpMux.HandleFunc("/live/", ls.HandlePush)
	}
	opts.HttpMux.HandleFunc("/recordings/", ls.HandleRecordings)
	return ls, nil
}

//StartMediaServer starts the LPMS server
func (s *LivepeerServer) StartMediaServer(ctx context.Context, httpAddr string) error {
	glog.V(common.SHORT).Infof("Transcode Job Type: %v", BroadcastJobVideoProfiles)

	//LPMS handlers for handling RTMP video
	s.LPMS.HandleRTMPPublish(createRTMPStreamIDHandler(s), gotRTMPStreamHandler(s), endRTMPStreamHandler(s))
	s.LPMS.HandleRTMPPlay(getRTMPStreamHandler(s))

	//LPMS hanlder for handling HLS video play
	s.LPMS.HandleHLSPlay(getHLSMasterPlaylistHandler(s), getHLSMediaPlaylistHandler(s), getHLSSegmentHandler(s))

	//Start the LPMS server
	lpmsCtx, cancel := context.WithCancel(ctx)

	ec := make(chan error, 2)
	go func() {
		if err := s.LPMS.Start(lpmsCtx); err != nil {
			// typically triggered if there's an error with broadcaster LPMS
			// transcoder LPMS should return without an error
			ec <- s.LPMS.Start(lpmsCtx)
		}
	}()
	if s.LivepeerNode.NodeType == core.BroadcasterNode {
		go func() {
			glog.V(4).Infof("HTTP Server listening on http://%v", httpAddr)
			ec <- http.ListenAndServe(httpAddr, s.HTTPMux)
		}()
	}

	select {
	case err := <-ec:
		glog.Infof("LPMS Server Error: %v.  Quitting...", err)
		cancel()
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

//RTMP Publish Handlers
func createRTMPStreamIDHandler(s *LivepeerServer) func(url *url.URL) (strmID stream.AppData) {
	return func(url *url.URL) (strmID stream.AppData) {
		//Check webhook for ManifestID
		//If ManifestID is returned from webhook, use it
		//Else check URL for ManifestID
		//If ManifestID is passed in URL, use that one
		//Else create one
		var resp *authWebhookResponse
		var mid core.ManifestID
		var err error
		var key string
		var os, ros drivers.OSDriver
		var oss, ross drivers.OSSession
		profiles := []ffmpeg.VideoProfile{}
		if resp, err = authenticateStream(url.String()); err != nil {
			glog.Errorf("Authentication denied for streamID url=%s err=%v", url.String(), err)
			return nil
		}
		if resp != nil {
			mid, key = parseManifestID(resp.ManifestID), resp.StreamKey
			// Process transcoding options presets
			if len(resp.Presets) > 0 {
				profiles = parsePresets(resp.Presets)
			}

			parsedProfiles, err := jsonProfileToVideoProfile(resp)
			if err != nil {
				glog.Errorf("Failed to parse JSON video profile for streamID url=%s err=%v", url.String(), err)
				return nil
			}
			profiles = append(profiles, parsedProfiles...)

			// Only set defaults if user did not specify a preset/profile
			if len(resp.Profiles) <= 0 && len(resp.Presets) <= 0 {
				profiles = BroadcastJobVideoProfiles
			}

			// set OS if it was provided
			if resp.ObjectStore != "" {
				os, err = drivers.ParseOSURL(resp.ObjectStore, false)
				if err != nil {
					glog.Errorf("Failed to parse object store url for streamID url=%s err=%v", url.String(), err)
					return nil
				}
			}
			// set Recording OS if it was provided
			if resp.RecordObjectStore != "" {
				ros, err = drivers.ParseOSURL(resp.RecordObjectStore, true)
				if err != nil {
					glog.Errorf("Failed to parse recording object store url for streamID url=%s err=%v", url.String(), err)
					return nil
				}
			}
		} else {
			profiles = BroadcastJobVideoProfiles
		}

		sid := parseStreamID(url.Path)
		extmid := sid.ManifestID
		if mid == "" {
			mid, key = sid.ManifestID, sid.Rendition
		}
		if mid == "" {
			mid = core.RandomManifestID()
		}
		// Generate RTMP part of StreamID
		if key == "" {
			key = common.RandomIDGenerator(StreamKeyBytes)
		}

		if os != nil {
			oss = os.NewSession(string(mid))
		}

		recordPath := fmt.Sprintf("%s/%s", extmid, monitor.NodeID)
		if ros != nil {
			ross = ros.NewSession(recordPath)
		} else if drivers.RecordStorage != nil {
			ross = drivers.RecordStorage.NewSession(recordPath)
		}
		// Ensure there's no concurrent StreamID with the same name
		s.connectionLock.RLock()
		defer s.connectionLock.RUnlock()
		if core.MaxSessions > 0 && len(s.rtmpConnections) >= core.MaxSessions {
			glog.Errorf("Too many connections for streamID url=%s err=%v", url.String(), err)
			return nil
		}
		return &core.StreamParameters{
			ManifestID: mid,
			RtmpKey:    key,
			// HTTP push mutates `profiles` so make a copy of it
			Profiles: append([]ffmpeg.VideoProfile(nil), profiles...),
			OS:       oss,
			RecordOS: ross,
		}
	}
}

func authenticateStream(url string) (*authWebhookResponse, error) {
	if AuthWebhookURL == "" {
		return nil, nil
	}
	started := time.Now()
	values := map[string]string{"url": url}
	jsonValue, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(AuthWebhookURL, "application/json", bytes.NewBuffer(jsonValue))

	if err != nil {
		return nil, err
	}
	rbody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status=%d error=%s", resp.StatusCode, string(rbody))
	}
	if len(rbody) == 0 {
		return nil, nil
	}
	var authResp authWebhookResponse
	err = json.Unmarshal(rbody, &authResp)
	if err != nil {
		return nil, err
	}
	if authResp.ManifestID == "" {
		return nil, errors.New("Empty manifest id not allowed")
	}
	took := time.Since(started)
	glog.Infof("Stream authentication for url=%s dur=%s", url, took)
	if monitor.Enabled {
		monitor.AuthWebhookFinished(took)
	}
	return &authResp, nil
}

func jsonProfileToVideoProfile(resp *authWebhookResponse) ([]ffmpeg.VideoProfile, error) {
	profiles := []ffmpeg.VideoProfile{}
	for _, profile := range resp.Profiles {
		name := profile.Name
		if name == "" {
			name = "webhook_" + common.DefaultProfileName(
				profile.Width,
				profile.Height,
				profile.Bitrate)
		}
		var gop time.Duration
		if profile.GOP != "" {
			if profile.GOP == "intra" {
				gop = ffmpeg.GOPIntraOnly
			} else {
				gopFloat, err := strconv.ParseFloat(profile.GOP, 64)
				if err != nil {
					return nil, err
				}
				if gopFloat <= 0.0 {
					return nil, errors.New("invalid gop value")
				}
				gop = time.Duration(gopFloat * float64(time.Second))
			}
		}
		encodingProfile, err := common.EncoderProfileNameToValue(profile.Profile)
		if err != nil {
			return nil, err
		}
		prof := ffmpeg.VideoProfile{
			Name:         name,
			Bitrate:      fmt.Sprint(profile.Bitrate),
			Framerate:    profile.FPS,
			FramerateDen: profile.FPSDen,
			Resolution:   fmt.Sprintf("%dx%d", profile.Width, profile.Height),
			Profile:      encodingProfile,
			GOP:          gop,
		}
		profiles = append(profiles, prof)
	}
	return profiles, nil
}

func streamParams(d stream.AppData) *core.StreamParameters {
	p, ok := d.(*core.StreamParameters)
	if !ok {
		glog.Error("Mismatched type for RTMP app data")
		return nil
	}
	return p
}

func gotRTMPStreamHandler(s *LivepeerServer) func(url *url.URL, rtmpStrm stream.RTMPVideoStream) (err error) {
	return func(url *url.URL, rtmpStrm stream.RTMPVideoStream) (err error) {

		cxn, err := s.registerConnection(rtmpStrm)
		if err != nil {
			return err
		}

		mid := cxn.mid
		nonce := cxn.nonce
		startSeq := 0

		streamStarted := false
		//Segment the stream, insert the segments into the broadcaster
		go func(rtmpStrm stream.RTMPVideoStream) {
			hid := string(core.RandomManifestID()) // ffmpeg m3u8 output name
			hlsStrm := stream.NewBasicHLSVideoStream(hid, stream.DefaultHLSStreamWin)
			hlsStrm.SetSubscriber(func(seg *stream.HLSSegment, eof bool) {
				if eof {
					// XXX update HLS manifest
					return
				}
				if !streamStarted {
					streamStarted = true
					if monitor.Enabled {
						monitor.StreamStarted(nonce)
					}
				}
				go processSegment(cxn, seg)
			})

			segOptions := segmenter.SegmenterOptions{
				StartSeq:  startSeq,
				SegLength: SegLen,
			}
			err := s.RTMPSegmenter.SegmentRTMPToHLS(context.Background(), rtmpStrm, hlsStrm, segOptions)
			if err != nil {
				// Stop the incoming RTMP connection.
				// TODO retry segmentation if err != SegmenterTimeout; may be recoverable
				rtmpStrm.Close()
			}

		}(rtmpStrm)

		if monitor.Enabled {
			monitor.StreamCreated(string(mid), nonce)
		}

		glog.Infof("\n\nVideo Created With ManifestID: %v\n\n", mid)

		return nil
	}
}

func endRTMPStreamHandler(s *LivepeerServer) func(url *url.URL, rtmpStrm stream.RTMPVideoStream) error {
	return func(url *url.URL, rtmpStrm stream.RTMPVideoStream) error {
		params := streamParams(rtmpStrm.AppData())
		if params == nil {
			return errMismatchedParams
		}

		//Remove RTMP stream
		err := removeRTMPStream(s, params.ManifestID)
		if err != nil {
			return err
		}
		return nil
	}
}

func (s *LivepeerServer) registerConnection(rtmpStrm stream.RTMPVideoStream) (*rtmpConnection, error) {
	nonce := rand.Uint64()

	// Set up the connection tracking
	params := streamParams(rtmpStrm.AppData())
	if params == nil {
		return nil, errMismatchedParams
	}
	mid := params.ManifestID
	if drivers.NodeStorage == nil {
		glog.Error("Missing node storage")
		return nil, errStorage
	}
	// Build the source video profile from the RTMP stream.
	if params.Resolution == "" {
		params.Resolution = fmt.Sprintf("%vx%v", rtmpStrm.Width(), rtmpStrm.Height())
	}
	if params.OS == nil {
		params.OS = drivers.NodeStorage.NewSession(string(mid))
	}
	storage := params.OS

	// Generate and set capabilities
	caps, err := core.JobCapabilities(params)
	if err != nil {
		return nil, err
	}
	params.Capabilities = caps

	recordStorage := params.RecordOS
	vProfile := ffmpeg.VideoProfile{
		Name:       "source",
		Resolution: params.Resolution,
		Bitrate:    "4000k", // Fix this
		Format:     params.Format,
	}
	hlsStrmID := core.MakeStreamID(mid, &vProfile)
	s.connectionLock.RLock()
	// Fast path - check early if session exists - creating new session can take time
	oldCxn, exists := s.rtmpConnections[mid]
	s.connectionLock.RUnlock()
	if exists {
		// We can only have one concurrent stream per ManifestID
		return oldCxn, errAlreadyExists
	}

	playlist := core.NewBasicPlaylistManager(mid, storage, recordStorage)
	var stakeRdr stakeReader
	if s.LivepeerNode.Eth != nil {
		stakeRdr = &storeStakeReader{store: s.LivepeerNode.Database}
	}
	cxn := &rtmpConnection{
		mid:         mid,
		nonce:       nonce,
		stream:      rtmpStrm,
		pl:          playlist,
		profile:     &vProfile,
		params:      params,
		sessManager: NewSessionManager(s.LivepeerNode, params, NewMinLSSelector(stakeRdr, 1.0)),
		lastUsed:    time.Now(),
	}

	s.connectionLock.Lock()
	oldCxn, exists = s.rtmpConnections[mid]
	// Check if session exist again - potentially two sessions can be created simultaneously,
	// so we don't want to overwrite one that was already created
	if exists {
		// We can only have one concurrent stream per ManifestID
		s.connectionLock.Unlock()
		cxn.sessManager.cleanup()
		return oldCxn, errAlreadyExists
	}
	s.rtmpConnections[mid] = cxn
	s.lastManifestID = mid
	s.lastHLSStreamID = hlsStrmID
	sessionsNumber := len(s.rtmpConnections)
	s.connectionLock.Unlock()

	if monitor.Enabled {
		monitor.CurrentSessions(sessionsNumber)
	}

	return cxn, nil
}

func removeRTMPStream(s *LivepeerServer, extmid core.ManifestID) error {
	s.connectionLock.Lock()
	defer s.connectionLock.Unlock()
	intmid := extmid
	if _intmid, exists := s.internalManifests[extmid]; exists {
		// Use the internal manifestID that was stored for the provided manifestID
		// to index into rtmpConnections
		intmid = _intmid
	}
	cxn, ok := s.rtmpConnections[intmid]
	if !ok || cxn.pl == nil {
		glog.Warningf("Attempted to end unknown stream with manifestID=%s", extmid)
		return errUnknownStream
	}
	cxn.stream.Close()
	cxn.sessManager.cleanup()
	cxn.pl.Cleanup()
	glog.Infof("Ended stream with manifestID=%s external manifestID=%s", intmid, extmid)
	delete(s.rtmpConnections, intmid)
	delete(s.internalManifests, extmid)

	if monitor.Enabled {
		monitor.StreamEnded(cxn.nonce)
		monitor.CurrentSessions(len(s.rtmpConnections))
	}

	return nil
}

//End RTMP Publish Handlers

//HLS Play Handlers
func getHLSMasterPlaylistHandler(s *LivepeerServer) func(url *url.URL) (*m3u8.MasterPlaylist, error) {
	return func(url *url.URL) (*m3u8.MasterPlaylist, error) {
		var manifestID core.ManifestID
		if s.ExposeCurrentManifest && strings.ToLower(url.Path) == "/stream/current.m3u8" {
			manifestID = s.LastManifestID()
		} else {
			sid := parseStreamID(url.Path)
			if sid.Rendition != "" {
				// requesting a media PL, not master PL
				return nil, vidplayer.ErrNotFound
			}
			manifestID = sid.ManifestID
		}

		s.connectionLock.RLock()
		defer s.connectionLock.RUnlock()
		cxn, ok := s.rtmpConnections[manifestID]
		if !ok || cxn.pl == nil {
			return nil, vidplayer.ErrNotFound
		}
		cpl := cxn.pl

		if cpl.ManifestID() != manifestID {
			return nil, vidplayer.ErrNotFound
		}
		return cpl.GetHLSMasterPlaylist(), nil
	}
}

func getHLSMediaPlaylistHandler(s *LivepeerServer) func(url *url.URL) (*m3u8.MediaPlaylist, error) {
	return func(url *url.URL) (*m3u8.MediaPlaylist, error) {
		strmID := parseStreamID(url.Path)
		mid := strmID.ManifestID
		s.connectionLock.RLock()
		defer s.connectionLock.RUnlock()
		cxn, ok := s.rtmpConnections[mid]
		if !ok || cxn.pl == nil {
			return nil, vidplayer.ErrNotFound
		}

		//Get the hls playlist
		pl := cxn.pl.GetHLSMediaPlaylist(strmID.Rendition)
		if pl == nil {
			return nil, vidplayer.ErrNotFound
		}
		return pl, nil
	}
}

func getHLSSegmentHandler(s *LivepeerServer) func(url *url.URL) ([]byte, error) {
	return func(url *url.URL) ([]byte, error) {
		// Strip the /stream/ prefix
		segName := cleanStreamPrefix(url.Path)
		if segName == "" || drivers.NodeStorage == nil {
			glog.Error("SegName not found or storage nil")
			return nil, vidplayer.ErrNotFound
		}
		parts := strings.SplitN(segName, "/", 2)
		if len(parts) <= 0 {
			glog.Error("Unexpected path structure")
			return nil, vidplayer.ErrNotFound
		}
		memoryOS, ok := drivers.NodeStorage.(*drivers.MemoryOS)
		if !ok {
			return nil, vidplayer.ErrNotFound
		}
		// We index the session by the first entry of the path, eg
		// <session>/<more-path>/<data>
		os := memoryOS.GetSession(parts[0])
		if os == nil {
			return nil, vidplayer.ErrNotFound
		}
		data := os.GetData(segName)
		if len(data) > 0 {
			return data, nil
		}
		return nil, vidplayer.ErrNotFound
	}
}

//End HLS Play Handlers

//Start RTMP Play Handlers
func getRTMPStreamHandler(s *LivepeerServer) func(url *url.URL) (stream.RTMPVideoStream, error) {
	return func(url *url.URL) (stream.RTMPVideoStream, error) {
		mid := parseManifestID(url.Path)
		s.connectionLock.RLock()
		cxn, ok := s.rtmpConnections[mid]
		defer s.connectionLock.RUnlock()
		if !ok {
			glog.Error("Cannot find RTMP stream for ManifestID ", mid)
			return nil, vidplayer.ErrNotFound
		}

		//Could use a subscriber, but not going to here because the RTMP stream doesn't need to be available for consumption by multiple views.  It's only for the segmenter.
		return cxn.stream, nil
	}
}

//End RTMP Handlers

// HandlePush processes request for HTTP ingest
func (s *LivepeerServer) HandlePush(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != "POST" && r.Method != "PUT" {
		httpErr := fmt.Sprintf(`http push request wrong method=%s url=%s host=%s`, r.Method, r.URL, r.Host)
		glog.Error(httpErr)
		http.Error(w, httpErr, http.StatusMethodNotAllowed)
		return
	}
	// we read this unconditionally, mostly for ffmpeg
	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		httpErr := fmt.Sprintf(`Error reading http request body: %s`, err.Error())
		glog.Error(httpErr)
		http.Error(w, httpErr, http.StatusInternalServerError)
		return
	}
	r.Body.Close()
	r.URL = &url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path}

	// Determine the input format the request is claiming to have
	ext := path.Ext(r.URL.Path)
	format := common.ProfileExtensionFormat(ext)
	if ffmpeg.FormatNone == format {
		// ffmpeg sends us a m3u8 as well, so ignore
		// Alternatively, reject m3u8s explicitly and take any other type
		// TODO also look at use content-type
		httpErr := fmt.Sprintf(`ignoring file extension: %s`, ext)
		glog.Error(httpErr)
		http.Error(w, httpErr, http.StatusBadRequest)
		return
	}
	glog.Infof("Got push request at url=%s ua=%s addr=%s bytes=%d dur=%s resolution=%s", r.URL.String(), r.UserAgent(), r.RemoteAddr, len(body),
		r.Header.Get("Content-Duration"), r.Header.Get("Content-Resolution"))

	now := time.Now()
	mid := parseManifestID(r.URL.Path)
	if mid == "" {
		httpErr := fmt.Sprintf("Bad URL url=%s", r.URL)
		glog.Error(httpErr)
		http.Error(w, httpErr, http.StatusBadRequest)
		return
	}
	s.connectionLock.RLock()
	if intmid, exists := s.internalManifests[mid]; exists {
		mid = intmid
	}
	cxn, exists := s.rtmpConnections[mid]
	if exists && cxn != nil {
		cxn.lastUsed = now
	}
	s.connectionLock.RUnlock()

	// Check for presence and register if a fresh cxn
	if !exists {
		appData := (createRTMPStreamIDHandler(s))(r.URL)
		if appData == nil {
			httpErr := fmt.Sprintf("Could not create stream ID: url=%s", r.URL)
			glog.Error(httpErr)
			http.Error(w, httpErr, http.StatusInternalServerError)
			return
		}
		params := streamParams(appData)
		params.Resolution = r.Header.Get("Content-Resolution")
		params.Format = format
		s.connectionLock.RLock()
		if mid != params.ManifestID && s.rtmpConnections[params.ManifestID] != nil && s.internalManifests[mid] == "" {
			// Pre-existing connection found for this new stream with the same underlying manifestID
			var oldStreamID core.ManifestID
			for k, v := range s.internalManifests {
				if v == params.ManifestID {
					oldStreamID = k
					break
				}
			}
			s.connectionLock.RUnlock()
			if oldStreamID != "" && mid != oldStreamID {
				// Close the old connection, and open a new one
				// TODO try to re-use old HLS playlist?
				glog.Warningf("Ending streamID=%v as new streamID=%s with same manifestID=%s has arrived",
					oldStreamID, mid, params.ManifestID)
				removeRTMPStream(s, oldStreamID)
			}
		} else {
			s.connectionLock.RUnlock()
		}
		st := stream.NewBasicRTMPVideoStream(appData)
		// Set output formats if not explicitly specified
		for i, v := range params.Profiles {
			if ffmpeg.FormatNone == v.Format {
				params.Profiles[i].Format = format
			}
		}

		cxn, err = s.registerConnection(st)
		if err != nil {
			st.Close()
			if err != errAlreadyExists {
				httpErr := fmt.Sprintf("http push error url=%s err=%v", r.URL, err)
				glog.Error(httpErr)
				http.Error(w, httpErr, http.StatusInternalServerError)
				return
			} // else we continue with the old cxn
		} else {
			// Start a watchdog to remove session after a period of inactivity
			ticker := time.NewTicker(httpPushTimeout)
			go func(s *LivepeerServer, intmid, extmid core.ManifestID) {
				defer ticker.Stop()
				for range ticker.C {
					var lastUsed time.Time
					s.connectionLock.RLock()
					if cxn, exists := s.rtmpConnections[intmid]; exists {
						lastUsed = cxn.lastUsed
					}
					if _, exists := s.internalManifests[extmid]; !exists && intmid != extmid {
						s.connectionLock.RUnlock()
						glog.Warningf("Watchdog tried closing session for streamID=%s, which was already closed", extmid)
						return
					}
					s.connectionLock.RUnlock()
					if time.Since(lastUsed) > httpPushTimeout {
						_ = removeRTMPStream(s, extmid)
						return
					}
				}
			}(s, cxn.mid, mid)
		}
		// Regardless of old/new cxn returned by registerConnection, we make sure
		// our internalManifests mapping is OK before moving on
		if cxn.mid != mid {
			// AuthWebhook provided different ManifestID
			s.connectionLock.Lock()
			s.internalManifests[mid] = cxn.mid
			s.connectionLock.Unlock()
			mid = cxn.mid
		}
	}
	defer func(now time.Time) {
		glog.Infof("Finished push request at url=%s ua=%s addr=%s len=%d dur=%s resolution=%s took=%s", r.URL.String(), r.UserAgent(), r.RemoteAddr, len(body),
			r.Header.Get("Content-Duration"), r.Header.Get("Content-Resolution"), time.Since(now))
	}(now)

	fname := path.Base(r.URL.Path)
	seq, err := strconv.ParseUint(strings.TrimSuffix(fname, ext), 10, 64)
	if err != nil {
		seq = 0
	}

	duration, err := strconv.Atoi(r.Header.Get("Content-Duration"))
	if err != nil {
		duration = 2000
		glog.Info("Missing duration; filling in a default of 2000ms")
	}

	seg := &stream.HLSSegment{
		Data:     body,
		Name:     fname,
		SeqNo:    seq,
		Duration: float64(duration) / 1000.0,
	}

	// Kick watchdog periodically so session doesn't time out during long transcodes
	requestEnded := make(chan struct{}, 1)
	defer func() { requestEnded <- struct{}{} }()
	go func() {
		for {
			tick, cancel := httpPushResetTimer()
			select {
			case <-requestEnded:
				cancel()
				return
			case <-tick.Done():
				glog.V(common.VERBOSE).Infof("watchdog reset manifestID=%s seq=%d dur=%v started=%v", mid, seq, duration, now)
				s.connectionLock.RLock()
				if cxn, exists := s.rtmpConnections[mid]; exists {
					cxn.lastUsed = time.Now()
				}
				s.connectionLock.RUnlock()
			}
		}
	}()

	// Do the transcoding!
	urls, err := processSegment(cxn, seg)
	if err != nil {
		// TODO distinguish between user errors (400) and server errors (500)
		httpErr := fmt.Sprintf("http push error processing segment url=%s manifestID=%s err=%v", r.URL, mid, err)
		glog.Error(httpErr)
		http.Error(w, httpErr, http.StatusInternalServerError)
		return
	}
	select {
	case <-r.Context().Done():
		// HTTP request already timed out
		if monitor.Enabled {
			monitor.HTTPClientTimedOut1()
		}
		return
	default:
	}
	if len(urls) == 0 {
		glog.Infof("No sessions available for manifestID=%s seqNo=%d name=%s url=%s", mid, seq, fname, r.URL)
		http.Error(w, "No sessions available", http.StatusServiceUnavailable)
		return
	}
	renditionData := make([][]byte, len(urls))
	// find data in local storage
	memOS, ok := cxn.pl.GetOSSession().(*drivers.MemorySession)
	if ok {
		for i, fname := range urls {
			data := memOS.GetData(fname)
			if data != nil {
				renditionData[i] = data
			}
		}
	}
	glog.Infof("Finished transcoding push request at url=%s manifestID=%s seqNo=%d took=%s", r.URL.String(), mid, seq, time.Since(now))

	boundary := common.RandName()
	accept := r.Header.Get("Accept")
	if accept == "multipart/mixed" {
		contentType := "multipart/mixed; boundary=" + boundary
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if accept != "multipart/mixed" {
		return
	}
	mw := multipart.NewWriter(w)
	var fw io.Writer
	for i, url := range urls {
		mw.SetBoundary(boundary)
		var typ, ext string
		length := len(renditionData[i])
		if length == 0 {
			typ, ext, length = "application/vnd+livepeer.uri", ".txt", len(url)
		} else {
			format := cxn.params.Profiles[i].Format
			ext, err = common.ProfileFormatExtension(format)
			if err != nil {
				glog.Error("Unknown extension for format: ", err)
				break
			}
			typ, err = common.ProfileFormatMimeType(format)
			if err != nil {
				glog.Errorf("Unknown mime type for format url=%s manifestID=%s err=%v ", r.URL, mid, err)
			}
		}
		profile := cxn.params.Profiles[i].Name
		fname := fmt.Sprintf(`"%s_%d%s"`, profile, seq, ext)
		hdrs := textproto.MIMEHeader{
			"Content-Type":        {typ + "; name=" + fname},
			"Content-Length":      {strconv.Itoa(length)},
			"Content-Disposition": {"attachment; filename=" + fname},
			"Rendition-Name":      {profile},
		}
		fw, err = mw.CreatePart(hdrs)
		if err != nil {
			glog.Error("Could not create multipart part ", err)
			break
		}
		if len(renditionData[i]) > 0 {
			_, err = io.Copy(fw, bytes.NewBuffer(renditionData[i]))
			if err != nil {
				break
			}
		} else {
			_, err = fw.Write([]byte(url))
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		glog.Errorf("Error sending transcoded response url=%s err=%v", r.URL.String(), err)
		if monitor.Enabled {
			monitor.HTTPClientTimedOut2()
		}
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	roundtripTime := time.Since(start)
	select {
	case <-r.Context().Done():
		// HTTP request already timed out
		if monitor.Enabled {
			monitor.HTTPClientTimedOut2()
		}
		return
	default:
	}
	if monitor.Enabled {
		monitor.SegmentFullyProcessed(seg.Duration, roundtripTime.Seconds())
	}
}

// getPlaylistsFromStore finds all the json playlist files belonging to the provided manifests
// returns:
// - a map of manifestID -> a list of indices pointing to JSON files in the returned list of JSON files
// - a list of JSON files for all manifestIDs provided
// - the latest playlist time
func getPlaylistsFromStore(ctx context.Context, sess drivers.OSSession, manifests []string) (map[string][]int, []string, time.Time, error) {
	var latestPlaylistTime time.Time
	var jsonFiles []string
	filesMap := make(map[string][]int)
	for _, manifestID := range manifests {
		filesMap[manifestID] = nil
		start := time.Now()
		filesPage, err := sess.ListFiles(ctx, manifestID+"/", "/")
		if err != nil {
			return nil, nil, latestPlaylistTime, err
		}
		glog.V(common.VERBOSE).Infof("Listing directories for manifestID=%s took=%s", manifestID, time.Since(start))
		dirs := filesPage.Directories()
		if len(dirs) == 0 {
			continue
		}
		for _, dirName := range dirs {
			start = time.Now()
			dirOnePage, err := sess.ListFiles(ctx, dirName+"playlist_", "")
			glog.V(common.VERBOSE).Infof("Listing playlist files for manifestID=%s took=%s", manifestID, time.Since(start))
			if err != nil {
				return nil, nil, latestPlaylistTime, err
			}
			for {
				playlistsNames := dirOnePage.Files()
				for _, plf := range playlistsNames {
					if plf.LastModified.After(latestPlaylistTime) {
						latestPlaylistTime = plf.LastModified
					}
					filesMap[manifestID] = append(filesMap[manifestID], len(jsonFiles))
					jsonFiles = append(jsonFiles, plf.Name)
				}
				if !dirOnePage.HasNextPage() {
					break
				}
				dirOnePage, err = dirOnePage.NextPage()
				if err != nil {
					return nil, nil, latestPlaylistTime, err
				}
			}
		}
	}
	return filesMap, jsonFiles, latestPlaylistTime, nil
}

// HandleRecordings handle requests to /recordings/ endpoint
func (s *LivepeerServer) HandleRecordings(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		glog.Errorf(`/recordings request wrong method=%s url=%s host=%s`, r.Method, r.URL, r.Host)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ext := path.Ext(r.URL.Path)
	if ext != ".m3u8" && ext != ".ts" {
		glog.Errorf(`/recordings request wrong extension=%s url=%s host=%s`, ext, r.URL, r.Host)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.URL.Host = r.Host
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	pp := strings.Split(r.URL.Path, "/")
	finalize := r.URL.Query().Get("finalize") == "true"
	_, finalizeSet := r.URL.Query()["finalize"]
	if len(pp) < 4 {
		glog.Errorf(`/recordings request wrong url structure url=%s host=%s`, r.URL, r.Host)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	glog.V(common.VERBOSE).Infof("/recordings request=%s", r.URL.String())
	now := time.Now()
	defer func() {
		glog.V(common.VERBOSE).Infof("request=%s took=%s headers=%+v", r.URL.String(), time.Since(now), w.Header())
	}()
	returnMasterPlaylist := pp[3] == "index.m3u8"
	var track string
	if !returnMasterPlaylist {
		tp := strings.Split(pp[3], ".")
		track = tp[0]
	}
	manifestID := pp[2]
	requestFileName := strings.Join(pp[2:], "/")
	var fromCache bool
	var err error
	var resp *authWebhookResponse
	if cresp, has := s.recordingsAuthResponses.Get(manifestID); has {
		resp = cresp.(*authWebhookResponse)
		fromCache = true
	} else if resp, err = authenticateStream(r.URL.String()); err != nil {
		glog.Errorf("Authentication denied for url=%s err=%v", r.URL.String(), err)
		if strings.Contains(err.Error(), "not found") {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	var sess drivers.OSSession
	ctx := r.Context()
	if resp != nil && !fromCache {
		s.recordingsAuthResponses.SetDefault(manifestID, resp)
	}

	if resp != nil && resp.RecordObjectStore != "" {
		os, err := drivers.ParseOSURL(resp.RecordObjectStore, true)
		if err != nil {
			glog.Errorf("Error parsing OS URL err=%v request url=%s", err, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		sess = os.NewSession(manifestID)
	} else if drivers.RecordStorage != nil {
		sess = drivers.RecordStorage.NewSession(manifestID)
	} else {
		glog.Errorf("No record object store defined for request url=%s", r.URL)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	startRead := time.Now()
	fi, err := sess.ReadData(ctx, requestFileName)
	if err == context.Canceled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err == nil && fi != nil && fi.Body != nil {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
		if ext == ".ts" {
			contentType, _ := common.TypeByExtension(".ts")
			w.Header().Set("Content-Type", contentType)
		} else {
			w.Header().Set("Cache-Control", "max-age=5")
			w.Header().Set("Content-Type", "application/x-mpegURL")
		}
		w.Header().Set("Connection", "keep-alive")
		startWrite := time.Now()
		io.Copy(w, fi.Body)
		fi.Body.Close()
		glog.V(common.VERBOSE).Infof("request url=%s streaming filename=%s took=%s from_read_took=%s", r.URL.String(), requestFileName, time.Since(startWrite), time.Since(startRead))
		return
	}
	var manifests []string
	if len(resp.PreviousSessions) > 0 {
		manifests = append(resp.PreviousSessions, manifestID)
	} else {
		manifests = []string{manifestID}
	}
	jsonFilesMap, jsonFiles, latestPlaylistTime, err := getPlaylistsFromStore(ctx, sess, manifests)
	if err != nil {
		glog.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	glog.V(common.VERBOSE).Infof("request url=%s found json files: %+v", r.URL, jsonFiles)

	if len(jsonFiles) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if time.Since(latestPlaylistTime) > 24*time.Hour && !finalizeSet {
		finalize = true
	}

	now1 := time.Now()
	_, datas, err := drivers.ParallelReadFiles(ctx, sess, jsonFiles, 16)
	if err != nil {
		glog.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	glog.V(common.VERBOSE).Infof("Finished reading num=%d playlist files for manifestID=%s took=%s", len(jsonFiles), manifestID, time.Since(now1))

	var jsonPlaylists []*core.JsonPlaylist
	for _, manifestID := range manifests {
		if len(jsonFilesMap[manifestID]) == 0 {
			continue
		}
		// reconstruct sessions
		manifestMainJspl := core.NewJSONPlaylist()
		jsonPlaylists = append(jsonPlaylists, manifestMainJspl)
		for _, i := range jsonFilesMap[manifestID] {
			jspl := &core.JsonPlaylist{}
			err = json.Unmarshal(datas[i], jspl)
			if err != nil {
				glog.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			manifestMainJspl.AddMaster(jspl)
			if finalize {
				for trackName := range jspl.Segments {
					manifestMainJspl.AddTrack(jspl, trackName)
				}
			} else if track != "" {
				manifestMainJspl.AddTrack(jspl, track)
			}
		}
	}
	var mainJspl *core.JsonPlaylist
	if len(jsonPlaylists) == 1 {
		mainJspl = jsonPlaylists[0]
	} else {
		mainJspl = core.NewJSONPlaylist()
		// join sessions
		for _, jspl := range jsonPlaylists {
			mainJspl.AddMaster(jspl)
			if finalize {
				for trackName := range jspl.Segments {
					mainJspl.AddDiscontinuedTrack(jspl, trackName)
				}
			} else if track != "" {
				mainJspl.AddDiscontinuedTrack(jspl, track)
			}
		}
	}

	masterPList := m3u8.NewMasterPlaylist()
	mediaLists := make(map[string]*m3u8.MediaPlaylist)

	for _, track := range mainJspl.Tracks {
		segments := mainJspl.Segments[track.Name]
		mpl, err := m3u8.NewMediaPlaylist(uint(len(segments)), uint(len(segments)))
		if err != nil {
			glog.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		url := fmt.Sprintf("%s.m3u8", track.Name)
		vParams := m3u8.VariantParams{Bandwidth: track.Bandwidth, Resolution: track.Resolution}
		masterPList.Append(url, mpl, vParams)
		mpl.Live = false
		mediaLists[track.Name] = mpl
	}
	select {
	case <-ctx.Done():
		w.WriteHeader(http.StatusBadRequest)
		return
	default:
	}
	glog.V(common.VERBOSE).Infof("Playlist generation for manifestID=%s took=%s", manifestID, time.Since(now1))
	if finalize {
		for trackName := range mainJspl.Segments {
			mpl := mediaLists[trackName]
			mainJspl.AddSegmentsToMPL(manifests, trackName, mpl, resp.RecordObjectStoreURL)
			fileName := trackName + ".m3u8"
			nows := time.Now()
			_, err = sess.SaveData(fileName, mpl.Encode().Bytes(), nil)
			glog.V(common.VERBOSE).Infof("Saving playlist fileName=%s for manifestID=%s took=%s", fileName, manifestID, time.Since(nows))
			if err != nil {
				glog.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		nows := time.Now()
		_, err = sess.SaveData("index.m3u8", masterPList.Encode().Bytes(), nil)
		glog.V(common.VERBOSE).Infof("Saving playlist fileName=%s for manifestID=%s took=%s", "index.m3u8", manifestID, time.Since(nows))
		if err != nil {
			glog.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if !returnMasterPlaylist {
		mpl := mediaLists[track]
		mainJspl.AddSegmentsToMPL(manifests, track, mpl, resp.RecordObjectStoreURL)
		// check (debug code)
		startSeq := mpl.Segments[0].SeqId
		for _, seg := range mpl.Segments[1:] {
			if seg.SeqId != startSeq+1 {
				glog.Infof("prev seq is %d but next is %d", startSeq, seg.SeqId)
			}
			startSeq = seg.SeqId
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
	w.Header().Set("Cache-Control", "max-age=5")
	w.Header().Set("Content-Type", "application/x-mpegURL")
	if returnMasterPlaylist {
		w.Header().Set("Connection", "keep-alive")
		_, err = w.Write(masterPList.Encode().Bytes())
	} else if track != "" {
		mediaPl := mediaLists[track]
		if mediaPl != nil {
			w.Header().Set("Connection", "keep-alive")
			_, err = w.Write(mediaPl.Encode().Bytes())
		} else {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
}

//Helper Methods Begin

// StreamPrefix match all leading spaces, slashes and optionally `stream/`
var StreamPrefix = regexp.MustCompile(`^[ /]*(stream/)?|(live/)?`) // test carefully!

func cleanStreamPrefix(reqPath string) string {
	return StreamPrefix.ReplaceAllString(reqPath, "")
}

func parseStreamID(reqPath string) core.StreamID {
	// remove extension and create streamid
	p := strings.TrimSuffix(reqPath, path.Ext(reqPath))
	return core.SplitStreamIDString(cleanStreamPrefix(p))
}

func parseManifestID(reqPath string) core.ManifestID {
	return parseStreamID(reqPath).ManifestID
}

func parsePresets(presets []string) []ffmpeg.VideoProfile {
	profs := make([]ffmpeg.VideoProfile, 0)
	for _, v := range presets {
		if p, ok := ffmpeg.VideoProfileLookup[strings.TrimSpace(v)]; ok {
			profs = append(profs, p)
		}
	}
	return profs
}

func (s *LivepeerServer) LastManifestID() core.ManifestID {
	s.connectionLock.RLock()
	defer s.connectionLock.RUnlock()
	return s.lastManifestID
}

func (s *LivepeerServer) LastHLSStreamID() core.StreamID {
	s.connectionLock.RLock()
	defer s.connectionLock.RUnlock()
	return s.lastHLSStreamID
}

func (s *LivepeerServer) GetNodeStatus() *net.NodeStatus {
	// not threadsafe; need to deep copy the playlist
	m := make(map[string]*m3u8.MasterPlaylist)

	s.connectionLock.RLock()
	defer s.connectionLock.RUnlock()
	streamInfo := make(map[string]net.StreamInfo)
	for _, cxn := range s.rtmpConnections {
		if cxn.pl == nil {
			continue
		}
		cpl := cxn.pl
		m[string(cpl.ManifestID())] = cpl.GetHLSMasterPlaylist()
		sb := atomic.LoadUint64(&cxn.sourceBytes)
		tb := atomic.LoadUint64(&cxn.transcodedBytes)
		streamInfo[string(cpl.ManifestID())] = net.StreamInfo{
			SourceBytes:     sb,
			TranscodedBytes: tb,
		}
	}
	res := &net.NodeStatus{
		Manifests:             m,
		InternalManifests:     make(map[string]string),
		StreamInfo:            streamInfo,
		Version:               core.LivepeerVersion,
		GolangRuntimeVersion:  runtime.Version(),
		GOArch:                runtime.GOARCH,
		GOOS:                  runtime.GOOS,
		OrchestratorPool:      []string{},
		RegisteredTranscoders: []net.RemoteTranscoderInfo{},
		LocalTranscoding:      s.LivepeerNode.TranscoderManager == nil,
	}
	for k, v := range s.internalManifests {
		res.InternalManifests[string(k)] = string(v)
	}
	if s.LivepeerNode.TranscoderManager != nil {
		res.RegisteredTranscodersNumber = s.LivepeerNode.TranscoderManager.RegisteredTranscodersCount()
		res.RegisteredTranscoders = s.LivepeerNode.TranscoderManager.RegisteredTranscodersInfo()
	}
	if s.LivepeerNode.OrchestratorPool != nil {
		urls := s.LivepeerNode.OrchestratorPool.GetURLs()
		for _, url := range urls {
			res.OrchestratorPool = append(res.OrchestratorPool, url.String())
		}
	}
	return res
}

// Debug helpers
func (s *LivepeerServer) LatestPlaylist() core.PlaylistManager {
	s.connectionLock.RLock()
	defer s.connectionLock.RUnlock()
	cxn, ok := s.rtmpConnections[s.lastManifestID]
	if !ok || cxn.pl == nil {
		return nil
	}
	return cxn.pl
}

func shouldStopStream(err error) bool {
	_, ok := err.(pm.ErrSenderValidation)
	return ok
}
