package memcache

import (
	"bytes"
	"net"
	"sync/atomic"
	"time"

	"github.com/felixhao/overlord/lib/bufio"
	"github.com/felixhao/overlord/lib/conv"
	"github.com/felixhao/overlord/lib/pool"
	"github.com/felixhao/overlord/lib/stat"
	"github.com/felixhao/overlord/proto"
	"github.com/pkg/errors"
)

const (
	handlerOpening = int32(0)
	handlerClosed  = int32(1)

	handlerWriteBufferSize = 8 * 1024   // NOTE: write command, so relatively small
	handlerReadBufferSize  = 128 * 1024 // NOTE: read data, so relatively large
)

type handler struct {
	cluster string
	addr    string
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	bss     [][]byte
	buf     []byte

	readTimeout  time.Duration
	writeTimeout time.Duration

	closed int32
}

// Dial returns pool Dial func.
func Dial(cluster, addr string, dialTimeout, readTimeout, writeTimeout time.Duration) (dial func() (pool.Conn, error)) {
	dial = func() (pool.Conn, error) {
		conn, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err != nil {
			return nil, err
		}
		h := &handler{
			cluster:      cluster,
			addr:         addr,
			conn:         conn,
			bw:           bufio.NewWriterSize(conn, handlerWriteBufferSize),
			br:           bufio.NewReaderSize(conn, handlerReadBufferSize),
			bss:          make([][]byte, 2), // NOTE: like: 'VALUE a_11 0 0 3\r\naaa\r\nEND\r\n', and not copy 'END\r\n'
			readTimeout:  readTimeout,
			writeTimeout: writeTimeout,
		}
		return h, nil
	}
	return
}

// Handle call server node by request and read response returned.
func (h *handler) Handle(req *proto.Request) (resp *proto.Response, err error) {
	if h.Closed() {
		err = errors.Wrap(ErrClosed, "MC Handler handle request")
		return
	}
	mcr, ok := req.Proto().(*MCRequest)
	if !ok {
		err = errors.Wrap(ErrAssertRequest, "MC Handler handle assert MCRequest")
		return
	}
	if h.writeTimeout > 0 {
		h.conn.SetWriteDeadline(time.Now().Add(h.writeTimeout))
	}
	h.bw.WriteString(mcr.rTp.String())
	h.bw.WriteByte(spaceByte)
	if mcr.rTp == RequestTypeGat || mcr.rTp == RequestTypeGats {
		h.bw.Write(mcr.data) // NOTE: exptime
		h.bw.WriteByte(spaceByte)
		h.bw.Write(mcr.key)
		h.bw.Write(crlfBytes)
	} else {
		h.bw.Write(mcr.key)
		h.bw.Write(mcr.data)
	}
	if err = h.bw.Flush(); err != nil {
		err = errors.Wrap(err, "MC Handler handle flush request bytes")
		return
	}
	if h.readTimeout > 0 {
		h.conn.SetReadDeadline(time.Now().Add(h.readTimeout))
	}
	bs, err := h.br.ReadBytes(delim)
	if err != nil {
		err = errors.Wrap(err, "MC Handler handle read response bytes")
		return
	}
	if mcr.rTp == RequestTypeGet || mcr.rTp == RequestTypeGets || mcr.rTp == RequestTypeGat || mcr.rTp == RequestTypeGats {
		if !bytes.Equal(bs, endBytes) {
			stat.Hit(h.cluster, h.addr)
			c := bytes.Count(bs, spaceBytes)
			if c < 3 {
				err = errors.Wrap(ErrBadResponse, "MC Handler handle read response bytes split")
				return
			}
			var (
				lenBs  []byte
				length int64
			)
			i := bytes.IndexByte(bs, spaceByte) + 1 // VALUE <key> <flags> <bytes> [<cas unique>]\r\n
			i = i + bytes.IndexByte(bs[i:], spaceByte) + 1
			i = i + bytes.IndexByte(bs[i:], spaceByte) + 1
			if c == 3 { // NOTE: if c==3, means get|gat
				lenBs = bs[i:]
				l := len(lenBs)
				if l < 2 {
					err = errors.Wrap(ErrBadResponse, "MC Handler handle read response bytes check")
					return
				}
				lenBs = lenBs[:l-2] // NOTE: get|gat contains '\r\n'
			} else { // NOTE: if c>3, means gets|gats
				j := i + bytes.IndexByte(bs[i:], spaceByte)
				lenBs = bs[i:j]
			}
			if length, err = conv.Btoi(lenBs); err != nil {
				err = errors.Wrap(ErrBadResponse, "MC Handler handle read response bytes length")
				return
			}
			var bs2 []byte
			if bs2, err = h.br.ReadFull(int(length + 2)); err != nil { // NOTE: +2 read contains '\r\n'
				err = errors.Wrap(ErrBadResponse, "MC Handler handle read response bytes read")
				return
			}
			h.bss = h.bss[:2]
			h.bss[0] = bs
			h.bss[1] = bs2
			tl := len(bs) + len(bs2)
			var bs3 []byte
			for !bytes.Equal(bs3, endBytes) {
				if bs3 != nil { // NOTE: here, avoid copy 'END\r\n'
					h.bss = append(h.bss, bs3)
					tl += len(bs3)
				}
				if h.readTimeout > 0 {
					h.conn.SetReadDeadline(time.Now().Add(h.readTimeout))
				}
				if bs3, err = h.br.ReadBytes(delim); err != nil {
					err = errors.Wrap(err, "MC Handler handle reread response bytes")
					return
				}
			}
			const endBytesLen = 5 // NOTE: endBytes length
			tmp := h.makeBytes(tl + endBytesLen)
			off := 0
			for i := range h.bss {
				copy(tmp[off:], h.bss[i])
				off += len(h.bss[i])
			}
			copy(tmp[off:], endBytes)
			bs = tmp
		} else {
			stat.Miss(h.cluster, h.addr)
		}
	}
	resp = &proto.Response{Type: proto.CacheTypeMemcache}
	pr := &MCResponse{rTp: mcr.rTp, data: bs}
	resp.WithProto(pr)
	return
}

func (h *handler) Close() error {
	if atomic.CompareAndSwapInt32(&h.closed, handlerOpening, handlerClosed) {
		return h.conn.Close()
	}
	return nil
}

func (h *handler) Closed() bool {
	return atomic.LoadInt32(&h.closed) == handlerClosed
}

func (h *handler) makeBytes(n int) (ss []byte) {
	switch {
	case n == 0:
		return []byte{}
	case n >= handlerWriteBufferSize:
		return make([]byte, n)
	default:
		if len(h.buf) < n {
			h.buf = make([]byte, handlerReadBufferSize)
		}
		ss, h.buf = h.buf[:n:n], h.buf[n:]
		return ss
	}
}
