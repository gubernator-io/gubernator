package gubernator_test

import (
	"testing"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/mailgun/holster/v4/clock"
	"github.com/stretchr/testify/assert"
)

func TestFromTimeStamp(t *testing.T) {
	clock.Freeze(clock.Now().Truncate(clock.Millisecond))
	defer clock.Unfreeze()

	for _, test := range []struct {
		name string
		ts   int64
		want clock.Duration
	}{
		{name: "FutureResetTime", ts: clock.Now().Add(30 * clock.Second).UnixMilli(), want: 30 * clock.Second},
		{name: "PastResetTime", ts: clock.Now().Add(-5 * clock.Second).UnixMilli(), want: -5 * clock.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, gubernator.FromTimeStamp(test.ts))
		})
	}
}
