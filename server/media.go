package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

// Why this endpoint does not fetch on demand
//
// MLS Grid media links are minted per OData response: they carry a token and
// an `expires` stamp exactly one hour out, and they are SINGLE USE — a second
// GET of the same URL returns 429 "Request limit reached" even seconds later,
// while a freshly minted URL for the same image succeeds. (Verified against
// the live feed 2026-07-24.)
//
// So media.media_url, captured whenever sync last saw the record, is dead by
// the time any browser asks for it. A read-through cache cannot fetch from it.
// Minting a fresh link costs a full OData round trip for the parent listing,
// under the same ~1 rps license cap that governs everything else — far too
// slow to sit inside a page load, and trivially self-DoSing when a listing
// page requests twenty photos at once.
//
// The endpoint therefore serves what is already stored and, on a miss,
// registers the image for background prefetch. The attachment worker mints
// fresh URLs one listing at a time (all of a listing's photos share a single
// token, so one refresh unlocks all of them) and fills storage in the
// background. Steady state is a pure cache hit; the cold path resolves within
// a worker cycle rather than blocking a visitor.

// prefetchBlockingStatuses are the job states that mean "this media key is
// already spoken for" — re-registering it would just create duplicate work.
// canceled and permanently_failed are absent by design: a re-request is a
// legitimate signal to try again.
var prefetchBlockingStatuses = []attachmentjob.Status{
	attachmentjob.StatusPending,
	attachmentjob.StatusRetrying,
	attachmentjob.StatusInProgress,
	attachmentjob.StatusSucceeded,
}

const defaultMediaMaxAge = 24 * time.Hour

// MediaOptions configures the media endpoint. A nil Storer leaves the route
// unregistered, so a deployment without storage 404s rather than 500s.
type MediaOptions struct {
	// Storer must also implement storage.Fetcher for anything to be served.
	Storer storage.Storer

	// KeyPrefix must match the attachment worker's storage.key_prefix, or the
	// handler will look for objects under a path the worker never wrote.
	// Both compose <prefix>media/<mediaKey>/<sha256>.
	KeyPrefix string

	// CacheMaxAge is the max-age on a hit. Objects are content-addressed by
	// sha256, so a revised photo lands at a new key and a long TTL is safe.
	CacheMaxAge time.Duration

	// PublicBaseURL, when set, makes a hit answer 302 to
	// <PublicBaseURL>/<objectKey> instead of streaming the bytes. Point it at
	// whatever serves the bucket publicly — an R2 custom domain, a CDN in
	// front of the container — and image bytes stop transiting this process
	// entirely: serve resolves MediaKey → sha256 out of Postgres and hands the
	// browser the address. That is the difference between one always-on
	// replica being in the path of every photo on the site and it being in the
	// path of none of them.
	//
	// Redirect mode needs no Storer at all, so a deployment can run serve
	// without storage credentials and leave uploads to the worker.
	//
	// The trade-off: nothing checks that the object is really there, because
	// checking would put this process back in the path it just left. A media
	// row pointing at a deleted object redirects to a 404 rather than falling
	// back to prefetch the way the streaming path does. Rows are written only
	// after a successful upload, so that means out-of-band deletion.
	PublicBaseURL string

	// DisablePrefetch turns off miss-triggered registration, leaving the
	// endpoint a pure cache reader.
	DisablePrefetch bool
}

// mediaHandler serves attachment binaries by RESO MediaKey.
type mediaHandler struct {
	client     *ent.Client
	fetcher    storage.Fetcher // nil when the backend cannot read back
	publicBase *url.URL        // non-nil redirects instead of streaming
	prefix     string
	maxAge     time.Duration
	prefetch   bool

	// prefetchEvent caches the synthetic SyncEvent that on-demand jobs hang
	// off. AttachmentJob.sync_event_id is a required edge and a browser
	// request has no originating sync, so serve owns one backfill event and
	// reuses it. Cached because otherwise every cold image costs a lookup.
	prefetchOnce  sync.Once
	prefetchEvent uuid.UUID
	prefetchErr   error
}

func newMediaHandler(client *ent.Client, opts MediaOptions) *mediaHandler {
	h := &mediaHandler{
		client:   client,
		prefix:   opts.KeyPrefix,
		maxAge:   opts.CacheMaxAge,
		prefetch: !opts.DisablePrefetch,
	}
	if f, ok := opts.Storer.(storage.Fetcher); ok {
		h.fetcher = f
	}
	if opts.PublicBaseURL != "" {
		// A malformed base is loud but not fatal, matching how serve treats an
		// unreachable storage backend: lose the redirect, keep the API. When a
		// Fetcher is configured the endpoint quietly falls back to streaming;
		// when it is not, every request becomes a prefetch-queued miss.
		base, err := url.Parse(opts.PublicBaseURL)
		if err != nil || base.Scheme == "" || base.Host == "" {
			log.Errorf("media: ignoring unusable public base URL %q (want scheme://host): %v",
				opts.PublicBaseURL, err)
		} else {
			h.publicBase = base
		}
	}
	if h.maxAge <= 0 {
		h.maxAge = defaultMediaMaxAge
	}
	if h.prefix != "" && h.prefix[len(h.prefix)-1] != '/' {
		h.prefix += "/"
	}
	return h
}

// objectKey MUST stay byte-identical to the attachment worker's key
// construction (sync/attachment.go), or reader and writer disagree about
// where a given image lives.
func (h *mediaHandler) objectKey(mediaKey, hash string) string {
	return h.prefix + "media/" + mediaKey + "/" + hash
}

func (h *mediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediaKey := r.PathValue("mediaKey")
	if mediaKey == "" {
		http.Error(w, "media key required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	m, err := h.client.Media.Get(ctx, mediaKey)
	if err != nil {
		if ent.IsNotFound(err) {
			http.Error(w, "media not found", http.StatusNotFound)
			return
		}
		log.Errorf("media %s: load: %v", mediaKey, err)
		http.Error(w, "media lookup failed", http.StatusInternalServerError)
		return
	}

	if m.AttachmentID != nil && (h.publicBase != nil || h.fetcher != nil) {
		var (
			served bool
			err    error
		)
		if h.publicBase != nil {
			served, err = h.redirectToPublic(ctx, w, r, mediaKey, *m.AttachmentID)
		} else {
			served, err = h.serveFromStorage(ctx, w, r, mediaKey, *m.AttachmentID)
		}
		if err != nil {
			log.Errorf("media %s: serve from storage: %v", mediaKey, err)
			http.Error(w, "media read failed", http.StatusBadGateway)
			return
		}
		if served {
			return
		}
		// The row pointed at an object the backend does not have — manual
		// deletion, a lifecycle rule, or a restore into a fresh bucket. Treat
		// it as a miss and let prefetch repopulate rather than 404 forever.
		log.Warnf("media %s: attachment row present but object missing; re-registering", mediaKey)
	}

	h.registerPrefetch(ctx, mediaKey)

	// 404 rather than a redirect to media_url: that URL is expired, single
	// use, or both, so redirecting would hand the visitor a guaranteed 429.
	// The site renders its placeholder and the image appears on a later load.
	w.Header().Set("X-Media-Status", "prefetch-queued")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "media not yet available", http.StatusNotFound)
}

// attachmentFor resolves the row a media record points at, reporting
// (nil, nil) for a dangling pointer so callers treat it as a miss.
func (h *mediaHandler) attachmentFor(ctx context.Context, attachmentID uuid.UUID) (*ent.Attachment, error) {
	att, err := h.client.Attachment.Get(ctx, attachmentID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load attachment: %w", err)
	}
	return att, nil
}

// redirectToPublic points the browser at the object's public address instead
// of proxying its bytes. Reports (false, nil) on a dangling attachment pointer
// so the caller falls through to prefetch, matching serveFromStorage.
func (h *mediaHandler) redirectToPublic(
	ctx context.Context, w http.ResponseWriter, r *http.Request, mediaKey string, attachmentID uuid.UUID,
) (bool, error) {
	att, err := h.attachmentFor(ctx, attachmentID)
	if err != nil || att == nil {
		return false, err
	}

	// JoinPath escapes each element and cleans the result, so a key is safe to
	// splice into the path whatever it contains.
	target := h.publicBase.JoinPath(strings.Split(h.objectKey(mediaKey, att.SourceHash), "/")...)

	// 302, deliberately not 301/308. The target embeds the sha256 of the
	// current bytes, so re-photographing a listing moves this MediaKey to a
	// different object — and a permanent redirect is cached by browsers with
	// no practical way to recall it, pinning visitors to the old image
	// forever. max-age without immutable for the same reason: the object is
	// immutable, this mapping is not.
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(h.maxAge.Seconds())))
	w.Header().Set("X-Media-Status", "redirect")
	http.Redirect(w, r, target.String(), http.StatusFound)
	return true, nil
}

// serveFromStorage streams a cached object, reporting (false, nil) when the
// object is simply absent so the caller can treat that as a miss.
func (h *mediaHandler) serveFromStorage(
	ctx context.Context, w http.ResponseWriter, r *http.Request, mediaKey string, attachmentID uuid.UUID,
) (bool, error) {
	att, err := h.attachmentFor(ctx, attachmentID)
	if err != nil || att == nil {
		return false, err // dangling pointer; re-register
	}

	body, ctype, err := h.fetcher.Download(ctx, h.objectKey(mediaKey, att.SourceHash))
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return false, nil
		}
		return false, err
	}
	defer body.Close()

	// The recorded mime type wins: it is what the origin declared at download
	// time, where some backends report only a generic type.
	if att.MimeType != nil && *att.MimeType != "" {
		ctype = *att.MimeType
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	if att.SizeBytes != nil {
		w.Header().Set("Content-Length", strconv.Itoa(*att.SizeBytes))
	}
	// immutable is honest here: bytes are content-addressed by sha256, so a
	// revised photo becomes a different object rather than mutating this one.
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable", int(h.maxAge.Seconds())))
	w.Header().Set("X-Media-Status", "hit")

	if r.Method == http.MethodHead {
		return true, nil
	}
	if _, err := io.Copy(w, body); err != nil {
		// Headers are committed, so this can only be logged. Usually just a
		// client that navigated away mid-image.
		log.Debugf("media %s: stream interrupted: %v", mediaKey, err)
	}
	return true, nil
}

// registerPrefetch records that someone actually wanted this image, so the
// worker downloads it on its next cycle. Best effort throughout: a visitor's
// request must not fail because bookkeeping did.
func (h *mediaHandler) registerPrefetch(ctx context.Context, mediaKey string) {
	if !h.prefetch {
		return
	}

	exists, err := h.client.AttachmentJob.Query().
		Where(
			attachmentjob.MediaKey(mediaKey),
			attachmentjob.StatusIn(prefetchBlockingStatuses...),
		).
		Exist(ctx)
	if err != nil {
		log.Errorf("media %s: check existing job: %v", mediaKey, err)
		return
	}
	if exists {
		return // already queued or already done
	}

	eventID, err := h.prefetchSyncEvent(ctx)
	if err != nil {
		log.Errorf("media %s: prefetch sync event: %v", mediaKey, err)
		return
	}
	if err := h.client.AttachmentJob.Create().
		SetMediaKey(mediaKey).
		SetSyncEventID(eventID).
		Exec(ctx); err != nil {
		log.Errorf("media %s: enqueue prefetch: %v", mediaKey, err)
		return
	}
	log.Debugf("media %s: prefetch queued", mediaKey)
}

// prefetchSyncEvent returns the SyncEvent that on-demand jobs attach to,
// creating it once per process. run_type=backfill is the accurate label: this
// is filling in history the regular sync did not download.
func (h *mediaHandler) prefetchSyncEvent(ctx context.Context) (uuid.UUID, error) {
	h.prefetchOnce.Do(func() {
		// Reuse a prior process's event when one is present, so restarts do
		// not accumulate a row per deploy.
		existing, err := h.client.SyncEvent.Query().
			Where(
				syncevent.ResourceEQ(syncevent.ResourceMedia),
				syncevent.RunTypeEQ(syncevent.RunTypeBackfill),
				syncevent.StatusEQ(syncevent.StatusRunning),
			).
			First(ctx)
		if err == nil {
			h.prefetchEvent = existing.ID
			return
		}
		if !ent.IsNotFound(err) {
			h.prefetchErr = fmt.Errorf("query prefetch event: %w", err)
			return
		}

		src, err := h.client.SourceSystem.Query().First(ctx)
		if err != nil {
			h.prefetchErr = fmt.Errorf("no source system (run init first): %w", err)
			return
		}
		created, err := h.client.SyncEvent.Create().
			SetSourceSystemID(src.ID).
			SetResource(syncevent.ResourceMedia).
			SetRunType(syncevent.RunTypeBackfill).
			SetStatus(syncevent.StatusRunning).
			SetProcessorVersion("serve-prefetch").
			SetStartedAt(time.Now()).
			Save(ctx)
		if err != nil {
			h.prefetchErr = fmt.Errorf("create prefetch event: %w", err)
			return
		}
		h.prefetchEvent = created.ID
	})
	return h.prefetchEvent, h.prefetchErr
}
