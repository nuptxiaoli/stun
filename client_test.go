package stun

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type TestAgent struct {
	h Handler
	e chan Event
}

func (n *TestAgent) SetHandler(h Handler) error {
	n.h = h
	return nil
}

func (n *TestAgent) Close() error {
	close(n.e)
	return nil
}

func (TestAgent) Collect(time.Time) error { return nil }

func (TestAgent) Process(m *Message) error { return nil }

func (n *TestAgent) Start(id [TransactionIDSize]byte, deadline time.Time) error {
	n.e <- Event{
		TransactionID: id,
	}
	return nil
}

func (n *TestAgent) Stop([TransactionIDSize]byte) error {
	return nil
}

type noopConnection struct{}

func (noopConnection) Write(b []byte) (int, error) {
	return len(b), nil
}

func (noopConnection) Read(b []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return 0, io.EOF
}

func (noopConnection) Close() error {
	return nil
}

func BenchmarkClient_Do(b *testing.B) {
	b.ReportAllocs()
	agent := &TestAgent{
		e: make(chan Event, 1000),
	}
	client, err := NewClient(noopConnection{},
		WithAgent(agent),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	noopF := func(event Event) {
		// pass
	}
	b.RunParallel(func(pb *testing.PB) {
		go func() {
			for e := range agent.e {
				agent.h(e)
			}
		}()
		m := New()
		m.NewTransactionID()
		m.Encode()
		for pb.Next() {
			if err := client.Do(m, noopF); err != nil {
				b.Error(err)
			}
		}
	})
}

type testConnection struct {
	write      func([]byte) (int, error)
	read       func([]byte) (int, error)
	b          []byte
	stopped    bool
	stoppedMux sync.Mutex
}

func (t *testConnection) Write(b []byte) (int, error) {
	return t.write(b)
}

func (t *testConnection) Close() error {
	t.stoppedMux.Lock()
	defer t.stoppedMux.Unlock()
	if t.stopped {
		return errors.New("already stopped")
	}
	t.stopped = true
	return nil
}

func (t *testConnection) Read(b []byte) (int, error) {
	t.stoppedMux.Lock()
	defer t.stoppedMux.Unlock()
	if t.stopped {
		return 0, io.EOF
	}
	if t.read != nil {
		return t.read(b)
	}
	return copy(b, t.b), nil
}

func TestClosedOrPanic(t *testing.T) {
	closedOrPanic(nil)
	closedOrPanic(ErrAgentClosed)
	func() {
		defer func() {
			r := recover()
			if r != io.EOF {
				t.Error(r)
			}
		}()
		closedOrPanic(io.EOF)
	}()
}

func TestClient_Start(t *testing.T) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	write := make(chan struct{}, 1)
	read := make(chan struct{}, 1)
	conn := &testConnection{
		b: response.Raw,
		read: func(i []byte) (int, error) {
			t.Log("waiting for read")
			select {
			case <-read:
				t.Log("reading")
				copy(i, response.Raw)
				return len(response.Raw), nil
			case <-time.After(time.Millisecond * 10):
				return 0, errors.New("read timed out")
			}
		},
		write: func(bytes []byte) (int, error) {
			t.Log("waiting for write")
			select {
			case <-write:
				t.Log("writing")
				return len(bytes), nil
			case <-time.After(time.Millisecond * 10):
				return 0, errors.New("write timed out")
			}
		},
	}
	c, err := NewClient(conn)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
		if err := c.Close(); err == nil {
			t.Error("second close should fail")
		}
		if err := c.Do(MustBuild(TransactionID), nil); err == nil {
			t.Error("Do after Close should fail")
		}
	}()
	m := MustBuild(response, BindingRequest)
	t.Log("init")
	got := make(chan struct{})
	write <- struct{}{}
	t.Log("starting the first transaction")
	if err := c.Start(m, func(event Event) {
		t.Log("got first transaction callback")
		if event.Error != nil {
			t.Error(event.Error)
		}
		got <- struct{}{}
	}); err != nil {
		t.Error(err)
	}
	t.Log("starting the second transaction")
	if err := c.Start(m, func(e Event) {
		t.Error("should not be called")
	}); err != ErrTransactionExists {
		t.Errorf("unexpected error %v", err)
	}
	read <- struct{}{}
	select {
	case <-got:
		// pass
	case <-time.After(time.Millisecond * 10):
		t.Error("timed out")
	}
}

func TestClient_Do(t *testing.T) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	conn := &testConnection{
		b: response.Raw,
		write: func(bytes []byte) (int, error) {
			return len(bytes), nil
		},
	}
	c, err := NewClient(conn)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
		if err := c.Close(); err == nil {
			t.Error("second close should fail")
		}
		if err := c.Do(MustBuild(TransactionID), nil); err == nil {
			t.Error("Do after Close should fail")
		}
	}()
	m := MustBuild(
		NewTransactionIDSetter(response.TransactionID),
	)
	if err := c.Do(m, func(event Event) {
		if event.Error != nil {
			t.Error(event.Error)
		}
	}); err != nil {
		t.Error(err)
	}
	m = MustBuild(TransactionID)
	if err := c.Do(m, nil); err != nil {
		t.Error(err)
	}
}

func TestCloseErr_Error(t *testing.T) {
	for id, c := range []struct {
		Err CloseErr
		Out string
	}{
		{CloseErr{}, "failed to close: <nil> (connection), <nil> (agent)"},
		{CloseErr{
			AgentErr: io.ErrUnexpectedEOF,
		}, "failed to close: <nil> (connection), unexpected EOF (agent)"},
		{CloseErr{
			ConnectionErr: io.ErrUnexpectedEOF,
		}, "failed to close: unexpected EOF (connection), <nil> (agent)"},
	} {
		if out := c.Err.Error(); out != c.Out {
			t.Errorf("[%d]: Error(%#v) %q (got) != %q (expected)",
				id, c.Err, out, c.Out,
			)
		}
	}
}

func TestStopErr_Error(t *testing.T) {
	for id, c := range []struct {
		Err StopErr
		Out string
	}{
		{StopErr{}, "error while stopping due to <nil>: <nil>"},
		{StopErr{
			Err: io.ErrUnexpectedEOF,
		}, "error while stopping due to <nil>: unexpected EOF"},
		{StopErr{
			Cause: io.ErrUnexpectedEOF,
		}, "error while stopping due to unexpected EOF: <nil>"},
	} {
		if out := c.Err.Error(); out != c.Out {
			t.Errorf("[%d]: Error(%#v) %q (got) != %q (expected)",
				id, c.Err, out, c.Out,
			)
		}
	}
}

type errorAgent struct {
	startErr        error
	stopErr         error
	closeErr        error
	setHandlerError error
}

func (a errorAgent) SetHandler(h Handler) error { return a.setHandlerError }

func (a errorAgent) Close() error { return a.closeErr }

func (errorAgent) Collect(time.Time) error { return nil }

func (errorAgent) Process(m *Message) error { return nil }

func (a errorAgent) Start(id [TransactionIDSize]byte, deadline time.Time) error {
	return a.startErr
}

func (a errorAgent) Stop([TransactionIDSize]byte) error {
	return a.stopErr
}

func TestClientAgentError(t *testing.T) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	conn := &testConnection{
		b: response.Raw,
		write: func(bytes []byte) (int, error) {
			return len(bytes), nil
		},
	}
	c, err := NewClient(conn,
		WithAgent(errorAgent{
			startErr: io.ErrUnexpectedEOF,
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	m := MustBuild(NewTransactionIDSetter(response.TransactionID))
	if err := c.Do(m, nil); err != nil {
		t.Error(err)
	}
	if err := c.Do(m, func(event Event) {
		if event.Error == nil {
			t.Error("error expected")
		}
	}); err != io.ErrUnexpectedEOF {
		t.Error("error expected")
	}
}

func TestClientConnErr(t *testing.T) {
	conn := &testConnection{
		write: func(bytes []byte) (int, error) {
			return 0, io.ErrClosedPipe
		},
	}
	c, err := NewClient(conn)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	m := MustBuild(TransactionID)
	if err := c.Do(m, nil); err == nil {
		t.Error("error expected")
	}
	if err := c.Do(m, NoopHandler); err == nil {
		t.Error("error expected")
	}
}

func TestClientConnErrStopErr(t *testing.T) {
	conn := &testConnection{
		write: func(bytes []byte) (int, error) {
			return 0, io.ErrClosedPipe
		},
	}
	c, err := NewClient(conn,
		WithAgent(errorAgent{
			stopErr: io.ErrUnexpectedEOF,
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	m := MustBuild(TransactionID)
	if err := c.Do(m, NoopHandler); err == nil {
		t.Error("error expected")
	}
}

func TestCallbackWaitHandler_setCallback(t *testing.T) {
	c := callbackWaitHandler{}
	defer func() {
		if err := recover(); err == nil {
			t.Error("should panic")
		}
	}()
	c.setCallback(nil)
}

func TestCallbackWaitHandler_HandleEvent(t *testing.T) {
	c := callbackWaitHandler{}
	defer func() {
		if err := recover(); err == nil {
			t.Error("should panic")
		}
	}()
	c.HandleEvent(Event{})
}

func TestNewClientNoConnection(t *testing.T) {
	c, err := NewClient(nil)
	if c != nil {
		t.Error("c should be nil")
	}
	if err != ErrNoConnection {
		t.Error("bad error")
	}
}

func TestDial(t *testing.T) {
	c, err := Dial("udp4", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err = c.Close(); err != nil {
			t.Error(err)
		}
	}()
}

func TestDialError(t *testing.T) {
	_, err := Dial("bad?network", "?????")
	if err == nil {
		t.Fatal("error expected")
	}
}
func TestClientCloseErr(t *testing.T) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	conn := &testConnection{
		b: response.Raw,
		write: func(bytes []byte) (int, error) {
			return len(bytes), nil
		},
	}
	c, err := NewClient(conn,
		WithAgent(errorAgent{
			closeErr: io.ErrUnexpectedEOF,
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err, ok := c.Close().(CloseErr); !ok || err.AgentErr != io.ErrUnexpectedEOF {
			t.Error("unexpected close err")
		}
	}()
}

type gcWaitAgent struct {
	gc chan struct{}
}

func (a *gcWaitAgent) SetHandler(h Handler) error {
	return nil
}

func (a *gcWaitAgent) Stop(id [TransactionIDSize]byte) error {
	return nil
}

func (a *gcWaitAgent) Close() error {
	close(a.gc)
	return nil
}

func (a *gcWaitAgent) Collect(time.Time) error {
	a.gc <- struct{}{}
	return nil
}

func (a *gcWaitAgent) Process(m *Message) error {
	return nil
}

func (a *gcWaitAgent) Start(id [TransactionIDSize]byte, deadline time.Time) error {
	return nil
}

func TestClientGC(t *testing.T) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	conn := &testConnection{
		b: response.Raw,
		write: func(bytes []byte) (int, error) {
			return len(bytes), nil
		},
	}
	agent := &gcWaitAgent{
		gc: make(chan struct{}),
	}
	c, err := NewClient(conn,
		WithAgent(agent),
		WithTimeoutRate(time.Millisecond),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err = c.Close(); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-agent.gc:
	case <-time.After(time.Millisecond * 200):
		t.Error("timed out")
	}
}

func TestClientCheckInit(t *testing.T) {
	if err := (&Client{}).Indicate(nil); err != ErrClientNotInitialized {
		t.Error("unexpected error")
	}
	if err := (&Client{}).Do(nil, nil); err != ErrClientNotInitialized {
		t.Error("unexpected error")
	}
}

func captureLog() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	f := log.Flags()
	log.SetFlags(0)
	return &buf, func() {
		log.SetFlags(f)
		log.SetOutput(os.Stderr)
	}
}

func TestClientFinalizer(t *testing.T) {
	buf, stopCapture := captureLog()
	defer stopCapture()
	clientFinalizer(nil) // should not panic
	clientFinalizer(&Client{})
	conn := &testConnection{
		write: func(bytes []byte) (int, error) {
			return 0, io.ErrClosedPipe
		},
	}
	c, err := NewClient(conn)
	if err != nil {
		log.Fatal(err)
	}
	clientFinalizer(c)
	clientFinalizer(c)
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	conn = &testConnection{
		b: response.Raw,
		write: func(bytes []byte) (int, error) {
			return len(bytes), nil
		},
	}
	c, err = NewClient(conn,
		WithAgent(errorAgent{
			closeErr: io.ErrUnexpectedEOF,
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	clientFinalizer(c)
	reader := bufio.NewScanner(buf)
	var lines int
	var expectedLines = []string{
		"client: called finalizer on non-closed client: client not initialized",
		"client: called finalizer on non-closed client",
		"client: called finalizer on non-closed client: failed to close: " +
			"<nil> (connection), unexpected EOF (agent)",
	}
	for reader.Scan() {
		if reader.Text() != expectedLines[lines] {
			t.Error(reader.Text(), "!=", expectedLines[lines])
		}
		lines++
	}
	if reader.Err() != nil {
		t.Error(err)
	}
	if lines != 3 {
		t.Error("incorrect count of log lines:", lines)
	}
}

func TestCallbackWaitHandler(t *testing.T) {
	h := callbackWaitHandlerPool.Get().(*callbackWaitHandler)
	for i := 0; i < 100; i++ {
		h.setCallback(func(event Event) {})
		go func() {
			time.Sleep(time.Microsecond * 100)
			h.HandleEvent(Event{})
		}()
		h.wait()
		h.reset()
	}
}

type manualCollector struct {
	f func(t time.Time)
}

func (m *manualCollector) Collect(t time.Time) {
	m.f(t)
}

func (m *manualCollector) Start(rate time.Duration, f func(t time.Time)) error {
	m.f = f
	return nil
}

func (m *manualCollector) Close() error {
	return nil
}

type manualClock struct {
	mux     sync.Mutex
	current time.Time
}

func (m *manualClock) Add(d time.Duration) time.Time {
	m.mux.Lock()
	v := m.current.Add(d)
	m.current = v
	m.mux.Unlock()
	return v
}

func (m *manualClock) Now() time.Time {
	m.mux.Lock()
	defer m.mux.Unlock()
	return m.current
}

type manualAgent struct {
	start func(id [TransactionIDSize]byte, deadline time.Time) error
	h     Handler
}

func (n *manualAgent) SetHandler(h Handler) error {
	n.h = h
	return nil
}

func (n *manualAgent) Close() error {
	return nil
}

func (manualAgent) Collect(time.Time) error { return nil }

func (manualAgent) Process(m *Message) error { return nil }

func (n *manualAgent) Start(id [TransactionIDSize]byte, deadline time.Time) error {
	return n.start(id, deadline)
}

func (n *manualAgent) Stop([TransactionIDSize]byte) error {
	return nil
}

func TestClientRetransmission(t *testing.T) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	connL, connR := net.Pipe()
	defer connL.Close()
	collector := new(manualCollector)
	clock := &manualClock{current: time.Now()}
	agent := &manualAgent{}
	attempt := 0
	agent.start = func(id [TransactionIDSize]byte, deadline time.Time) error {
		if attempt == 0 {
			attempt++
			go agent.h(Event{
				TransactionID: id,
				Error:         ErrTransactionTimeOut,
			})
		} else {
			go agent.h(Event{
				TransactionID: id,
				Message:       response,
			})
		}
		return nil
	}
	c, err := NewClient(connR,
		WithAgent(agent),
		WithClock(clock),
		WithCollector(collector),
		WithRTO(time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.SetRTO(time.Second)
	gotReads := make(chan struct{})
	go func() {
		buf := make([]byte, 1500)
		readN, readErr := connL.Read(buf)
		if readErr != nil {
			t.Error(readErr)
		}
		if !IsMessage(buf[:readN]) {
			t.Error("should be STUN")
		}
		readN, readErr = connL.Read(buf)
		if readErr != nil {
			t.Error(readErr)
		}
		if !IsMessage(buf[:readN]) {
			t.Error("should be STUN")
		}
		gotReads <- struct{}{}
	}()
	if doErr := c.Do(MustBuild(response, BindingRequest), func(event Event) {
		if event.Error != nil {
			t.Error("failed")
		}
	}); doErr != nil {
		t.Fatal(err)
	}
	<-gotReads
}

func testClientDoConcurrent(t *testing.T, concurrency int) {
	response := MustBuild(TransactionID, BindingSuccess)
	response.Encode()
	connL, connR := net.Pipe()
	defer connL.Close()
	collector := new(manualCollector)
	clock := &manualClock{current: time.Now()}
	agent := &manualAgent{}
	agent.start = func(id [TransactionIDSize]byte, deadline time.Time) error {
		go agent.h(Event{
			TransactionID: id,
			Message:       response,
		})
		return nil
	}
	c, err := NewClient(connR,
		WithAgent(agent),
		WithClock(clock),
		WithCollector(collector),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.SetRTO(time.Second)
	conns := new(sync.WaitGroup)
	wg := new(sync.WaitGroup)
	for i := 0; i < concurrency; i++ {
		conns.Add(1)
		go func() {
			defer conns.Done()
			buf := make([]byte, 1500)
			for {
				readN, readErr := connL.Read(buf)
				if readErr != nil {
					if readErr == io.EOF {
						break
					}
					t.Error(readErr)
				}
				if !IsMessage(buf[:readN]) {
					t.Error("should be STUN")
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if doErr := c.Do(MustBuild(TransactionID, BindingRequest), func(event Event) {
				if event.Error != nil {
					t.Error("failed")
				}
			}); doErr != nil {
				t.Error(doErr)
			}
		}()
	}
	wg.Wait()
	if connErr := connR.Close(); connErr != nil {
		t.Error(connErr)
	}
	conns.Wait()
}

func TestClient_DoConcurrent(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{
		1, 5, 10, 25, 100, 500,
	} {
		t.Run(fmt.Sprintf("%d", concurrency), func(t *testing.T) {
			testClientDoConcurrent(t, concurrency)
		})
	}
}

type errorCollector struct {
	startError error
	closeError error
}

func (c errorCollector) Start(rate time.Duration, f func(now time.Time)) error {
	return c.startError
}

func (c errorCollector) Close() error { return c.closeError }

func TestNewClient(t *testing.T) {
	t.Run("SetCallbackError", func(t *testing.T) {
		setHandlerError := errors.New("set handler error")
		if _, createErr := NewClient(noopConnection{},
			WithAgent(&errorAgent{
				setHandlerError: setHandlerError,
			}),
		); createErr != setHandlerError {
			t.Errorf("unexpected error returned: %v", createErr)
		}
	})
	t.Run("CollectorStartError", func(t *testing.T) {
		startError := errors.New("start error")
		if _, createErr := NewClient(noopConnection{},
			WithAgent(&TestAgent{}),
			WithCollector(errorCollector{
				startError: startError,
			}),
		); createErr != startError {
			t.Errorf("unexpected error returned: %v", createErr)
		}
	})
}

func TestClient_Close(t *testing.T) {
	t.Run("CollectorCloseError", func(t *testing.T) {
		closeErr := errors.New("start error")
		c, createErr := NewClient(noopConnection{},
			WithCollector(errorCollector{
				closeError: closeErr,
			}),
			WithAgent(&TestAgent{}),
		)
		if createErr != nil {
			t.Errorf("unexpected create error returned: %v", createErr)
		}
		gotCloseErr := c.Close()
		if gotCloseErr != closeErr {
			t.Errorf("unexpected close error returned: %v", gotCloseErr)
		}
	})
}
