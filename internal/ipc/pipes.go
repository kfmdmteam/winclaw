//go:build windows

package ipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HandlerFunc is the type for IPC method handlers.
type HandlerFunc func(params json.RawMessage) (interface{}, error)

// IPCMessage is an inbound RPC request over the named pipe.
type IPCMessage struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// IPCResponse is an outbound RPC response over the named pipe.
type IPCResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// pipePathFor returns the canonical Windows named pipe path for a given session ID.
func pipePathFor(sessionID string) string {
	return fmt.Sprintf(`\\.\pipe\WinClaw-%s`, sessionID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Security descriptor helpers
// ─────────────────────────────────────────────────────────────────────────────

// Windows named-pipe access flags not directly re-exported by golang.org/x/sys/windows.
const (
	pipeAccessDuplex       = uint32(0x00000003)
	fileFlagOverlapped     = uint32(0x40000000)
	pipeTypeByte           = uint32(0x00000000)
	pipeWait               = uint32(0x00000000)
	pipeUnlimitedInstances = uint32(255)
	pipeBufferSize         = uint32(65536)

	// Composite access mask for full pipe access.
	pipeFullAccess = windows.GENERIC_READ | windows.GENERIC_WRITE | windows.SYNCHRONIZE
)

// buildCurrentUserDACL constructs a DACL granting full access only to the
// current OS user and to the built-in SYSTEM account.
func buildCurrentUserDACL() (*windows.ACL, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open process token: %w", err)
	}
	defer tok.Close()

	user, err := tok.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("get token user: %w", err)
	}

	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid, nil)
	if err != nil {
		return nil, fmt.Errorf("create SYSTEM SID: %w", err)
	}

	ea := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: pipeFullAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
			},
		},
		{
			AccessPermissions: pipeFullAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
	}

	return windows.ACLFromEntries(ea, nil)
}

// buildSecurityAttributes creates SECURITY_ATTRIBUTES that restrict pipe
// access to the current user and SYSTEM only.
func buildSecurityAttributes() (*windows.SecurityAttributes, error) {
	dacl, err := buildCurrentUserDACL()
	if err != nil {
		return nil, err
	}

	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("new security descriptor: %w", err)
	}
	if err := sd.SetDACL(dacl, true, false); err != nil {
		return nil, fmt.Errorf("set DACL: %w", err)
	}
	// SE_DACL_PROTECTED prevents inheriting ACEs from the parent object.
	_ = sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED)

	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// IPCServer
// ─────────────────────────────────────────────────────────────────────────────

// IPCServer listens on a per-session Windows named pipe and dispatches
// length-prefixed JSON-RPC messages to registered handlers.
type IPCServer struct {
	pipePath string
	listener net.Listener
	handlers map[string]HandlerFunc
	mu       sync.RWMutex
	cancel   context.CancelFunc
}

// NewIPCServer creates the named pipe listener and prepares the server.
// The pipe is locked to the current OS user and SYSTEM via a custom DACL.
func NewIPCServer(sessionID string) (*IPCServer, error) {
	path := pipePathFor(sessionID)

	sa, err := buildSecurityAttributes()
	if err != nil {
		return nil, fmt.Errorf("ipc: build security attributes: %w", err)
	}

	ln, err := newPipeListener(path, sa)
	if err != nil {
		return nil, fmt.Errorf("ipc: create named pipe listener: %w", err)
	}

	return &IPCServer{
		pipePath: path,
		listener: ln,
		handlers: make(map[string]HandlerFunc),
	}, nil
}

// Register adds a handler for the given RPC method name.  Thread-safe.
func (s *IPCServer) Register(method string, handler HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler
}

// Start begins accepting connections and blocking until ctx is cancelled or
// Stop is called.
func (s *IPCServer) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	// Close listener when the context is done so Accept unblocks.
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("ipc: accept: %w", err)
			}
		}
		go s.handleConn(conn)
	}
}

// Stop shuts down the server.
func (s *IPCServer) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.listener != nil {
		s.listener.Close()
	}
}

func (s *IPCServer) handleConn(conn net.Conn) {
	defer conn.Close()
	for {
		msg, err := readIPCMessage(conn)
		if err != nil {
			// io.EOF / pipe closed — normal client disconnect.
			return
		}
		resp := s.dispatch(msg)
		if err := writeJSON(conn, resp); err != nil {
			return
		}
	}
}

func (s *IPCServer) dispatch(msg *IPCMessage) *IPCResponse {
	s.mu.RLock()
	handler, ok := s.handlers[msg.Method]
	s.mu.RUnlock()

	resp := &IPCResponse{ID: msg.ID}
	if !ok {
		resp.Error = fmt.Sprintf("unknown method: %s", msg.Method)
		return resp
	}

	result, err := handler(msg.Params)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}

	raw, err := json.Marshal(result)
	if err != nil {
		resp.Error = fmt.Sprintf("marshal result: %v", err)
		return resp
	}
	resp.Result = raw
	return resp
}

// ─────────────────────────────────────────────────────────────────────────────
// IPCClient
// ─────────────────────────────────────────────────────────────────────────────

// IPCClient connects to an IPCServer via its named pipe.
type IPCClient struct {
	conn net.Conn
	mu   sync.Mutex
}

// NewIPCClient dials the named pipe for the given session ID.
func NewIPCClient(sessionID string) (*IPCClient, error) {
	path := pipePathFor(sessionID)
	conn, err := dialPipe(path)
	if err != nil {
		return nil, fmt.Errorf("ipc: dial %s: %w", path, err)
	}
	return &IPCClient{conn: conn}, nil
}

// Call invokes a remote method and returns the raw JSON result payload.
func (c *IPCClient) Call(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("ipc: marshal params: %w", err)
	}

	msg := &IPCMessage{
		ID:     randomID(),
		Method: method,
		Params: rawParams,
	}

	if err := writeJSON(c.conn, msg); err != nil {
		return nil, fmt.Errorf("ipc: write request: %w", err)
	}

	var resp IPCResponse
	if err := readJSONInto(c.conn, &resp); err != nil {
		return nil, fmt.Errorf("ipc: read response: %w", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("ipc: remote error: %s", resp.Error)
	}
	return resp.Result, nil
}

// Close releases the client connection.
func (c *IPCClient) Close() error {
	return c.conn.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Message framing: 4-byte big-endian length prefix + JSON body
// ─────────────────────────────────────────────────────────────────────────────

const maxMessageSize = 64 * 1024 * 1024 // 64 MiB

func writeJSON(w io.Writer, v interface{}) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(body)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readIPCMessage(r io.Reader) (*IPCMessage, error) {
	var msg IPCMessage
	if err := readJSONInto(r, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func readJSONInto(r io.Reader, dst interface{}) error {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(hdr)
	if size == 0 || size > maxMessageSize {
		return fmt.Errorf("ipc: invalid message length %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// ─────────────────────────────────────────────────────────────────────────────
// Named pipe listener (net.Listener over Windows HANDLE)
// ─────────────────────────────────────────────────────────────────────────────

// pipeListener implements net.Listener backed by Windows named pipes.
type pipeListener struct {
	path string
	sa   *windows.SecurityAttributes
	mu   sync.Mutex
	// first is the pre-created handle for the initial Accept call.
	first windows.Handle
	done  chan struct{}
	once  sync.Once
}

func newPipeListener(path string, sa *windows.SecurityAttributes) (*pipeListener, error) {
	h, err := createNamedPipeHandle(path, sa)
	if err != nil {
		return nil, err
	}
	l := &pipeListener{
		path:  path,
		sa:    sa,
		first: h,
		done:  make(chan struct{}),
	}
	return l, nil
}

func createNamedPipeHandle(path string, sa *windows.SecurityAttributes) (windows.Handle, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, err := windows.CreateNamedPipe(
		path16,
		pipeAccessDuplex|fileFlagOverlapped,
		pipeTypeByte|pipeWait,
		pipeUnlimitedInstances,
		pipeBufferSize,
		pipeBufferSize,
		0,
		sa,
	)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("CreateNamedPipe(%s): %w", path, err)
	}
	return h, nil
}

func (l *pipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	h := l.first
	if h != windows.InvalidHandle {
		l.first = windows.InvalidHandle
		l.mu.Unlock()
	} else {
		l.mu.Unlock()
		var err error
		h, err = createNamedPipeHandle(l.path, l.sa)
		if err != nil {
			select {
			case <-l.done:
				return nil, net.ErrClosed
			default:
				return nil, err
			}
		}
	}

	// Create a manual-reset event for the overlapped ConnectNamedPipe.
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("CreateEvent: %w", err)
	}
	defer windows.CloseHandle(event)

	ov := windows.Overlapped{HEvent: event}
	connErr := windows.ConnectNamedPipe(h, &ov)
	switch connErr {
	case nil:
		// Connection completed synchronously.
	case windows.ERROR_PIPE_CONNECTED:
		// A client was already waiting — connection is established.
	case windows.ERROR_IO_PENDING:
		// Wait for the connection or listener shutdown.
		done := l.done
		waitResult, _ := waitForHandles(event, done)
		if !waitResult {
			// Listener was closed.
			windows.CloseHandle(h)
			return nil, net.ErrClosed
		}
		// Confirm the overlapped operation completed successfully.
		var transferred uint32
		if err := windows.GetOverlappedResult(h, &ov, &transferred, true); err != nil {
			windows.CloseHandle(h)
			return nil, fmt.Errorf("GetOverlappedResult (connect): %w", err)
		}
	default:
		windows.CloseHandle(h)
		select {
		case <-l.done:
			return nil, net.ErrClosed
		default:
			return nil, fmt.Errorf("ConnectNamedPipe: %w", connErr)
		}
	}

	return newPipeFileConn(h), nil
}

// waitForHandles waits for either the event handle or the done channel to
// become signalled. Returns (true, nil) if the event fired, (false, nil) if
// the done channel was closed.
func waitForHandles(event windows.Handle, done <-chan struct{}) (bool, error) {
	// Windows constants for WaitForSingleObject return values.
	const (
		waitObject0  = uint32(0x00000000) // WAIT_OBJECT_0
		waitTimeout  = uint32(0x00000102) // WAIT_TIMEOUT
		waitFailed   = uint32(0xFFFFFFFF) // WAIT_FAILED
		pollInterval = uint32(200)        // 200 ms polling interval
	)

	for {
		r, err := windows.WaitForSingleObject(event, pollInterval)
		switch r {
		case waitObject0:
			return true, nil
		case waitTimeout:
			select {
			case <-done:
				return false, nil
			default:
				// Continue polling.
			}
		case waitFailed:
			return false, fmt.Errorf("WaitForSingleObject failed: %w", err)
		default:
			return false, fmt.Errorf("WaitForSingleObject unexpected result %d", r)
		}
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	l.mu.Lock()
	h := l.first
	l.first = windows.InvalidHandle
	l.mu.Unlock()
	if h != windows.InvalidHandle {
		windows.CloseHandle(h)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.path) }

// ─────────────────────────────────────────────────────────────────────────────
// dialPipe — client-side named-pipe connection
// ─────────────────────────────────────────────────────────────────────────────

func dialPipe(path string) (net.Conn, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const waitTimeout = uint32(5000) // 5 s
	for {
		h, err := windows.CreateFile(
			path16,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED,
			0,
		)
		if err == nil {
			return newPipeFileConn(h), nil
		}
		if err == windows.ERROR_PIPE_BUSY {
			if werr := windows.WaitNamedPipe(path16, waitTimeout); werr != nil {
				return nil, fmt.Errorf("WaitNamedPipe: %w", werr)
			}
			continue
		}
		return nil, fmt.Errorf("CreateFile (pipe): %w", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// pipeFileConn — net.Conn backed by a synchronous Windows HANDLE
// ─────────────────────────────────────────────────────────────────────────────

type pipeFileConn struct {
	h      windows.Handle
	mu     sync.Mutex
	closed bool
}

func newPipeFileConn(h windows.Handle) *pipeFileConn { return &pipeFileConn{h: h} }

func (c *pipeFileConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, fmt.Errorf("Read CreateEvent: %w", err)
	}
	defer windows.CloseHandle(event)

	ov := windows.Overlapped{HEvent: event}
	var n uint32
	readErr := windows.ReadFile(c.h, b, &n, &ov)
	if readErr == windows.ERROR_IO_PENDING {
		if werr := windows.GetOverlappedResult(c.h, &ov, &n, true); werr != nil {
			switch werr {
			case windows.ERROR_BROKEN_PIPE, windows.ERROR_PIPE_NOT_CONNECTED,
				windows.ERROR_HANDLE_EOF:
				return int(n), io.EOF
			}
			return int(n), werr
		}
		return int(n), nil
	}
	if readErr != nil {
		switch readErr {
		case windows.ERROR_BROKEN_PIPE, windows.ERROR_PIPE_NOT_CONNECTED,
			windows.ERROR_HANDLE_EOF:
			return int(n), io.EOF
		}
		return int(n), readErr
	}
	return int(n), nil
}

func (c *pipeFileConn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, fmt.Errorf("Write CreateEvent: %w", err)
	}
	defer windows.CloseHandle(event)

	ov := windows.Overlapped{HEvent: event}
	var n uint32
	writeErr := windows.WriteFile(c.h, b, &n, &ov)
	if writeErr == windows.ERROR_IO_PENDING {
		if werr := windows.GetOverlappedResult(c.h, &ov, &n, true); werr != nil {
			return int(n), werr
		}
		return int(n), nil
	}
	if writeErr != nil {
		return int(n), writeErr
	}
	return int(n), nil
}

func (c *pipeFileConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	windows.FlushFileBuffers(c.h)
	windows.DisconnectNamedPipe(c.h)
	return windows.CloseHandle(c.h)
}

func (c *pipeFileConn) LocalAddr() net.Addr               { return pipeAddr("local") }
func (c *pipeFileConn) RemoteAddr() net.Addr              { return pipeAddr("remote") }
func (c *pipeFileConn) SetDeadline(_ time.Time) error      { return nil }
func (c *pipeFileConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *pipeFileConn) SetWriteDeadline(_ time.Time) error { return nil }

// pipeAddr satisfies net.Addr for named-pipe connections.
type pipeAddr string

func (p pipeAddr) Network() string { return "pipe" }
func (p pipeAddr) String() string  { return string(p) }

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// randomID generates a short hex identifier using the Windows CSPRNG.
func randomID() string {
	var b [8]byte
	if err := windows.RtlGenRandom(b[:]); err != nil {
		// Fallback: use current time nanoseconds.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
