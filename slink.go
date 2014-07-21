//
//   date  : 2014-07-20 14:31
//   author: xjdrew
//
package main

import (
	"crypto/rc4"
	"errors"
	"net"
	"time"
)

type StableLink struct {
	id uint32

	// build
	secret uint64
	index  uint32

	// conn pair
	remote *net.TCPConn
	local  *net.TCPConn

	// data
	received uint32
	sent     uint32
	used     int
	cache    []byte

	// chan
	errCh   chan *net.TCPConn
	reuseCh chan *ReuseConn

	workers int
	doneCh  chan bool

	//
	broken bool

	// rc4
	recvRc4 *rc4.Cipher
	sendRc4 *rc4.Cipher
}

// return value:
//    false: need break
func (s *StableLink) setErrConn(conn *net.TCPConn) bool {
	s.errCh <- conn
	if s.broken {
		return false
	}
	return true
}

func (s *StableLink) forwardToLocal() {
	defer s.done()
	cache := make([]byte, 4096)
	remote, local := s.remote, s.local

	for {
		// pump from remote
		n, err := remote.Read(cache)
		if err != nil { // remote error
			if !s.setErrConn(remote) {
				break
			}
		}

		if !s.setErrConn(nil) {
			break
		}
		// reuse point
		if remote != s.remote || n == 0 {
			remote = s.remote
			// drop read from old remote
			continue
		}

		// pour into local
		s.received += uint32(n)
		s.recvRc4.XORKeyStream(cache[:n], cache[:n])
		err = WriteAll(local, cache[:n])
		if err != nil { // local error, shoud close link
			s.setErrConn(local)
			break
		}
	}
}

func (s *StableLink) forwardToRemote() {
	defer s.done()
	cache := make([]byte, 4096)
	remote, local := s.remote, s.local

	for {
		// pump from local
		n, err := local.Read(cache)
		if err != nil { // local error, shoud close link
			s.setErrConn(local)
			break
		}

		if !s.setErrConn(nil) {
			break
		}

		// reuse point
		if remote != s.remote {
			remote = s.remote
		}

		// pour into remote
		// cache last sent
		s.sendRc4.XORKeyStream(cache[:n], cache[:n])

		s.sent += uint32(n)
		if s.used+n > cap(s.cache) {
			s.used = cap(s.cache) - n
			copy(s.cache, s.cache[:s.used])
		}
		copy(s.cache[s.used:], cache[:n])
		s.used += n

		err = WriteAll(remote, cache[:n])
		if err != nil {
			if !s.setErrConn(remote) {
				break
			}
		}
	}
}

func (s *StableLink) waitReuse() *ReuseConn {
	for {
		var rc *ReuseConn
		var conn *net.TCPConn
		select {
		case conn = <-s.errCh:
		case rc = <-s.reuseCh:
			return rc
		}
		if conn == s.local { // local error
			return nil
		} else if conn == s.remote { // remote error
			select {
			case rc = <-s.reuseCh:
			case <-time.After(time.Second * time.Duration(options.Timeout)):
				Info("link(%d) wait reuse timeout", s.id)
			}
			return rc
		}
	}
}

func (s *StableLink) reuse(rc *ReuseConn) error {
	conn := rc.conn
	req := rc.req
	Info("link(%d) reuse conn:%v", s.id, conn.RemoteAddr())

	// index must be equal
	if req.index != s.index {
		Info("link(%d) reuse failed, index expired", s.id)
		conn.Close()
		return nil
	}

	var diff uint32
	if s.sent < req.received {
		diff = s.sent + (0xffffffff - req.received)
	} else {
		diff = s.sent - req.received
	}

	if diff > uint32(s.used) {
		Info("link(%d) reuse failed:%d", s.id, 406)
		WriteReuseConnResp(conn, 0, 406)
		conn.Close()
		return errors.New("406 buffer not enough")
	}

	err := WriteReuseConnResp(conn, s.received, 200)
	if err != nil {
		Error("link(%d) write reuse conn resp failed:%v", s.id, err.Error())
		conn.Close()
		return nil
	}

	// resend buffered
	if diff > 0 {
		Error("link(%d) resend buffer:%d", s.id, diff)
		from := uint32(s.used) - diff
		err = WriteAll(conn, s.cache[from:s.used])
		if err != nil {
			Error("link(%d) resend buffer:%v", s.id, err.Error())
			conn.Close()
			return nil
		}
	}

	// everything is ok, reuse
	Info("link(%d) reuse succeed:%v -> %v", s.id, s.remote.RemoteAddr(), conn.RemoteAddr())
	s.remote.Close()
	s.remote = conn
	return nil
}

func (s *StableLink) done() {
	s.doneCh <- true
}

// start forward
func (s *StableLink) Run() {
	s.workers = 1
	token, secret := GenToken(s.secret)
	s.secret = secret

	Info("link(%d) run, remote:%v, local:%v, secret:%x", s.id, s.remote.RemoteAddr(), s.local.RemoteAddr(), s.secret)
	err := WriteNewConnResp(s.remote, s.id, token)
	if err != nil {
		Error("link(%d) write new conn resp failed:%v", s.id, err.Error())
		return
	}

	key := make([]byte, 8)
	GenRC4Key(s.secret, 0, key)
	s.recvRc4, _ = rc4.NewCipher(key)
	s.sendRc4, _ = rc4.NewCipher(key)

	s.workers += 1
	go s.forwardToLocal()
	s.workers += 1
	go s.forwardToRemote()
	for {
		rc := s.waitReuse()
		if rc == nil {
			break
		}
		err := s.reuse(rc)
		if err != nil {
			break
		}
	}
	s.broken = true
}

func (s *StableLink) VerifyReuse(req *ReuseConnReq) uint32 {
	if s.index >= req.index {
		return 403
	}

	if !VerifySecret(s.secret, req) {
		return 401
	}

	// update index
	s.index = req.index
	return 200
}

func (s *StableLink) Reuse(rc *ReuseConn) {
	s.reuseCh <- rc
}

func (s *StableLink) StopReuse() {
	close(s.reuseCh)
}

func (s *StableLink) Wait() {
	s.remote.Close()
	s.local.Close()

	done := s.workers
	for {
		select {
		case <-s.errCh:
			// do nothing
		case <-s.doneCh:
			done -= 1
		case _, ok := <-s.reuseCh:
			if !ok {
				done -= 1
				s.reuseCh = nil
			}
		}
		if done == 0 {
			break
		}
	}

	//
	close(s.errCh)
	close(s.doneCh)
	Info("link(%d) close", s.id)
}

func NewStableLink(id uint32, remote *net.TCPConn, local *net.TCPConn, key uint64) *StableLink {
	link := new(StableLink)

	link.id = id
	link.secret = key
	link.remote = remote
	link.local = local

	link.errCh = make(chan *net.TCPConn)
	link.reuseCh = make(chan *ReuseConn)
	link.doneCh = make(chan bool)

	link.used = 0
	link.cache = make([]byte, options.SendBuf)

	link.broken = false
	return link
}
