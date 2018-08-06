package syslog

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"code.cloudfoundry.org/rfc5424"
)

// TODO: Address issues where messages are malformed but we are not notifying
// the user.

type Sink struct {
	Addr      string `json:"addr"`
	Namespace string `json:"namespace"`
	TLS       *TLS   `json:"tls"`

	conn               net.Conn
	maintainConnection func() error
}

type TLS struct {
	InsecureSkipVerify bool          `json:"insecure_skip_verify"`
	Timeout            time.Duration `json:"timeout"`
}

// Out writes fluentbit messages via syslog TCP (RFC 5424 and RFC 6587).
type Out struct {
	sinks map[string][]*Sink
}

// NewOut returns a new Out which handles both tcp and tls connections.
func NewOut(sinks []*Sink) *Out {
	m := make(map[string][]*Sink)
	for _, s := range sinks {
		if s.TLS != nil {
			s.maintainConnection = tlsMaintainConn(s)
		} else {
			s.maintainConnection = tcpMaintainConn(s)
		}

		m[s.Namespace] = append(m[s.Namespace], s)
	}

	return &Out{
		sinks: m,
	}
}

// Write takes a record, timestamp, and tag, converts it into a syslog message
// and routes it to the connection with the matching namespace. If there are
// no connections configured for a record's namespace, it drops the message.
// If no connection is established one will be established per sink upon a
// Write operation. If all sinks for a namespace fail to write, Write will
// return an error.
func (o *Out) Write(
	record map[interface{}]interface{},
	ts time.Time,
	tag string,
) error {
	msg, namespace := convert(record, ts, tag)

	namespaceSinks, ok := o.sinks[namespace]
	if !ok {
		// TODO: track ignored messages
		return nil
	}

	var errCount int
	for _, s := range namespaceSinks {
		if s.write(msg) != nil {
			errCount++
		}
	}

	if errCount == len(namespaceSinks) {
		return fmt.Errorf("failed to write to all sinks for namespace: %s", namespace)
	}
	return nil
}

// write writes a rfc5424 syslog message to the connection of the specified
// sink. It recreates the connection if one isn't established yet.
func (s *Sink) write(m *rfc5424.Message) error {
	err := s.maintainConnection()
	if err != nil {
		return err
	}

	_, err = m.WriteTo(s.conn)
	if err != nil {
		s.conn = nil
		return err
	}
	return nil
}

func tlsMaintainConn(s *Sink) func() error {
	return func() error {
		if s.conn == nil {
			dialer := net.Dialer{
				Timeout: s.TLS.Timeout,
			}
			var conn net.Conn // conn needs to be of type net.Conn, not *tls.Conn
			conn, err := tls.DialWithDialer(
				&dialer,
				"tcp",
				s.Addr,
				&tls.Config{
					InsecureSkipVerify: s.TLS.InsecureSkipVerify,
				},
			)
			if err == nil {
				s.conn = conn
			}
			return err
		}
		return nil
	}
}

func tcpMaintainConn(s *Sink) func() error {
	return func() error {
		if s.conn == nil {
			conn, err := net.Dial("tcp", s.Addr)
			s.conn = conn
			return err
		}
		return nil
	}
}

func convert(
	record map[interface{}]interface{},
	ts time.Time,
	tag string,
) (*rfc5424.Message, string) {
	var (
		logmsg []byte
		k8sMap map[interface{}]interface{}
	)

	for k, v := range record {
		key, ok := k.(string)
		if !ok {
			continue
		}

		switch key {
		case "log":
			v2, ok2 := v.([]byte)
			if !ok2 {
				continue
			}
			logmsg = v2
		case "kubernetes":
			v2, ok2 := v.(map[interface{}]interface{})
			if !ok2 {
				continue
			}
			k8sMap = v2
		}
	}

	var (
		host          string
		appName       string
		podName       string
		namespaceName string
		containerName string
	)
	for k, v := range k8sMap {
		key, ok := k.(string)
		if !ok {
			continue
		}

		switch key {
		case "host":
			v2, ok2 := v.([]byte)
			if !ok2 {
				continue
			}
			host = string(v2)
		case "container_name":
			v2, ok2 := v.([]byte)
			if !ok2 {
				continue
			}
			containerName = string(v2)
		case "pod_name":
			v2, ok2 := v.([]byte)
			if !ok2 {
				continue
			}
			podName = string(v2)
		case "namespace_name":
			v2, ok2 := v.([]byte)
			if !ok2 {
				continue
			}
			namespaceName = string(v2)
		}
	}

	if len(k8sMap) != 0 {
		// sample: kube-system/pod/kube-dns-86f4d74b45-lfgj7/dnsmasq
		appName = fmt.Sprintf(
			"%s/%s/%s/%s",
			namespaceName,
			"pod",
			podName,
			containerName,
		)
		// APP-NAME is limited to 48 chars in RFC 5424
		// https://tools.ietf.org/html/rfc5424#section-6
		if len(appName) > 48 {
			appName = appName[:48]
		}
	}

	if !bytes.HasSuffix(logmsg, []byte("\n")) {
		logmsg = append(logmsg, byte('\n'))
	}

	return &rfc5424.Message{
		Priority:  rfc5424.Info + rfc5424.User,
		Timestamp: ts,
		Hostname:  host,
		AppName:   appName,
		Message:   logmsg,
	}, namespaceName
}
