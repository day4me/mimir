// Included-from-location: https://github.com/thanos-io/thanos/blob/main/pkg/store/limiter_test.go
// Included-from-license: Apache-2.0
// Included-from-copyright: The Thanos Authors.

package store

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	prom_testutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestLimiter(t *testing.T) {
	c := promauto.With(nil).NewCounter(prometheus.CounterOpts{})
	l := NewLimiter(10, c)

	assert.NoError(t, l.Reserve(5))
	assert.Equal(t, float64(0), prom_testutil.ToFloat64(c))

	assert.NoError(t, l.Reserve(5))
	assert.Equal(t, float64(0), prom_testutil.ToFloat64(c))

	assert.Error(t, l.Reserve(1))
	assert.Equal(t, float64(1), prom_testutil.ToFloat64(c))

	assert.Error(t, l.Reserve(2))
	assert.Equal(t, float64(1), prom_testutil.ToFloat64(c))
}
