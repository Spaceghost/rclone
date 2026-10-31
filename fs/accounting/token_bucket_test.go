package accounting

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestLimitBandwidthContextCancellation(t *testing.T) {
	limiter := rate.NewLimiter(rate.Every(time.Hour), 1)
	require.True(t, limiter.Allow())
	tb := tokenBucket{}
	tb.curr[TokenBucketSlotTransportRx] = limiter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- tb.LimitBandwidthContext(ctx, TokenBucketSlotTransportRx, 1) }()
	require.Eventually(t, func() bool { return limiter.Tokens() < 0 }, time.Second, time.Millisecond,
		"the waiter must reserve bandwidth before cancellation")
	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceling the operation did not interrupt its bandwidth wait")
	}
}

func TestSetBwLimitDoesNotWaitForTransfer(t *testing.T) {
	limiter := rate.NewLimiter(rate.Every(time.Hour), 10)
	require.True(t, limiter.AllowN(time.Now(), 10))
	tb := tokenBucket{}
	tb.curr[TokenBucketSlotTransportRx] = limiter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- tb.LimitBandwidthContext(ctx, TokenBucketSlotTransportRx, 10) }()
	require.Eventually(t, func() bool { return limiter.Tokens() < 0 }, time.Second, time.Millisecond,
		"the transfer must be waiting before its limit changes")
	updated := make(chan struct{})
	go func() {
		tb.SetBwLimit(fs.BwPair{})
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("a pending transfer blocked the bandwidth setting update")
	}
	assert.False(t, tb.TransportBandwidthLimited())
	require.NoError(t, tb.LimitBandwidthContext(context.Background(), TokenBucketSlotTransportRx, 10),
		"the new limit must apply to subsequent waits")
	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("the original wait did not remain cancellable after the setting changed")
	}
}

func TestTransportBandwidthLimited(t *testing.T) {
	for _, test := range []struct {
		name  string
		slots []TokenBucketSlot
		want  bool
	}{
		{name: "off"},
		{name: "receive", slots: []TokenBucketSlot{TokenBucketSlotTransportRx}, want: true},
		{name: "transmit", slots: []TokenBucketSlot{TokenBucketSlotTransportTx}, want: true},
		{name: "both", slots: []TokenBucketSlot{TokenBucketSlotTransportRx, TokenBucketSlotTransportTx}, want: true},
		{name: "accounting only", slots: []TokenBucketSlot{TokenBucketSlotAccounting}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tb := tokenBucket{}
			for _, slot := range test.slots {
				tb.curr[slot] = rate.NewLimiter(1, 1)
			}
			assert.Equal(t, test.want, tb.TransportBandwidthLimited())
		})
	}
}

func TestTokenBucketBurstScalesLargeBandwidthWithoutOverflow(t *testing.T) {
	bandwidth := 4 * fs.Tebi
	want := bandwidth / tokenBucketBurstScale
	if want > fs.SizeSuffix(math.MaxInt) {
		want = fs.SizeSuffix(math.MaxInt)
	}

	tb := newEmptyTokenBucket(bandwidth)
	require.NotNil(t, tb)
	assert.Equal(t, rate.Limit(bandwidth), tb.Limit())
	assert.Equal(t, int(want), tb.Burst())
}

func TestTokenBucketBurstCapsAtMaxInt(t *testing.T) {
	want := fs.SizeSuffix(fs.SizeSuffixMaxValue / tokenBucketBurstScale)
	if want > fs.SizeSuffix(math.MaxInt) {
		want = fs.SizeSuffix(math.MaxInt)
	}

	assert.Equal(t, int(want), tokenBucketBurst(fs.SizeSuffixMaxValue))
}

func TestRcBwLimit(t *testing.T) {
	call := rc.Calls.Get("core/bwlimit")
	assert.NotNil(t, call)

	// Set
	in := rc.Params{
		"rate": "1M",
	}
	out, err := call.Fn(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, rc.Params{
		"bytesPerSecond":   int64(1048576),
		"bytesPerSecondTx": int64(1048576),
		"bytesPerSecondRx": int64(1048576),
		"rate":             "1Mi",
	}, out)
	assert.Equal(t, rate.Limit(1048576), TokenBucket.curr[0].Limit())

	// Query
	in = rc.Params{}
	out, err = call.Fn(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, rc.Params{
		"bytesPerSecond":   int64(1048576),
		"bytesPerSecondTx": int64(1048576),
		"bytesPerSecondRx": int64(1048576),
		"rate":             "1Mi",
	}, out)

	// Set
	in = rc.Params{
		"rate": "10M:1M",
	}
	out, err = call.Fn(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, rc.Params{
		"bytesPerSecond":   int64(10485760),
		"bytesPerSecondTx": int64(10485760),
		"bytesPerSecondRx": int64(1048576),
		"rate":             "10Mi:1Mi",
	}, out)
	assert.Equal(t, rate.Limit(10485760), TokenBucket.curr[0].Limit())

	// Query
	in = rc.Params{}
	out, err = call.Fn(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, rc.Params{
		"bytesPerSecond":   int64(10485760),
		"bytesPerSecondTx": int64(10485760),
		"bytesPerSecondRx": int64(1048576),
		"rate":             "10Mi:1Mi",
	}, out)

	// Reset
	in = rc.Params{
		"rate": "off",
	}
	out, err = call.Fn(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, rc.Params{
		"bytesPerSecond":   int64(-1),
		"bytesPerSecondTx": int64(-1),
		"bytesPerSecondRx": int64(-1),
		"rate":             "off",
	}, out)
	assert.Nil(t, TokenBucket.curr[0])

	// Query
	in = rc.Params{}
	out, err = call.Fn(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, rc.Params{
		"bytesPerSecond":   int64(-1),
		"bytesPerSecondTx": int64(-1),
		"bytesPerSecondRx": int64(-1),
		"rate":             "off",
	}, out)

}
