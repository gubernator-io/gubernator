package gubernator_test

import (
	"context"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/gubernator-io/gubernator/v2/cluster"
	"github.com/mailgun/holster/v4/clock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	rate10PerMinute = 6000
	metricScheduled = "gubernator_scheduled_count"
	metricCeiling   = "gubernator_scheduled_ceiling_refused_count"
	metricReset     = "gubernator_scheduled_debt_reset_count"
	metricOverLimit = "gubernator_over_limit_counter"
)

// leaky10PerMinute is the requester's example from the blueprint, 10 per minute with a burst of 10.
func leaky10PerMinute(name, key string, hits, maxWait int64) *guber.RateLimitReq {
	return &guber.RateLimitReq{
		Name:      name,
		UniqueKey: key,
		Algorithm: guber.Algorithm_LEAKY_BUCKET,
		Duration:  guber.Minute,
		Limit:     10,
		Burst:     10,
		Hits:      hits,
		MaxWait:   maxWait,
	}
}

func sendOne(t *testing.T, client guber.V1Client, req *guber.RateLimitReq) *guber.RateLimitResp {
	t.Helper()
	resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{req},
	})
	require.NoError(t, err)
	require.Len(t, resp.Responses, 1)
	assert.GreaterOrEqual(t, resp.Responses[0].Remaining, int64(0))
	return resp.Responses[0]
}

func TestScheduledAdmissionWorkedExample(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name, key = "TestScheduledAdmissionWorkedExample", "account:1"
	scheduledBefore := getMetricValue(t, cluster.DaemonAt(0), metricScheduled)

	for i := 1; i <= 10; i++ {
		resp := sendOne(t, client, leaky10PerMinute(name, key, 1, 120000))
		assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Status)
		assert.Equal(t, int64(10-i), resp.Remaining)
		assert.Equal(t, int64(0), resp.RetryAfter)
	}
	for i := 1; i <= 20; i++ {
		resp := sendOne(t, client, leaky10PerMinute(name, key, 1, 120000))
		assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
		assert.Equal(t, int64(i*rate10PerMinute), resp.RetryAfter)
		assert.Equal(t, int64(0), resp.Remaining)
	}

	// The 31st is 126000ms out, past the 120000ms deadline. Asking again shows nothing was charged.
	for range 2 {
		resp := sendOne(t, client, leaky10PerMinute(name, key, 1, 120000))
		assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
		assert.Equal(t, int64(126000), resp.RetryAfter)
	}
	assert.Equal(t, float64(20), getMetricValue(t, cluster.DaemonAt(0), metricScheduled)-scheduledBefore)
}

func TestScheduledAdmissionBoundary(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name, key = "TestScheduledAdmissionBoundary", "account:1"

	sendOne(t, client, leaky10PerMinute(name, key, 10, 0))

	// A wait equal to max_wait is booked; one millisecond more is refused.
	resp := sendOne(t, client, leaky10PerMinute(name, key, 1, 5999))
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Equal(t, int64(6000), resp.RetryAfter)
	resp = sendOne(t, client, leaky10PerMinute(name, key, 1, 6000))
	assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
	assert.Equal(t, int64(6000), resp.RetryAfter)
	resp = sendOne(t, client, leaky10PerMinute(name, key, 1, 11999))
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Equal(t, int64(12000), resp.RetryAfter)
}

func TestScheduledAdmissionCeiling(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name, key = "TestScheduledAdmissionCeiling", "account:1"
	refusedBefore := getMetricValue(t, cluster.DaemonAt(0), metricCeiling)

	// One token every 200s, so the second hit waits 200s and the third 400s, past the default 5m ceiling.
	req := func(maxWait int64) *guber.RateLimitReq {
		return &guber.RateLimitReq{
			Name: name, UniqueKey: key, Algorithm: guber.Algorithm_LEAKY_BUCKET,
			Duration: 200_000, Limit: 1, Burst: 1, Hits: 1, MaxWait: maxWait,
		}
	}
	assert.Equal(t, guber.Status_UNDER_LIMIT, sendOne(t, client, req(1_000_000)).Status)
	resp := sendOne(t, client, req(1_000_000))
	assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
	assert.Equal(t, int64(200_000), resp.RetryAfter)

	for range 2 {
		resp = sendOne(t, client, req(1_000_000))
		assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
		assert.Equal(t, int64(400_000), resp.RetryAfter)
		assert.LessOrEqual(t, resp.RetryAfter, int64(1_000_000))
	}
	assert.Equal(t, float64(2), getMetricValue(t, cluster.DaemonAt(0), metricCeiling)-refusedBefore)

	// A deadline shorter than the wait is a deadline refusal, not a ceiling refusal.
	resp = sendOne(t, client, req(300_000))
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Equal(t, int64(400_000), resp.RetryAfter)
	assert.Equal(t, float64(2), getMetricValue(t, cluster.DaemonAt(0), metricCeiling)-refusedBefore)
}

func TestScheduledAdmissionConfiguredCeiling(t *testing.T) {
	d := spawnSingleDaemon(t, guber.BehaviorConfig{ScheduleMaxWait: 10 * time.Second})
	client := d.MustClient()
	defer clock.Freeze(clock.Now()).Unfreeze()

	req := func(hits int64) *guber.RateLimitReq {
		return leaky10PerMinute("TestScheduledAdmissionConfiguredCeiling", "account:1", hits, 120000)
	}
	sendOne(t, client, req(10))

	// 6000ms fits under the 10s ceiling; the next, at 12000ms, does not.
	assert.Equal(t, guber.Status_SCHEDULED, sendOne(t, client, req(1)).Status)
	resp := sendOne(t, client, req(1))
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Equal(t, int64(12000), resp.RetryAfter)
}

func TestScheduledAdmissionConfigFromEnv(t *testing.T) {
	// Other tests in this package leave GUBER_ variables behind, and some of them change how the
	// config parses, so start from an environment without any.
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "GUBER_") {
			continue
		}
		require.NoError(t, os.Unsetenv(name))
		t.Cleanup(func() { _ = os.Setenv(name, value) })
	}
	// The env file sets the variable on the process, so register it to be unset again.
	t.Setenv("GUBER_SCHEDULE_MAX_WAIT", "")
	conf, err := guber.SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader("GUBER_SCHEDULE_MAX_WAIT=90s"))
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, conf.Behaviors.ScheduleMaxWait)
}

func TestScheduledAdmissionNegativeCeilingFailsStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := guber.SpawnDaemon(ctx, guber.DaemonConfig{
		Logger:            logrus.NewEntry(logrus.New()),
		InstanceID:        "negative",
		GRPCListenAddress: "127.0.0.1:0",
		HTTPListenAddress: "127.0.0.1:0",
		AdvertiseAddress:  "127.0.0.1:0",
		Behaviors:         guber.BehaviorConfig{ScheduleMaxWait: -time.Second},
	})
	require.ErrorContains(t, err, "ScheduleMaxWait cannot be negative")
}

func TestScheduledAdmissionNeverFits(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name = "TestScheduledAdmissionNeverFits"
	overBefore := getMetricValue(t, cluster.DaemonAt(0), metricOverLimit)

	// Existing bucket.
	sendOne(t, client, leaky10PerMinute(name, "existing", 1, 0))
	resp := sendOne(t, client, leaky10PerMinute(name, "existing", 11, 60000))
	assert.Equal(t, guber.Status_NEVER_FITS, resp.Status)
	assert.Equal(t, int64(0), resp.RetryAfter)
	resp = sendOne(t, client, leaky10PerMinute(name, "existing", 0, 0))
	assert.Equal(t, int64(9), resp.Remaining)

	// Key with no bucket: nothing is created, so the key is still full.
	resp = sendOne(t, client, leaky10PerMinute(name, "unknown", 11, 60000))
	assert.Equal(t, guber.Status_NEVER_FITS, resp.Status)
	assert.Equal(t, int64(0), resp.RetryAfter)
	resp = sendOne(t, client, leaky10PerMinute(name, "unknown", 0, 0))
	assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Status)
	assert.Equal(t, int64(10), resp.Remaining)

	assert.Equal(t, float64(2), getMetricValue(t, cluster.DaemonAt(0), metricOverLimit)-overBefore)
}

func TestScheduledAdmissionPlainRequestsOnKeyInDebt(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name = "TestScheduledAdmissionPlainRequestsOnKeyInDebt"
	resetBefore := getMetricValue(t, cluster.DaemonAt(0), metricReset)

	// Two bookings put the balance at -2, so the next slot is 18000ms out.
	probe := func(key string) *guber.RateLimitResp {
		return sendOne(t, client, leaky10PerMinute(name, key, 1, 1000))
	}
	book := func(key string) {
		sendOne(t, client, leaky10PerMinute(name, key, 10, 0))
		for range 2 {
			assert.Equal(t, guber.Status_SCHEDULED, sendOne(t, client, leaky10PerMinute(name, key, 1, 60000)).Status)
		}
		assert.Equal(t, int64(18000), probe(key).RetryAfter)
	}

	// A plain request is refused and leaves the debt alone.
	book("plain")
	plain := leaky10PerMinute(name, "plain", 1, 0)
	resp := sendOne(t, client, plain)
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Equal(t, int64(0), resp.Remaining)
	assert.Equal(t, int64(0), resp.RetryAfter)
	assert.Equal(t, int64(18000), probe("plain").RetryAfter)

	// DRAIN_OVER_LIMIT never raises debt to zero.
	book("drain")
	drain := leaky10PerMinute(name, "drain", 1, 0)
	drain.Behavior = guber.Behavior_DRAIN_OVER_LIMIT
	assert.Equal(t, guber.Status_OVER_LIMIT, sendOne(t, client, drain).Status)
	assert.Equal(t, int64(18000), probe("drain").RetryAfter)

	// A changed burst does not refill a bucket in debt.
	book("burst")
	bigger := leaky10PerMinute(name, "burst", 1, 0)
	bigger.Burst = 20
	assert.Equal(t, guber.Status_OVER_LIMIT, sendOne(t, client, bigger).Status)
	assert.Equal(t, int64(18000), probe("burst").RetryAfter)

	// Negative hits do not pay off debt.
	book("refund")
	sendOne(t, client, leaky10PerMinute(name, "refund", -1, 0))
	assert.Equal(t, int64(18000), probe("refund").RetryAfter)

	// A status query neither books nor reads as over the limit, with or without max_wait.
	book("query")
	for _, maxWait := range []int64{0, 60000} {
		resp = sendOne(t, client, leaky10PerMinute(name, "query", 0, maxWait))
		assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Status)
		assert.Equal(t, int64(0), resp.RetryAfter)
		assert.Equal(t, int64(0), resp.Remaining)
	}
	assert.Equal(t, int64(18000), probe("query").RetryAfter)

	// RESET_REMAINING is a documented reset; it forgives the debt and is counted.
	book("reset")
	reset := leaky10PerMinute(name, "reset", 0, 0)
	reset.Behavior = guber.Behavior_RESET_REMAINING
	resp = sendOne(t, client, reset)
	assert.Equal(t, int64(10), resp.Remaining)
	assert.Equal(t, float64(1), getMetricValue(t, cluster.DaemonAt(0), metricReset)-resetBefore)

	// A reset of a key that holds no debt forgives nothing, so it is not counted.
	reset = leaky10PerMinute(name, "reset", 0, 0)
	reset.Behavior = guber.Behavior_RESET_REMAINING
	sendOne(t, client, reset)
	assert.Equal(t, float64(1), getMetricValue(t, cluster.DaemonAt(0), metricReset)-resetBefore)
}

func TestScheduledAdmissionBurstChangeKeepsDebtCached(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name, key = "TestScheduledAdmissionBurstChangeKeepsDebtCached", "account:1"

	sendOne(t, client, leaky10PerMinute(name, key, 10, 0))
	for range 2 {
		sendOne(t, client, leaky10PerMinute(name, key, 1, 60000))
	}

	// Raising burst to 20 moves full_at out to 22 tokens, 132000ms, past the expiry set by the bookings.
	bigger := leaky10PerMinute(name, key, 0, 0)
	bigger.Burst = 20
	sendOne(t, client, bigger)

	clock.Advance(131999 * time.Millisecond)
	assert.Equal(t, int64(19), sendOne(t, client, bigger).Remaining)
}

// expiryStore records the expiry each item had when the Store was told about the change.
type expiryStore struct {
	mu      sync.Mutex
	expires map[string]int64
}

func (s *expiryStore) OnChange(_ context.Context, _ *guber.RateLimitReq, item *guber.CacheItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires[item.Key] = item.ExpireAt
}

func (s *expiryStore) Get(context.Context, *guber.RateLimitReq) (*guber.CacheItem, bool) {
	return nil, false
}

func (s *expiryStore) Remove(context.Context, string) {}

func TestScheduledAdmissionStoreSeesExtendedExpiry(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	store := &expiryStore{expires: map[string]int64{}}
	d := spawnSingleDaemonWithStore(t, store)
	client := d.MustClient()
	const name, key = "TestScheduledAdmissionStoreSeesExtendedExpiry", "account:1"
	start := clock.Now().UnixMilli()

	sendOne(t, client, leaky10PerMinute(name, key, 10, 0))
	for range 2 {
		sendOne(t, client, leaky10PerMinute(name, key, 1, 60000))
	}
	bigger := leaky10PerMinute(name, key, 0, 0)
	bigger.Burst = 20
	sendOne(t, client, bigger)

	// A restart reloads the key from the Store, so the Store needs the 132000ms expiry, not the old one.
	store.mu.Lock()
	defer store.mu.Unlock()
	assert.GreaterOrEqual(t, store.expires[name+"_"+key], start+132000)
}

func TestScheduledAdmissionBurstRaiseAfterDebtRepaid(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name = "TestScheduledAdmissionBurstRaiseAfterDebtRepaid"
	req := func(key string, hits, maxWait int64) *guber.RateLimitReq {
		return &guber.RateLimitReq{
			Name: name, UniqueKey: key, Algorithm: guber.Algorithm_LEAKY_BUCKET,
			Duration: 10000, Limit: 14, Burst: 14, Hits: hits, MaxWait: maxWait,
		}
	}

	// A key that never held debt and sits at zero gains the 6 tokens of a burst raised from 14 to 20.
	sendOne(t, client, req("plain", 14, 0))
	raised := req("plain", 0, 0)
	raised.Burst = 20
	plain := sendOne(t, client, raised)

	// A key whose debt leaked away by the time of the request is treated the same.
	sendOne(t, client, req("repaid", 14, 0))
	sendOne(t, client, req("repaid", 7, 100000))
	clock.Advance(5000 * time.Millisecond)
	raised = req("repaid", 0, 0)
	raised.Burst = 20
	repaid := sendOne(t, client, raised)

	assert.Equal(t, int64(6), plain.Remaining)
	assert.Equal(t, plain.Remaining, repaid.Remaining)
}

func TestScheduledAdmissionHugeWaitIsRefused(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	req := func(maxWait int64) *guber.RateLimitReq {
		return &guber.RateLimitReq{
			Name: "TestScheduledAdmissionHugeWaitIsRefused", UniqueKey: "account:1",
			Algorithm: guber.Algorithm_LEAKY_BUCKET, Duration: 1e15, Limit: 1,
			Burst: 10000000, Hits: 10000000, MaxWait: maxWait,
		}
	}

	// Refilling 10 million tokens at one per 1e15ms takes about 1e22ms, more than an int64 holds.
	assert.Equal(t, guber.Status_UNDER_LIMIT, sendOne(t, client, req(60000)).Status)
	resp := sendOne(t, client, req(60000))
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Greater(t, resp.RetryAfter, int64(300000))
}

func TestScheduledAdmissionNonPositiveRateNeverFits(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	for _, test := range []struct {
		name     string
		limit    int64
		duration int64
		final    guber.Status
	}{
		{name: "NegativeLimit", limit: -1, duration: 60000, final: guber.Status_NEVER_FITS},
		// The bucket is already expired when created, so every request finds a new one.
		{name: "NegativeDuration", limit: 5, duration: -60000, final: guber.Status_UNDER_LIMIT},
		{name: "ZeroDuration", limit: 5, duration: 0, final: guber.Status_NEVER_FITS},
	} {
		t.Run(test.name, func(t *testing.T) {
			var resp *guber.RateLimitResp
			for range 3 {
				resp = sendOne(t, client, &guber.RateLimitReq{
					Name: "TestScheduledAdmissionNonPositiveRateNeverFits" + test.name, UniqueKey: "account:1",
					Algorithm: guber.Algorithm_LEAKY_BUCKET, Duration: test.duration, Limit: test.limit,
					Burst: 5, Hits: 5, MaxWait: 60000,
				})
				assert.NotEqual(t, guber.Status_SCHEDULED, resp.Status)
			}
			assert.Equal(t, test.final, resp.Status)
			assert.Equal(t, int64(0), resp.RetryAfter)
		})
	}
}

func TestScheduledAdmissionConcurrentCallersGetDistinctSlots(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	const callers = 20
	// Each datacenter has its own hash ring and so its own owner, so stay within one.
	var peers []guber.PeerInfo
	for _, peer := range cluster.GetPeers() {
		if peer.DataCenter == cluster.DataCenterNone {
			peers = append(peers, peer)
		}
	}
	waits := make([]int64, callers)
	var group sync.WaitGroup
	for i := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			// Callers enter through different peers, so most requests are forwarded to the owner.
			client, err := guber.DialV1Server(peers[i%len(peers)].GRPCAddress, nil)
			if !assert.NoError(t, err) {
				return
			}
			req := leaky10PerMinute("TestScheduledAdmissionConcurrentCallersGetDistinctSlots", "account:1", 1, 120000)
			req.Burst = 1
			waits[i] = sendOne(t, client, req).RetryAfter
		}()
	}
	group.Wait()

	// One caller is admitted now and the others hold consecutive slots, none shared.
	sort.Slice(waits, func(a, b int) bool { return waits[a] < waits[b] })
	for i, wait := range waits {
		assert.Equal(t, int64(i)*rate10PerMinute, wait)
	}
}

func TestScheduledAdmissionRepaidDebtIsNotDebt(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name = "TestScheduledAdmissionRepaidDebtIsNotDebt"
	resetBefore := getMetricValue(t, cluster.DaemonAt(0), metricReset)

	// 5000ms at 14 per 10s leaks 6.999999999999999 tokens, which leaves a debt of 7 about -9e-16 short of repaid.
	req := func(key string, hits, maxWait int64) *guber.RateLimitReq {
		return &guber.RateLimitReq{
			Name: name, UniqueKey: key, Algorithm: guber.Algorithm_LEAKY_BUCKET,
			Duration: 10000, Limit: 14, Burst: 14, Hits: hits, MaxWait: maxWait,
		}
	}
	for _, key := range []string{"refund", "reset"} {
		sendOne(t, client, req(key, 14, 0))
		resp := sendOne(t, client, req(key, 7, 100000))
		assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
		assert.Equal(t, int64(5000), resp.RetryAfter)
	}
	clock.Advance(5000 * time.Millisecond)

	// A refund on a repaid bucket is applied like on any bucket at zero.
	sendOne(t, client, req("refund", -1, 0))
	assert.Equal(t, int64(1), sendOne(t, client, req("refund", 0, 0)).Remaining)

	// Resetting a repaid bucket forgives nothing, so it is not counted.
	reset := req("reset", 0, 0)
	reset.Behavior = guber.Behavior_RESET_REMAINING
	sendOne(t, client, reset)
	assert.Equal(t, float64(0), getMetricValue(t, cluster.DaemonAt(0), metricReset)-resetBefore)
}

func TestScheduledAdmissionZeroLimitNeverFits(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	req := &guber.RateLimitReq{
		Name: "TestScheduledAdmissionZeroLimitNeverFits", UniqueKey: "account:1",
		Algorithm: guber.Algorithm_LEAKY_BUCKET, Duration: 10000, Limit: 0, Burst: 5, Hits: 3, MaxWait: 100000,
	}
	assert.Equal(t, guber.Status_UNDER_LIMIT, sendOne(t, client, req).Status)
	for range 2 {
		resp := sendOne(t, client, req)
		assert.Equal(t, guber.Status_NEVER_FITS, resp.Status)
		assert.Equal(t, int64(0), resp.RetryAfter)
	}
}

func TestScheduledAdmissionKeepsDebtCached(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name, key = "TestScheduledAdmissionKeepsDebtCached", "account:1"

	// 10 admitted plus 11 booked leaves a balance of -11, which takes 126000ms to repay, longer than duration.
	sendOne(t, client, leaky10PerMinute(name, key, 10, 0))
	for i := 1; i <= 11; i++ {
		resp := sendOne(t, client, leaky10PerMinute(name, key, 1, 120000))
		assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
		assert.Equal(t, int64(i*rate10PerMinute), resp.RetryAfter)
	}

	// 1ms before the bucket is full again it still holds burst-1, not the burst of a fresh bucket.
	clock.Advance(125999 * time.Millisecond)
	resp := sendOne(t, client, leaky10PerMinute(name, key, 0, 0))
	assert.Equal(t, int64(9), resp.Remaining)
}

func TestScheduledAdmissionPlainRequestsKeepWorking(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	for _, behavior := range []guber.Behavior{
		0,
		guber.Behavior_NO_BATCHING,
		guber.Behavior_DRAIN_OVER_LIMIT,
		guber.Behavior_RESET_REMAINING,
	} {
		key := "behavior:" + behavior.String()
		for i := 1; i <= 12; i++ {
			req := leaky10PerMinute("TestScheduledAdmissionPlainRequestsKeepWorking", key, 1, 0)
			req.Behavior = behavior
			resp := sendOne(t, client, req)
			assert.Equal(t, int64(0), resp.RetryAfter)
			if behavior == guber.Behavior_RESET_REMAINING {
				assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Status)
				assert.Equal(t, int64(9), resp.Remaining)
				continue
			}
			if i <= 10 {
				assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Status)
				assert.Equal(t, int64(10-i), resp.Remaining)
				continue
			}
			assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
			assert.Equal(t, int64(0), resp.Remaining)
		}
	}
}

func TestScheduledAdmissionErrors(t *testing.T) {
	forceGlobal := spawnSingleDaemon(t, guber.BehaviorConfig{ForceGlobal: true})
	clusterClient, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	for _, test := range []struct {
		name    string
		client  guber.V1Client
		mutate  func(*guber.RateLimitReq)
		wantErr string
	}{
		{
			name:    "token bucket",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.Algorithm = guber.Algorithm_TOKEN_BUCKET },
			wantErr: "max_wait is only supported by LEAKY_BUCKET",
		},
		{
			name:    "global",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.Behavior = guber.Behavior_GLOBAL },
			wantErr: "max_wait cannot be combined with GLOBAL",
		},
		{
			name:    "forced global",
			client:  forceGlobal.MustClient(),
			mutate:  func(r *guber.RateLimitReq) {},
			wantErr: "max_wait cannot be combined with GLOBAL",
		},
		{
			name:    "gregorian",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.Behavior = guber.Behavior_DURATION_IS_GREGORIAN },
			wantErr: "max_wait cannot be combined with DURATION_IS_GREGORIAN",
		},
		{
			name:    "drain over limit",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.Behavior = guber.Behavior_DRAIN_OVER_LIMIT },
			wantErr: "max_wait cannot be combined with DRAIN_OVER_LIMIT",
		},
		{
			name:    "reset remaining",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.Behavior = guber.Behavior_RESET_REMAINING },
			wantErr: "max_wait cannot be combined with RESET_REMAINING",
		},
		{
			name:    "negative hits",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.Hits = -1 },
			wantErr: "max_wait cannot be combined with negative hits",
		},
		{
			name:    "negative max_wait",
			client:  clusterClient,
			mutate:  func(r *guber.RateLimitReq) { r.MaxWait = -1 },
			wantErr: "field 'max_wait' cannot be negative",
		},
		{
			name:   "negative max_wait wins over token bucket",
			client: clusterClient,
			mutate: func(r *guber.RateLimitReq) {
				r.MaxWait = -1
				r.Algorithm = guber.Algorithm_TOKEN_BUCKET
			},
			wantErr: "field 'max_wait' cannot be negative",
		},
		{
			name:   "first row wins",
			client: clusterClient,
			mutate: func(r *guber.RateLimitReq) {
				r.Algorithm = guber.Algorithm_TOKEN_BUCKET
				r.Behavior = guber.Behavior_GLOBAL | guber.Behavior_DRAIN_OVER_LIMIT
				r.Hits = -1
			},
			wantErr: "max_wait is only supported by LEAKY_BUCKET",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer clock.Freeze(clock.Now()).Unfreeze()
			key := "errors:" + test.name
			req := leaky10PerMinute("TestScheduledAdmissionErrors", key, 1, 60000)
			test.mutate(req)

			resp := sendOne(t, test.client, req)
			assert.Equal(t, test.wantErr, resp.Error)
			assert.Equal(t, int64(0), resp.RetryAfter)

			// Nothing was charged or booked: a plain status query finds a full bucket.
			status := leaky10PerMinute("TestScheduledAdmissionErrors", key, 0, 0)
			resp = sendOne(t, test.client, status)
			assert.Empty(t, resp.Error)
			assert.Equal(t, int64(10), resp.Remaining)
		})
	}
}

func TestScheduledAdmissionSpacing(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	const (
		windowMillis = 60000
		perWindow    = 7
		maxWait      = 120000
	)
	rate := float64(windowMillis) / perWindow
	random := rand.New(rand.NewSource(42))
	start := clock.Now()
	var grants []int64
	var elapsed int64

	for i := 0; i < 300; i++ {
		clock.Advance(time.Duration(random.Intn(5000)) * time.Millisecond)
		elapsed = clock.Now().Sub(start).Milliseconds()
		req := &guber.RateLimitReq{
			Name: "TestScheduledAdmissionSpacing", UniqueKey: "account:1",
			Algorithm: guber.Algorithm_LEAKY_BUCKET, Duration: windowMillis,
			Limit: perWindow, Burst: 1, Hits: 1,
		}
		switch random.Intn(3) {
		case 0:
			req.MaxWait = maxWait
		case 1:
			req.Hits = 0
			req.MaxWait = int64(random.Intn(2)) * maxWait
		}
		resp := sendOne(t, client, req)
		switch {
		case req.Hits == 0:
			assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Status)
		case resp.Status == guber.Status_UNDER_LIMIT:
			grants = append(grants, elapsed)
		case resp.Status == guber.Status_SCHEDULED:
			grants = append(grants, elapsed+resp.RetryAfter)
		}
	}

	require.Greater(t, len(grants), 20)
	sort.Slice(grants, func(i, j int) bool { return grants[i] < grants[j] })
	for i := 1; i < len(grants); i++ {
		assert.GreaterOrEqual(t, float64(grants[i]-grants[i-1]), rate-1)
	}
	for i := range grants {
		inWindow := 0
		for _, g := range grants[i:] {
			if g >= grants[i]+windowMillis {
				break
			}
			inWindow++
		}
		assert.LessOrEqual(t, inWindow, perWindow)
	}
}

// behaviorPeerServer records the behavior flags of each request it is sent.
type behaviorPeerServer struct {
	guber.UnimplementedPeersV1Server
	mu        sync.Mutex
	behaviors []guber.Behavior
}

func (s *behaviorPeerServer) GetPeerRateLimits(_ context.Context, r *guber.GetPeerRateLimitsReq) (*guber.GetPeerRateLimitsResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &guber.GetPeerRateLimitsResp{}
	for _, req := range r.Requests {
		s.behaviors = append(s.behaviors, req.Behavior)
		resp.RateLimits = append(resp.RateLimits, &guber.RateLimitResp{Remaining: 77})
	}
	return resp, nil
}

func TestScheduledAdmissionForwardedWithoutBatching(t *testing.T) {
	owner := &behaviorPeerServer{}
	d, ownerPeer := spawnWithFakeOwner(t, owner, guber.BehaviorConfig{})
	key := keyOwnedBy(t, d, ownerPeer, "TestScheduledAdmissionForwardedWithoutBatching")
	client := d.MustClient()

	sendOne(t, client, leaky10PerMinute("TestScheduledAdmissionForwardedWithoutBatching", key, 1, 60000))
	sendOne(t, client, leaky10PerMinute("TestScheduledAdmissionForwardedWithoutBatching", key, 1, 0))

	// Only the scheduling request asks the owner to skip the batch queue.
	owner.mu.Lock()
	defer owner.mu.Unlock()
	require.Len(t, owner.behaviors, 2)
	assert.True(t, guber.HasBehavior(owner.behaviors[0], guber.Behavior_NO_BATCHING))
	assert.False(t, guber.HasBehavior(owner.behaviors[1], guber.Behavior_NO_BATCHING))
}

// stalledPeerServer accepts requests and never answers within the forwarding peer's timeout.
type stalledPeerServer struct {
	guber.UnimplementedPeersV1Server
	rpcs atomic.Int64
}

func (s *stalledPeerServer) GetPeerRateLimits(ctx context.Context, _ *guber.GetPeerRateLimitsReq) (*guber.GetPeerRateLimitsResp, error) {
	s.rpcs.Add(1)
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	return nil, status.Error(codes.Unavailable, "stalled")
}

// The retry guard in asyncRequest cannot be reached from the surface, since a request with max_wait
// is sent unbatched and no error from that path matches the retry condition. This pins the outcome.
func TestScheduledAdmissionForwardFailureIsNotRetried(t *testing.T) {
	owner := &stalledPeerServer{}
	d, ownerPeer := spawnWithFakeOwner(t, owner, guber.BehaviorConfig{BatchTimeout: 200 * time.Millisecond})
	key := keyOwnedBy(t, d, ownerPeer, "TestScheduledAdmissionForwardFailureIsNotRetried")

	resp := sendOne(t, d.MustClient(), leaky10PerMinute("TestScheduledAdmissionForwardFailureIsNotRetried", key, 1, 60000))
	assert.NotEmpty(t, resp.Error)
	require.Never(t, func() bool { return owner.rpcs.Load() > 1 }, time.Second, 50*time.Millisecond)
}

func spawnSingleDaemon(t *testing.T, behaviors guber.BehaviorConfig) *guber.Daemon {
	t.Helper()
	return spawnSingleDaemonWith(t, guber.DaemonConfig{Behaviors: behaviors})
}

func spawnSingleDaemonWithStore(t *testing.T, store guber.Store) *guber.Daemon {
	t.Helper()
	return spawnSingleDaemonWith(t, guber.DaemonConfig{Store: store})
}

func spawnSingleDaemonWith(t *testing.T, conf guber.DaemonConfig) *guber.Daemon {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conf.Logger = logrus.NewEntry(logrus.New())
	conf.InstanceID = "single"
	conf.GRPCListenAddress = "127.0.0.1:0"
	conf.HTTPListenAddress = "127.0.0.1:0"
	conf.AdvertiseAddress = "127.0.0.1:0"
	d, err := guber.SpawnDaemon(ctx, conf)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	d.PeerInfo = guber.PeerInfo{
		GRPCAddress: d.GRPCListeners[0].Addr().String(),
		HTTPAddress: d.HTTPListener.Addr().String(),
	}
	d.SetPeers([]guber.PeerInfo{d.PeerInfo})
	return d
}

type peersServer interface {
	guber.PeersV1Server
}

// spawnWithFakeOwner starts a daemon whose only other peer is owner, a server this test controls.
func spawnWithFakeOwner(t *testing.T, owner peersServer, behaviors guber.BehaviorConfig) (*guber.Daemon, guber.PeerInfo) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	guber.RegisterPeersV1Server(srv, owner)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	d := spawnSingleDaemon(t, behaviors)
	ownerPeer := guber.PeerInfo{GRPCAddress: lis.Addr().String()}
	d.SetPeers([]guber.PeerInfo{d.PeerInfo, ownerPeer})
	return d, ownerPeer
}

func keyOwnedBy(t *testing.T, d *guber.Daemon, owner guber.PeerInfo, name string) string {
	t.Helper()
	for range 5000 {
		key := guber.RandomString(10)
		peer, err := d.V1Server.GetPeer(context.Background(), name+"_"+key)
		require.NoError(t, err)
		if peer.Info().GRPCAddress == owner.GRPCAddress {
			return key
		}
	}
	require.Fail(t, "no key hashes to the fake owner")
	return ""
}

func TestScheduledAdmissionCeilingBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		ceiling time.Duration
		want    guber.Status
	}{
		{name: "wait equals ceiling", ceiling: 12000 * time.Millisecond, want: guber.Status_SCHEDULED},
		{name: "wait one millisecond over ceiling", ceiling: 11999 * time.Millisecond, want: guber.Status_OVER_LIMIT},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := spawnSingleDaemon(t, guber.BehaviorConfig{ScheduleMaxWait: test.ceiling}).MustClient()
			defer clock.Freeze(clock.Now()).Unfreeze()

			sendOne(t, client, leaky10PerMinute("TestScheduledAdmissionCeilingBoundary", "account:1", 10, 0))
			resp := sendOne(t, client, leaky10PerMinute("TestScheduledAdmissionCeilingBoundary", "account:1", 2, 120000))
			assert.Equal(t, test.want, resp.Status)
			assert.Equal(t, int64(12000), resp.RetryAfter)
		})
	}
}

func TestScheduledAdmissionThroughEveryPeer(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	const name, key = "TestScheduledAdmissionThroughEveryPeer", "account:1"

	// Whichever peer receives the request, the owner books it in arrival order.
	for i := 0; i < 6; i++ {
		client, err := guber.DialV1Server(cluster.PeerAt(i).GRPCAddress, nil)
		require.NoError(t, err)
		if i == 0 {
			sendOne(t, client, leaky10PerMinute(name, key, 10, 0))
		}
		resp := sendOne(t, client, leaky10PerMinute(name, key, 1, 120000))
		assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
		assert.Equal(t, int64((i+1)*rate10PerMinute), resp.RetryAfter)
		assert.Equal(t, int64(0), resp.Remaining)
	}
}

func TestScheduledAdmissionCombinesWithIgnoredBehaviors(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	for _, behavior := range []guber.Behavior{guber.Behavior_NO_BATCHING, guber.Behavior_MULTI_REGION} {
		key := "behavior:" + behavior.String()
		sendOne(t, client, leaky10PerMinute("TestScheduledAdmissionCombinesWithIgnoredBehaviors", key, 10, 0))
		req := leaky10PerMinute("TestScheduledAdmissionCombinesWithIgnoredBehaviors", key, 1, 60000)
		req.Behavior = behavior
		resp := sendOne(t, client, req)
		assert.Empty(t, resp.Error)
		assert.Equal(t, guber.Status_SCHEDULED, resp.Status)
		assert.Equal(t, int64(6000), resp.RetryAfter)
	}
}

func TestScheduledAdmissionPlainRequestKeepsNeverFitsBehavior(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)
	const name, key = "TestScheduledAdmissionPlainRequestKeepsNeverFitsBehavior", "account:1"

	// Without max_wait, hits above burst is a plain refusal that stores an empty bucket.
	resp := sendOne(t, client, leaky10PerMinute(name, key, 11, 0))
	assert.Equal(t, guber.Status_OVER_LIMIT, resp.Status)
	assert.Equal(t, int64(0), resp.RetryAfter)
	assert.Equal(t, int64(0), resp.Remaining)
	resp = sendOne(t, client, leaky10PerMinute(name, key, 0, 0))
	assert.Equal(t, int64(0), resp.Remaining)
}

func TestScheduledAdmissionOwnerRejectsUnvalidatedRequests(t *testing.T) {
	peer, err := guber.NewPeerClient(guber.PeerConfig{Info: cluster.PeerAt(0)})
	require.NoError(t, err)
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	for _, test := range []struct {
		name    string
		mutate  func(*guber.RateLimitReq)
		wantErr string
	}{
		{
			name:    "token bucket",
			mutate:  func(r *guber.RateLimitReq) { r.Algorithm = guber.Algorithm_TOKEN_BUCKET },
			wantErr: "max_wait is only supported by LEAKY_BUCKET",
		},
		{
			name:    "global",
			mutate:  func(r *guber.RateLimitReq) { r.Behavior = guber.Behavior_GLOBAL },
			wantErr: "max_wait cannot be combined with GLOBAL",
		},
		{
			name:    "reset remaining",
			mutate:  func(r *guber.RateLimitReq) { r.Behavior = guber.Behavior_RESET_REMAINING },
			wantErr: "max_wait cannot be combined with RESET_REMAINING",
		},
		{
			name:    "negative hits",
			mutate:  func(r *guber.RateLimitReq) { r.Hits = -1 },
			wantErr: "max_wait cannot be combined with negative hits",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer clock.Freeze(clock.Now()).Unfreeze()
			key := "owner:" + test.name
			req := leaky10PerMinute("TestScheduledAdmissionOwnerRejectsUnvalidatedRequests", key, 1, 60000)
			test.mutate(req)

			resp, err := peer.GetPeerRateLimits(context.Background(), &guber.GetPeerRateLimitsReq{
				Requests: []*guber.RateLimitReq{req},
			})
			require.NoError(t, err)
			require.Len(t, resp.RateLimits, 1)
			assert.Equal(t, test.wantErr, resp.RateLimits[0].Error)
			assert.Equal(t, int64(0), resp.RateLimits[0].RetryAfter)

			status := sendOne(t, client, leaky10PerMinute("TestScheduledAdmissionOwnerRejectsUnvalidatedRequests", key, 0, 0))
			assert.Equal(t, int64(10), status.Remaining)
		})
	}
}
