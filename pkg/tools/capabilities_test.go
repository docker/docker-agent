package tools_test

import (
	"sync"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestSubscribers_FanOutAndUnsubscribe(t *testing.T) {
	t.Parallel()

	var subs tools.Subscribers[int]
	var got1, got2 []int
	unsub1 := subs.Subscribe(func(v int) { got1 = append(got1, v) })
	unsub2 := subs.Subscribe(func(v int) { got2 = append(got2, v) })

	subs.Notify(1)
	unsub1()
	unsub1() // idempotent
	subs.Notify(2)
	unsub2()
	subs.Notify(3)

	assert.DeepEqual(t, got1, []int{1})
	assert.DeepEqual(t, got2, []int{1, 2})
}

func TestSubscribers_NilCallbackIsNoop(t *testing.T) {
	t.Parallel()

	var subs tools.Subscribers[int]
	unsub := subs.Subscribe(nil)
	subs.Notify(1)
	unsub()
}

// A callback that unsubscribes itself (or subscribes another) during Notify
// must not deadlock the registry.
func TestSubscribers_ReentrantFromCallback(t *testing.T) {
	t.Parallel()

	var subs tools.Subscribers[struct{}]
	calls := 0
	var unsub func()
	unsub = subs.Subscribe(func(struct{}) {
		calls++
		unsub()
		subs.Subscribe(func(struct{}) { calls += 10 })
	})

	subs.Notify(struct{}{})
	subs.Notify(struct{}{})
	assert.Equal(t, calls, 11)
}

func TestSubscribers_ConcurrentSubscribeNotify(t *testing.T) {
	t.Parallel()

	var subs tools.Subscribers[int]
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { subs.Subscribe(func(int) {})() })
		wg.Go(func() { subs.Notify(1) })
	}
	wg.Wait()
}

func TestChangeSubscribers(t *testing.T) {
	t.Parallel()

	var subs tools.ChangeSubscribers
	calls := 0
	unsub := subs.Subscribe(func() { calls++ })
	noop := subs.Subscribe(nil)

	subs.Notify()
	unsub()
	noop()
	subs.Notify()
	assert.Check(t, is.Equal(calls, 1))
}
