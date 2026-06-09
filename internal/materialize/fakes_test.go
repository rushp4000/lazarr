package materialize

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/torbox"
)

func testCtx() context.Context { return context.Background() }

// ---- fake catalog.Store -----------------------------------------------------------------

type fakeStore struct {
	mu       sync.Mutex
	releases map[string]*catalog.Release
	files    map[string][]catalog.File
	links    map[string]*catalog.DLLink // key: hash|fileID

	touches    int
	setStates  int
	setLinkErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		releases: make(map[string]*catalog.Release),
		files:    make(map[string][]catalog.File),
		links:    make(map[string]*catalog.DLLink),
	}
}

func linkKey(hash string, fileID int) string { return hash + "|" + strconv.Itoa(fileID) }

func (s *fakeStore) addRelease(r *catalog.Release) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	s.releases[r.Hash] = &cp
}

func (s *fakeStore) UpsertRelease(r *catalog.Release, files []catalog.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	s.releases[r.Hash] = &cp
	s.files[r.Hash] = files
	return nil
}

func (s *fakeStore) GetRelease(hash string) (*catalog.Release, []catalog.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.releases[hash]
	if !ok {
		return nil, nil, nil
	}
	cp := *r
	return &cp, s.files[hash], nil
}

func (s *fakeStore) ListByCategory(category string) ([]*catalog.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*catalog.Release
	for _, r := range s.releases {
		if r.Category == category {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeStore) SetState(hash string, st catalog.State, torboxID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStates++
	if r, ok := s.releases[hash]; ok {
		r.State = st
		r.TorBoxID = torboxID
	}
	return nil
}

func (s *fakeStore) TouchAccess(hash string, ts int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touches++
	if r, ok := s.releases[hash]; ok {
		r.LastAccess = ts
	}
	return nil
}

func (s *fakeStore) IdleCandidates(before int64) ([]*catalog.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*catalog.Release
	for _, r := range s.releases {
		if r.State == catalog.StateMaterialized && r.LastAccess < before {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeStore) OverMaxHold(before int64) ([]*catalog.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*catalog.Release
	for _, r := range s.releases {
		if r.State == catalog.StateMaterialized && r.AddedOn < before {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeStore) MaterializedIDs() ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for _, r := range s.releases {
		if r.State == catalog.StateMaterialized && r.TorBoxID != 0 {
			out = append(out, r.TorBoxID)
		}
	}
	return out, nil
}

func (s *fakeStore) GetLink(hash string, fileID int) (*catalog.DLLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[linkKey(hash, fileID)]
	if !ok {
		return nil, nil
	}
	cp := *l
	return &cp, nil
}

func (s *fakeStore) SetLink(l *catalog.DLLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setLinkErr != nil {
		return s.setLinkErr
	}
	cp := *l
	s.links[linkKey(l.Hash, l.FileID)] = &cp
	return nil
}

func (s *fakeStore) DeleteRelease(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.releases, hash)
	delete(s.files, hash)
	return nil
}

func (s *fakeStore) Close() error { return nil }

func (s *fakeStore) state(hash string) catalog.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.releases[hash]; ok {
		return r.State
	}
	return ""
}

// ---- fake torbox.Client -----------------------------------------------------------------

type fakeTorBox struct {
	mu sync.Mutex

	activeSlots int

	createCalls   int32
	requestDL     int32
	deleteCalls   int32
	createErr     error               // returned by CreateTorrent if set
	createErrOnce error               // returned once then cleared
	nextID        int64               // id to assign on CreateTorrent
	dlURLFn       func(id int64, fileID int) string // builds the presigned URL per call
	requestDLErr  error

	deleted map[int64]bool
	myList  []torbox.TorrentDetail
}

func newFakeTorBox() *fakeTorBox {
	return &fakeTorBox{
		activeSlots: 0,
		nextID:      1000,
		deleted:     make(map[int64]bool),
	}
}

func (t *fakeTorBox) CheckCached(hashes []string) (map[string]torbox.CachedItem, error) {
	return map[string]torbox.CachedItem{}, nil
}
func (t *fakeTorBox) TorrentInfo(hash string) (*torbox.CachedItem, error) { return nil, nil }

func (t *fakeTorBox) CreateTorrent(magnet string, addOnlyIfCached bool) (int64, string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	atomic.AddInt32(&t.createCalls, 1)
	if t.createErrOnce != nil {
		err := t.createErrOnce
		t.createErrOnce = nil
		return 0, "", err
	}
	if t.createErr != nil {
		return 0, "", t.createErr
	}
	id := t.nextID
	t.nextID++
	return id, "hash", nil
}

func (t *fakeTorBox) RequestDL(torrentID int64, fileID int) (string, error) {
	atomic.AddInt32(&t.requestDL, 1)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.requestDLErr != nil {
		return "", t.requestDLErr
	}
	if t.dlURLFn != nil {
		return t.dlURLFn(torrentID, fileID), nil
	}
	return "", fmt.Errorf("no dlURLFn set")
}

func (t *fakeTorBox) ControlDelete(torrentID int64) error {
	atomic.AddInt32(&t.deleteCalls, 1)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deleted[torrentID] = true
	return nil
}

func (t *fakeTorBox) MyList(offset int) ([]torbox.TorrentDetail, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if offset > 0 {
		return nil, nil
	}
	return t.myList, nil
}

func (t *fakeTorBox) MyListByID(id int64) (*torbox.TorrentDetail, error) { return nil, nil }

func (t *fakeTorBox) UserMe() (*torbox.Account, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return &torbox.Account{Plan: 1, ActiveSlots: t.activeSlots}, nil
}

func (t *fakeTorBox) createCount() int  { return int(atomic.LoadInt32(&t.createCalls)) }
func (t *fakeTorBox) requestDLCount() int { return int(atomic.LoadInt32(&t.requestDL)) }
func (t *fakeTorBox) deleteCount() int  { return int(atomic.LoadInt32(&t.deleteCalls)) }

// ---- fake CDN (httptest) ----------------------------------------------------------------

// fakeCDN reproduces the contract in testdata/cdn/README.md: ranged GET -> 206 with a
// correct Content-Range whose total == the full content size; an expiry mode that returns a
// 4xx (one of 400/403/410) for the FIRST N requests then serves bytes again.
type fakeCDN struct {
	srv       *httptest.Server
	content   []byte
	mu        sync.Mutex
	expireFor int   // serve a 4xx for the next N requests, then succeed
	expireSt  int   // which 4xx to return
	hits      int32 // total GETs received
	rangeHits int32 // GETs that carried a Range header
}

func newFakeCDN(content []byte) *fakeCDN {
	c := &fakeCDN{content: content, expireSt: http.StatusForbidden}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	return c
}

func (c *fakeCDN) close() { c.srv.Close() }

// url builds a presigned-style URL on the fake server for a file (mirrors the requestdl
// fixture shape: /dl/<hash>/<fileID>/<name>?token=...&expires=...).
func (c *fakeCDN) url(hash string, fileID int) string {
	return fmt.Sprintf("%s/dl/%s/%d/file.mp4?token=secrettoken&expires=9999999999", c.srv.URL, hash, fileID)
}

func (c *fakeCDN) handle(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&c.hits, 1)

	c.mu.Lock()
	if c.expireFor > 0 {
		c.expireFor--
		st := c.expireSt
		c.mu.Unlock()
		w.WriteHeader(st)
		return
	}
	c.mu.Unlock()

	total := int64(len(c.content))
	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(c.content)
		return
	}
	atomic.AddInt32(&c.rangeHits, 1)

	start, end, ok := parseRange(rng, total)
	if !ok {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body := c.content[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

// parseRange parses "bytes=a-b" and clamps b to the last byte index.
func parseRange(h string, total int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(h, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	var err error
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if parts[1] == "" {
		end = total - 1
	} else {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	if start < 0 || start >= total {
		return 0, 0, false
	}
	if end >= total {
		end = total - 1
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

func (c *fakeCDN) totalHits() int { return int(atomic.LoadInt32(&c.hits)) }

func (c *fakeCDN) setExpire(n, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireFor = n
	c.expireSt = status
}
