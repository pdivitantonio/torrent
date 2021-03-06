package udp_tracker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"time"

	"github.com/anacrolix/torrent/tracker"
)

type Action int32

const (
	Connect Action = iota
	Announce
	Scrape
	Error

	// BEP 41
	optionTypeEndOfOptions = 0
	optionTypeNOP          = 1
	optionTypeURLData      = 2
)

type ConnectionRequest struct {
	ConnectionId int64
	Action       int32
	TransctionId int32
}

type ConnectionResponse struct {
	ConnectionId int64
}

type ResponseHeader struct {
	Action        Action
	TransactionId int32
}

type RequestHeader struct {
	ConnectionId  int64
	Action        Action
	TransactionId int32
} // 16 bytes

type AnnounceResponseHeader struct {
	Interval int32
	Leechers int32
	Seeders  int32
}

type Peer struct {
	IP   [4]byte
	Port uint16
}

func init() {
	tracker.RegisterClientScheme("udp", newClient)
}

func newClient(url *url.URL) tracker.Client {
	return &client{
		url: url,
	}
}

func newTransactionId() int32 {
	return int32(rand.Uint32())
}

func timeout(contiguousTimeouts int) (d time.Duration) {
	if contiguousTimeouts > 8 {
		contiguousTimeouts = 8
	}
	d = 15 * time.Second
	for ; contiguousTimeouts > 0; contiguousTimeouts-- {
		d *= 2
	}
	return
}

type client struct {
	contiguousTimeouts   int
	connectionIdReceived time.Time
	connectionId         int64
	socket               net.Conn
	url                  *url.URL
}

func (c *client) URL() string {
	return c.url.String()
}

func (c *client) String() string {
	return c.URL()
}

func (c *client) Announce(req *tracker.AnnounceRequest) (res tracker.AnnounceResponse, err error) {
	if !c.connected() {
		err = tracker.ErrNotConnected
		return
	}
	reqURI := c.url.RequestURI()
	// Clearly this limits the request URI to 255 bytes. BEP 41 supports
	// longer but I'm not fussed.
	options := append([]byte{optionTypeURLData, byte(len(reqURI))}, []byte(reqURI)...)
	b, err := c.request(Announce, req, options)
	if err != nil {
		return
	}
	var h AnnounceResponseHeader
	err = readBody(b, &h)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		err = fmt.Errorf("error parsing announce response: %s", err)
		return
	}
	res.Interval = h.Interval
	res.Leechers = h.Leechers
	res.Seeders = h.Seeders
	for {
		var p Peer
		err = binary.Read(b, binary.BigEndian, &p)
		switch err {
		case nil:
		case io.EOF:
			err = nil
			fallthrough
		default:
			return
		}
		res.Peers = append(res.Peers, tracker.Peer{
			IP:   p.IP[:],
			Port: int(p.Port),
		})
	}
}

// body is the binary serializable request body. trailer is optional data
// following it, such as for BEP 41.
func (c *client) write(h *RequestHeader, body interface{}, trailer []byte) (err error) {
	buf := &bytes.Buffer{}
	err = binary.Write(buf, binary.BigEndian, h)
	if err != nil {
		panic(err)
	}
	if body != nil {
		err = binary.Write(buf, binary.BigEndian, body)
		if err != nil {
			panic(err)
		}
	}
	_, err = buf.Write(trailer)
	if err != nil {
		return
	}
	n, err := c.socket.Write(buf.Bytes())
	if err != nil {
		return
	}
	if n != buf.Len() {
		panic("write should send all or error")
	}
	return
}

func read(r io.Reader, data interface{}) error {
	return binary.Read(r, binary.BigEndian, data)
}

func write(w io.Writer, data interface{}) error {
	return binary.Write(w, binary.BigEndian, data)
}

// args is the binary serializable request body. trailer is optional data
// following it, such as for BEP 41.
func (c *client) request(action Action, args interface{}, options []byte) (responseBody *bytes.Reader, err error) {
	tid := newTransactionId()
	err = c.write(&RequestHeader{
		ConnectionId:  c.connectionId,
		Action:        action,
		TransactionId: tid,
	}, args, options)
	if err != nil {
		return
	}
	c.socket.SetReadDeadline(time.Now().Add(timeout(c.contiguousTimeouts)))
	b := make([]byte, 0x10000) // IP limits packet size to 64KB
	for {
		var n int
		n, err = c.socket.Read(b)
		if opE, ok := err.(*net.OpError); ok {
			if opE.Timeout() {
				c.contiguousTimeouts++
				return
			}
		}
		if err != nil {
			return
		}
		buf := bytes.NewBuffer(b[:n])
		var h ResponseHeader
		err = binary.Read(buf, binary.BigEndian, &h)
		switch err {
		case io.ErrUnexpectedEOF:
			continue
		case nil:
		default:
			return
		}
		if h.TransactionId != tid {
			continue
		}
		c.contiguousTimeouts = 0
		if h.Action == Error {
			err = errors.New(buf.String())
		}
		responseBody = bytes.NewReader(buf.Bytes())
		return
	}
}

func readBody(r *bytes.Reader, data ...interface{}) (err error) {
	for _, datum := range data {
		err = binary.Read(r, binary.BigEndian, datum)
		if err != nil {
			break
		}
	}
	return
}

func (c *client) connected() bool {
	return !c.connectionIdReceived.IsZero() && time.Now().Before(c.connectionIdReceived.Add(time.Minute))
}

func (c *client) Connect() (err error) {
	if c.connected() {
		return nil
	}
	c.connectionId = 0x41727101980
	if c.socket == nil {
		c.socket, err = net.Dial("udp", c.url.Host)
		if err != nil {
			return
		}
	}
	b, err := c.request(Connect, nil, nil)
	if err != nil {
		return
	}
	var res ConnectionResponse
	err = readBody(b, &res)
	if err != nil {
		return
	}
	c.connectionId = res.ConnectionId
	c.connectionIdReceived = time.Now()
	return
}
