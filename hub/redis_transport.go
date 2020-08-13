package hub

import (
	"encoding/json"
	"fmt"
	"github.com/go-redis/redis"
	log "github.com/sirupsen/logrus"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const defaultRedisStreamName = "mercure-hub-updates"

func redisNilToNil(err error) error {
	if err == redis.Nil {
		return nil
	}
	return err
}

// RedisTransport implements the TransportInterface using the Redis database.
type RedisTransport struct {
	sync.RWMutex
	client           *redis.Client
	streamName       string
	size             uint64
	subscribers      map[*Subscriber]struct{}
	closed           chan struct{}
	closedOnce       sync.Once
	lastSeq          string
	lastEventID      string
}

// NewRedisTransport create a new RedisTransport.
func NewRedisTransport(u *url.URL) (*RedisTransport, error) {
	var err error
	q := u.Query()
	streamName := defaultRedisStreamName
	if q.Get("stream_name") != "" {
		streamName = q.Get("stream_name")
		q.Del("stream_name")
	}

	masterName := ""
	if q.Get("master_name") != "" {
		masterName = q.Get("master_name")
		q.Del("stream_name")
	}

	size := uint64(0)
	sizeParameter := q.Get("size")
	if sizeParameter != "" {
		size, err = strconv.ParseUint(sizeParameter, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(`%q: invalid "size" parameter %q: %s: %w`, u, sizeParameter, err, ErrInvalidTransportDSN)
		}
		q.Del("size")
	}

	u.RawQuery = q.Encode()

	redisOptions, err := redis.ParseURL(u.String())
	if err != nil {
		return nil, fmt.Errorf(`%q: invalid "redis" dsn %q: %w`, u, u.String(), ErrInvalidTransportDSN)
	}

	var client *redis.Client
	if masterName != "" {
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    masterName,
			DB:            redisOptions.DB,
			Password:      redisOptions.Password,
			SentinelAddrs: []string{redisOptions.Addr},
		})
	} else {
		client = redis.NewClient(redisOptions)
	}

	if _, err := client.Ping().Result(); err != nil {
		return nil, fmt.Errorf(`%q: redis connection error "%s": %w`, u, err, ErrInvalidTransportDSN)
	}

	transport := &RedisTransport{
		client:           client,
		streamName:       streamName,
		size:             size,
		subscribers:      make(map[*Subscriber]struct{}),
		closed:           make(chan struct{}),
		lastEventID:      getLastEventId(client, streamName),
	}

	go transport.SubscribeToMessageStream()
	return transport, nil
}

// cacheKeyID provides a unique cache identifier for the given ID
func (t *RedisTransport) cacheKeyID(ID string) string {
	return fmt.Sprintf("%s/%s", t.streamName, ID)
}

func getLastEventId(client *redis.Client, streamName string) string {
	lastEventID := EarliestLastEventID
	messages, err := client.XRevRangeN(streamName, "+", "-", 1).Result()
	if err != nil {
		return lastEventID
	}

	for _, entry := range messages {
		lastEventID = entry.ID
	}

	return lastEventID
}

// Dispatch dispatches an update to all subscribers and persists it in RedisDB.
func (t *RedisTransport) Dispatch(update *Update) error {
	select {
	case <-t.closed:
		return ErrClosedTransport
	default:
	}

	AssignUUID(update)
	updateJSON, err := json.Marshal(*update)
	if err != nil {
		return err
	}

	t.Lock()
	defer t.Unlock()
	if err := t.persist(update.ID, updateJSON); err != nil {
		return err
	}

	for subscriber := range t.subscribers {
		if !subscriber.Dispatch(update, false) {
			delete(t.subscribers, subscriber)
		}
	}
	t.lastEventID = update.ID
	return nil
}

// persist stores update in the database.
func (t *RedisTransport) persist(updateID string, updateJSON []byte) error {
	var script string
	if t.size > 0 {
		// Script Explanation
		// Convert the <Arg:History Size> into a number
		// Add to <Key:Stream Name> using Auto-Generated Entry ID, Limiting the length to <Arg:History Size> add an entry with the data key set to <Arg:Update JSON> and return <res:Entry ID>
		// Add to the end of the <Key:cacheKeyId(updateID)> List the <res:Entry ID>
		// Add to the end of the <Key:cacheKeyId("") List the <Key:cacheKeyId(updateID)>
		// While the length of the <Key:cacheKeyId("")> List is over <Arg:History Size>
		//  - Get the first key in the list
		//  - Remove it from the list
		//  - If the length of that list is 0
		//     - Delete that key

		script = `
			local limit = tonumber(ARGV[1])
			local entryId = redis.call("XADD", KEYS[1], "*", "MAXLEN", ARGV[1], "data", ARGV[2])
			redis.call("RPUSH", KEYS[2], entryId)
			redis.call("RPUSH", KEYS[3], KEYS[2])
			while (redis.call("LLEN", KEYS[3]) > limit) do
				local key = redis.call("LPOP", KEYS[3])
				redis.call("LPOP", key)
				if redis.call("LLEN", key) == 0 then
					redis.call("DEL", key)
				end
			end`
	} else {
		script = `
			local streamID = redis.call("XADD", KEYS[1], "*", "data", ARGV[2])
			redis.call("RPUSH", KEYS[2], streamID)`
	}

	if err := t.client.Eval(script, []string{t.streamName, t.cacheKeyID(updateID), t.cacheKeyID("")}, t.size, updateJSON).Err(); err != nil {
		return redisNilToNil(err)
	}
	return nil
}

// AddSubscriber adds a new subscriber to the transport.
func (t *RedisTransport) AddSubscriber(s *Subscriber) error {
	select {
	case <-t.closed:
		return ErrClosedTransport
	default:
	}

	t.Lock()
	t.subscribers[s] = struct{}{}
	toSeq := t.lastSeq
	t.Unlock()

	if s.RequestLastEventID != "" {
		t.dispatchHistory(s, toSeq)
	}
	return nil
}

// GetSubscribers get the list of active subscribers.
func (t *RedisTransport) GetSubscribers() (lastEventID string, subscribers []*Subscriber) {
	t.RLock()
	defer t.RUnlock()
	subscribers = make([]*Subscriber, len(t.subscribers))

	i := 0
	for subscriber := range t.subscribers {
		subscribers[i] = subscriber
		i++
	}

	return t.lastEventID, subscribers
}

func (t *RedisTransport) dispatchHistory(s *Subscriber, toSeq string) {
	if toSeq == "" {
		toSeq = "+"
	}

	fromSeq := s.RequestLastEventID
	if fromSeq != EarliestLastEventID {
		var err error
		fromSeq, err = t.client.LIndex(fromSeq, 0).Result()
		if err != nil {
			log.Error(fmt.Errorf("[Redis] Dispatch History List Index Error: %w\n", err))
			s.HistoryDispatched(EarliestLastEventID)
			return // No data
		}
	} else {
		fromSeq = "-"
	}


	messages, err := t.client.XRange(t.streamName, fromSeq, toSeq).Result()
	if err != nil {
		log.Error(fmt.Errorf("[Redis] XRange error: %w", err))
		s.HistoryDispatched(EarliestLastEventID)
		return
	}

	responseLastEventID := fromSeq
	for _, entry := range messages {
		message, ok := entry.Values["data"]
		if !ok {
			log.Error(fmt.Errorf("[Redis] Read History Entry Error\n"))
			s.HistoryDispatched(responseLastEventID)
			break
		}

		var update *Update
		if err := json.Unmarshal([]byte(fmt.Sprintf("%v", message)), &update); err != nil {
			log.Error(fmt.Errorf(`[Redis] stream return an invalid entry: %v\n`, err))
			s.HistoryDispatched(responseLastEventID)
			break
		}

		if !s.Dispatch(update, true) {
			log.Error(fmt.Errorf("[Redis] Dispatch error\n"))
			s.HistoryDispatched(responseLastEventID)
			break
		}
		s.HistoryDispatched(responseLastEventID)
	}
}

// Close closes the Transport.
func (t *RedisTransport) Close() (err error) {
	select {
	case <-t.closed:
		// Already closed. Don't close again.
	default:
		t.closedOnce.Do(func() {
			t.Lock()
			defer t.Unlock()
			close(t.closed)
			for subscriber := range t.subscribers {
				subscriber.Disconnect()
				delete(t.subscribers, subscriber)
			}
		})
	}
	return nil
}

func (t *RedisTransport) SubscribeToMessageStream() {
	streamArgs := &redis.XReadArgs{
		Streams: []string{t.streamName, "$"},
		Count:   1,
		Block:   0,
	}

	for {
		select {
		case <-t.closed:
			break
		default:
		}

		streams, err := t.client.XRead(streamArgs).Result()
		if err != nil {
			log.Error(fmt.Errorf("[Redis] XREAD error: %w", err))
			continue
		}

		stream := streams[0]
		for _, entry := range stream.Messages {
			message, ok := entry.Values["data"]
			if !ok {
				log.Error(fmt.Errorf("[Redis] Read History Entry Error\n"))
				continue
			}

			var update *Update
			if err := json.Unmarshal([]byte(fmt.Sprintf("%v", message)), &update); err != nil {
				log.Error(fmt.Errorf(`[Redis] stream return an invalid entry: %v\n`, err))
				continue
			}

			if err := t.Dispatch(update); err != nil {
				log.Error(fmt.Errorf("[Redis] Dispatch error\n"))
				continue
			}
			streamArgs.Streams[1] = entry.ID
		}

		time.Sleep(1 * time.Millisecond) // avoid infinite loop consuming CPU
	}
}