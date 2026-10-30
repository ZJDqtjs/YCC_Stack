package vmhost

import (
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"starstack/internal/vm"
)

// The provider's IPv6 ingress is handled by this small proxy rather than by
// QEMU's slirp hostfwd. slirp's inbound forwarding is IPv4-only in most builds,
// and even where IPv6 is compiled in, the host-address syntax varies. Binding
// our own ipv6-only listener and dialling QEMU's IPv4 loopback forward keeps the
// public contract ("[seller IPv6]:port reaches the guest") the same on Linux,
// Windows and macOS.
type forwarder struct {
	closers []io.Closer
	once    sync.Once
}

func (f *forwarder) Close() {
	f.once.Do(func() {
		for _, c := range f.closers {
			c.Close()
		}
	})
}

func startForwarder(plan *vm.Plan) (*forwarder, error) {
	f := &forwarder{}
	if plan.Network.Mode == vm.NetSlirp {
		for _, p := range plan.Network.Forwards {
			target := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.HostPort))
			if p.Proto == "udp" {
				conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: p.HostPort})
				if err != nil {
					f.Close()
					return nil, err
				}
				f.closers = append(f.closers, conn)
				go proxyUDP(conn, target)
				continue
			}
			ln, err := net.Listen("tcp6", "[::]:"+strconv.Itoa(p.HostPort))
			if err != nil {
				f.Close()
				return nil, err
			}
			f.closers = append(f.closers, ln)
			go proxyTCP(ln, target)
		}
	}
	if plan.Network.ConsolePort > 0 {
		port := plan.Network.ConsolePort
		ln, err := net.Listen("tcp6", "[::]:"+strconv.Itoa(port))
		if err != nil {
			f.Close()
			return nil, err
		}
		f.closers = append(f.closers, ln)
		go proxyTCP(ln, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	return f, nil
}

func proxyTCP(ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(src net.Conn) {
			defer src.Close()
			dst, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				return
			}
			defer dst.Close()
			done := make(chan struct{}, 2)
			go func() {
				io.Copy(dst, src)
				if tc, ok := dst.(*net.TCPConn); ok {
					tc.CloseWrite()
				}
				done <- struct{}{}
			}()
			go func() {
				io.Copy(src, dst)
				if tc, ok := src.(*net.TCPConn); ok {
					tc.CloseWrite()
				}
				done <- struct{}{}
			}()
			<-done
		}(c)
	}
}

type udpSession struct {
	conn   *net.UDPConn
	client *net.UDPAddr
	last   time.Time
}

func proxyUDP(ln *net.UDPConn, target string) {
	var mu sync.Mutex
	sessions := map[string]*udpSession{}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				mu.Lock()
				for k, s := range sessions {
					if now.Sub(s.last) > 60*time.Second {
						s.conn.Close()
						delete(sessions, k)
					}
				}
				mu.Unlock()
			}
		}
	}()
	defer close(stop)
	defer func() {
		mu.Lock()
		for _, s := range sessions {
			s.conn.Close()
		}
		mu.Unlock()
	}()
	buf := make([]byte, 65535)
	for {
		n, client, err := ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		key := client.String()
		mu.Lock()
		s := sessions[key]
		if s == nil {
			conn, err := net.Dial("udp", target)
			if err != nil {
				mu.Unlock()
				continue
			}
			uc := conn.(*net.UDPConn)
			s = &udpSession{conn: uc, client: client, last: time.Now()}
			sessions[key] = s
			go func(key string, s *udpSession) {
				rb := make([]byte, 65535)
				for {
					n, err := s.conn.Read(rb)
					if err != nil {
						return
					}
					ln.WriteToUDP(rb[:n], s.client)
					mu.Lock()
					s.last = time.Now()
					mu.Unlock()
				}
			}(key, s)
		}
		s.last = time.Now()
		mu.Unlock()
		if _, err := s.conn.Write(buf[:n]); err != nil {
			log.Printf("vm udp forward write: %v", err)
		}
	}
}
