package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
)

// TestFetchConcurrencyBounded launches many parallel fetches against a slow
// server and asserts no more than cap(fetchSem) are in flight at once.
func TestFetchConcurrencyBounded(t *testing.T) {
	var inFlight, peak int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&inFlight, -1)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("done"))
	}))
	t.Cleanup(srv.Close)

	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 10}, ParseAllowList([]string{"127.0.0.1"}))
	const calls = 12
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct URLs so the cache doesn't coalesce the requests.
			_, _ = c.Fetch(context.Background(), fmt.Sprintf("%s/p%d", srv.URL, i), 100, 0, "")
		}(i)
	}
	// Let the queue build up, then release every handler at once.
	for atomic.LoadInt32(&inFlight) < int32(cap(fetchSem)) {
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&peak); got > int32(cap(fetchSem)) {
		t.Errorf("peak in-flight fetches = %d, want <= %d", got, cap(fetchSem))
	}
}
