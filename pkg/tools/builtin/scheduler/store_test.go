package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreAddAndList(t *testing.T) {
	t.Parallel()

	s := newStore()
	a, err := s.add("later", "do A", "in:10m", testNow)
	require.NoError(t, err)
	b, err := s.add("sooner", "do B", "in:1m", testNow)
	require.NoError(t, err)

	require.NotEqual(t, a.ID, b.ID)

	list := s.list()
	require.Len(t, list, 2)
	require.Equal(t, b.ID, list[0].ID)
	require.Equal(t, a.ID, list[1].ID)
	require.Equal(t, testNow.Add(time.Minute), list[0].NextFire)
}

func TestStoreAddInvalidSpec(t *testing.T) {
	t.Parallel()

	s := newStore()
	_, err := s.add("bad", "x", "whenever", testNow)
	require.Error(t, err)
	require.Empty(t, s.list())
}

func TestStoreCancel(t *testing.T) {
	t.Parallel()

	s := newStore()
	sc, err := s.add("x", "do x", "hourly", testNow)
	require.NoError(t, err)

	require.False(t, s.cancel("nope"))
	require.True(t, s.cancel(sc.ID))
	require.Empty(t, s.list())
	require.False(t, s.cancel(sc.ID))
}

func TestStoreClaimDueOneShot(t *testing.T) {
	t.Parallel()

	s := newStore()
	sc, err := s.add("once", "do once", "in:10m", testNow)
	require.NoError(t, err)

	require.Empty(t, s.claimDue(testNow.Add(9*time.Minute)))
	due := s.claimDue(testNow.Add(10 * time.Minute))
	require.Len(t, due, 1)
	require.Equal(t, sc.ID, due[0].ID)
	require.Empty(t, s.claimDue(testNow.Add(10*time.Minute)))

	s.finishFire(sc.ID, testNow.Add(10*time.Minute), true)
	require.Empty(t, s.list())
}

func TestStoreFinishFireRecurringReArms(t *testing.T) {
	t.Parallel()

	s := newStore()
	sc, err := s.add("loop", "tick", "every:1h", testNow)
	require.NoError(t, err)

	due := s.claimDue(testNow.Add(time.Hour))
	require.Len(t, due, 1)
	s.finishFire(sc.ID, testNow.Add(time.Hour), true)

	require.Empty(t, s.claimDue(testNow.Add(time.Hour)))
	list := s.list()
	require.Len(t, list, 1)
	require.Equal(t, testNow.Add(2*time.Hour), list[0].NextFire)
}

func TestStoreFinishFireRecurringSkipsMissedSlots(t *testing.T) {
	t.Parallel()

	s := newStore()
	sc, err := s.add("loop", "tick", "every:1h", testNow)
	require.NoError(t, err)

	due := s.claimDue(testNow.Add(3*time.Hour + 30*time.Minute))
	require.Len(t, due, 1)
	s.finishFire(sc.ID, testNow.Add(3*time.Hour+30*time.Minute), true)
	require.Equal(t, testNow.Add(4*time.Hour), s.list()[0].NextFire)
}

func TestStoreFinishFireFailureRetriesWithoutChangingRecurringCadence(t *testing.T) {
	t.Parallel()

	s := newStore()
	sc, err := s.add("loop", "tick", "every:1h", testNow)
	require.NoError(t, err)

	due := s.claimDue(testNow.Add(time.Hour))
	require.Len(t, due, 1)
	s.finishFire(sc.ID, testNow.Add(time.Hour), false)
	require.Equal(t, testNow.Add(time.Hour+recallRetryDelay), s.list()[0].NextFire)

	retryAt := testNow.Add(time.Hour + recallRetryDelay)
	due = s.claimDue(retryAt)
	require.Len(t, due, 1)
	s.finishFire(sc.ID, retryAt, true)
	require.Equal(t, testNow.Add(2*time.Hour), s.list()[0].NextFire)
}

func TestStoreCancelWhileFiring(t *testing.T) {
	t.Parallel()

	s := newStore()
	sc, err := s.add("once", "do once", "in:10m", testNow)
	require.NoError(t, err)
	require.Len(t, s.claimDue(testNow.Add(10*time.Minute)), 1)

	require.True(t, s.cancel(sc.ID))
	s.finishFire(sc.ID, testNow.Add(10*time.Minute), false)
	require.Empty(t, s.list())
}

func TestStoreUntilNextIgnoresInFlightSchedules(t *testing.T) {
	t.Parallel()

	s := newStore()
	_, err := s.add("x", "do x", "in:5m", testNow)
	require.NoError(t, err)
	require.Len(t, s.claimDue(testNow.Add(5*time.Minute)), 1)

	_, ok := s.untilNext(testNow.Add(5 * time.Minute))
	require.False(t, ok)
}

func TestStoreUntilNext(t *testing.T) {
	t.Parallel()

	s := newStore()
	_, ok := s.untilNext(testNow)
	require.False(t, ok)

	_, err := s.add("x", "do x", "in:5m", testNow)
	require.NoError(t, err)
	d, ok := s.untilNext(testNow)
	require.True(t, ok)
	require.Equal(t, 5*time.Minute, d)

	d, ok = s.untilNext(testNow.Add(10 * time.Minute))
	require.True(t, ok)
	require.Equal(t, time.Duration(0), d)
}
