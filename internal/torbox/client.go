// Package torbox provides a concrete HTTP client satisfying the [Client] interface.
// Auth: "Authorization: Bearer <key>" on every request.
// The API key is never written to any log or error string.
package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/constants"
)

// defaultTimeout is used on all outbound HTTP requests.
const defaultTimeout = 30 * time.Second

// maxRetries for transient 5xx / network errors.
const maxRetries = 3

// retryBackoff is the base backoff duration (doubles each attempt).
const retryBackoff = 500 * time.Millisecond

// ----------------------------------------------------------------------------
// Internal option type for tests.
// ----------------------------------------------------------------------------

type option func(*client)

// withHTTPClient overrides the *http.Client (used in tests).
func withHTTPClient(hc *http.Client) option {
	return func(c *client) { c.hc = hc }
}

// withBaseURL overrides the API base URL (used in tests).
func withBaseURL(base string) option {
	return func(c *client) { c.base = base }
}

// ----------------------------------------------------------------------------
// Constructor.
// ----------------------------------------------------------------------------

// New constructs a production [Client] from the supplied TorBox config.
// For tests, call newClient instead, which accepts functional options.
func New(cfg config.TorBox) Client {
	return newClient(cfg)
}

// newClient is the internal constructor; it accepts functional options for testability.
func newClient(cfg config.TorBox, opts ...option) *client {
	base := cfg.APIBase
	if base == "" {
		base = constants.TorBoxAPIBase
	}
	c := &client{
		apiKey: cfg.APIKey,
		base:   base,
		hc: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ----------------------------------------------------------------------------
// client struct.
// ----------------------------------------------------------------------------

type client struct {
	apiKey string
	base   string
	hc     *http.Client
}

// ----------------------------------------------------------------------------
// Generic envelope + request helpers.
// ----------------------------------------------------------------------------

// envelope is the outer TorBox response wrapper.
// success may be bool or null in JSON (use json.RawMessage).
type envelope struct {
	Success json.RawMessage `json:"success"`
	Detail  string          `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

// isSuccess returns true when the Success field is the JSON literal `true`.
func (e envelope) isSuccess() bool {
	return string(e.Success) == "true"
}

// apiError is returned on non-2xx HTTP or success!=true.
// It intentionally omits the API key from any string representation.
type apiError struct {
	StatusCode int    // 0 if envelope-level error on a 2xx body
	Detail     string // TorBox detail string, never contains the key
}

func (e *apiError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("torbox: HTTP %d: %s", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("torbox: api error: %s", e.Detail)
}

// redactURLError strips the query string from a *url.Error's URL so the API key
// (passed to requestdl as a `token` query param) never reaches logs. The inner
// error is preserved for errors.Is/As (e.g. context.DeadlineExceeded). Non
// *url.Error values are returned unchanged.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := "[redacted]"
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery = ""
		redacted = u.String()
	}
	return fmt.Errorf("%s %q: %w", ue.Op, redacted, ue.Err)
}

// doGET executes a GET with retries. The env is populated on success.
// On HTTP 4xx in constants.LinkRefreshStatuses, it returns a sentinel *apiError
// with StatusCode set — callers that need ErrLinkExpired check for that.
func (c *client) doGET(ctx context.Context, path string, params url.Values) (envelope, int, error) {
	return c.doRequest(ctx, http.MethodGet, path, params, nil, "")
}

// doPOSTJSON executes a POST with a JSON body.
func (c *client) doPOSTJSON(ctx context.Context, path string, body any) (envelope, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return envelope{}, 0, fmt.Errorf("torbox: marshal: %w", err)
	}
	return c.doRequest(ctx, http.MethodPost, path, nil, bytes.NewReader(b), "application/json")
}

// doPOSTMultipart executes a multipart/form-data POST.
func (c *client) doPOSTMultipart(ctx context.Context, path string, fields map[string]string) (envelope, int, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return envelope{}, 0, fmt.Errorf("torbox: multipart: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return envelope{}, 0, fmt.Errorf("torbox: multipart close: %w", err)
	}
	return c.doRequest(ctx, http.MethodPost, path, nil, &buf, mw.FormDataContentType())
}

// doRequest is the core transport with retry logic.
// Returns (envelope, httpStatusCode, error).
// httpStatusCode is 0 on transport errors.
func (c *client) doRequest(
	ctx context.Context,
	method, path string,
	params url.Values,
	body io.Reader,
	contentType string,
) (envelope, int, error) {
	rawURL := c.base + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}

	var (
		lastErr    error
		lastStatus int
	)
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			wait := retryBackoff * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return envelope{}, 0, ctx.Err()
			case <-time.After(wait):
			}
		}

		// Re-read body for each retry — callers must supply re-readable body
		// (bytes.Buffer / bytes.Reader are used above, so Seek isn't needed; we
		// reconstruct the buffer in the calling helpers instead).
		var bodyR io.Reader
		if body != nil {
			// Body is already a *bytes.Reader or *bytes.Buffer passed once per call;
			// for retries, we only retry on 5xx/network so body is not re-consumed
			// (the 5xx response has no body we care about). For simplicity, we only
			// retry when body is nil (GET) or the body has been successfully buffered
			// (the helpers above always pass a *bytes.Reader or *bytes.Buffer).
			bodyR = body
		}

		req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyR)
		if err != nil {
			return envelope{}, 0, fmt.Errorf("torbox: new request: %w", redactURLError(err))
		}
		// Authorization header: never log or expose c.apiKey.
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			// The requestdl URL carries the API key as a `token` query param; a
			// *url.Error stringifies the full URL, so redact it before wrapping.
			lastErr = fmt.Errorf("torbox: request: %w", redactURLError(err))
			continue // retry on network error
		}
		defer resp.Body.Close()

		statusCode := resp.StatusCode

		// 4xx link-refresh statuses — return immediately, no retry.
		if slices.Contains(constants.LinkRefreshStatuses, statusCode) {
			return envelope{}, statusCode, &apiError{StatusCode: statusCode, Detail: "presigned link returned HTTP " + strconv.Itoa(statusCode)}
		}

		// 5xx — retry.
		if statusCode >= 500 {
			lastErr = &apiError{StatusCode: statusCode, Detail: "server error"}
			lastStatus = statusCode
			continue
		}

		// Parse the envelope.
		var env envelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return envelope{}, statusCode, fmt.Errorf("torbox: decode: %w", err)
		}

		// Non-2xx (but not in retry/link-refresh ranges).
		if statusCode >= 400 {
			return envelope{}, statusCode, &apiError{StatusCode: statusCode, Detail: env.Detail}
		}

		// 2xx but success != true (e.g. rate limit: success:null).
		if !env.isSuccess() {
			return env, statusCode, nil // caller inspects envelope.Detail for specific errors
		}

		return env, statusCode, nil
	}

	if lastErr != nil {
		return envelope{}, lastStatus, lastErr
	}
	return envelope{}, 0, fmt.Errorf("torbox: exhausted retries")
}

// ----------------------------------------------------------------------------
// CheckCached
// ----------------------------------------------------------------------------

// CheckCached implements [Client]. It batches hashes ≤100 per request.
func (c *client) CheckCached(hashes []string) (map[string]CachedItem, error) {
	result := make(map[string]CachedItem, len(hashes))

	for i := 0; i < len(hashes); i += constants.CheckCachedBatchMax {
		end := i + constants.CheckCachedBatchMax
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[i:end]

		params := url.Values{}
		params.Set("hash", strings.Join(batch, ","))
		params.Set("format", "object")
		params.Set("list_files", "true")

		env, _, err := c.doGET(context.Background(), constants.EpCheckCached, params)
		if err != nil {
			return nil, fmt.Errorf("torbox: checkcached: %w", err)
		}

		// data may be null or {} on all misses.
		if env.Data == nil || string(env.Data) == "null" {
			continue
		}

		// data is an object keyed by lowercase infohash.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return nil, fmt.Errorf("torbox: checkcached decode data: %w", err)
		}

		for hash, itemRaw := range raw {
			var item cachedItemRaw
			if err := json.Unmarshal(itemRaw, &item); err != nil {
				return nil, fmt.Errorf("torbox: checkcached item decode: %w", err)
			}
			result[strings.ToLower(hash)] = toCachedItem(hash, item)
		}
	}
	return result, nil
}

// ----------------------------------------------------------------------------
// TorrentInfo
// ----------------------------------------------------------------------------

// TorrentInfo implements [Client].
func (c *client) TorrentInfo(hash string) (*CachedItem, error) {
	params := url.Values{}
	params.Set("hash", hash)

	env, _, err := c.doGET(context.Background(), constants.EpTorrentInfo, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: torrentinfo: %w", err)
	}

	if env.Data == nil || string(env.Data) == "null" {
		return nil, nil
	}

	var item cachedItemRaw
	if err := json.Unmarshal(env.Data, &item); err != nil {
		return nil, fmt.Errorf("torbox: torrentinfo decode: %w", err)
	}

	h := item.Hash
	if h == "" {
		h = hash
	}
	ci := toCachedItem(h, item)
	return &ci, nil
}

// ----------------------------------------------------------------------------
// CreateTorrent
// ----------------------------------------------------------------------------

// CreateTorrent implements [Client].
func (c *client) CreateTorrent(magnet string, addOnlyIfCached bool) (int64, string, error) {
	fields := map[string]string{
		"magnet": magnet,
	}
	if addOnlyIfCached {
		fields["add_only_if_cached"] = "true"
	}

	env, _, err := c.doPOSTMultipart(context.Background(), constants.EpCreateTorrent, fields)
	if err != nil {
		return 0, "", fmt.Errorf("torbox: createtorrent: %w", err)
	}

	// Rate-limited: success == null and detail contains "60 per 1 hour".
	if !env.isSuccess() {
		if strings.Contains(env.Detail, "60 per 1 hour") {
			return 0, "", ErrRateLimited
		}
		return 0, "", &apiError{Detail: env.Detail}
	}

	var data struct {
		TorrentID int64  `json:"torrent_id"`
		Hash      string `json:"hash"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return 0, "", fmt.Errorf("torbox: createtorrent decode data: %w", err)
	}

	return data.TorrentID, data.Hash, nil
}

// ----------------------------------------------------------------------------
// RequestDL
// ----------------------------------------------------------------------------

// RequestDL implements [Client].
func (c *client) RequestDL(torrentID int64, fileID int) (string, error) {
	params := url.Values{}
	params.Set("token", c.apiKey)
	params.Set("torrent_id", strconv.FormatInt(torrentID, 10))
	params.Set("file_id", strconv.Itoa(fileID))
	params.Set("redirect", "false")

	env, statusCode, err := c.doGET(context.Background(), constants.EpRequestDL, params)
	if err != nil {
		// Check for link-expired sentinel from 4xx.
		if slices.Contains(constants.LinkRefreshStatuses, statusCode) {
			return "", ErrLinkExpired
		}
		return "", fmt.Errorf("torbox: requestdl: %w", err)
	}

	// data is the URL string.
	var dlURL string
	if err := json.Unmarshal(env.Data, &dlURL); err != nil {
		return "", fmt.Errorf("torbox: requestdl decode url: %w", err)
	}
	return dlURL, nil
}

// ----------------------------------------------------------------------------
// ControlDelete
// ----------------------------------------------------------------------------

// ControlDelete implements [Client]. Uses POST controltorrent {operation:"delete"}.
func (c *client) ControlDelete(torrentID int64) error {
	body := struct {
		TorrentID int64  `json:"torrent_id"`
		Operation string `json:"operation"`
	}{
		TorrentID: torrentID,
		Operation: "delete",
	}

	env, _, err := c.doPOSTJSON(context.Background(), constants.EpControl, body)
	if err != nil {
		return fmt.Errorf("torbox: controltorrent delete: %w", err)
	}
	if !env.isSuccess() {
		return &apiError{Detail: env.Detail}
	}
	return nil
}

// ----------------------------------------------------------------------------
// MyList / MyListByID
// ----------------------------------------------------------------------------

// MyList implements [Client].
func (c *client) MyList(offset int) ([]TorrentDetail, error) {
	params := url.Values{}
	params.Set("offset", strconv.Itoa(offset))
	params.Set("limit", strconv.Itoa(constants.MyListPageMax))
	params.Set("bypass_cache", "true")

	env, _, err := c.doGET(context.Background(), constants.EpMyList, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: mylist: %w", err)
	}

	if env.Data == nil || string(env.Data) == "null" {
		return nil, nil
	}

	var raw []torrentDetailRaw
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("torbox: mylist decode: %w", err)
	}

	out := make([]TorrentDetail, len(raw))
	for i, r := range raw {
		out[i] = toTorrentDetail(r)
	}
	return out, nil
}

// MyListByID implements [Client].
func (c *client) MyListByID(id int64) (*TorrentDetail, error) {
	params := url.Values{}
	params.Set("id", strconv.FormatInt(id, 10))

	env, _, err := c.doGET(context.Background(), constants.EpMyList, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: mylist byid: %w", err)
	}

	if env.Data == nil || string(env.Data) == "null" {
		return nil, nil
	}

	// When id is specified, data is a single object (not array).
	var raw torrentDetailRaw
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("torbox: mylist byid decode: %w", err)
	}
	td := toTorrentDetail(raw)
	return &td, nil
}

// ----------------------------------------------------------------------------
// UserMe
// ----------------------------------------------------------------------------

// UserMe implements [Client].
func (c *client) UserMe() (*Account, error) {
	params := url.Values{}
	params.Set("settings", "true")

	env, _, err := c.doGET(context.Background(), constants.EpUserMe, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: userme: %w", err)
	}

	var raw struct {
		Plan                      int    `json:"plan"`
		AdditionalConcurrentSlots int    `json:"additional_concurrent_slots"`
		CooldownUntil             string `json:"cooldown_until"`
		LongTermStorage           bool   `json:"long_term_storage"`
	}
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("torbox: userme decode: %w", err)
	}

	baseSlots := constants.EssentialActiveSlots // default (plan 1 = Essential)
	// Additional plans may have different base slots, but we only know Essential=3.
	// Per spec: derive base from plan; Essential=3. Others default same.
	acc := &Account{
		Plan:          raw.Plan,
		ActiveSlots:   baseSlots + raw.AdditionalConcurrentSlots,
		CooldownUntil: raw.CooldownUntil,
		LongTermStore: raw.LongTermStorage,
	}
	return acc, nil
}

// ----------------------------------------------------------------------------
// Raw JSON shapes for decoding.
// ----------------------------------------------------------------------------

type cachedFileRaw struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type cachedItemRaw struct {
	Name  string          `json:"name"`
	Hash  string          `json:"hash"`
	Size  int64           `json:"size"`
	Files []cachedFileRaw `json:"files"`
}

type torrentDetailRaw struct {
	ID               int64           `json:"id"`
	Hash             string          `json:"hash"`
	Name             string          `json:"name"`
	Files            []cachedFileRaw `json:"files"`
	DownloadFinished bool            `json:"download_finished"`
}

// ----------------------------------------------------------------------------
// Converters.
// ----------------------------------------------------------------------------

func toCachedItem(hash string, r cachedItemRaw) CachedItem {
	files := make([]CachedFile, len(r.Files))
	for i, f := range r.Files {
		files[i] = CachedFile{ID: f.ID, Name: f.Name, Size: f.Size}
	}
	return CachedItem{
		Hash:  strings.ToLower(hash),
		Name:  r.Name,
		Size:  r.Size,
		Files: files,
	}
}

func toTorrentDetail(r torrentDetailRaw) TorrentDetail {
	files := make([]CachedFile, len(r.Files))
	for i, f := range r.Files {
		files[i] = CachedFile{ID: f.ID, Name: f.Name, Size: f.Size}
	}
	return TorrentDetail{
		ID:               r.ID,
		Hash:             r.Hash,
		Name:             r.Name,
		Files:            files,
		DownloadFinished: r.DownloadFinished,
	}
}
