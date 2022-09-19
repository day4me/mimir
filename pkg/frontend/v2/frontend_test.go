// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/frontend/v2/frontend_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package v2

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/test"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"
	"google.golang.org/grpc"

	"github.com/grafana/mimir/pkg/frontend/v2/frontendv2pb"
	"github.com/grafana/mimir/pkg/querier/stats"
	"github.com/grafana/mimir/pkg/scheduler/schedulerdiscovery"
	"github.com/grafana/mimir/pkg/scheduler/schedulerpb"
)

const testFrontendWorkerConcurrency = 5

func setupFrontend(t *testing.T, reg prometheus.Registerer, schedulerReplyFunc func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend) (*Frontend, *mockScheduler) {
	l, err := net.Listen("tcp", "")
	require.NoError(t, err)

	server := grpc.NewServer()

	h, p, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)

	grpcPort, err := strconv.Atoi(p)
	require.NoError(t, err)

	cfg := Config{}
	flagext.DefaultValues(&cfg)
	cfg.SchedulerAddress = l.Addr().String()
	cfg.WorkerConcurrency = testFrontendWorkerConcurrency
	cfg.Addr = h
	cfg.Port = grpcPort

	//logger := log.NewLogfmtLogger(os.Stdout)
	logger := log.NewNopLogger()
	f, err := NewFrontend(cfg, logger, reg)
	require.NoError(t, err)

	frontendv2pb.RegisterFrontendForQuerierServer(server, f)

	ms := newMockScheduler(t, f, schedulerReplyFunc)
	schedulerpb.RegisterSchedulerForFrontendServer(server, ms)

	require.NoError(t, services.StartAndAwaitRunning(context.Background(), f))
	t.Cleanup(func() {
		_ = services.StopAndAwaitTerminated(context.Background(), f)
	})

	go func() {
		_ = server.Serve(l)
	}()

	t.Cleanup(func() {
		_ = l.Close()
	})

	// Wait for frontend to connect to scheduler.
	test.Poll(t, 1*time.Second, 1, func() interface{} {
		ms.mu.Lock()
		defer ms.mu.Unlock()

		return len(ms.frontendAddr)
	})

	return f, ms
}

func sendResponseWithDelay(f *Frontend, delay time.Duration, userID string, queryID uint64, resp *httpgrpc.HTTPResponse) {
	if delay > 0 {
		time.Sleep(delay)
	}

	ctx := user.InjectOrgID(context.Background(), userID)
	_, _ = f.QueryResult(ctx, &frontendv2pb.QueryResultRequest{
		QueryID:      queryID,
		HttpResponse: resp,
		Stats:        &stats.Stats{},
	})
}

func TestFrontendBasicWorkflow(t *testing.T) {
	const (
		body   = "all fine here"
		userID = "test"
	)

	f, _ := setupFrontend(t, nil, func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend {
		// We cannot call QueryResult directly, as Frontend is not yet waiting for the response.
		// It first needs to be told that enqueuing has succeeded.
		go sendResponseWithDelay(f, 100*time.Millisecond, userID, msg.QueryID, &httpgrpc.HTTPResponse{
			Code: 200,
			Body: []byte(body),
		})

		return &schedulerpb.SchedulerToFrontend{Status: schedulerpb.OK}
	})

	resp, err := f.RoundTripGRPC(user.InjectOrgID(context.Background(), userID), &httpgrpc.HTTPRequest{})
	require.NoError(t, err)
	require.Equal(t, int32(200), resp.Code)
	require.Equal(t, []byte(body), resp.Body)
}

func TestFrontendRequestsPerWorkerMetric(t *testing.T) {
	const (
		body   = "all fine here"
		userID = "test"
	)

	reg := prometheus.NewRegistry()

	f, _ := setupFrontend(t, reg, func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend {
		// We cannot call QueryResult directly, as Frontend is not yet waiting for the response.
		// It first needs to be told that enqueuing has succeeded.
		go sendResponseWithDelay(f, 100*time.Millisecond, userID, msg.QueryID, &httpgrpc.HTTPResponse{
			Code: 200,
			Body: []byte(body),
		})

		return &schedulerpb.SchedulerToFrontend{Status: schedulerpb.OK}
	})

	expectedMetrics := fmt.Sprintf(`
		# HELP cortex_query_frontend_workers_enqueued_requests_total Total number of requests enqueued by each query frontend worker (regardless of the result), labeled by scheduler address.
		# TYPE cortex_query_frontend_workers_enqueued_requests_total counter
		cortex_query_frontend_workers_enqueued_requests_total{scheduler_address="%s"} 0
	`, f.cfg.SchedulerAddress)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expectedMetrics), "cortex_query_frontend_workers_enqueued_requests_total"))

	resp, err := f.RoundTripGRPC(user.InjectOrgID(context.Background(), userID), &httpgrpc.HTTPRequest{})
	require.NoError(t, err)
	require.Equal(t, int32(200), resp.Code)
	require.Equal(t, []byte(body), resp.Body)

	expectedMetrics = fmt.Sprintf(`
		# HELP cortex_query_frontend_workers_enqueued_requests_total Total number of requests enqueued by each query frontend worker (regardless of the result), labeled by scheduler address.
		# TYPE cortex_query_frontend_workers_enqueued_requests_total counter
		cortex_query_frontend_workers_enqueued_requests_total{scheduler_address="%s"} 1
	`, f.cfg.SchedulerAddress)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expectedMetrics), "cortex_query_frontend_workers_enqueued_requests_total"))

	// Manually remove the address, check that label is removed.
	f.schedulerWorkers.AddressRemoved(f.cfg.SchedulerAddress)
	expectedMetrics = ``
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expectedMetrics), "cortex_query_frontend_workers_enqueued_requests_total"))
}

func TestFrontendRetryEnqueue(t *testing.T) {
	// Frontend uses worker concurrency to compute number of retries. We use one less failure.
	failures := atomic.NewInt64(testFrontendWorkerConcurrency - 1)
	const (
		body   = "hello world"
		userID = "test"
	)

	f, _ := setupFrontend(t, nil, func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend {
		fail := failures.Dec()
		if fail >= 0 {
			return &schedulerpb.SchedulerToFrontend{Status: schedulerpb.SHUTTING_DOWN}
		}

		go sendResponseWithDelay(f, 100*time.Millisecond, userID, msg.QueryID, &httpgrpc.HTTPResponse{
			Code: 200,
			Body: []byte(body),
		})

		return &schedulerpb.SchedulerToFrontend{Status: schedulerpb.OK}
	})
	_, err := f.RoundTripGRPC(user.InjectOrgID(context.Background(), userID), &httpgrpc.HTTPRequest{})
	require.NoError(t, err)
}

func TestFrontendTooManyRequests(t *testing.T) {
	f, _ := setupFrontend(t, nil, func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend {
		return &schedulerpb.SchedulerToFrontend{Status: schedulerpb.TOO_MANY_REQUESTS_PER_TENANT}
	})

	resp, err := f.RoundTripGRPC(user.InjectOrgID(context.Background(), "test"), &httpgrpc.HTTPRequest{})
	require.NoError(t, err)
	require.Equal(t, int32(http.StatusTooManyRequests), resp.Code)
}

func TestFrontendEnqueueFailure(t *testing.T) {
	f, _ := setupFrontend(t, nil, func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend {
		return &schedulerpb.SchedulerToFrontend{Status: schedulerpb.SHUTTING_DOWN}
	})

	_, err := f.RoundTripGRPC(user.InjectOrgID(context.Background(), "test"), &httpgrpc.HTTPRequest{})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "failed to enqueue request"))
}

func TestFrontendCancellation(t *testing.T) {
	f, ms := setupFrontend(t, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	resp, err := f.RoundTripGRPC(user.InjectOrgID(ctx, "test"), &httpgrpc.HTTPRequest{})
	require.EqualError(t, err, context.DeadlineExceeded.Error())
	require.Nil(t, resp)

	// We wait a bit to make sure scheduler receives the cancellation request.
	test.Poll(t, time.Second, 2, func() interface{} {
		ms.mu.Lock()
		defer ms.mu.Unlock()

		return len(ms.msgs)
	})

	ms.checkWithLock(func() {
		require.Equal(t, 2, len(ms.msgs))
		require.True(t, ms.msgs[0].Type == schedulerpb.ENQUEUE)
		require.True(t, ms.msgs[1].Type == schedulerpb.CANCEL)
		require.True(t, ms.msgs[0].QueryID == ms.msgs[1].QueryID)
	})
}

// When frontendWorker that processed the request is busy (processing a new request or cancelling a previous one)
// we still need to make sure that the cancellation reach the scheduler at some point.
// Issue: https://github.com/grafana/mimir/issues/740
func TestFrontendWorkerCancellation(t *testing.T) {
	f, ms := setupFrontend(t, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// send multiple requests > maxconcurrency of scheduler. So that it keeps all the frontend worker busy in serving requests.
	reqCount := testFrontendWorkerConcurrency + 5
	var wg sync.WaitGroup
	for i := 0; i < reqCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := f.RoundTripGRPC(user.InjectOrgID(ctx, "test"), &httpgrpc.HTTPRequest{})
			require.EqualError(t, err, context.DeadlineExceeded.Error())
			require.Nil(t, resp)
		}()
	}

	wg.Wait()

	// We wait a bit to make sure scheduler receives the cancellation request.
	// 2 * reqCount because for every request, should also be corresponding cancel request
	test.Poll(t, 5*time.Second, 2*reqCount, func() interface{} {
		ms.mu.Lock()
		defer ms.mu.Unlock()

		return len(ms.msgs)
	})

	ms.checkWithLock(func() {
		require.Equal(t, 2*reqCount, len(ms.msgs))
		msgTypeCounts := map[schedulerpb.FrontendToSchedulerType]int{}
		for _, msg := range ms.msgs {
			msgTypeCounts[msg.Type]++
		}
		expectedMsgTypeCounts := map[schedulerpb.FrontendToSchedulerType]int{
			schedulerpb.ENQUEUE: reqCount,
			schedulerpb.CANCEL:  reqCount,
		}
		require.Equalf(t, expectedMsgTypeCounts, msgTypeCounts,
			"Should receive %d enqueue (%d) requests, and %d cancel (%d) requests.", reqCount, schedulerpb.ENQUEUE, reqCount, schedulerpb.CANCEL,
		)
	})
}

func TestFrontendFailedCancellation(t *testing.T) {
	f, ms := setupFrontend(t, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)

		// stop scheduler workers
		addr := ""
		f.schedulerWorkers.mu.Lock()
		for k := range f.schedulerWorkers.workers {
			addr = k
			break
		}
		f.schedulerWorkers.mu.Unlock()

		f.schedulerWorkers.AddressRemoved(addr)

		// Wait for worker goroutines to stop.
		time.Sleep(100 * time.Millisecond)

		// Cancel request. Frontend will try to send cancellation to scheduler, but that will fail (not visible to user).
		// Everything else should still work fine.
		cancel()
	}()

	// send request
	resp, err := f.RoundTripGRPC(user.InjectOrgID(ctx, "test"), &httpgrpc.HTTPRequest{})
	require.EqualError(t, err, context.Canceled.Error())
	require.Nil(t, resp)

	ms.checkWithLock(func() {
		require.Equal(t, 1, len(ms.msgs))
	})
}

type mockScheduler struct {
	t *testing.T
	f *Frontend

	replyFunc func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend

	mu           sync.Mutex
	frontendAddr map[string]int
	msgs         []*schedulerpb.FrontendToScheduler
}

func newMockScheduler(t *testing.T, f *Frontend, replyFunc func(f *Frontend, msg *schedulerpb.FrontendToScheduler) *schedulerpb.SchedulerToFrontend) *mockScheduler {
	return &mockScheduler{t: t, f: f, frontendAddr: map[string]int{}, replyFunc: replyFunc}
}

func (m *mockScheduler) checkWithLock(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fn()
}

func (m *mockScheduler) FrontendLoop(frontend schedulerpb.SchedulerForFrontend_FrontendLoopServer) error {
	init, err := frontend.Recv()
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.frontendAddr[init.FrontendAddress]++
	m.mu.Unlock()

	// Ack INIT from frontend.
	if err := frontend.Send(&schedulerpb.SchedulerToFrontend{Status: schedulerpb.OK}); err != nil {
		return err
	}

	for {
		msg, err := frontend.Recv()
		if err != nil {
			return err
		}

		m.mu.Lock()
		m.msgs = append(m.msgs, msg)
		m.mu.Unlock()

		reply := &schedulerpb.SchedulerToFrontend{Status: schedulerpb.OK}
		if m.replyFunc != nil {
			reply = m.replyFunc(m.f, msg)
		}

		if err := frontend.Send(reply); err != nil {
			return err
		}
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := map[string]struct {
		setup       func(cfg *Config)
		expectedErr string
	}{
		"should pass with default config": {
			setup: func(cfg *Config) {},
		},
		"should pass if scheduler address is configured, and query-scheduler discovery mode is the default one": {
			setup: func(cfg *Config) {
				cfg.SchedulerAddress = "localhost:9095"
			},
		},
		"should fail if query-scheduler service discovery is set to ring, and scheduler address is configured": {
			setup: func(cfg *Config) {
				cfg.QuerySchedulerDiscovery.Mode = schedulerdiscovery.ModeRing
				cfg.SchedulerAddress = "localhost:9095"
			},
			expectedErr: `scheduler address cannot be specified when query-scheduler service discovery mode is set to 'ring'`,
		},
	}

	for testName, testData := range tests {
		t.Run(testName, func(t *testing.T) {
			cfg := Config{}
			flagext.DefaultValues(&cfg)
			testData.setup(&cfg)

			actualErr := cfg.Validate(log.NewNopLogger())
			if testData.expectedErr == "" {
				require.NoError(t, actualErr)
			} else {
				require.Error(t, actualErr)
				assert.ErrorContains(t, actualErr, testData.expectedErr)
			}
		})
	}
}
