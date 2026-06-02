package daemon

import (
	"bytes"
	"net"
	"sync"
)

const MaxBufferSize = 256 * 1024

type Broadcaster struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	clients map[*broadcastClient]struct{}
	closed  bool
	dropped int64
}

type broadcastClient struct {
	conn   net.Conn
	ch     chan []byte
	replay []byte
	mu     sync.Mutex
	closed bool
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients: make(map[*broadcastClient]struct{}),
	}
}

func (b *Broadcaster) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return len(p), nil
	}
	n, err := b.buffer.Write(p)
	if err != nil {
		b.mu.Unlock()
		return n, err
	}
	if b.buffer.Len() > MaxBufferSize {
		excess := b.buffer.Len() - MaxBufferSize
		b.dropped += int64(excess)
		retained := make([]byte, MaxBufferSize)
		copy(retained, b.buffer.Bytes()[excess:])
		b.buffer = *bytes.NewBuffer(retained)
	}

	clients := make([]*broadcastClient, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.Unlock()

	for _, conn := range clients {
		if !conn.enqueue(p) {
			b.removeClient(conn)
		}
	}

	return n, nil
}

func (b *Broadcaster) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *Broadcaster) AddClient(conn net.Conn) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = conn.Close()
		return
	}
	client := &broadcastClient{conn: conn, ch: make(chan []byte, 64), replay: append([]byte(nil), b.buffer.Bytes()...)}
	b.clients[client] = struct{}{}
	b.mu.Unlock()

	go client.run(b)
}

func (b *Broadcaster) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	clients := make([]*broadcastClient, 0, len(b.clients))
	for client := range b.clients {
		clients = append(clients, client)
	}
	b.clients = make(map[*broadcastClient]struct{})
	b.mu.Unlock()

	for _, client := range clients {
		client.close()
	}
}

func (b *Broadcaster) removeClient(client *broadcastClient) {
	b.mu.Lock()
	delete(b.clients, client)
	b.mu.Unlock()
	client.close()
}

func (c *broadcastClient) enqueue(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	payload := append([]byte(nil), data...)
	select {
	case c.ch <- payload:
		return true
	default:
		return false
	}
}

func (c *broadcastClient) run(b *Broadcaster) {
	if len(c.replay) > 0 {
		if _, err := c.conn.Write(c.replay); err != nil {
			b.removeClient(c)
			return
		}
	}

	for data := range c.ch {
		if len(data) == 0 {
			continue
		}
		if _, err := c.conn.Write(data); err != nil {
			b.removeClient(c)
			return
		}
	}
	_ = c.conn.Close()
}

func (c *broadcastClient) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.ch)
	c.mu.Unlock()
	_ = c.conn.Close()
}
