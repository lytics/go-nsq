package nsq

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mreiferson/go-snappystream"
)

// IdentifyResponse represents the metadata
// returned from an IDENTIFY command to nsqd
type IdentifyResponse struct {
	MaxRdyCount int64 `json:"max_rdy_count"`
	TLSv1       bool  `json:"tls_v1"`
	Deflate     bool  `json:"deflate"`
	Snappy      bool  `json:"snappy"`
}

type msgResponse struct {
	msg     *Message
	cmd     *Command
	success bool
	backoff bool
}

// Conn represents a connection to nsqd
//
// Conn exposes a set of callbacks for the
// various events that occur on a connection
type Conn struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	messagesInFlight int64
	maxRdyCount      int64
	rdyCount         int64
	lastRdyCount     int64
	lastMsgTimestamp int64

	mtx sync.Mutex

	config *Config

	conn    *net.TCPConn
	tlsConn *tls.Conn
	addr    string

	Delegate ConnDelegate

	logger *log.Logger
	logLvl LogLevel
	logFmt string

	r io.Reader
	w io.Writer

	backoffCounter int32
	rdyRetryTimer  *time.Timer

	cmdChan         chan *Command
	msgResponseChan chan *msgResponse
	exitChan        chan int
	drainReady      chan int

	closeFlag int32
	stopper   sync.Once
	wg        sync.WaitGroup

	readLoopRunning int32
}

// NewConn returns a new Conn instance
func NewConn(addr string, config *Config) *Conn {
	return &Conn{
		addr: addr,

		config: config,

		maxRdyCount:      2500,
		lastMsgTimestamp: time.Now().UnixNano(),

		cmdChan:         make(chan *Command),
		msgResponseChan: make(chan *msgResponse),
		exitChan:        make(chan int),
		drainReady:      make(chan int),
	}
}

// SetLogger assigns the logger to use as well as a level.
//
// The format parameter is expected to be a printf compatible string with
// a single %s argument.  This is useful if you want to provide additional
// context to the log messages that the connection will print, the default
// is '(%s)'.
func (c *Conn) SetLogger(logger *log.Logger, lvl LogLevel, format string) {
	c.logger = logger
	c.logLvl = lvl
	c.logFmt = format
	if c.logFmt == "" {
		c.logFmt = "(%s)"
	}
}

// Connect dials and bootstraps the nsqd connection
// (including IDENTIFY) and returns the IdentifyResponse
func (c *Conn) Connect() (*IdentifyResponse, error) {
	conn, err := net.DialTimeout("tcp", c.addr, time.Second)
	if err != nil {
		return nil, err
	}
	c.conn = conn.(*net.TCPConn)
	c.r = conn
	c.w = conn

	_, err = c.Write(MagicV2)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("[%s] failed to write magic - %s", c.addr, err)
	}

	resp, err := c.identify()
	if err != nil {
		return nil, err
	}

	c.wg.Add(2)
	atomic.StoreInt32(&c.readLoopRunning, 1)
	go c.readLoop()
	go c.writeLoop()
	return resp, nil
}

// Close idempotently initiates connection close
func (c *Conn) Close() error {
	atomic.StoreInt32(&c.closeFlag, 1)
	if c.conn != nil && atomic.LoadInt64(&c.messagesInFlight) == 0 {
		return c.conn.CloseRead()
	}
	return nil
}

// IsClosing indicates whether or not the
// connection is currently in the processing of
// gracefully closing
func (c *Conn) IsClosing() bool {
	return atomic.LoadInt32(&c.closeFlag) == 1
}

// RDY returns the current RDY count
func (c *Conn) RDY() int64 {
	return atomic.LoadInt64(&c.rdyCount)
}

// LastRDY returns the previously set RDY count
func (c *Conn) LastRDY() int64 {
	return atomic.LoadInt64(&c.lastRdyCount)
}

// SetRDY stores the specified RDY count
func (c *Conn) SetRDY(rdy int64) {
	atomic.StoreInt64(&c.rdyCount, rdy)
	atomic.StoreInt64(&c.lastRdyCount, rdy)
}

// MaxRDY returns the nsqd negotiated maximum
// RDY count that it will accept for this connection
func (c *Conn) MaxRDY() int64 {
	return c.maxRdyCount
}

// LastMessageTime returns a time.Time representing
// the time at which the last message was received
func (c *Conn) LastMessageTime() time.Time {
	return time.Unix(0, atomic.LoadInt64(&c.lastMsgTimestamp))
}

// RemoteAddr returns the configured destination nsqd address
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// String returns the fully-qualified address
func (c *Conn) String() string {
	return c.addr
}

// Read performs a deadlined read on the underlying TCP connection
func (c *Conn) Read(p []byte) (int, error) {
	c.conn.SetReadDeadline(time.Now().Add(c.config.readTimeout))
	return c.r.Read(p)
}

// Write performs a deadlined write on the underlying TCP connection
func (c *Conn) Write(p []byte) (int, error) {
	c.conn.SetWriteDeadline(time.Now().Add(c.config.writeTimeout))
	return c.w.Write(p)
}

// WriteCommand is a goroutine safe method to write a Command
// to this connection, and flush.
func (c *Conn) WriteCommand(cmd *Command) error {
	c.mtx.Lock()

	_, err := cmd.WriteTo(c)
	if err != nil {
		goto exit
	}
	err = c.Flush()

exit:
	c.mtx.Unlock()
	if err != nil {
		c.log(LogLevelError, "IO error - %s", err)
		c.Delegate.OnIOError(c, err)
	}
	return err
}

type flusher interface {
	Flush() error
}

// Flush writes all buffered data to the underlying TCP connection
func (c *Conn) Flush() error {
	if f, ok := c.w.(flusher); ok {
		return f.Flush()
	}
	return nil
}

func (c *Conn) identify() (*IdentifyResponse, error) {
	ci := make(map[string]interface{})
	ci["client_id"] = c.config.clientID
	ci["hostname"] = c.config.hostname
	ci["user_agent"] = c.config.userAgent
	ci["short_id"] = c.config.clientID // deprecated
	ci["long_id"] = c.config.hostname  // deprecated
	ci["tls_v1"] = c.config.tlsV1
	ci["deflate"] = c.config.deflate
	ci["deflate_level"] = c.config.deflateLevel
	ci["snappy"] = c.config.snappy
	ci["feature_negotiation"] = true
	ci["heartbeat_interval"] = int64(c.config.heartbeatInterval / time.Millisecond)
	ci["sample_rate"] = c.config.sampleRate
	ci["output_buffer_size"] = c.config.outputBufferSize
	ci["output_buffer_timeout"] = int64(c.config.outputBufferTimeout / time.Millisecond)
	cmd, err := Identify(ci)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	err = c.WriteCommand(cmd)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	frameType, data, err := ReadUnpackedResponse(c)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	if frameType == FrameTypeError {
		return nil, ErrIdentify{string(data)}
	}

	// check to see if the server was able to respond w/ capabilities
	// i.e. it was a JSON response
	if data[0] != '{' {
		return nil, nil
	}

	resp := &IdentifyResponse{}
	err = json.Unmarshal(data, resp)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	c.log(LogLevelDebug, "IDENTIFY response: %+v", resp)

	c.maxRdyCount = resp.MaxRdyCount

	if resp.TLSv1 {
		c.log(LogLevelInfo, "upgrading to TLS")
		err := c.upgradeTLS(c.config.tlsConfig)
		if err != nil {
			return nil, ErrIdentify{err.Error()}
		}
	}

	if resp.Deflate {
		c.log(LogLevelInfo, "upgrading to Deflate")
		err := c.upgradeDeflate(c.config.deflateLevel)
		if err != nil {
			return nil, ErrIdentify{err.Error()}
		}
	}

	if resp.Snappy {
		c.log(LogLevelInfo, "upgrading to Snappy")
		err := c.upgradeSnappy()
		if err != nil {
			return nil, ErrIdentify{err.Error()}
		}
	}

	// now that connection is bootstrapped, enable read buffering
	// (and write buffering if it's not already capable of Flush())
	c.r = bufio.NewReader(c.r)
	if _, ok := c.w.(flusher); !ok {
		c.w = bufio.NewWriter(c.w)
	}

	return resp, nil
}

func (c *Conn) upgradeTLS(conf *tls.Config) error {
	c.tlsConn = tls.Client(c.conn, conf)
	err := c.tlsConn.Handshake()
	if err != nil {
		return err
	}
	c.r = c.tlsConn
	c.w = c.tlsConn
	frameType, data, err := ReadUnpackedResponse(c)
	if err != nil {
		return err
	}
	if frameType != FrameTypeResponse || !bytes.Equal(data, []byte("OK")) {
		return errors.New("invalid response from TLS upgrade")
	}
	return nil
}

func (c *Conn) upgradeDeflate(level int) error {
	conn := net.Conn(c.conn)
	if c.tlsConn != nil {
		conn = c.tlsConn
	}
	fw, _ := flate.NewWriter(conn, level)
	c.r = flate.NewReader(conn)
	c.w = fw
	frameType, data, err := ReadUnpackedResponse(c)
	if err != nil {
		return err
	}
	if frameType != FrameTypeResponse || !bytes.Equal(data, []byte("OK")) {
		return errors.New("invalid response from Deflate upgrade")
	}
	return nil
}

func (c *Conn) upgradeSnappy() error {
	conn := net.Conn(c.conn)
	if c.tlsConn != nil {
		conn = c.tlsConn
	}
	c.r = snappystream.NewReader(conn, snappystream.SkipVerifyChecksum)
	c.w = snappystream.NewWriter(conn)
	frameType, data, err := ReadUnpackedResponse(c)
	if err != nil {
		return err
	}
	if frameType != FrameTypeResponse || !bytes.Equal(data, []byte("OK")) {
		return errors.New("invalid response from Snappy upgrade")
	}
	return nil
}

func (c *Conn) readLoop() {
	for {
		if atomic.LoadInt32(&c.closeFlag) == 1 {
			goto exit
		}

		frameType, data, err := ReadUnpackedResponse(c)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				c.log(LogLevelError, "IO error - %s", err)
				c.Delegate.OnIOError(c, err)
			}
			goto exit
		}

		if frameType == FrameTypeResponse && bytes.Equal(data, []byte("_heartbeat_")) {
			c.log(LogLevelDebug, "heartbeat received")
			c.Delegate.OnHeartbeat(c)
			err := c.WriteCommand(Nop())
			if err != nil {
				c.log(LogLevelError, "IO error - %s", err)
				c.Delegate.OnIOError(c, err)
				goto exit
			}
			continue
		}

		switch frameType {
		case FrameTypeResponse:
			c.Delegate.OnResponse(c, data)
		case FrameTypeMessage:
			msg, err := DecodeMessage(data)
			if err != nil {
				c.log(LogLevelError, "IO error - %s", err)
				c.Delegate.OnIOError(c, err)
				goto exit
			}
			msg.Delegate = &connMessageDelegate{c}

			atomic.AddInt64(&c.rdyCount, -1)
			atomic.AddInt64(&c.messagesInFlight, 1)
			atomic.StoreInt64(&c.lastMsgTimestamp, time.Now().UnixNano())

			c.Delegate.OnMessage(c, msg)
		case FrameTypeError:
			c.log(LogLevelError, "protocol error - %s", data)
			c.Delegate.OnError(c, data)
		default:
			c.log(LogLevelError, "IO error - %s", err)
			c.Delegate.OnIOError(c, fmt.Errorf("unknown frame type %d", frameType))
		}
	}

exit:
	atomic.StoreInt32(&c.readLoopRunning, 0)
	// start the connection close
	messagesInFlight := atomic.LoadInt64(&c.messagesInFlight)
	if messagesInFlight == 0 {
		// if we exited readLoop with no messages in flight
		// we need to explicitly trigger the close because
		// writeLoop won't
		c.close()
	} else {
		c.log(LogLevelWarning, "delaying close, %d outstanding messages", messagesInFlight)
	}
	c.wg.Done()
	c.log(LogLevelInfo, "readLoop exiting")
}

func (c *Conn) writeLoop() {
	for {
		select {
		case <-c.exitChan:
			c.log(LogLevelInfo, "breaking out of writeLoop")
			// Indicate drainReady because we will not pull any more off msgResponseChan
			close(c.drainReady)
			goto exit
		case cmd := <-c.cmdChan:
			err := c.WriteCommand(cmd)
			if err != nil {
				c.log(LogLevelError, "error sending command %s - %s", cmd, err)
				c.close()
				continue
			}
		case resp := <-c.msgResponseChan:
			// Decrement this here so it is correct even if we can't respond to nsqd
			msgsInFlight := atomic.AddInt64(&c.messagesInFlight, -1)

			err := c.WriteCommand(resp.cmd)
			if err != nil {
				c.log(LogLevelError, "error sending command %s - %s", resp.cmd, err)
				c.close()
				continue
			}

			if resp.success {
				c.log(LogLevelDebug, "FIN %s", resp.msg.ID)
				c.Delegate.OnMessageFinished(c, resp.msg)
				if resp.backoff {
					c.Delegate.OnResume(c)
				}
			} else {
				c.log(LogLevelDebug, "REQ %s", resp.msg.ID)
				c.Delegate.OnMessageRequeued(c, resp.msg)
				if resp.backoff {
					c.Delegate.OnBackoff(c)
				}
			}

			if msgsInFlight == 0 &&
				atomic.LoadInt32(&c.closeFlag) == 1 {
				c.close()
				continue
			}
		}
	}

exit:
	c.wg.Done()
	c.log(LogLevelInfo, "writeLoop exiting")
}

func (c *Conn) close() {
	// a "clean" connection close is orchestrated as follows:
	//
	//     1. CLOSE cmd sent to nsqd
	//     2. CLOSE_WAIT response received from nsqd
	//     3. set c.closeFlag
	//     4. readLoop() exits
	//         a. if messages-in-flight > 0 delay close()
	//             i. writeLoop() continues receiving on c.msgResponseChan chan
	//                 x. when messages-in-flight == 0 call close()
	//         b. else call close() immediately
	//     5. c.exitChan close
	//         a. writeLoop() exits
	//             i. c.drainReady close
	//     6a. launch cleanup() goroutine (we're racing with intraprocess
	//        routed messages, see comments below)
	//         a. wait on c.drainReady
	//         b. loop and receive on c.msgResponseChan chan
	//            until messages-in-flight == 0
	//            i. ensure that readLoop has exited
	//     6b. launch waitForCleanup() goroutine
	//         b. wait on waitgroup (covers readLoop() and writeLoop()
	//            and cleanup goroutine)
	//         c. underlying TCP connection close
	//         d. trigger Delegate OnClose()
	//
	c.stopper.Do(func() {
		c.log(LogLevelInfo, "beginning close")
		close(c.exitChan)
		c.conn.CloseRead()

		c.wg.Add(1)
		go c.cleanup()

		go c.waitForCleanup()
	})
}

func (c *Conn) cleanup() {
	<-c.drainReady
	ticker := time.NewTicker(100 * time.Millisecond)
	// writeLoop has exited, drain any remaining in flight messages
	for {
		// we're racing with readLoop which potentially has a message
		// for handling so infinitely loop until messagesInFlight == 0
		// and readLoop has exited
		var msgsInFlight int64
		select {
		case <-c.msgResponseChan:
			msgsInFlight = atomic.AddInt64(&c.messagesInFlight, -1)
		case <-ticker.C:
			msgsInFlight = atomic.LoadInt64(&c.messagesInFlight)
		}
		if msgsInFlight > 0 {
			c.log(LogLevelWarning, "draining... waiting for %d messages in flight", msgsInFlight)
			continue
		}
		// until the readLoop has exited we cannot be sure that there
		// still won't be a race
		if atomic.LoadInt32(&c.readLoopRunning) == 1 {
			c.log(LogLevelWarning, "draining... readLoop still running")
			continue
		}
		goto exit
	}

exit:
	ticker.Stop()
	c.wg.Done()
	c.log(LogLevelInfo, "finished draining, cleanup exiting")
}

func (c *Conn) waitForCleanup() {
	// this blocks until readLoop and writeLoop
	// (and cleanup goroutine above) have exited
	c.wg.Wait()
	c.conn.CloseWrite()
	c.log(LogLevelInfo, "clean close complete")
	c.Delegate.OnClose(c)
}

func (c *Conn) onMessageFinish(m *Message) {
	c.msgResponseChan <- &msgResponse{m, Finish(m.ID), true, true}
}

func (c *Conn) onMessageRequeue(m *Message, delay time.Duration, backoff bool) {
	if delay == -1 {
		// linear delay
		delay = c.config.defaultRequeueDelay * time.Duration(m.Attempts)
		// bound the requeueDelay to configured max
		if delay > c.config.maxRequeueDelay {
			delay = c.config.maxRequeueDelay
		}
	}
	c.msgResponseChan <- &msgResponse{m, Requeue(m.ID, delay), false, backoff}
}

func (c *Conn) onMessageTouch(m *Message) {
	select {
	case c.cmdChan <- Touch(m.ID):
	case <-c.exitChan:
	}
}

func (c *Conn) log(lvl LogLevel, line string, args ...interface{}) {
	var prefix string

	if c.logger == nil {
		return
	}

	if c.logLvl > lvl {
		return
	}

	switch lvl {
	case LogLevelDebug:
		prefix = "DBG"
	case LogLevelInfo:
		prefix = "INF"
	case LogLevelWarning:
		prefix = "WRN"
	case LogLevelError:
		prefix = "ERR"
	}

	c.logger.Printf("%-4s %s %s", prefix,
		fmt.Sprintf(c.logFmt, c.String()),
		fmt.Sprintf(line, args...))
}
