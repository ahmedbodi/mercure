package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"

	"github.com/go-redis/redis"
	log "github.com/sirupsen/logrus"
)

const defaultRedisStreamName = "mercure-hub-updates"

func redisNilToNil(err error) error {
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return err
}

// RedisTransport implements the TransportInterface using the Redis database.
type RedisTransport struct {
	sync.RWMutex
	client      *redis.Client
	streamName  string
	size        int64
	subscribers map[*Subscriber]struct{}
	closed      chan struct{}
	closedOnce  sync.Once
	lastSeq     string
	lastEventID string
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

	size := int64(0)
	sizeParameter := q.Get("size")
	if sizeParameter != "" {
		size, err = strconv.ParseInt(sizeParameter, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(`%q: invalid "size" parameter %q: %s: %w\n`, u, sizeParameter, err, ErrInvalidTransportDSN)
		}
		q.Del("size")
	}

	fmt.Printf("Limiting Redis Queue Size to %d\n", size)
	u.RawQuery = q.Encode()

	redisOptions, err := redis.ParseURL(u.String())
	if err != nil {
		return nil, fmt.Errorf(`%q: invalid "redis" dsn %q: %w\n`, u, u.String(), ErrInvalidTransportDSN)
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
		return nil, fmt.Errorf(`%q: redis connection error "%s": %w\n`, u, err, ErrInvalidTransportDSN)
	}

	transport := &RedisTransport{
		client:      client,
		streamName:  streamName,
		size:        size,
		subscribers: make(map[*Subscriber]struct{}),
		closed:      make(chan struct{}),
		lastEventID: getLastEventID(client, streamName),
	}
	return transport, nil
}

// cacheKeyID provides a unique cache identifier for the given ID.
func (t *RedisTransport) cacheKeyID(id string) string {
	return fmt.Sprintf("%s/%s", t.streamName, id)
}

func getLastEventID(client *redis.Client, streamName string) string {
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

	log.Info(fmt.Sprintf("Update Persisted. Entry ID: %s\n", update.ID))

	for subscriber := range t.subscribers {
		if !subscriber.Dispatch(update, false) {
			t.closeSubscriberChannel(subscriber)
			log.Warn(fmt.Sprintf("Couldn't Dispatch Entry ID: %s. Connection Closed to Subscriber: %s\n", update.ID, subscriber.ID))
		}
	}

	return nil
}

// persist stores update in the database.
func (t *RedisTransport) persist(updateID string, updateJSON []byte) error {
	var script string
	if t.size > 0 {
		// Script Explanation
		// Convert the <Arg:History Size> into a number
		// Add to <Key:Stream Name> using Auto-Generated Entry ID, Limiting the length to <Arg:History Size> add an entry with the data key set to <Arg:Update JSON> and return <res:Entry ID>
		// Add to the end of the <Key:cacheKeyID(updateID)> List the <res:Entry ID>
		// Add to the end of the <Key:cacheKeyID("") List the <Key:cacheKeyID(updateID)>
		// While the length of the <Key:cacheKeyID("")> List is over <Arg:History Size>
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

	// If a Last-Event-ID is given we will send out the history
	// Then we initiale the Subscriber Goroutine
	// If it isnt given then we start it straight away
	if s.RequestLastEventID != "" {
		t.dispatchHistory(s, toSeq)
	} else {
		go t.SubscribeToMessageStream(s, "$")
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

func (t *RedisTransport) historyDispatched(s *Subscriber, lastUpdateID string, lastSequenceID string) {
	s.HistoryDispatched(lastUpdateID)
	go t.SubscribeToMessageStream(s, lastSequenceID)
}

func (t *RedisTransport) dispatchHistory(s *Subscriber, toSeq string) {
	if toSeq == "" {
		toSeq = "+"
	}

	fromSeq := s.RequestLastEventID
	responseLastEventID := s.RequestLastEventID

	// This is a very complicated flow which ideally needs to be simplified
	// We first get the Update ID the client gave and find it inside our db
	// If it doesnt exist we cancel sending and start from fresh
	// If it does then we search the redis stream for the update that came after this event
	// Once we have that, we can then go through the stream from that stream ID to the end of the query
	// If this fails at any point, we exit history sending and just start the goroutine to start sending new events from this point onwards
	if fromSeq != EarliestLastEventID {
		// Get the Sequence ID Of the Message They Received
		var err error
		fromSeq, err = t.client.LIndex(t.cacheKeyID(fromSeq), 0).Result()
		if err != nil {
			t.historyDispatched(s, responseLastEventID, "$")
			return
		}

		// Get the Next Sequence ID
		streamArgs := &redis.XReadArgs{Streams: []string{t.streamName, fromSeq}, Count: 1, Block: 0}
		result, err := t.client.XRead(streamArgs).Result()
		if err != nil {
			t.historyDispatched(s, responseLastEventID, "$")
			return
		}
		fromSeq = result[0].Messages[0].ID
	} else {
		fromSeq = "-"
	}

	messages, err := t.client.XRange(t.streamName, fromSeq, toSeq).Result()
	if err != nil {
		t.historyDispatched(s, responseLastEventID, "$")
		return
	}

	for _, entry := range messages {
		message, ok := entry.Values["data"]
		if !ok {
			t.historyDispatched(s, responseLastEventID, "$")
			return
		}

		var update *Update
		if err := json.Unmarshal([]byte(fmt.Sprintf("%v", message)), &update); err != nil {
			t.historyDispatched(s, responseLastEventID, "$")
			return
		}

		if !s.Dispatch(update, true) {
			t.historyDispatched(s, responseLastEventID, "$")
			return
		}
		responseLastEventID = entry.ID
	}
	t.historyDispatched(s, responseLastEventID, responseLastEventID)
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

func (t *RedisTransport) SubscribeToMessageStream(subscriber *Subscriber, lastSequenceID string) {
	streamArgs := &redis.XReadArgs{Streams: []string{t.streamName, lastSequenceID}, Count: 1, Block: 1}

	for {
		select {
		case <-t.closed:
			t.closeSubscriberChannel(subscriber)
			log.Info(fmt.Sprintf("Closing Transport. Entry ID: %s\n", streamArgs.Streams[1]))
			return
		case <-subscriber.disconnected:
			t.closeSubscriberChannel(subscriber)
			log.Info(fmt.Sprintf("Subscriber Disconnected. Entry ID: %s\n", streamArgs.Streams[1]))
			return
		default:
			log.Info(fmt.Sprintf("Looking For Messages. Entry ID: %s\n", streamArgs.Streams[1]))

			streams, err := t.client.XRead(streamArgs).Result()
			if err != nil {
				log.Error(fmt.Errorf("[Redis] XREAD error: %w", err))
				continue
			}
			log.Info(fmt.Sprintf("Event Found. Last Entry ID: %s\n", streamArgs.Streams[1]))

			// If we get an error in this block we dont exit
			// We do this incase there's some sort of inconsistency in the redis data allowing us to keep the client connected
			// then thanks to the for loop we can just continue until we find a good message
			entry := streams[0].Messages[0]
			message, ok := entry.Values["data"]
			if !ok {
				streamArgs.Streams[1] = entry.ID
				log.Warn(fmt.Sprintf("Couldn't Decode Entry. Last Entry ID: %s\n", streamArgs.Streams[1]))
				continue
			}

			var update *Update
			if err := json.Unmarshal([]byte(fmt.Sprintf("%v", message)), &update); err != nil {
				streamArgs.Streams[1] = entry.ID
				log.Warn(fmt.Sprintf("Couldn't JSON Load Entry ID: %s\n", entry.ID))
				continue
			}

			if !subscriber.Dispatch(update, false) {
				// This is the only place where we close the connection
				// If this errors out, it means the clients gone. we shouldnt run this anymore
				t.closeSubscriberChannel(subscriber)
				log.Warn(fmt.Sprintf("Couldn't Dispatch Entry ID: %s. Connection Closed to Subscriber: %s\n", entry.ID, subscriber.ID))
				return
			}

			log.Info(fmt.Sprintf("Event Transmitted. ID: %s\n", entry.ID))
			subscriber.responseLastEventID <- entry.ID
			streamArgs.Streams[1] = entry.ID
		}
	}
}

func (t *RedisTransport) closeSubscriberChannel(subscriber *Subscriber) {
	t.Lock()
	defer t.Unlock()
	subscriber.Disconnect()
	delete(t.subscribers, subscriber)
}
