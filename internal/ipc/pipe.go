//go:build windows

// Package ipc implements a named-pipe channel between the WinClaw service
// (server) and the terminal or any other client on the same machine.
//
// Security model:
//   - Pipe name: \\.\pipe\WinClaw
//   - Pipe DACL: SYSTEM, LocalService, and the SID of the installing user only.
//   - Protocol: 4-byte big-endian length prefix + JSON payload per frame.
//   - One pipe instance; requests are serialised (the client blocks if busy).
package ipc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const PipeName = `\\.\pipe\WinClaw`

var (
	modKernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procDisconnectNamedPipe = modKernel32.NewProc("DisconnectNamedPipe")
)

func disconnectNamedPipe(h windows.Handle) {
	procDisconnectNamedPipe.Call(uintptr(h)) //nolint:errcheck
}

// ─────────────────────────────────────────────────────────────────────────────
// Protocol types
// ─────────────────────────────────────────────────────────────────────────────

// Request is the message sent from a client to the service.
type Request struct {
	Session string `json:"session"` // session ID or name; empty = use default
	Prompt  string `json:"prompt"`
}

// Chunk is one response frame streamed from the service to the client.
type Chunk struct {
	Text  string `json:"text,omitempty"`
	Tool  string `json:"tool,omitempty"` // non-empty when a tool is being called
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Frame I/O helpers
// ─────────────────────────────────────────────────────────────────────────────

// readFull reads exactly len(buf) bytes from the pipe, retrying on short reads.
func readFull(h windows.Handle, buf []byte) error {
	total := 0
	for total < len(buf) {
		var n uint32
		if err := windows.ReadFile(h, buf[total:], &n, nil); err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		total += int(n)
	}
	return nil
}

// writeAll writes all bytes to the pipe.
func writeAll(h windows.Handle, buf []byte) error {
	written := 0
	for written < len(buf) {
		var n uint32
		if err := windows.WriteFile(h, buf[written:], &n, nil); err != nil {
			return err
		}
		written += int(n)
	}
	return nil
}

// WriteFrame encodes v as JSON and sends it with a 4-byte big-endian length header.
func WriteFrame(h windows.Handle, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("pipe: marshal: %w", err)
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(data)))
	if err := writeAll(h, hdr); err != nil {
		return fmt.Errorf("pipe: write header: %w", err)
	}
	if err := writeAll(h, data); err != nil {
		return fmt.Errorf("pipe: write body: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed frame and returns the raw JSON bytes.
func ReadFrame(h windows.Handle) ([]byte, error) {
	hdr := make([]byte, 4)
	if err := readFull(h, hdr); err != nil {
		return nil, fmt.Errorf("pipe: read header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr)
	if n == 0 || n > 1<<20 { // 0 or > 1 MiB is invalid
		return nil, fmt.Errorf("pipe: invalid frame length %d", n)
	}
	buf := make([]byte, n)
	if err := readFull(h, buf); err != nil {
		return nil, fmt.Errorf("pipe: read body: %w", err)
	}
	return buf, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Server
// ─────────────────────────────────────────────────────────────────────────────

// Server owns the named pipe instance and accepts connections one at a time.
type Server struct {
	handle      windows.Handle
	ownerSIDStr string
	closeOnce   sync.Once
	closed      chan struct{}
}

// NewServer creates the named pipe with a protected DACL and returns a Server.
// ownerSIDStr is the string representation of the installing user's SID
// (e.g. "S-1-5-21-..."). Only that user, SYSTEM, and LocalService may connect.
func NewServer(ownerSIDStr string) (*Server, error) {
	sa, err := pipeSecurityAttributes(ownerSIDStr)
	if err != nil {
		return nil, err
	}

	namePtr, err := windows.UTF16PtrFromString(PipeName)
	if err != nil {
		return nil, fmt.Errorf("pipe: encode name: %w", err)
	}

	h, err := windows.CreateNamedPipe(
		namePtr,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		1,    // max instances — serialise requests
		4096, // output buffer
		4096, // input buffer
		0,    // default timeout
		sa,
	)
	if err != nil {
		return nil, fmt.Errorf("pipe: CreateNamedPipe: %w", err)
	}

	return &Server{
		handle:      h,
		ownerSIDStr: ownerSIDStr,
		closed:      make(chan struct{}),
	}, nil
}

// ServeForever accepts client connections in a loop and calls handler for each.
// handler receives the active pipe handle; it must call Disconnect when done.
// Returns when Close is called.
func (s *Server) ServeForever(handler func(h windows.Handle)) {
	for {
		if err := windows.ConnectNamedPipe(s.handle, nil); err != nil {
			// A client that connected before ConnectNamedPipe was called is fine.
			if err == windows.ERROR_PIPE_CONNECTED {
				handler(s.handle)
				disconnectNamedPipe(s.handle)
				continue
			}
			// Any other error — check if we're shutting down.
			select {
			case <-s.closed:
				return
			default:
				// Unexpected error; continue to avoid busy-spinning.
				continue
			}
		}
		handler(s.handle)
		disconnectNamedPipe(s.handle)
	}
}

// Close closes the server pipe handle, unblocking any pending ConnectNamedPipe.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = windows.CloseHandle(s.handle)
	})
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────────────────

// Send connects to the running WinClaw service, submits a prompt, and returns
// a channel that streams response Chunks. The channel is closed after the
// final Chunk (Done==true) is received or on error.
func Send(sessionID, prompt string) (<-chan Chunk, error) {
	namePtr, err := windows.UTF16PtrFromString(PipeName)
	if err != nil {
		return nil, fmt.Errorf("pipe: encode name: %w", err)
	}

	h, err := windows.CreateFile(
		namePtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,   // no sharing
		nil, // default security
		windows.OPEN_EXISTING,
		0, // synchronous
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("pipe: connect to %s: %w\n(is the WinClaw service running?)", PipeName, err)
	}

	req := Request{Session: sessionID, Prompt: prompt}
	if err := WriteFrame(h, req); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("pipe: send request: %w", err)
	}

	ch := make(chan Chunk, 16)
	go func() {
		defer windows.CloseHandle(h)
		defer close(ch)
		for {
			data, err := ReadFrame(h)
			if err != nil {
				ch <- Chunk{Done: true, Error: err.Error()}
				return
			}
			var c Chunk
			if err := json.Unmarshal(data, &c); err != nil {
				ch <- Chunk{Done: true, Error: fmt.Sprintf("parse chunk: %v", err)}
				return
			}
			ch <- c
			if c.Done {
				return
			}
		}
	}()
	return ch, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Security descriptor
// ─────────────────────────────────────────────────────────────────────────────

// pipeSecurityAttributes builds a SECURITY_ATTRIBUTES containing a protected
// DACL that grants GENERIC_ALL to SYSTEM, LocalService, and the owner SID.
func pipeSecurityAttributes(ownerSIDStr string) (*windows.SecurityAttributes, error) {
	// SDDL components:
	//   D:P            — protected DACL (no inheritance from parent)
	//   (A;;GA;;;SY)   — SYSTEM: GENERIC_ALL
	//   (A;;GA;;;LS)   — LocalService: GENERIC_ALL
	//   (A;;GA;;;<sid>) — installing user: GENERIC_ALL
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;LS)(A;;GA;;;%s)", ownerSIDStr)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("pipe: build security descriptor: %w", err)
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}, nil
}
