package hit_tracker_client

import (
	"context"
	"flag"
	"sync"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/proxy_util"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/real_environment"
	"github.com/buildbuddy-io/buildbuddy/server/util/alert"
	"github.com/buildbuddy-io/buildbuddy/server/util/authutil"
	"github.com/buildbuddy-io/buildbuddy/server/util/claims"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/usageutil"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	capb "github.com/buildbuddy-io/buildbuddy/proto/cache"
	hitpb "github.com/buildbuddy-io/buildbuddy/proto/hit_tracker"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rspb "github.com/buildbuddy-io/buildbuddy/proto/resource"
	gstatus "google.golang.org/grpc/status"
)

var (
	remoteHitTrackerTarget       = flag.String("cache_proxy.remote_hit_tracker.target", "", "The gRPC target of the remote cache-hit-tracking service.")
	remoteHitTrackerPollInterval = flag.Duration("cache_proxy.remote_hit_tracker.update_interval", 250*time.Millisecond, "The time interval to wait between sending remote cache-hit-tracking RPCs.")
	maxPendingHitsPerGroup       = flag.Int("cache_proxy.remote_hit_tracker.max_pending_hits_per_group", 2_500_000, "The maximum number of pending cache-hit updates to store in memory for a given group.")
	maxHitsPerUpdate             = flag.Int("cache_proxy.remote_hit_tracker.max_hits_per_update", 250_000, "The maximum number of cache-hit updates to send in one request to the hit-tracking backend.")
	remoteHitTrackerWorkers      = flag.Int("cache_proxy.remote_hit_tracker.workers", 1, "The number of workers to use to send asynchronous remote cache-hit-tracking RPCs.")
)

func Register(env *real_environment.RealEnv) error {
	if *remoteHitTrackerTarget == "" || *remoteHitTrackerWorkers < 1 {
		env.SetHitTrackerFactory(&NoOpHitTrackerFactory{})
		return nil
	}

	conn, err := grpc_client.DialInternal(env, *remoteHitTrackerTarget)
	if err != nil {
		return err
	}
	env.SetHitTrackerFactory(newHitTrackerClient(env.GetServerContext(), env, conn))
	return nil
}

type NoOpHitTrackerFactory struct{}

func (h *NoOpHitTrackerFactory) NewACHitTracker(ctx context.Context, requestMetadata *repb.RequestMetadata) interfaces.HitTracker {
	return &NoOpHitTracker{}
}

func (h *NoOpHitTrackerFactory) NewCASHitTracker(ctx context.Context, requestMetadata *repb.RequestMetadata) interfaces.HitTracker {
	return &NoOpHitTracker{}
}

func newHitTrackerClient(ctx context.Context, env *real_environment.RealEnv, conn grpc.ClientConnInterface) *HitTrackerFactory {
	factory := HitTrackerFactory{
		authenticator:          env.GetAuthenticator(),
		pollInterval:           *remoteHitTrackerPollInterval,
		quit:                   make(chan struct{}, 1),
		maxPendingHitsPerGroup: *maxPendingHitsPerGroup,
		maxHitsPerUpdate:       *maxHitsPerUpdate,
		hitsByGroup:            map[groupID]*cacheHits{},
		client:                 hitpb.NewHitTrackerServiceClient(conn),
	}
	for i := 0; i < *remoteHitTrackerWorkers; i++ {
		factory.wg.Add(1)
		go func() {
			factory.runWorker(ctx)
			factory.wg.Done()
		}()
	}
	env.GetHealthChecker().RegisterShutdownFunction(factory.shutdown)
	return &factory
}

type groupID string
type cacheHits struct {
	maxPendingHitsPerGroup int
	gid                    groupID
	mu                     sync.Mutex
	authHeaders            map[string][]string
	usageHeaders           map[string][]string
	hits                   []*hitpb.CacheHit
}

func (c *cacheHits) enqueue(hit *hitpb.CacheHit, authHeaders map[string][]string, usageHeaders map[string][]string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.hits) >= c.maxPendingHitsPerGroup {
		return false
	}

	// Store the latest headers for this group for use in the async RPC.
	// TODO(jdhollen): send separate requests for different usage headers.
	c.authHeaders = authHeaders
	c.usageHeaders = usageHeaders
	c.hits = append(c.hits, hit)
	return true
}

type HitTrackerFactory struct {
	authenticator interfaces.Authenticator

	pollInterval time.Duration
	quit         chan struct{}
	wg           sync.WaitGroup

	mu                     sync.Mutex
	maxPendingHitsPerGroup int
	maxHitsPerUpdate       int
	hitsByGroup            map[groupID]*cacheHits
	hitsQueue              []*cacheHits

	client hitpb.HitTrackerServiceClient
}

func (h *HitTrackerFactory) NewACHitTracker(ctx context.Context, requestMetadata *repb.RequestMetadata) interfaces.HitTracker {
	if !proxy_util.SkipRemote(ctx) {
		// For Action Cache hit-tracking hitting the remote cache, the
		// authoritative cache should always take care of hit-tracking.
		alert.UnexpectedEvent("Unexpected call to NewACHitTracker in the proxy")
	}

	// Use a hit-tracker that sends information
	// about local cache hits to the RPC service at the configured backend.
	return &HitTrackerClient{ctx: ctx, enqueueFn: h.enqueue, client: h.client, requestMetadata: requestMetadata, cacheType: rspb.CacheType_AC}
}

func (h *HitTrackerFactory) NewCASHitTracker(ctx context.Context, requestMetadata *repb.RequestMetadata) interfaces.HitTracker {
	// For CAS hit-tracking, use a hit-tracker that sends information about
	// local cache hits to the RPC service at the configured backend.
	return &HitTrackerClient{ctx: ctx, enqueueFn: h.enqueue, client: h.client, requestMetadata: requestMetadata, cacheType: rspb.CacheType_CAS}
}

type NoOpHitTracker struct{}

func (h *NoOpHitTracker) SetExecutedActionMetadata(md *repb.ExecutedActionMetadata) {
}

func (h *NoOpHitTracker) TrackMiss(d *repb.Digest) error {
	return nil
}

func (h *NoOpHitTracker) TrackDownload(d *repb.Digest) interfaces.TransferTimer {
	return &NoOpTransferTimer{}
}

func (h *NoOpHitTracker) TrackUpload(d *repb.Digest) interfaces.TransferTimer {
	return &NoOpTransferTimer{}
}

type NoOpTransferTimer struct {
}

func (t *NoOpTransferTimer) CloseWithBytesTransferred(bytesTransferredCache, bytesTransferredClient int64, compressor repb.Compressor_Value, serverLabel string) error {
	return nil
}

func (t *NoOpTransferTimer) Record(bytesTransferred int64, duration time.Duration, compressor repb.Compressor_Value) error {
	return nil
}

type HitTrackerClient struct {
	ctx             context.Context
	enqueueFn       func(context.Context, *hitpb.CacheHit)
	client          hitpb.HitTrackerServiceClient
	requestMetadata *repb.RequestMetadata
	cacheType       rspb.CacheType
}

// TODO(https://github.com/buildbuddy-io/buildbuddy-internal/issues/4875) Implement
func (h *HitTrackerClient) SetExecutedActionMetadata(md *repb.ExecutedActionMetadata) {
	// This is used to track action durations and is not used for non-RBE executions.
	// Currently skip-remote behavior is not used for RBE, so do nothing in this case.
	if proxy_util.SkipRemote(h.ctx) {
		return
	}
	// By default, AC hit tracking should be handled by the remote cache.
	alert.UnexpectedEvent("Unexpected call to SetExecutedActionMetadata")
}

// TODO(https://github.com/buildbuddy-io/buildbuddy-internal/issues/4875) Implement
func (h *HitTrackerClient) TrackMiss(d *repb.Digest) error {
	// For requests that hit the backing cache: local cache misses hit the backing
	// cache, which will take care of hit-tracking for this request.
	//
	// For requests that skip the backing cache: tracking misses is only used for
	// populating the cache scorecard for Bazel builds with remote caching.
	// Currently skip-remote behavior is only used for workflows + Remote Bazel,
	// and not typical Bazel builds, so don't worry about tracking misses.
	return nil
}

func (h *HitTrackerFactory) groupID(ctx context.Context) groupID {
	claims, err := claims.ClaimsFromContext(ctx)
	if err != nil {
		return interfaces.AuthAnonymousUser
	}
	return groupID(claims.GetGroupID())
}

func (h *HitTrackerFactory) enqueue(ctx context.Context, hit *hitpb.CacheHit) {
	groupID := h.groupID(ctx)

	h.mu.Lock()
	if h.shouldFlushSynchronously() {
		h.mu.Unlock()
		log.CtxInfof(ctx, "hit_tracker_client.enqueue after worker shutdown, sending RPC synchronously")
		if _, err := h.client.Track(ctx, &hitpb.TrackRequest{Hits: []*hitpb.CacheHit{hit}}); err != nil {
			log.CtxWarningf(ctx, "Error sending HitTrackerService.Track RPC: %v", err)
		}
		return
	}

	if _, ok := h.hitsByGroup[groupID]; !ok {
		hits := cacheHits{
			maxPendingHitsPerGroup: h.maxPendingHitsPerGroup,
			gid:                    groupID,
			hits:                   []*hitpb.CacheHit{},
		}
		h.hitsByGroup[groupID] = &hits
		h.hitsQueue = append(h.hitsQueue, &hits)
	}

	groupHits := h.hitsByGroup[groupID]
	h.mu.Unlock()
	authHeaders := authutil.GetAuthHeaders(ctx)
	usageHeaders := make(map[string][]string, 0)
	for k, v := range usageutil.GetUsageHeaders(ctx) {
		// TODO(jdhollen): pass other headers once we're actually storing them separately.
		if k == usageutil.OriginHeaderName {
			usageHeaders[k] = v
		}
	}

	if groupHits.enqueue(hit, authHeaders, usageHeaders) {
		metrics.RemoteHitTrackerUpdates.WithLabelValues(
			string(groupID),
			"enqueued",
		).Add(1)
		return
	}

	metrics.RemoteHitTrackerUpdates.WithLabelValues(
		string(groupID),
		"dropped_too_many_updates",
	).Add(1)
}

type TransferTimer struct {
	ctx              context.Context
	enqueueFn        func(context.Context, *hitpb.CacheHit)
	invocationID     string
	requestMetadata  *repb.RequestMetadata
	digest           *repb.Digest
	start            time.Time
	client           hitpb.HitTrackerServiceClient
	cacheType        rspb.CacheType
	cacheRequestType capb.RequestType
}

func (t *TransferTimer) CloseWithBytesTransferred(bytesTransferredCache, bytesTransferredClient int64, compressor repb.Compressor_Value, serverLabel string) error {
	hit := &hitpb.CacheHit{
		RequestMetadata: t.requestMetadata,
		Resource: &rspb.ResourceName{
			Digest:    t.digest,
			CacheType: t.cacheType,
		},
		SizeBytes:        bytesTransferredClient,
		Duration:         durationpb.New(time.Since(t.start)),
		CacheRequestType: t.cacheRequestType,
	}
	t.enqueueFn(t.ctx, hit)
	return nil
}

func (t *TransferTimer) Record(bytesTransferred int64, duration time.Duration, compressor repb.Compressor_Value) error {
	return status.InternalError("Unxpected call to hit_tracker_client.Record()")
}

func (h *HitTrackerClient) TrackDownload(digest *repb.Digest) interfaces.TransferTimer {
	return &TransferTimer{
		ctx:              h.ctx,
		enqueueFn:        h.enqueueFn,
		requestMetadata:  h.requestMetadata,
		digest:           digest,
		start:            time.Now(),
		client:           h.client,
		cacheType:        h.cacheType,
		cacheRequestType: capb.RequestType_READ,
	}
}

func (h *HitTrackerClient) TrackUpload(digest *repb.Digest) interfaces.TransferTimer {
	if proxy_util.SkipRemote(h.ctx) {
		return &TransferTimer{
			ctx:              h.ctx,
			enqueueFn:        h.enqueueFn,
			requestMetadata:  h.requestMetadata,
			digest:           digest,
			start:            time.Now(),
			client:           h.client,
			cacheType:        h.cacheType,
			cacheRequestType: capb.RequestType_WRITE,
		}
	}
	// If writes hit the backing cache, it will handle hit tracking.
	return &NoOpTransferTimer{}
}

func (h *HitTrackerFactory) runWorker(ctx context.Context) {
	for {
		select {
		case <-h.quit:
			return
		case <-time.After(h.pollInterval):
		}
		// Keep flushing until there is nothing to flush.
		for h.sendTrackRequest(ctx) > 0 {
		}
	}
}

func (h *HitTrackerFactory) shouldFlushSynchronously() bool {
	select {
	case <-h.quit:
		return true
	default:
		return false
	}
}

// Sends the oldest pending batch of hits from the queue. This function sends
// one RPC and returns the number of updates sent.
func (h *HitTrackerFactory) sendTrackRequest(ctx context.Context) int {
	h.mu.Lock()
	if len(h.hitsQueue) == 0 {
		h.mu.Unlock()
		return 0
	}
	hitsToSend := h.hitsQueue[0]
	h.hitsQueue = h.hitsQueue[1:]
	hitsToSend.mu.Lock()
	if len(hitsToSend.hits) <= h.maxHitsPerUpdate {
		delete(h.hitsByGroup, hitsToSend.gid)
	} else {
		hitsToEnqueue := cacheHits{
			gid:          hitsToSend.gid,
			authHeaders:  hitsToSend.authHeaders,
			usageHeaders: hitsToSend.usageHeaders,
			hits:         hitsToSend.hits[h.maxHitsPerUpdate:],
		}
		hitsToSend.hits = hitsToSend.hits[:h.maxHitsPerUpdate]
		h.hitsQueue = append(h.hitsQueue, &hitsToEnqueue)
		h.hitsByGroup[hitsToEnqueue.gid] = &hitsToEnqueue
	}
	h.mu.Unlock()

	ctx = authutil.AddAuthHeadersToContext(ctx, hitsToSend.authHeaders, h.authenticator)
	ctx = usageutil.AddUsageHeadersToContext(ctx, hitsToSend.usageHeaders)
	trackRequest := hitpb.TrackRequest{Hits: hitsToSend.hits}
	groupID := hitsToSend.gid
	hitCount := len(hitsToSend.hits)
	hitsToSend.mu.Unlock()

	_, err := h.client.Track(ctx, &trackRequest)
	metrics.RemoteHitTrackerRequests.WithLabelValues(
		string(groupID),
		gstatus.Code(err).String(),
	).Observe(float64(hitCount))
	if err != nil {
		log.CtxWarningf(ctx, "Error sending Track request to record cache hit-tracking state group %s: %v", groupID, err)
	}
	return hitCount
}

func (h *HitTrackerFactory) shutdown(ctx context.Context) error {
	close(h.quit)
	h.wg.Wait()

	// Make a best-effort attempt to flush pending updates.
	// TODO(iain): we could do something fancier here if necessary, like
	// fire-and-forget these RPCs with a rate-limiter. Let's try this for now.
	for h.sendTrackRequest(ctx) > 0 {
	}

	return nil
}
