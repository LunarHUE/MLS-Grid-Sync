package mls

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/lunarhue/libs-go/log"
	"golang.org/x/time/rate"
)

const (
	// maxRetries bounds non-429 retry attempts (transient network errors,
	// 5xx, etc.) before FetchPage gives up. 429 has its own budget below.
	maxRetries = 3
	// maxConsecutive429 bounds courtesy retries when the server keeps
	// returning 429. Without it, an upstream stuck on 429 would loop
	// forever honoring Retry-After.
	maxConsecutive429 = 5
	// retryAfterCap is the ceiling on a single Retry-After sleep — guards
	// against a wildly large server value that would freeze the worker.
	retryAfterCap = 5 * time.Minute
	// fallback429Sleep is used when a 429 arrives with no Retry-After
	// header or an unparseable one.
	fallback429Sleep = 2 * time.Second
)

type Client struct {
	httpClient  *http.Client
	rateLimiter *rate.Limiter
	bearerToken string
	// sleep is used for retry back-off; overridable in tests to avoid delays.
	sleep func(time.Duration)
}

func NewClient(token string) *Client {
	return &Client{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		rateLimiter: rate.NewLimiter(rate.Limit(1), 1),
		bearerToken: token,
		sleep:       time.Sleep,
	}
}

// NewClientWithURL creates a client for testing. It uses an unlimited rate limiter
// and a no-op sleep so retry tests run instantly.
func NewClientWithURL(token, _ string) *Client {
	return &Client{
		httpClient:  &http.Client{Timeout: 5 * time.Second},
		rateLimiter: rate.NewLimiter(rate.Inf, 1),
		bearerToken: token,
		sleep:       func(time.Duration) {}, // no-op for tests
	}
}

// FetchPage handles rate limiting, retries, and JSON decoding for one OData page.
//
// 429 responses are treated specially: a courtesy retry honoring the
// server's Retry-After header (RFC 7231 §7.1.3) that does NOT consume the
// 3-attempt budget reserved for transient failures. This matters during
// `init`, the request-heaviest path in the system — without it, a 429
// burst would burn the budget in ~6 seconds and give up.
func (c *Client) FetchPage(ctx context.Context, pageURL string) (*ODataResponse, error) {
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	var (
		resp            *http.Response
		err             error
		nonRateAttempts int
		consec429       int
	)
	for {
		var req *http.Request
		log.Debugf("fetching '%s'", pageURL)
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)

		resp, err = c.httpClient.Do(req)

		// Network-level error → countable attempt, exponential backoff.
		if err != nil {
			nonRateAttempts++
			if nonRateAttempts >= maxRetries {
				break
			}
			c.sleep(time.Duration(1<<(nonRateAttempts-1)) * 2 * time.Second)
			continue
		}

		// 200 → done.
		if resp.StatusCode == http.StatusOK {
			break
		}

		// 429 → courtesy retry honoring Retry-After. Does NOT consume the
		// 3-attempt budget; capped at maxConsecutive429 to avoid an
		// infinite loop if the server is stuck.
		if resp.StatusCode == http.StatusTooManyRequests {
			consec429++
			if consec429 > maxConsecutive429 {
				break // surface the final 429 as the error below
			}
			delay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			if delay <= 0 {
				delay = fallback429Sleep
			}
			if delay > retryAfterCap {
				delay = retryAfterCap
			}
			resp.Body.Close()
			c.sleep(delay)
			continue
		}

		// Other non-200 → countable attempt, exponential backoff.
		consec429 = 0
		nonRateAttempts++
		if nonRateAttempts >= maxRetries {
			break
		}
		resp.Body.Close()
		c.sleep(time.Duration(1<<(nonRateAttempts-1)) * 2 * time.Second)
	}

	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()

		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("mls grid status %d: failed to read error body: %v", resp.StatusCode, readErr)
		}

		return nil, fmt.Errorf("mls grid status %d, response: %s", resp.StatusCode, string(bodyBytes))
	}

	var odata ODataResponse
	if err := json.NewDecoder(resp.Body).Decode(&odata); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &odata, nil
}

// parseRetryAfter parses an RFC 7231 §7.1.3 Retry-After header value,
// which may be either delta-seconds (decimal integer) or an HTTP-date.
// Returns 0 for an empty or unparseable value — the caller falls back
// to a small default.
func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}
