package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

// SyncResource runs a delta sync (ModificationTimestamp ge lastModified)
// for one resource. Returns the max source_modified_at of rows actually
// written this run — zero time.Time means "no new rows" and the caller
// should carry forward the cursor it queried with (Phase 4 plan §8).
func (s *Service) SyncResource(ctx context.Context, syncEventID uuid.UUID, v2url, originatingSystem, resourceName string, lastModified time.Time) (time.Time, error) {
	return s.fetchProcessAndEnqueue(ctx, syncEventID, resourceName, mls.DeltaURL(v2url, originatingSystem, resourceName, lastModified))
}

// InitialSync runs a full import (no time filter) for one resource.
// Returns the max source_modified_at observed. Initial syncs that return
// zero records yield a zero HWM — the caller may stamp time.Now() so the
// next delta starts from the import boundary.
func (s *Service) InitialSync(ctx context.Context, syncEventID uuid.UUID, v2url, originatingSystem, resourceName string) (time.Time, error) {
	return s.fetchProcessAndEnqueue(ctx, syncEventID, resourceName, mls.InitialURL(v2url, originatingSystem, resourceName))
}

// fetchProcessAndEnqueue runs the three-step pipeline for one resource:
//
//  1. paginate every page from firstURL into raw_output (splitting Property
//     children if applicable);
//  2. runProcessor over the resource + any expand-only children — this is
//     what creates the typed `media` rows that step 3 FKs to;
//  3. EnqueueAttachmentJobs over the media children accumulated in step 1.
//
// Step 3 must run AFTER step 2 because attachment_job.media_key has a
// Required FK to media.media_key — enqueueing before the media processor
// types its rows violates the constraint. (The pre-split code path had
// the same ordering bug latently; the standalone /v2/Media fetch always
// 501'd before enqueue could fire, so the FK violation never landed.)
func (s *Service) fetchProcessAndEnqueue(ctx context.Context, syncEventID uuid.UUID, resourceName, firstURL string) (time.Time, error) {
	hwm, mediaForEnqueue, err := s.paginate(ctx, syncEventID, resourceName, firstURL)
	if err != nil {
		return time.Time{}, err
	}
	if err := s.runProcessor(ctx, resourceName); err != nil {
		return time.Time{}, err
	}
	if len(mediaForEnqueue) > 0 {
		if err := s.EnqueueAttachmentJobs(ctx, syncEventID, mediaForEnqueue); err != nil {
			return time.Time{}, fmt.Errorf("enqueue: %w", err)
		}
	}
	return hwm, nil
}

// runProcessor invokes the registered processor for the resource after a
// successful pagination pass. For Property, the pass for property is
// followed by passes for each expand-only child resource whose rows the
// splitter just landed (media, property_rooms, property_unit_types); see
// [processor.ChildResources]. Children are iterated in dependency order so
// each child processor finds its parent already typed.
//
// If no processor is registered, or no processor handles a particular
// resource, the call is a no-op for that resource. A processor failure
// halts the chain and propagates — RunInitial / RunDelta stamp the
// sync_event failed (the raw_output rows are still safe in the DB).
func (s *Service) runProcessor(ctx context.Context, resourceName string) error {
	if s.processor == nil {
		return nil
	}
	dbResource, err := MLSToDBResource(resourceName)
	if err != nil {
		// Unknown resource: do not fail the sync just because the processor
		// can't address it. The raw rows are already persisted.
		log.Errorf("processor skip: %v", err)
		return nil
	}
	passes := append([]rawoutput.Resource{dbResource}, processor.ChildResources(dbResource)...)
	for _, r := range passes {
		if err := s.processor.RunPass(ctx, r); err != nil {
			if errors.Is(err, processor.ErrNoProcessor) {
				log.Infof("processor pass for %s skipped: no processor registered yet", r)
				continue
			}
			log.Errorf("processor pass for %s failed: %v", r, err)
			return &PassError{Pass: r, Err: err}
		}
	}
	return nil
}

// PassError identifies which downstream processor pass failed inside a
// resource step. Property's step runs passes for property + media +
// property_rooms + property_unit_types; without this typed wrapper, a
// halt during the media pass surfaces as "init halted on Property"
// (accurate but misleading at the worst moment — the failing pass is
// what operators need to see first). Init's halt formatter unwraps via
// errors.As and names the failing pass with the riding-step parenthetical
// when they differ.
type PassError struct {
	Pass rawoutput.Resource
	Err  error
}

func (e *PassError) Error() string {
	return fmt.Sprintf("processor[%s]: %v", e.Pass, e.Err)
}

func (e *PassError) Unwrap() error { return e.Err }

// paginate walks every page from firstURL into raw_output. Returns the max
// parent source_modified_at across pages and the accumulated media child
// payloads suitable for EnqueueAttachmentJobs — non-empty only when
// resourceName == Property and at least one fetched record carried Media
// in its $expand. Enqueue itself happens upstream in
// fetchProcessAndEnqueue, after runProcessor types the Media rows that
// attachment_job FKs to.
func (s *Service) paginate(ctx context.Context, syncEventID uuid.UUID, resourceName, firstURL string) (time.Time, []json.RawMessage, error) {
	nextURL := firstURL
	pageCount := 1
	var maxHWM time.Time
	var mediaForEnqueue []json.RawMessage
	for nextURL != "" {
		log.Infof("Fetching %s page %d...", resourceName, pageCount)

		odata, err := s.mlsClient.FetchPage(ctx, nextURL)
		if err != nil {
			return time.Time{}, nil, fmt.Errorf("page %d fetch: %w", pageCount, err)
		}

		if len(odata.Value) > 0 {
			pageHWM, pageMedia, err := s.saveToRawOutput(ctx, syncEventID, resourceName, odata.Value)
			if err != nil {
				return time.Time{}, nil, fmt.Errorf("page %d save: %w", pageCount, err)
			}
			if pageHWM.After(maxHWM) {
				maxHWM = pageHWM
			}
			mediaForEnqueue = append(mediaForEnqueue, pageMedia...)
		}

		nextURL = odata.NextLink
		pageCount++
	}
	// Per-resource fetch-volume signal. If MLS Grid's hourly/daily caps ever
	// get tripped, these counts let you see which resource ate the budget.
	log.Infof("fetched %d pages for %s", pageCount-1, resourceName)
	return maxHWM, mediaForEnqueue, nil
}
