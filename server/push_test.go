package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/drivers"
	lpmon "github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/vidplayer"
)

func requestSetup(s *LivepeerServer) (http.Handler, *strings.Reader, *httptest.ResponseRecorder) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.HandlePush(w, r)
	})
	reader := strings.NewReader("")
	writer := httptest.NewRecorder()
	return handler, reader, writer
}

func TestPush_MultipartReturn(t *testing.T) {
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	reader := strings.NewReader("InsteadOf.TS")
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/mani/17.ts", reader)

	dummyRes := func(tSegData []*net.TranscodedSegmentData) *net.TranscodeResult {
		return &net.TranscodeResult{
			Result: &net.TranscodeResult_Data{
				Data: &net.TranscodeData{
					Segments: tSegData,
					Sig:      []byte("bar"),
				},
			},
		}
	}

	// Create stub server
	ts, mux := stubTLSServer()
	defer ts.Close()

	segPath := "/transcoded/segment.ts"
	tSegData := []*net.TranscodedSegmentData{{Url: ts.URL + segPath, Pixels: 100}}
	tr := dummyRes(tSegData)
	buf, err := proto.Marshal(tr)
	require.Nil(t, err)

	mux.HandleFunc("/segment", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(buf)
	})
	mux.HandleFunc(segPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("transcoded binary data"))
	})

	sess := StubBroadcastSession(ts.URL)
	sess.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	sess.Params.ManifestID = "mani"
	bsm := bsmWithSessList([]*BroadcastSession{sess})

	url, _ := url.ParseRequestURI("test://some.host")
	osd := drivers.NewMemoryDriver(url)
	osSession := osd.NewSession("testPath")

	pl := core.NewBasicPlaylistManager("xx", osSession, nil)

	cxn := &rtmpConnection{
		mid:         core.ManifestID("mani"),
		nonce:       7,
		pl:          pl,
		profile:     &ffmpeg.P144p30fps16x9,
		sessManager: bsm,
		params:      &core.StreamParameters{Profiles: []ffmpeg.VideoProfile{ffmpeg.P144p25fps16x9}},
	}

	s.rtmpConnections["mani"] = cxn

	req.Header.Set("Accept", "multipart/mixed")
	s.HandlePush(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	assert.Equal(200, resp.StatusCode)

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	assert.Equal("multipart/mixed", mediaType)
	assert.Nil(err)
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var i int
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		assert.NoError(err)
		mediaType, params, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		assert.Nil(err)
		assert.Contains(params, "name")
		assert.Len(params, 1)
		assert.Equal(params["name"], "P144p25fps16x9_17.txt")
		assert.Equal(`attachment; filename="P144p25fps16x9_17.txt"`, p.Header.Get("Content-Disposition"))
		assert.Equal("P144p25fps16x9", p.Header.Get("Rendition-Name"))
		bodyPart, err := ioutil.ReadAll(p)
		assert.NoError(err)
		assert.Equal("application/vnd+livepeer.uri", mediaType)
		up, err := url.Parse(string(bodyPart))
		assert.Nil(err)
		assert.Equal(segPath, up.Path)

		i++
	}
	assert.Equal(1, i)
	assert.Equal(uint64(12), cxn.sourceBytes)
	assert.Equal(uint64(0), cxn.transcodedBytes)

	bsm.sel.Clear()
	bsm.sel.Add([]*BroadcastSession{sess})
	sess.BroadcasterOS = osSession
	// Body should be empty if no Accept header specified
	reader.Seek(0, 0)
	req = httptest.NewRequest("POST", "/live/mani/15.ts", reader)
	w = httptest.NewRecorder()
	s.HandlePush(w, req)
	resp = w.Result()
	defer resp.Body.Close()
	assert.Equal(200, resp.StatusCode)
	body, err := ioutil.ReadAll(resp.Body)
	require.Nil(t, err)
	assert.Equal("", strings.TrimSpace(string(body)))

	// Binary data should be returned
	bsm.sel.Clear()
	bsm.sel.Add([]*BroadcastSession{sess})
	reader.Seek(0, 0)
	reader = strings.NewReader("InsteadOf.TS")
	req = httptest.NewRequest("POST", "/live/mani/12.ts", reader)
	w = httptest.NewRecorder()
	req.Header.Set("Accept", "multipart/mixed")
	s.HandlePush(w, req)
	resp = w.Result()
	defer resp.Body.Close()
	assert.Equal(200, resp.StatusCode)

	mediaType, params, err = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	assert.Equal("multipart/mixed", mediaType)
	assert.Nil(err)
	mr = multipart.NewReader(resp.Body, params["boundary"])
	i = 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		assert.NoError(err)
		mediaType, params, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		assert.Nil(err)
		assert.Contains(params, "name")
		assert.Len(params, 1)
		assert.Equal("P144p25fps16x9_12.ts", params["name"])
		assert.Equal(`attachment; filename="P144p25fps16x9_12.ts"`, p.Header.Get("Content-Disposition"))
		assert.Equal("P144p25fps16x9", p.Header.Get("Rendition-Name"))
		bodyPart, err := ioutil.ReadAll(p)
		assert.Nil(err)
		assert.Equal("video/mp2t", strings.ToLower(mediaType))
		assert.Equal("transcoded binary data", string(bodyPart))

		i++
	}
	assert.Equal(1, i)
	assert.Equal(uint64(36), cxn.sourceBytes)
	assert.Equal(uint64(44), cxn.transcodedBytes)

	// No sessions error
	cxn.sessManager.sel.Clear()
	cxn.sessManager.lastSess = nil
	cxn.sessManager.sessMap = make(map[string]*BroadcastSession)
	reader.Seek(0, 0)
	req = httptest.NewRequest("POST", "/live/mani/13.ts", reader)
	w = httptest.NewRecorder()
	req.Header.Set("Accept", "multipart/mixed")
	s.HandlePush(w, req)
	resp = w.Result()
	defer resp.Body.Close()
	body, _ = ioutil.ReadAll(resp.Body)
	assert.Equal("No sessions available\n", string(body))
	assert.Equal(503, resp.StatusCode)
}

func TestPush_MemoryRequestError(t *testing.T) {
	// assert http request body error returned
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	handler, _, w := requestSetup(s)
	f, err := os.Open(`doesn't exist`)
	require.NotNil(t, err)
	req := httptest.NewRequest("POST", "/live/seg.ts", f)

	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(err)
	assert.Equal(http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(strings.TrimSpace(string(body)), "Error reading http request body")
}

func TestPush_EmptyURLError(t *testing.T) {
	// assert http request body error returned
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/.ts", nil)
	s.HandlePush(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	require.Nil(t, err)
	assert.Equal(http.StatusBadRequest, resp.StatusCode)
	assert.Contains(string(body), "Bad URL")
}

func TestPush_ShouldUpdateLastUsed(t *testing.T) {
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/mani1/1.ts", nil)
	s.HandlePush(w, req)
	resp := w.Result()
	resp.Body.Close()
	lu := s.rtmpConnections["mani1"].lastUsed
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/live/mani1/1.ts", nil)
	s.HandlePush(w, req)
	resp = w.Result()
	resp.Body.Close()
	assert.True(lu.Before(s.rtmpConnections["mani1"].lastUsed))
}

func TestPush_HTTPIngest(t *testing.T) {
	assert := assert.New(t)

	drivers.NodeStorage = drivers.NewMemoryDriver(nil)
	n, _ := core.NewLivepeerNode(nil, "./tmp", nil)
	n.NodeType = core.BroadcasterNode
	reader := strings.NewReader("")
	req := httptest.NewRequest("POST", "/live/name/1.mp4", reader)

	// HTTP ingest disabled
	s, _ := NewLivepeerServer("127.0.0.1:1938", n, false, "")
	h, pattern := s.HTTPMux.Handler(req)
	assert.Equal("", pattern)

	writer := httptest.NewRecorder()
	h.ServeHTTP(writer, req)
	resp := writer.Result()
	defer resp.Body.Close()
	assert.Equal(404, resp.StatusCode)

	// HTTP ingest enabled
	s, _ = NewLivepeerServer("127.0.0.1:1938", n, true, "")
	h, pattern = s.HTTPMux.Handler(req)
	assert.Equal("/live/", pattern)

	writer = httptest.NewRecorder()
	h.ServeHTTP(writer, req)
	resp = writer.Result()
	defer resp.Body.Close()
	assert.Equal(503, resp.StatusCode)

	body, err := ioutil.ReadAll(resp.Body)
	require.Nil(t, err)
	assert.Equal("No sessions available", strings.TrimSpace(string(body)))
}

func TestPush_MP4(t *testing.T) {
	// Do a bunch of setup. Would be nice to simplify this one day...
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	s.rtmpConnections = map[core.ManifestID]*rtmpConnection{}
	defer func() { s.rtmpConnections = map[core.ManifestID]*rtmpConnection{} }()
	segHandler := getHLSSegmentHandler(s)
	ts, mux := stubTLSServer()
	defer ts.Close()

	// sometimes LivepeerServer needs time  to start
	// esp if this is the only test in the suite being run (eg, via `-run)
	time.Sleep(10 * time.Millisecond)

	oldProfs := BroadcastJobVideoProfiles
	defer func() { BroadcastJobVideoProfiles = oldProfs }()
	BroadcastJobVideoProfiles = []ffmpeg.VideoProfile{ffmpeg.P720p25fps16x9}

	sd := &stubDiscovery{}
	sd.infos = []*net.OrchestratorInfo{{Transcoder: ts.URL, AuthToken: stubAuthToken}}
	s.LivepeerNode.OrchestratorPool = sd

	dummyRes := func(tSegData []*net.TranscodedSegmentData) *net.TranscodeResult {
		return &net.TranscodeResult{
			Result: &net.TranscodeResult_Data{
				Data: &net.TranscodeData{
					Segments: tSegData,
				},
			},
		}
	}
	segPath := "/random"
	tSegData := []*net.TranscodedSegmentData{{Url: ts.URL + segPath, Pixels: 100}}
	tr := dummyRes(tSegData)
	buf, err := proto.Marshal(tr)
	require.Nil(t, err)

	mux.HandleFunc("/segment", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(buf)
	})
	mux.HandleFunc(segPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("transcoded binary data"))
	})

	// Check default response: should be empty, with OS populated
	handler, reader, writer := requestSetup(s)
	reader = strings.NewReader("a video file goes here")
	req := httptest.NewRequest("POST", "/live/name/1.mp4", reader)
	handler.ServeHTTP(writer, req)
	resp := writer.Result()
	defer resp.Body.Close()
	assert.Equal(200, resp.StatusCode)
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(err)
	assert.Empty(body)
	// Check OS for source
	vpath, err := url.Parse("/stream/name/source/1.mp4")
	assert.Nil(err)
	body, err = segHandler(vpath)
	assert.Nil(err)
	assert.Equal("a video file goes here", string(body))
	// Check OS for transcoded rendition
	vpath, err = url.Parse("/stream/name/P720p25fps16x9/1.mp4")
	assert.Nil(err)
	body, err = segHandler(vpath)
	assert.Nil(err)
	assert.Equal("transcoded binary data", string(body))
	// Sanity check version with mpegts extension doesn't exist
	vpath, err = url.Parse("/stream/name/source/1.ts")
	assert.Nil(err)
	body, err = segHandler(vpath)
	assert.Equal(vidplayer.ErrNotFound, err)
	assert.Empty(body)
	vpath, err = url.Parse("/stream/name/P720p25fps16x9/1.ts")
	assert.Nil(err)
	body, err = segHandler(vpath)
	assert.Equal(vidplayer.ErrNotFound, err)
	assert.Empty(body)
	// We can't actually test the returned content type here.
	// (That is handled within LPMS, so assume it's fine from here.)

	// Check multipart response for MP4s
	reader = strings.NewReader("a new video goes here")
	writer = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/live/name/2.mp4", reader)
	req.Header.Set("Accept", "multipart/mixed")
	handler.ServeHTTP(writer, req)
	resp = writer.Result()
	assert.Equal(200, resp.StatusCode)
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	assert.Equal("multipart/mixed", mediaType)
	assert.Nil(err)
	mr := multipart.NewReader(resp.Body, params["boundary"])
	i := 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		assert.NoError(err)
		mediaType, params, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		assert.Nil(err)
		assert.Contains(params, "name")
		assert.Len(params, 1)
		assert.Equal(params["name"], "P720p25fps16x9_2.mp4")
		assert.Equal(`attachment; filename="P720p25fps16x9_2.mp4"`, p.Header.Get("Content-Disposition"))
		assert.Equal("P720p25fps16x9", p.Header.Get("Rendition-Name"))
		bodyPart, err := ioutil.ReadAll(p)
		assert.Nil(err)
		assert.Equal("video/mp4", mediaType)
		assert.Equal("transcoded binary data", string(bodyPart))

		i++
	}
	assert.Equal(1, i)

	// Check formats
	for _, cxn := range s.rtmpConnections {
		assert.Equal(ffmpeg.FormatMP4, cxn.profile.Format)
		for _, p := range cxn.params.Profiles {
			assert.Equal(ffmpeg.FormatMP4, p.Format)
		}
	}
}

func TestPush_SetVideoProfileFormats(t *testing.T) {
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	// sometimes LivepeerServer needs time  to start
	// esp if this is the only test in the suite being run (eg, via `-run)
	time.Sleep(10 * time.Millisecond)
	s.rtmpConnections = map[core.ManifestID]*rtmpConnection{}
	defer func() { s.rtmpConnections = map[core.ManifestID]*rtmpConnection{} }()

	oldProfs := BroadcastJobVideoProfiles
	defer func() { BroadcastJobVideoProfiles = oldProfs }()
	BroadcastJobVideoProfiles = []ffmpeg.VideoProfile{ffmpeg.P720p25fps16x9, ffmpeg.P720p60fps16x9}

	// Base case, mpegts
	h, r, w := requestSetup(s)
	req := httptest.NewRequest("POST", "/live/seg/0.ts", r)
	h.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	assert.Len(s.rtmpConnections, 1)
	for _, cxn := range s.rtmpConnections {
		assert.Equal(ffmpeg.FormatMPEGTS, cxn.profile.Format)
		assert.Len(cxn.params.Profiles, 2)
		assert.Len(BroadcastJobVideoProfiles, 2)
		for i, p := range cxn.params.Profiles {
			assert.Equal(ffmpeg.FormatMPEGTS, p.Format)
			// HTTP push mutates the profiles, causing undesirable changes to
			// the default set of broadcast profiles that persist to subsequent
			// streams. Make sure this doesn't happen!
			assert.Equal(ffmpeg.FormatNone, BroadcastJobVideoProfiles[i].Format)
		}
	}

	// Sending a MP4 under the same stream name doesn't change assigned profiles
	h, r, w = requestSetup(s)
	req = httptest.NewRequest("POST", "/live/seg/1.ts", r)
	h.ServeHTTP(w, req)
	resp = w.Result()
	defer resp.Body.Close()

	assert.Len(s.rtmpConnections, 1)
	for _, cxn := range s.rtmpConnections {
		assert.Equal(ffmpeg.FormatMPEGTS, cxn.profile.Format)
		assert.Len(cxn.params.Profiles, 2)
		assert.Len(BroadcastJobVideoProfiles, 2)
		for i, p := range cxn.params.Profiles {
			assert.Equal(ffmpeg.FormatMPEGTS, p.Format)
			assert.Equal(ffmpeg.FormatNone, BroadcastJobVideoProfiles[i].Format)
		}
	}

	// Sending a MP4 under a new stream name sets the profile correctly
	h, r, w = requestSetup(s)
	req = httptest.NewRequest("POST", "/live/new/0.mp4", r)
	h.ServeHTTP(w, req)
	resp = w.Result()
	defer resp.Body.Close()

	assert.Len(s.rtmpConnections, 2)
	cxn, ok := s.rtmpConnections["new"]
	assert.True(ok, "stream did not exist")
	assert.Equal(ffmpeg.FormatMP4, cxn.profile.Format)
	assert.Len(cxn.params.Profiles, 2)
	assert.Len(BroadcastJobVideoProfiles, 2)
	for i, p := range cxn.params.Profiles {
		assert.Equal(ffmpeg.FormatMP4, p.Format)
		assert.Equal(ffmpeg.FormatNone, BroadcastJobVideoProfiles[i].Format)
	}

	hookCalled := 0
	// Sanity check that default profile with webhook is copied
	// Checking since there is special handling for the default set of profiles
	// within the webhook hander.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := authWebhookResponse{ManifestID: "intweb"}
		val, err := json.Marshal(auth)
		assert.Nil(err, "invalid auth webhook response")
		w.Write(val)
		hookCalled++
	}))
	defer ts.Close()
	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = ts.URL

	h, r, w = requestSetup(s)
	req = httptest.NewRequest("POST", "/live/web/0.mp4", r)
	h.ServeHTTP(w, req)
	resp = w.Result()
	defer resp.Body.Close()
	assert.Equal(1, hookCalled)

	assert.Len(s.rtmpConnections, 3)
	cxn, ok = s.rtmpConnections["web"]
	assert.False(ok, "stream should not exist")
	cxn, ok = s.rtmpConnections["intweb"]
	assert.True(ok, "stream did not exist")
	assert.Equal(ffmpeg.FormatMP4, cxn.profile.Format)
	assert.Len(cxn.params.Profiles, 2)
	assert.Len(BroadcastJobVideoProfiles, 2)
	for i, p := range cxn.params.Profiles {
		assert.Equal(ffmpeg.FormatMP4, p.Format)
		assert.Equal(ffmpeg.FormatNone, BroadcastJobVideoProfiles[i].Format)
	}
	// Server has empty sessions list, so it will return 503
	assert.Equal(503, resp.StatusCode)

	h, r, w = requestSetup(s)
	req = httptest.NewRequest("POST", "/live/web/1.mp4", r)
	h.ServeHTTP(w, req)
	resp = w.Result()
	defer resp.Body.Close()
	// webhook should not be called again
	assert.Equal(1, hookCalled)

	assert.Len(s.rtmpConnections, 3)
	cxn, ok = s.rtmpConnections["web"]
	assert.False(ok, "stream should not exist")
	cxn, ok = s.rtmpConnections["intweb"]
	assert.True(ok, "stream did not exist")
	assert.Equal(503, resp.StatusCode)
}

func TestPush_ShouldRemoveSessionAfterTimeoutIfInternalMIDIsUsed(t *testing.T) {
	defer goleak.VerifyNone(t, common.IgnoreRoutines()...)

	oldRI := httpPushTimeout
	httpPushTimeout = 100 * time.Millisecond
	defer func() { httpPushTimeout = oldRI }()
	assert := assert.New(t)
	s, cancel := setupServerWithCancel()

	hookCalled := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := authWebhookResponse{ManifestID: "intmid"}
		val, err := json.Marshal(auth)
		assert.Nil(err, "invalid auth webhook response")
		w.Write(val)
		hookCalled++
	}))
	defer ts.Close()
	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = ts.URL

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/extmid1/1.ts", nil)
	s.HandlePush(w, req)
	resp := w.Result()
	resp.Body.Close()
	assert.Equal(1, hookCalled)
	s.connectionLock.Lock()
	_, exists := s.rtmpConnections["intmid"]
	_, existsExt := s.rtmpConnections["extmid1"]
	intmid := s.internalManifests["extmid1"]
	s.connectionLock.Unlock()
	assert.Equal("intmid", string(intmid))
	assert.True(exists)
	assert.False(existsExt)
	time.Sleep(150 * time.Millisecond)
	s.connectionLock.Lock()
	_, exists = s.rtmpConnections["intmid"]
	_, extEx := s.internalManifests["extmid1"]
	s.connectionLock.Unlock()
	cancel()
	assert.False(exists)
	assert.False(extEx)
}

func TestPush_ShouldRemoveSessionAfterTimeout(t *testing.T) {
	defer goleak.VerifyNone(t, common.IgnoreRoutines()...)

	oldRI := httpPushTimeout
	httpPushTimeout = 100 * time.Millisecond
	defer func() { httpPushTimeout = oldRI }()
	assert := assert.New(t)
	s, cancel := setupServerWithCancel()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/mani3/1.ts", nil)
	s.HandlePush(w, req)
	resp := w.Result()
	resp.Body.Close()
	s.connectionLock.Lock()
	_, exists := s.rtmpConnections["mani3"]
	s.connectionLock.Unlock()
	assert.True(exists)
	time.Sleep(150 * time.Millisecond)
	s.connectionLock.Lock()
	_, exists = s.rtmpConnections["mani3"]
	s.connectionLock.Unlock()
	cancel()
	assert.False(exists)
}

func TestPush_ShouldNotPanicIfSessionAlreadyRemoved(t *testing.T) {
	oldRI := httpPushTimeout
	httpPushTimeout = 100 * time.Millisecond
	defer func() { httpPushTimeout = oldRI }()
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/mani2/1.ts", nil)
	s.HandlePush(w, req)
	resp := w.Result()
	resp.Body.Close()
	s.connectionLock.Lock()
	_, exists := s.rtmpConnections["mani2"]
	s.connectionLock.Unlock()
	assert.True(exists)
	s.connectionLock.Lock()
	delete(s.rtmpConnections, "mani2")
	s.connectionLock.Unlock()
	time.Sleep(200 * time.Millisecond)
	s.connectionLock.Lock()
	_, exists = s.rtmpConnections["mani2"]
	s.connectionLock.Unlock()
	assert.False(exists)
}

func TestPush_ResetWatchdog(t *testing.T) {
	assert := assert.New(t)

	// wait for any earlier tests to complete
	assert.True(wgWait(&pushResetWg), "timed out waiting for earlier tests")

	s := setupServer()
	defer serverCleanup(s)

	waitBarrier := func(ch chan struct{}) bool {
		select {
		case <-ch:
			return true
		case <-time.After(1 * time.Second):
			return false
		}
	}

	// override reset func with our own instrumentation
	cancelCount := 0
	resetCount := 0
	var wrappedCancel func()
	var wg sync.WaitGroup // to synchronize on cancels of the watchdog
	timerCreationBarrier := make(chan struct{})
	oldResetTimer := httpPushResetTimer
	httpPushResetTimer = func() (context.Context, context.CancelFunc) {
		wg.Add(1)
		ctx, cancel := context.WithCancel(context.Background())
		resetCount++
		wrappedCancel = func() {
			cancelCount++
			cancel()
			wg.Done()
		}
		timerCreationBarrier <- struct{}{}
		return ctx, wrappedCancel
	}
	defer func() { httpPushResetTimer = oldResetTimer }()

	// sanity check : normal flow should result in a single cancel
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/name/0.ts", nil)
	s.HandlePush(w, req)
	assert.True(waitBarrier(timerCreationBarrier), "timer creation timed out")
	assert.True(wgWait(&wg), "watchdog did not exit")
	assert.Equal(1, cancelCount)
	assert.Equal(1, resetCount)

	// set up for "long transcode" : one timeout before returning normally
	ts, mux := stubTLSServer()
	defer ts.Close()
	serverBarrier := make(chan struct{})
	mux.HandleFunc("/segment", func(w http.ResponseWriter, r *http.Request) {
		assert.True(waitBarrier(serverBarrier), "server barrier timed out")
	})
	sess := StubBroadcastSession(ts.URL)
	bsm := bsmWithSessList([]*BroadcastSession{sess})
	s.connectionLock.Lock()
	cxn, exists := s.rtmpConnections["name"]
	assert.True(exists)
	cxn.sessManager = bsm
	s.connectionLock.Unlock()

	cancelCount = 0
	resetCount = 0
	pushFuncBarrier := make(chan struct{})
	go func() { s.HandlePush(w, req); pushFuncBarrier <- struct{}{} }()

	assert.True(waitBarrier(timerCreationBarrier), "timer creation timed out")
	assert.Equal(0, cancelCount)
	assert.Equal(1, resetCount)
	cxn.lastUsed = time.Time{} // reset. prob should be locked

	// induce a timeout via cancellation
	wrappedCancel()
	assert.True(waitBarrier(timerCreationBarrier), "timer creation timed out")
	assert.Equal(1, cancelCount)
	assert.Equal(2, resetCount)
	assert.NotEqual(time.Time{}, cxn.lastUsed, "lastUsed was not reset")

	// check things with a normal return
	cxn.lastUsed = time.Time{}  // reset again
	serverBarrier <- struct{}{} // induce server to return
	assert.True(waitBarrier(pushFuncBarrier), "push func timed out")
	assert.True(wgWait(&wg), "watchdog did not exit")
	assert.Equal(2, cancelCount)
	assert.Equal(2, resetCount)
	assert.Equal(time.Time{}, cxn.lastUsed, "lastUsed was reset")

	// check lastUsed is not reset if session disappears
	cancelCount = 0
	resetCount = 0
	go func() { s.HandlePush(w, req); pushFuncBarrier <- struct{}{} }()
	assert.True(waitBarrier(timerCreationBarrier), "timer creation timed out")
	assert.Equal(0, cancelCount)
	assert.Equal(1, resetCount)
	s.connectionLock.Lock()
	cxn, exists = s.rtmpConnections["name"]
	assert.True(exists)
	delete(s.rtmpConnections, "name") // disappear the session
	assert.NotEqual(time.Time{}, cxn.lastUsed, "lastUsed was not reset")
	cxn.lastUsed = time.Time{} // use time zero value as a sentinel
	s.connectionLock.Unlock()

	wrappedCancel() // induce tick
	assert.True(waitBarrier(timerCreationBarrier), "timer creation timed out")
	assert.Equal(1, cancelCount)
	assert.Equal(2, resetCount)
	assert.Equal(time.Time{}, cxn.lastUsed)

	// clean up and some more sanity checks
	serverBarrier <- struct{}{}
	assert.True(waitBarrier(pushFuncBarrier), "push func timed out")
	assert.True(wgWait(&wg), "watchdog did not exit")
	assert.Equal(2, cancelCount)
	assert.Equal(2, resetCount)
	assert.Equal(time.Time{}, cxn.lastUsed, "lastUsed was reset")

	// cancelling again should not lead to a timer reset since push is complete
	assert.Panics(wrappedCancel)
	assert.Equal(3, cancelCount)
	assert.Equal(2, resetCount)
}

func TestPush_FileExtensionError(t *testing.T) {
	// assert file extension error returned
	assert := assert.New(t)
	s := setupServer()
	handler, reader, w := requestSetup(s)
	defer serverCleanup(s)
	req := httptest.NewRequest("POST", "/live/seg.m3u8", reader)

	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	require.Nil(t, err)
	assert.Equal(http.StatusBadRequest, resp.StatusCode)
	assert.Contains(strings.TrimSpace(string(body)), "ignoring file extension")
}

func TestPush_StorageError(t *testing.T) {
	// assert storage error
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	handler, reader, w := requestSetup(s)

	tempStorage := drivers.NodeStorage
	drivers.NodeStorage = nil
	req := httptest.NewRequest("POST", "/live/seg.ts", reader)
	mid := parseManifestID(req.URL.Path)
	err := removeRTMPStream(s, mid)
	assert.Equal(errUnknownStream, err)

	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	require.Nil(t, err)
	assert.Equal(http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(strings.TrimSpace(string(body)), "ErrStorage")

	// reset drivers.NodeStorage to original value
	drivers.NodeStorage = tempStorage
}

func TestPush_ForAuthWebhookFailure(t *testing.T) {
	// assert app data error
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)
	handler, reader, w := requestSetup(s)

	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = "notaurl"
	req := httptest.NewRequest("POST", "/live/seg.ts", reader)

	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	require.Nil(t, err)
	assert.Equal(http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(strings.TrimSpace(string(body)), "Could not create stream ID")
}

func TestPush_ResolutionWithoutContentResolutionHeader(t *testing.T) {
	assert := assert.New(t)
	server := setupServer()
	defer serverCleanup(server)
	server.rtmpConnections = map[core.ManifestID]*rtmpConnection{}
	handler, reader, w := requestSetup(server)
	req := httptest.NewRequest("POST", "/live/seg.ts", reader)
	defaultRes := "0x0"

	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	assert.Len(server.rtmpConnections, 1)
	for _, cxn := range server.rtmpConnections {
		assert.Equal(cxn.profile.Resolution, defaultRes)
	}

	server.rtmpConnections = map[core.ManifestID]*rtmpConnection{}
}

func TestPush_ResolutionWithContentResolutionHeader(t *testing.T) {
	assert := assert.New(t)
	server := setupServer()
	defer serverCleanup(server)
	server.rtmpConnections = map[core.ManifestID]*rtmpConnection{}
	handler, reader, w := requestSetup(server)
	req := httptest.NewRequest("POST", "/live/seg.ts", reader)
	resolution := "123x456"
	req.Header.Set("Content-Resolution", resolution)

	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	assert.Len(server.rtmpConnections, 1)
	for _, cxn := range server.rtmpConnections {
		assert.Equal(resolution, cxn.profile.Resolution)
	}

	server.rtmpConnections = map[core.ManifestID]*rtmpConnection{}
}

func TestPush_WebhookRequestURL(t *testing.T) {
	assert := assert.New(t)
	s := setupServer()
	defer serverCleanup(s)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, _ := ioutil.ReadAll(r.Body)
		var req authWebhookReq
		err := json.Unmarshal(out, &req)
		if err != nil {
			glog.Error("Error parsing URL: ", err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		assert.Equal(req.URL, "http://example.com/live/seg.ts")
		w.Write(nil)
	}))

	defer ts.Close()

	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = ts.URL
	handler, reader, w := requestSetup(s)
	req := httptest.NewRequest("POST", "/live/seg.ts", reader)
	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	// Server has empty sessions list, so it will return 503
	assert.Equal(503, resp.StatusCode)
}

func TestPush_OSPerStream(t *testing.T) {
	lpmon.NodeID = "testNode"
	drivers.Testing = true
	assert := assert.New(t)
	drivers.NodeStorage = drivers.NewMemoryDriver(nil)
	n, _ := core.NewLivepeerNode(nil, "./tmp", nil)
	s, _ := NewLivepeerServer("127.0.0.1:1939", n, true, "")
	defer serverCleanup(s)

	whts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, _ := ioutil.ReadAll(r.Body)
		var req authWebhookReq
		err := json.Unmarshal(out, &req)
		if err != nil {
			glog.Error("Error parsing URL: ", err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		assert.Equal(req.URL, "http://example.com/live/sess1/1.ts")
		w.Write([]byte(`{"manifestID":"OSTEST01", "objectStore": "memory://store1", "recordObjectStore": "memory://store2"}`))
	}))

	defer whts.Close()
	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = whts.URL

	ts, mux := stubTLSServer()
	defer ts.Close()

	// sometimes LivepeerServer needs time  to start
	// esp if this is the only test in the suite being run (eg, via `-run)
	time.Sleep(10 * time.Millisecond)

	oldProfs := BroadcastJobVideoProfiles
	defer func() { BroadcastJobVideoProfiles = oldProfs }()
	BroadcastJobVideoProfiles = []ffmpeg.VideoProfile{ffmpeg.P720p25fps16x9}

	sd := &stubDiscovery{}
	sd.infos = []*net.OrchestratorInfo{{Transcoder: ts.URL, AuthToken: stubAuthToken}}
	s.LivepeerNode.OrchestratorPool = sd

	dummyRes := func(tSegData []*net.TranscodedSegmentData) *net.TranscodeResult {
		return &net.TranscodeResult{
			Result: &net.TranscodeResult_Data{
				Data: &net.TranscodeData{
					Segments: tSegData,
				},
			},
		}
	}
	segPath := "/random"
	tSegData := []*net.TranscodedSegmentData{{Url: ts.URL + segPath, Pixels: 100}}
	tr := dummyRes(tSegData)
	buf, err := proto.Marshal(tr)
	require.Nil(t, err)

	mux.HandleFunc("/segment", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(buf)
	})
	mux.HandleFunc(segPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("transcoded binary data"))
	})

	handler, reader, w := requestSetup(s)
	reader = strings.NewReader("segmentbody")
	req := httptest.NewRequest("POST", "/live/sess1/1.ts", reader)
	req.Header.Set("Accept", "multipart/mixed")
	handler.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	assert.NotNil(drivers.TestMemoryStorages)
	assert.Contains(drivers.TestMemoryStorages, "store1")
	assert.Contains(drivers.TestMemoryStorages, "store2")
	store1 := drivers.TestMemoryStorages["store1"]
	sess1 := store1.GetSession("OSTEST01")
	assert.NotNil(sess1)
	ctx := context.Background()
	fi, err := sess1.ReadData(ctx, "OSTEST01/source/1.ts")
	assert.Nil(err)
	assert.NotNil(fi)
	body, _ := ioutil.ReadAll(fi.Body)
	assert.Equal("segmentbody", string(body))
	assert.Equal("OSTEST01/source/1.ts", fi.Name)

	fi, err = sess1.ReadData(ctx, "OSTEST01/P720p25fps16x9/1.ts")
	assert.Nil(err)
	assert.NotNil(fi)
	body, _ = ioutil.ReadAll(fi.Body)
	assert.Equal("transcoded binary data", string(body))

	// Saving to record store is async so sleep for a bit
	time.Sleep(100 * time.Millisecond)

	store2 := drivers.TestMemoryStorages["store2"]
	sess2 := store2.GetSession("sess1/" + lpmon.NodeID)
	assert.NotNil(sess2)
	fi, err = sess2.ReadData(ctx, fmt.Sprintf("sess1/%s/source/1.ts", lpmon.NodeID))
	assert.Nil(err)
	assert.NotNil(fi)
	body, _ = ioutil.ReadAll(fi.Body)
	assert.Equal("segmentbody", string(body))

	fi, err = sess2.ReadData(ctx, fmt.Sprintf("sess1/%s/P720p25fps16x9/1.ts", lpmon.NodeID))
	assert.Nil(err)
	assert.NotNil(fi)
	body, _ = ioutil.ReadAll(fi.Body)
	assert.Equal("transcoded binary data", string(body))

	assert.Equal(200, resp.StatusCode)
	body, _ = ioutil.ReadAll(resp.Body)
	assert.True(len(body) > 0)
}

func TestPush_ConcurrentSegments(t *testing.T) {
	assert := assert.New(t)

	drivers.NodeStorage = drivers.NewMemoryDriver(nil)
	n, _ := core.NewLivepeerNode(nil, "./tmp", nil)
	n.NodeType = core.BroadcasterNode
	s, _ := NewLivepeerServer("127.0.0.1:1938", n, true, "")
	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = ""

	var wg sync.WaitGroup
	start := make(chan struct{})
	sendSeg := func(url string) {
		reader := strings.NewReader("")
		req := httptest.NewRequest("POST", url, reader)
		h, pattern := s.HTTPMux.Handler(req)
		assert.Equal("/live/", pattern)
		writer := httptest.NewRecorder()
		<-start
		h.ServeHTTP(writer, req)
		resp := writer.Result()
		defer resp.Body.Close()
		assert.Equal(503, resp.StatusCode)
		body, err := ioutil.ReadAll(resp.Body)
		require.Nil(t, err)
		assert.Equal("No sessions available", strings.TrimSpace(string(body)))
		wg.Done()
	}
	// Send concurrent segments on the same streamID
	wg.Add(2)
	go sendSeg("/live/streamID/0.ts")
	go sendSeg("/live/streamID/1.ts")
	time.Sleep(300 * time.Millisecond)
	// Send signal to go-routines so the requests are as-close-together-as-possible
	close(start)
	// Wait for goroutines to end
	wg.Wait()
}

func TestPush_ReuseIntmidWithDiffExtmid(t *testing.T) {
	defer goleak.VerifyNone(t, common.IgnoreRoutines()...)

	reader := strings.NewReader("InsteadOf.TS")
	oldRI := httpPushTimeout
	httpPushTimeout = 100 * time.Millisecond
	defer func() { httpPushTimeout = oldRI }()
	assert := assert.New(t)
	s, cancel := setupServerWithCancel()

	hookCalled := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := authWebhookResponse{ManifestID: "intmid"}
		val, err := json.Marshal(auth)
		assert.Nil(err, "invalid auth webhook response")
		w.Write(val)
		hookCalled++
	}))
	defer ts.Close()
	oldURL := AuthWebhookURL
	defer func() { AuthWebhookURL = oldURL }()
	AuthWebhookURL = ts.URL

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/live/extmid1/0.ts", reader)
	s.HandlePush(w, req)
	resp := w.Result()
	assert.Equal(503, resp.StatusCode)
	resp.Body.Close()
	assert.Equal(1, hookCalled)
	s.connectionLock.Lock()
	_, exists := s.rtmpConnections["intmid"]
	intmid := s.internalManifests["extmid1"]
	s.connectionLock.Unlock()
	assert.Equal("intmid", string(intmid))
	assert.True(exists)

	time.Sleep(4 * time.Millisecond)

	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/live/extmid2/0.ts", reader)
	s.HandlePush(w, req)
	resp = w.Result()
	assert.Equal(503, resp.StatusCode)
	resp.Body.Close()
	assert.Equal(2, hookCalled)
	s.connectionLock.Lock()
	_, exists = s.rtmpConnections["intmid"]
	intmid = s.internalManifests["extmid2"]
	_, existsOld := s.internalManifests["extmid1"]
	s.connectionLock.Unlock()
	assert.Equal("intmid", string(intmid))
	assert.True(exists)
	assert.False(existsOld)

	time.Sleep(200 * time.Millisecond)

	s.connectionLock.Lock()
	_, exists = s.rtmpConnections["intmid"]
	_, extEx := s.internalManifests["extmid1"]
	_, extEx2 := s.internalManifests["extmid2"]
	s.connectionLock.Unlock()
	cancel()
	assert.False(exists)
	assert.False(extEx)
	assert.False(extEx2)
}
