package vmhost

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// qmpConn is the minimum QEMU monitor connection this agent needs: set the VNC
// password after boot, ask whether the guest is running, and ask it to shut
// down. QMP is spoken over a loopback TCP socket with no authentication, which
// is why QEMU is started with the monitor on 127.0.0.1 only.
type qmpConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialQMP(port int) (*qmpConn, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	q := &qmpConn{c: c, r: bufio.NewReaderSize(c, 64<<10)}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := q.r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return nil, err
	}
	var greeting map[string]any
	if err := json.Unmarshal(line, &greeting); err != nil {
		c.Close()
		return nil, fmt.Errorf("QMP greeting: %w", err)
	}
	if _, ok := greeting["QMP"]; !ok {
		c.Close()
		return nil, fmt.Errorf("not a QMP greeting: %s", line)
	}
	return q, nil
}

func (q *qmpConn) Close() error {
	q.c.Close()
	return nil
}

// Cmd sends one command and waits for its reply, skipping asynchronous events.
func (q *qmpConn) Cmd(execute string, args map[string]any) (map[string]any, error) {
	req := map[string]any{"execute": execute}
	if args != nil {
		req["arguments"] = args
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	q.c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := q.c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if e, ok := msg["error"]; ok {
			return nil, fmt.Errorf("qmp %s: %v", execute, e)
		}
		ret, ok := msg["return"]
		if !ok {
			continue // event
		}
		if m, ok := ret.(map[string]any); ok {
			return m, nil
		}
		return map[string]any{"value": ret}, nil
	}
}

func (q *qmpConn) status() (string, error) {
	r, err := q.Cmd("query-status", nil)
	if err != nil {
		return "", err
	}
	s, _ := r["status"].(string)
	return s, nil
}
