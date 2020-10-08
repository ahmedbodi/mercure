package hub

import (
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedisTransportHistory(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	transport.client.FlushDB()
	defer transport.Close()

	topics := []string{"https://example.com/foo"}
	for i := 1; i <= 10; i++ {
		transport.Dispatch(&Update{
			Event:  Event{ID: strconv.Itoa(i)},
			Topics: topics,
		})
	}

	s := NewSubscriber("8", NewTopicSelectorStore())
	s.Topics = topics
	go s.start()

	require.Nil(t, transport.AddSubscriber(s))

	var count int
	for {
		u := <-s.Receive()
		// the reading loop must read the #9 and #10 messages
		assert.Equal(t, strconv.Itoa(9+count), u.ID)
		count++
		if count == 2 {
			return
		}
	}
}

func TestRedisTransportRetrieveAllHistory(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	transport.client.FlushDB()
	defer transport.Close()

	topics := []string{"https://example.com/foo"}
	for i := 1; i <= 10; i++ {
		transport.Dispatch(&Update{
			Event:  Event{ID: strconv.Itoa(i)},
			Topics: topics,
		})
	}

	s := NewSubscriber(EarliestLastEventID, NewTopicSelectorStore())
	s.Topics = topics
	go s.start()
	require.Nil(t, transport.AddSubscriber(s))

	var count int
	for {
		u := <-s.Receive()
		// the reading loop must read all messages
		count++
		assert.Equal(t, strconv.Itoa(count), u.ID)
		if count == 10 {
			return
		}
	}
}

func TestRedisTransportHistoryAndLive(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	transport.client.FlushDB()
	defer transport.Close()

	topics := []string{"https://example.com/foo"}
	for i := 1; i <= 10; i++ {
		transport.Dispatch(&Update{
			Topics: topics,
			Event:  Event{ID: strconv.Itoa(i)},
		})
	}

	s := NewSubscriber("8", NewTopicSelectorStore())
	s.Topics = topics
	go s.start()
	require.Nil(t, transport.AddSubscriber(s))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var count int
		for {
			u := <-s.Receive()

			// the reading loop must read the #9, #10 and #11 messages
			fmt.Printf("Got Entry: ID: %s\n", u.ID)
			assert.Equal(t, strconv.Itoa(9+count), u.ID)
			count++
			if count == 3 {
				return
			}
		}
	}()

	transport.Dispatch(&Update{
		Event:  Event{ID: "11"},
		Topics: topics,
	})

	wg.Wait()
}

func TestRedisTransportPurgeHistory(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9?size=5")
	transport, _ := NewRedisTransport(u)
	transport.client.FlushDB()
	defer transport.Close()

	for i := 0; i < 12; i++ {
		transport.Dispatch(&Update{
			Event:  Event{ID: strconv.Itoa(i)},
			Topics: []string{"https://example.com/foo"},
		})
	}

	length, _ := transport.client.LLen(transport.cacheKeyID("")).Result()
	assert.Equal(t, int64(5), length)
}

func TestNewRedisTransport(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9?stream_name=demo")
	transport, err := NewRedisTransport(u)
	assert.Nil(t, err)
	require.NotNil(t, transport)
	transport.Close()

	u, _ = url.Parse("redis://")
	_, _ = NewRedisTransport(u)

	u, _ = url.Parse("redis://localhost:6379")
	_, _ = NewRedisTransport(u)

	u, _ = url.Parse("redis://localhost:6379/9?size=invalid")
	_, err = NewRedisTransport(u)
	assert.EqualError(t, err, `"redis://localhost:6379/9?size=invalid": invalid "size" parameter "invalid": strconv.ParseInt: parsing "invalid": invalid syntax: invalid transport DSN\n`)
}

func TestRedisTransportDoNotDispatchedUntilListen(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	s := NewSubscriber("", NewTopicSelectorStore())
	go s.start()
	require.Nil(t, transport.AddSubscriber(s))

	var (
		readUpdate *Update
		ok         bool
		wg         sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		select {
		case readUpdate = <-s.Receive():
		case <-s.disconnected:
			ok = true
		}

		wg.Done()
	}()

	s.Disconnect()

	wg.Wait()
	assert.Nil(t, readUpdate)
	assert.True(t, ok)
}

func TestRedisTransportDispatch(t *testing.T) {
	ur, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(ur)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	s := NewSubscriber("", NewTopicSelectorStore())
	s.Topics = []string{"https://example.com/foo"}
	go s.start()

	require.Nil(t, transport.AddSubscriber(s))
	time.Sleep(1 * time.Millisecond)

	u := &Update{Topics: s.Topics}
	require.Nil(t, transport.Dispatch(u))
	assert.Equal(t, u, <-s.Receive())
}

func TestRedisTransportClosed(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	require.NotNil(t, transport)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	s := NewSubscriber("", NewTopicSelectorStore())
	s.Topics = []string{"https://example.com/foo"}
	go s.start()
	require.Nil(t, transport.AddSubscriber(s))

	require.Nil(t, transport.Close())
	require.NotNil(t, transport.AddSubscriber(s))

	assert.Equal(t, transport.Dispatch(&Update{Topics: s.Topics}), ErrClosedTransport)

	_, ok := <-s.disconnected
	assert.False(t, ok)
}

func TestRedisCleanDisconnectedSubscribers(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	require.NotNil(t, transport)
	defer transport.Close()

	tss := NewTopicSelectorStore()

	s1 := NewSubscriber("", tss)
	go s1.start()
	require.Nil(t, transport.AddSubscriber(s1))

	s2 := NewSubscriber("", tss)
	go s2.start()
	require.Nil(t, transport.AddSubscriber(s2))

	assert.Len(t, transport.subscribers, 2)

	s1.Disconnect()
	assert.Len(t, transport.subscribers, 2)

	transport.Dispatch(&Update{Topics: s1.Topics})
	time.Sleep(1 * time.Millisecond)
	assert.Len(t, transport.subscribers, 1)

	s2.Disconnect()
	assert.Len(t, transport.subscribers, 1)

	transport.Dispatch(&Update{})
	time.Sleep(1 * time.Millisecond)
	assert.Len(t, transport.subscribers, 0)
}

func TestRedisGetSubscribers(t *testing.T) {
	u, _ := url.Parse("redis://localhost:6379/9")
	transport, _ := NewRedisTransport(u)
	transport.client.FlushDB()
	require.NotNil(t, transport)
	defer transport.Close()

	tss := NewTopicSelectorStore()

	s1 := NewSubscriber("", tss)
	go s1.start()
	require.Nil(t, transport.AddSubscriber(s1))

	s2 := NewSubscriber("", tss)
	go s2.start()
	require.Nil(t, transport.AddSubscriber(s2))

	_, subscribers := transport.GetSubscribers()
	assert.Len(t, subscribers, 2)
	assert.Contains(t, subscribers, s1)
	assert.Contains(t, subscribers, s2)
}
