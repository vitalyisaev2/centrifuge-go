package centrifuge

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/centrifugal/centrifugo/libcentrifugo"
	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
)

// Timestamp is helper function to get current timestamp as string.
func Timestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// Credentials describe client connection parameters.
type Credentials struct {
	User      string
	Timestamp string
	Info      string
	Token     string
}

var (
	ErrTimeout              = errors.New("timed out")
	ErrDuplicateWaiter      = errors.New("waiter with uid already exists")
	ErrWaiterClosed         = errors.New("waiter closed")
	ErrClientStatus         = errors.New("wrong client status to make operation")
	ErrClientDisconnected   = errors.New("client disconnected")
	ErrClientExpired        = errors.New("client expired")
	ErrReconnectFailed      = errors.New("reconnect failed")
	ErrBadSubscribeStatus   = errors.New("bad subscribe status")
	ErrBadUnsubscribeStatus = errors.New("bad unsubscribe status")
	ErrBadPublishStatus     = errors.New("bad publish status")
)

const (
	DefaultPrivateChannelPrefix = "$"
	DefaultTimeout              = 1 * time.Second
)

// Config contains various client options.
type Config struct {
	Timeout              time.Duration
	PrivateChannelPrefix string
	Debug                bool
}

// DefaultConfig with standard private channel prefix and 1 second timeout.
var DefaultConfig = &Config{
	PrivateChannelPrefix: DefaultPrivateChannelPrefix,
	Timeout:              DefaultTimeout,
}

type clientCommand struct {
	UID    string      `json:"uid"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type response struct {
	UID    string          `json:"uid,omitempty"`
	Error  string          `json:"error"`
	Method string          `json:"method"`
	Body   json.RawMessage `json:"body"`
}

type PrivateSign struct {
	Sign string
	Info string
}

type PrivateRequest struct {
	ClientID string
	Channel  string
}

func newPrivateRequest(client string, channel string) *PrivateRequest {
	return &PrivateRequest{
		ClientID: client,
		Channel:  channel,
	}
}

// PrivateSubHandler needed to handle private channel subscriptions.
type PrivateSubHandler func(*Centrifuge, *PrivateRequest) (*PrivateSign, error)

// RefreshHandler handles refresh event when client's credentials expired and must be refreshed.
type RefreshHandler func(*Centrifuge) (*Credentials, error)

// DisconnectHandler is a function to handle disconnect event.
type DisconnectHandler func(*Centrifuge) error

// ErrorHandler is a function to handle critical protocol errors manually.
type ErrorHandler func(*Centrifuge, error)

// EventHandler contains callback functions that will be called when
// corresponding event happens with connection to Centrifuge.
type EventHandler struct {
	OnDisconnect DisconnectHandler
	OnPrivateSub PrivateSubHandler
	OnRefresh    RefreshHandler
	OnError      ErrorHandler
}

// Status shows actual connection status.
type Status int

const (
	DISCONNECTED = Status(iota)
	CONNECTED
	CLOSED
	RECONNECTING
)

// Centrifuge describes client connection to Centrifugo server.
type Centrifuge struct {
	mutex        sync.RWMutex
	URL          string
	config       *Config
	credentials  *Credentials
	conn         *websocket.Conn
	msgID        int32
	status       Status
	clientID     libcentrifugo.ConnID
	subsMutex    sync.RWMutex
	subs         map[string]*Sub
	waitersMutex sync.RWMutex
	waiters      map[string]chan response
	receive      chan []byte
	write        chan []byte
	closed       chan struct{}
	events       *EventHandler
}

// MessageHandler is a function to handle messages in channels.
type MessageHandler func(*Sub, libcentrifugo.Message) error

// JoinHandler is a function to handle join messages.
type JoinHandler func(*Sub, libcentrifugo.ClientInfo) error

// LeaveHandler is a function to handle leave messages.
type LeaveHandler func(*Sub, libcentrifugo.ClientInfo) error

// UnsubscribeHandler is a function to handle unsubscribe event.
type UnsubscribeHandler func(*Sub) error

// SubEventHandler contains callback functions that will be called when
// corresponding event happens with subscription to channel.
type SubEventHandler struct {
	OnMessage     MessageHandler
	OnJoin        JoinHandler
	OnLeave       LeaveHandler
	OnUnsubscribe UnsubscribeHandler
}

// Sub respresents subscription on channel.
type Sub struct {
	centrifuge    *Centrifuge
	Channel       string
	events        *SubEventHandler
	lastMessageID *libcentrifugo.MessageID
}

func (c *Centrifuge) newSub(channel string, events *SubEventHandler) *Sub {
	return &Sub{
		centrifuge: c,
		Channel:    channel,
		events:     events,
	}
}

// Publish JSON encoded data.
func (s *Sub) Publish(data []byte) error {
	return s.centrifuge.publish(s.Channel, data)
}

// History allows to extract channel history.
func (s *Sub) History() ([]libcentrifugo.Message, error) {
	return s.centrifuge.history(s.Channel)
}

// Presence allows to extract presence information for channel.
func (s *Sub) Presence() (map[libcentrifugo.ConnID]libcentrifugo.ClientInfo, error) {
	return s.centrifuge.presence(s.Channel)
}

// Unsubscribe allows to unsubscribe from channel.
func (s *Sub) Unsubscribe() error {
	return s.centrifuge.unsubscribe(s.Channel)
}

func (s *Sub) handleMessage(m libcentrifugo.Message) {
	var onMessage MessageHandler
	if s.events != nil && s.events.OnMessage != nil {
		onMessage = s.events.OnMessage
	}
	s.lastMessageID = &m.UID
	if onMessage != nil {
		onMessage(s, m)
	}
}

func (s *Sub) handleJoinMessage(info libcentrifugo.ClientInfo) {
	var onJoin JoinHandler
	if s.events != nil && s.events.OnJoin != nil {
		onJoin = s.events.OnJoin
	}
	if onJoin != nil {
		onJoin(s, info)
	}
}

func (s *Sub) handleLeaveMessage(info libcentrifugo.ClientInfo) {
	var onLeave LeaveHandler
	if s.events != nil && s.events.OnLeave != nil {
		onLeave = s.events.OnLeave
	}
	if onLeave != nil {
		onLeave(s, info)
	}
}

func (s *Sub) resubscribe() error {
	privateSign, err := s.centrifuge.privateSign(s.Channel)
	if err != nil {
		return err
	}
	body, err := s.centrifuge.sendSubscribe(s.Channel, s.lastMessageID, privateSign)
	if err != nil {
		return err
	}
	if !body.Status {
		return ErrBadSubscribeStatus
	}

	if len(body.Messages) > 0 {
		for i := len(body.Messages) - 1; i >= 0; i-- {
			s.handleMessage(body.Messages[i])
		}
	} else {
		s.lastMessageID = &body.Last
	}

	// resubscribe successfull.
	return nil
}

func (c *Centrifuge) nextMsgID() int32 {
	return atomic.AddInt32(&c.msgID, 1)
}

// NewCenrifuge initializes Centrifuge struct. It accepts URL to Centrifugo server,
// connection Credentials, event handler and Config.
func NewCentrifuge(u string, creds *Credentials, events *EventHandler, config *Config) *Centrifuge {
	c := &Centrifuge{
		URL:         u,
		subs:        make(map[string]*Sub),
		config:      config,
		credentials: creds,
		receive:     make(chan []byte, 64),
		write:       make(chan []byte, 64),
		closed:      make(chan struct{}),
		waiters:     make(map[string]chan response),
		events:      events,
	}
	return c
}

// SetCredentials allows to set new updated credentials when old
// credentials expired.
func (c *Centrifuge) SetCredentials(creds *Credentials) {
	c.mutex.Lock()
	defer c.mutex.RUnlock()
	c.credentials = creds
}

// Connected returns true if client is connected at moment.
func (c *Centrifuge) Connected() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.status == CONNECTED
}

// Subscribed returns true if client subscribed on channel.
func (c *Centrifuge) Subscribed(channel string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.subscribed(channel)
}

func (c *Centrifuge) subscribed(channel string) bool {
	c.subsMutex.RLock()
	_, ok := c.subs[channel]
	c.subsMutex.RUnlock()
	return ok
}

// ClientID returns client ID of this connection. It only available after connection
// was established and authorized.
func (c *Centrifuge) ClientID() string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return string(c.clientID)
}

func (c *Centrifuge) handleError(err error) {
	var onError ErrorHandler
	if c.events != nil && c.events.OnError != nil {
		onError = c.events.OnError
	}
	if onError != nil {
		onError(c, err)
	} else {
		log.Println(err)
		c.Close()
	}
}

// Close closes Centrifuge connection and clean ups everything.
func (c *Centrifuge) Close() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.conn != nil {

		if c.status == CONNECTED {
			for ch, sub := range c.subs {
				err := c.unsubscribe(sub.Channel)
				if err != nil {
					log.Println(err)
				}
				delete(c.subs, ch)
			}
		}

		c.conn.Close()
	}

	c.waitersMutex.Lock()
	for uid, ch := range c.waiters {
		close(ch)
		delete(c.waiters, uid)
	}
	c.waitersMutex.Unlock()

	select {
	case <-c.closed:
	default:
		close(c.closed)
	}

	c.status = CLOSED
}

func (c *Centrifuge) handleDisconnect(err error) {
	c.mutex.Lock()
	if c.status == CLOSED || c.status == RECONNECTING {
		c.mutex.Unlock()
		return
	}

	if c.conn != nil {
		c.conn.Close()
	}

	c.waitersMutex.Lock()
	for uid, ch := range c.waiters {
		close(ch)
		delete(c.waiters, uid)
	}
	c.waitersMutex.Unlock()

	select {
	case <-c.closed:
	default:
		close(c.closed)
	}

	c.status = DISCONNECTED

	var onDisconnect DisconnectHandler
	if c.events != nil && c.events.OnDisconnect != nil {
		onDisconnect = c.events.OnDisconnect
	}

	c.mutex.Unlock()

	if onDisconnect != nil {
		onDisconnect(c)
	}

}

type ReconnectStrategy interface {
	reconnect(c *Centrifuge) error
}

type PeriodicReconnect struct {
	ReconnectInterval time.Duration
	NumReconnect      int
}

var DefaultPeriodicReconnect = &PeriodicReconnect{
	ReconnectInterval: 1 * time.Second,
	NumReconnect:      0,
}

func (r *PeriodicReconnect) reconnect(c *Centrifuge) error {
	reconnects := 0
	for {
		if r.NumReconnect > 0 && reconnects >= r.NumReconnect {
			break
		}
		time.Sleep(r.ReconnectInterval)

		reconnects += 1

		err := c.doReconnect()
		if err != nil {
			log.Println(err)
			continue
		}

		// successfully reconnected
		return nil
	}
	return ErrReconnectFailed
}

type BackoffReconnect struct {
	// NumReconnect is maximum number of reconnect attempts, 0 means reconnect forever
	NumReconnect int
	//Factor is the multiplying factor for each increment step
	Factor float64
	//Jitter eases contention by randomizing backoff steps
	Jitter bool
	//Min and Max are the minimum and maximum values of the counter
	Min, Max time.Duration
}

var DefaultBackoffReconnect = &BackoffReconnect{
	NumReconnect: 0,
	Min:          100 * time.Millisecond,
	Max:          10 * time.Second,
	Factor:       2,
	Jitter:       true,
}

func (r *BackoffReconnect) reconnect(c *Centrifuge) error {
	b := &backoff.Backoff{
		Min:    r.Min,
		Max:    r.Max,
		Factor: r.Factor,
		Jitter: r.Jitter,
	}
	reconnects := 0
	for {
		if r.NumReconnect > 0 && reconnects >= r.NumReconnect {
			break
		}
		time.Sleep(b.Duration())

		reconnects += 1

		err := c.doReconnect()
		if err != nil {
			log.Println(err)
			continue
		}

		// successfully reconnected
		return nil
	}
	return ErrReconnectFailed
}

func (c *Centrifuge) doReconnect() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.closed = make(chan struct{})

	err := c.connect()
	if err != nil {
		close(c.closed)
		return err
	}

	err = c.resubscribe()
	if err != nil {
		close(c.closed)
		return err
	}

	return nil
}

func (c *Centrifuge) Reconnect(strategy ReconnectStrategy) error {
	c.mutex.Lock()
	c.status = RECONNECTING
	c.mutex.Unlock()
	return strategy.reconnect(c)
}

func (c *Centrifuge) resubscribe() error {
	for _, sub := range c.subs {
		err := sub.resubscribe()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Centrifuge) read() {
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			c.handleDisconnect(err)
			return
		}
		select {
		case <-c.closed:
			return
		default:
			c.receive <- message
		}
	}
}

func (c *Centrifuge) run() {
	for {
		select {
		case msg := <-c.receive:
			err := c.handle(msg)
			if err != nil {
				c.handleError(err)
			}
		case msg := <-c.write:
			c.conn.SetWriteDeadline(time.Now().Add(c.config.Timeout))
			err := c.conn.WriteMessage(websocket.TextMessage, msg)
			c.conn.SetWriteDeadline(time.Time{})
			if err != nil {
				c.handleError(err)
			}
		case <-c.closed:
			return
		}
	}
}

var (
	arrayJsonPrefix  byte = '['
	objectJsonPrefix byte = '{'
)

func responsesFromClientMsg(msg []byte) ([]response, error) {
	var resps []response
	firstByte := msg[0]
	switch firstByte {
	case objectJsonPrefix:
		// single command request
		var resp response
		err := json.Unmarshal(msg, &resp)
		if err != nil {
			return nil, err
		}
		resps = append(resps, resp)
	case arrayJsonPrefix:
		// array of commands received
		err := json.Unmarshal(msg, &resps)
		if err != nil {
			return nil, err
		}
	}
	return resps, nil
}

func (c *Centrifuge) handle(msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	resps, err := responsesFromClientMsg(msg)
	if err != nil {
		return err
	}
	for _, resp := range resps {
		if resp.UID != "" {
			c.waitersMutex.RLock()
			if waiter, ok := c.waiters[resp.UID]; ok {
				waiter <- resp
			}
			c.waitersMutex.RUnlock()
		} else {
			err := c.handleAsyncResponse(resp)
			if err != nil {
				c.handleError(err)
			}
		}
	}
	return nil
}

func (c *Centrifuge) handleAsyncResponse(resp response) error {
	method := resp.Method
	errorStr := resp.Error
	body := resp.Body
	if errorStr != "" {
		// Should never occur in usual workflow.
		return errors.New(errorStr)
	}
	switch method {
	case "message":
		var m libcentrifugo.Message
		err := json.Unmarshal(body, &m)
		if err != nil {
			// Malformed message received.
			return errors.New("malformed message received from server")
		}
		channel := m.Channel
		c.subsMutex.RLock()
		sub, ok := c.subs[string(channel)]
		c.subsMutex.RUnlock()
		if !ok {
			log.Println("message received but client not subscribed on channel")
			return nil
		}
		sub.handleMessage(m)
	case "join":
		var b libcentrifugo.JoinLeaveBody
		err := json.Unmarshal(body, &b)
		if err != nil {
			log.Println("malformed join message")
			return nil
		}
		channel := b.Channel
		c.subsMutex.RLock()
		sub, ok := c.subs[string(channel)]
		c.subsMutex.RUnlock()
		if !ok {
			log.Println("join received but client not subscribed on channel")
			c.mutex.RUnlock()
			return nil
		}
		sub.handleJoinMessage(b.Data)
	case "leave":
		var b libcentrifugo.JoinLeaveBody
		err := json.Unmarshal(body, &b)
		if err != nil {
			log.Println("malformed leave message")
			return nil
		}
		channel := b.Channel
		c.subsMutex.RLock()
		sub, ok := c.subs[string(channel)]
		c.subsMutex.RUnlock()
		if !ok {
			log.Println("leave received but client not subscribed on channel")
			c.mutex.RUnlock()
			return nil
		}
		sub.handleLeaveMessage(b.Data)
	default:
		return nil
	}
	return nil
}

// Lock must be held outside
func (c *Centrifuge) createWSConn() (*websocket.Conn, error) {
	wsHeaders := http.Header{}
	dialer := websocket.DefaultDialer
	conn, resp, err := dialer.Dial(c.URL, wsHeaders)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, errors.New("Wrong status code while connecting to server")
	}
	return conn, nil
}

// Lock must be held outside
func (c *Centrifuge) connectWS() error {
	conn, err := c.createWSConn()
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

// Lock must be held outside
func (c *Centrifuge) connect() error {

	err := c.connectWS()
	if err != nil {
		return err
	}

	go c.run()

	go c.read()

	var body libcentrifugo.ConnectBody

	body, err = c.sendConnect()
	if err != nil {
		return err
	}

	if body.Expires && body.Expired {
		// Try to refresh credentials and repeat connection attempt.
		err = c.refreshCredentials()
		if err != nil {
			return err
		}
		body, err = c.sendConnect()
		if err != nil {
			return err
		}
		if body.Expires && body.Expired {
			return ErrClientExpired
		}
	}

	c.clientID = body.Client

	if body.Expires {
		go func(interval int64) {
			tick := time.After(time.Duration(interval) * time.Second)
			select {
			case <-c.closed:
				return
			case <-tick:
				err := c.sendRefresh()
				if err != nil {
					log.Println(err)
				}
			}
		}(body.TTL)
	}

	c.status = CONNECTED

	return nil
}

// Connect connects to Centrifugo and sends connect message to authorize.
func (c *Centrifuge) Connect() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.status == CONNECTED {
		return ErrClientStatus
	}
	return c.connect()
}

func (c *Centrifuge) refreshCredentials() error {
	var onRefresh RefreshHandler
	if c.events != nil && c.events.OnRefresh != nil {
		onRefresh = c.events.OnRefresh
	}
	if onRefresh == nil {
		return errors.New("RefreshHandler must be set to handle expired credentials")
	}

	creds, err := onRefresh(c)
	if err != nil {
		return err
	}
	c.credentials = creds
	return nil
}

func (c *Centrifuge) sendRefresh() error {

	err := c.refreshCredentials()
	if err != nil {
		return err
	}

	params := c.refreshParams(c.credentials)
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "refresh",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	var body libcentrifugo.ConnectBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return err
	}
	if body.Expires {
		if body.Expired {
			return ErrClientExpired
		}
		go func(interval int64) {
			tick := time.After(time.Duration(interval) * time.Second)
			select {
			case <-c.closed:
				return
			case <-tick:
				err := c.sendRefresh()
				if err != nil {
					log.Println(err)
				}
			}
		}(body.TTL)
	}
	return nil
}

func (c *Centrifuge) refreshParams(creds *Credentials) *libcentrifugo.RefreshClientCommand {
	return &libcentrifugo.RefreshClientCommand{
		User:      libcentrifugo.UserID(creds.User),
		Timestamp: creds.Timestamp,
		Info:      creds.Info,
		Token:     creds.Token,
	}
}

func (c *Centrifuge) sendConnect() (libcentrifugo.ConnectBody, error) {
	params := c.connectParams()
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "connect",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return libcentrifugo.ConnectBody{}, err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return libcentrifugo.ConnectBody{}, err
	}
	if r.Error != "" {
		return libcentrifugo.ConnectBody{}, errors.New(r.Error)
	}
	var body libcentrifugo.ConnectBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return libcentrifugo.ConnectBody{}, err
	}
	return body, nil
}

func (c *Centrifuge) connectParams() *libcentrifugo.ConnectClientCommand {
	return &libcentrifugo.ConnectClientCommand{
		User:      libcentrifugo.UserID(c.credentials.User),
		Timestamp: c.credentials.Timestamp,
		Info:      c.credentials.Info,
		Token:     c.credentials.Token,
	}
}

func (c *Centrifuge) privateSign(channel string) (*PrivateSign, error) {
	var ps *PrivateSign
	var err error
	if strings.HasPrefix(channel, c.config.PrivateChannelPrefix) {
		if c.events != nil && c.events.OnPrivateSub != nil {
			privateReq := newPrivateRequest(c.ClientID(), channel)
			ps, err = c.events.OnPrivateSub(c, privateReq)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New("PrivateSubHandler must be set to handle private channel subscriptions")
		}
	}
	return ps, nil
}

// Subscribe allows to subscribe on channel.
func (c *Centrifuge) Subscribe(channel string, events *SubEventHandler) (*Sub, error) {
	if !c.Connected() {
		return nil, ErrClientDisconnected
	}
	privateSign, err := c.privateSign(channel)
	if err != nil {
		return nil, err
	}
	c.subsMutex.Lock()
	sub := c.newSub(channel, events)
	c.subs[channel] = sub
	c.subsMutex.Unlock()

	body, err := c.sendSubscribe(channel, sub.lastMessageID, privateSign)
	c.mutex.Lock()
	if err != nil {
		c.subsMutex.Lock()
		delete(c.subs, channel)
		c.subsMutex.Unlock()
		return nil, err
	}
	if !body.Status {
		c.subsMutex.Lock()
		delete(c.subs, channel)
		c.subsMutex.Unlock()
		return nil, ErrBadSubscribeStatus
	}

	if len(body.Messages) > 0 {
		for i := len(body.Messages) - 1; i >= 0; i-- {
			sub.handleMessage(body.Messages[i])
		}
	} else {
		sub.lastMessageID = &body.Last
	}

	c.mutex.Unlock()
	// Subscription on channel successfull.
	return sub, nil
}

func (c *Centrifuge) subscribeParams(channel string, lastMessageID *libcentrifugo.MessageID, privateSign *PrivateSign) *libcentrifugo.SubscribeClientCommand {
	cmd := &libcentrifugo.SubscribeClientCommand{
		Channel: libcentrifugo.Channel(channel),
	}
	if lastMessageID != nil {
		cmd.Recover = true
		cmd.Last = *lastMessageID
	}
	if privateSign != nil {
		cmd.Client = libcentrifugo.ConnID(c.ClientID())
		cmd.Info = privateSign.Info
		cmd.Sign = privateSign.Sign
	}
	return cmd
}

func (c *Centrifuge) sendSubscribe(channel string, lastMessageID *libcentrifugo.MessageID, privateSign *PrivateSign) (libcentrifugo.SubscribeBody, error) {
	params := c.subscribeParams(channel, lastMessageID, privateSign)
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "subscribe",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return libcentrifugo.SubscribeBody{}, err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return libcentrifugo.SubscribeBody{}, err
	}
	if r.Error != "" {
		return libcentrifugo.SubscribeBody{}, errors.New(r.Error)
	}
	var body libcentrifugo.SubscribeBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return libcentrifugo.SubscribeBody{}, err
	}
	return body, nil
}

func (c *Centrifuge) publish(channel string, data []byte) error {
	body, err := c.sendPublish(channel, data)
	if err != nil {
		return err
	}
	if !body.Status {
		return ErrBadPublishStatus
	}
	return nil
}

func (c *Centrifuge) publishParams(channel string, data []byte) *libcentrifugo.PublishClientCommand {
	return &libcentrifugo.PublishClientCommand{
		Channel: libcentrifugo.Channel(channel),
		Data:    json.RawMessage(data),
	}
}

func (c *Centrifuge) sendPublish(channel string, data []byte) (libcentrifugo.PublishBody, error) {
	params := c.publishParams(channel, data)
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "publish",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return libcentrifugo.PublishBody{}, err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return libcentrifugo.PublishBody{}, err
	}
	if r.Error != "" {
		return libcentrifugo.PublishBody{}, errors.New(r.Error)
	}
	var body libcentrifugo.PublishBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return libcentrifugo.PublishBody{}, err
	}
	return body, nil
}

func (c *Centrifuge) history(channel string) ([]libcentrifugo.Message, error) {
	body, err := c.sendHistory(channel)
	if err != nil {
		return []libcentrifugo.Message{}, err
	}
	return body.Data, nil
}

func (c *Centrifuge) historyParams(channel string) *libcentrifugo.HistoryClientCommand {
	return &libcentrifugo.HistoryClientCommand{
		Channel: libcentrifugo.Channel(channel),
	}
}

func (c *Centrifuge) sendHistory(channel string) (libcentrifugo.HistoryBody, error) {
	params := c.historyParams(channel)
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "history",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return libcentrifugo.HistoryBody{}, err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return libcentrifugo.HistoryBody{}, err
	}
	if r.Error != "" {
		return libcentrifugo.HistoryBody{}, errors.New(r.Error)
	}
	var body libcentrifugo.HistoryBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return libcentrifugo.HistoryBody{}, err
	}
	return body, nil
}

func (c *Centrifuge) presence(channel string) (map[libcentrifugo.ConnID]libcentrifugo.ClientInfo, error) {
	body, err := c.sendPresence(channel)
	if err != nil {
		return map[libcentrifugo.ConnID]libcentrifugo.ClientInfo{}, err
	}
	return body.Data, nil
}

func (c *Centrifuge) presenceParams(channel string) *libcentrifugo.PresenceClientCommand {
	return &libcentrifugo.PresenceClientCommand{
		Channel: libcentrifugo.Channel(channel),
	}
}

func (c *Centrifuge) sendPresence(channel string) (libcentrifugo.PresenceBody, error) {
	params := c.presenceParams(channel)
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "presence",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return libcentrifugo.PresenceBody{}, err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return libcentrifugo.PresenceBody{}, err
	}
	if r.Error != "" {
		return libcentrifugo.PresenceBody{}, errors.New(r.Error)
	}
	var body libcentrifugo.PresenceBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return libcentrifugo.PresenceBody{}, err
	}
	return body, nil
}

func (c *Centrifuge) unsubscribe(channel string) error {
	if !c.subscribed(channel) {
		return nil
	}
	body, err := c.sendUnsubscribe(channel)
	if err != nil {
		return err
	}
	if !body.Status {
		return ErrBadUnsubscribeStatus
	}
	c.subsMutex.Lock()
	delete(c.subs, channel)
	c.subsMutex.Unlock()
	return nil
}

func (c *Centrifuge) unsubscribeParams(channel string) *libcentrifugo.UnsubscribeClientCommand {
	return &libcentrifugo.UnsubscribeClientCommand{
		Channel: libcentrifugo.Channel(channel),
	}
}

func (c *Centrifuge) sendUnsubscribe(channel string) (libcentrifugo.UnsubscribeBody, error) {
	params := c.unsubscribeParams(channel)
	cmd := clientCommand{
		UID:    strconv.Itoa(int(c.nextMsgID())),
		Method: "unsubscribe",
		Params: params,
	}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return libcentrifugo.UnsubscribeBody{}, err
	}
	r, err := c.sendSync(cmd.UID, cmdBytes)
	if err != nil {
		return libcentrifugo.UnsubscribeBody{}, err
	}
	if r.Error != "" {
		return libcentrifugo.UnsubscribeBody{}, errors.New(r.Error)
	}
	var body libcentrifugo.UnsubscribeBody
	err = json.Unmarshal(r.Body, &body)
	if err != nil {
		return libcentrifugo.UnsubscribeBody{}, err
	}
	return body, nil
}

func (c *Centrifuge) sendSync(uid string, msg []byte) (response, error) {
	wait := make(chan response)
	err := c.addWaiter(uid, wait)
	defer c.removeWaiter(uid)
	if err != nil {
		return response{}, err
	}
	err = c.send(msg)
	if err != nil {
		return response{}, err
	}
	return c.wait(wait)
}

func (c *Centrifuge) send(msg []byte) error {
	select {
	case <-c.closed:
		return ErrClientDisconnected
	default:
		c.write <- msg
	}
	return nil
}

func (c *Centrifuge) addWaiter(uid string, ch chan response) error {
	c.waitersMutex.Lock()
	defer c.waitersMutex.Unlock()
	if _, ok := c.waiters[uid]; ok {
		return ErrDuplicateWaiter
	}
	c.waiters[uid] = ch
	return nil
}

func (c *Centrifuge) removeWaiter(uid string) error {
	c.waitersMutex.Lock()
	defer c.waitersMutex.Unlock()
	delete(c.waiters, uid)
	return nil
}

func (c *Centrifuge) wait(ch chan response) (response, error) {
	select {
	case data, ok := <-ch:
		if !ok {
			return response{}, ErrWaiterClosed
		}
		return data, nil
	case <-time.After(c.config.Timeout):
		return response{}, ErrTimeout
	case <-c.closed:
		return response{}, ErrClientDisconnected
	}
}
