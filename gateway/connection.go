package gateway

import (
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/segmentio/ksuid"
	"golang.org/x/crypto/ssh"
)

var (
	ErrServiceAlreadyRegistered = errors.New("gatewaysshd: service already registered")
	ErrServiceNotFound          = errors.New("gatewaysshd: service not found")
)

// a ssh connection
type Connection struct {
	id             string
	gateway        *Gateway
	conn           *ssh.ServerConn
	user           string
	remoteAddr     net.Addr
	localAddr      net.Addr
	sessions       []*Session
	sessionsClosed uint64
	tunnels        []*Tunnel
	tunnelsClosed  uint64
	services       map[string]map[uint16]bool
	lock           *sync.Mutex
	closeOnce      sync.Once
	usage          *usageStats
	admin          bool
	status         json.RawMessage
	location       map[string]interface{}
}

func newConnection(gateway *Gateway, conn *ssh.ServerConn, usage *usageStats, location map[string]interface{}) *Connection {
	log.Infof("new connection: user = %s, remote = %v, location = %v", conn.User(), conn.RemoteAddr(), location)

	admin := true
	if _, ok := conn.Permissions.Extensions["permit-port-forwarding"]; !ok {
		admin = false
	}

	connection := &Connection{
		id:         ksuid.New().String(),
		gateway:    gateway,
		conn:       conn,
		user:       conn.User(),
		remoteAddr: conn.RemoteAddr(),
		localAddr:  conn.LocalAddr(),
		services:   make(map[string]map[uint16]bool),
		lock:       &sync.Mutex{},
		usage:      usage,
		admin:      admin,
		location:   location,
	}
	return connection
}

// close the ssh connection
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		c.gateway.deleteConnection(c)

		for _, session := range c.Sessions() {
			session.Close()
		}

		for _, tunnel := range c.Tunnels() {
			tunnel.Close()
		}

		if err := c.conn.Close(); err != nil {
			log.Debugf("failed to close connection: %s", err)
		}

		log.Infof("connection closed: user = %s, remote = %v", c.user, c.remoteAddr)
	})
}

// sessions within a ssh connection
func (c *Connection) Sessions() []*Session {
	c.lock.Lock()
	defer c.lock.Unlock()

	sessions := make([]*Session, len(c.sessions))
	copy(sessions, c.sessions)
	return sessions
}

func (c *Connection) addSession(s *Session) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.sessions = append([]*Session{s}, c.sessions...)
}

func (c *Connection) deleteSession(s *Session) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// filter the list of channels
	sessions := make([]*Session, 0, len(c.sessions))
	for _, session := range c.sessions {
		if session != s {
			sessions = append(sessions, session)
		}
	}
	c.sessions = sessions
	c.sessionsClosed += 1
}

// tunnels within a ssh connection
func (c *Connection) Tunnels() []*Tunnel {
	c.lock.Lock()
	defer c.lock.Unlock()

	tunnels := make([]*Tunnel, len(c.tunnels))
	copy(tunnels, c.tunnels)
	return tunnels
}

func (c *Connection) addTunnel(t *Tunnel) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.tunnels = append([]*Tunnel{t}, c.tunnels...)
}

func (c *Connection) deleteTunnel(t *Tunnel) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// filter the list of channels
	tunnels := make([]*Tunnel, 0, len(c.tunnels))
	for _, tunnel := range c.tunnels {
		if tunnel != t {
			tunnels = append(tunnels, tunnel)
		}
	}
	c.tunnels = tunnels
	c.tunnelsClosed += 1
}

// returns the last used time of the connection
func (c *Connection) Used() time.Time {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.usage.used
}

// returns the list of services this connection advertises
func (c *Connection) Services() map[string][]uint16 {
	c.lock.Lock()
	defer c.lock.Unlock()

	services := make(map[string][]uint16)
	for host, ports := range c.services {
		for port, ok := range ports {
			if ok {
				services[host] = append(services[host], port)
			}
		}
	}

	return services
}

func (c *Connection) reportStatus(status json.RawMessage) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.status = status
}

func (c *Connection) gatherStatus() map[string]interface{} {
	c.lock.Lock()
	defer c.lock.Unlock()

	// services
	services := make(map[string][]uint16)
	for host, ports := range c.services {
		for port, ok := range ports {
			if ok {
				services[host] = append(services[host], port)
			}
		}
	}

	tunnels := make([]interface{}, 0, len(c.tunnels))
	for _, tunnel := range c.tunnels {
		tunnels = append(tunnels, tunnel.gatherStatus())
	}

	sessions := make([]interface{}, 0, len(c.sessions))
	for _, session := range c.sessions {
		sessions = append(sessions, session.gatherStatus())
	}

	return map[string]interface{}{
		"id":              c.id,
		"user":            c.user,
		"admin":           c.admin,
		"address":         c.remoteAddr.String(),
		"location":        c.location,
		"sessions":        sessions,
		"sessions_closed": c.sessionsClosed,
		"tunnels":         tunnels,
		"tunnels_closed":  c.tunnelsClosed,
		"created":         c.usage.created.Unix(),
		"used":            c.usage.used.Unix(),
		"up_time":         uint64(time.Since(c.usage.created).Seconds()),
		"idle_time":       uint64(time.Since(c.usage.used).Seconds()),
		"bytes_read":      c.usage.bytesRead,
		"bytes_written":   c.usage.bytesWritten,
		"services":        services,
		"status":          c.status,
	}
}

func (c *Connection) lookupService(host string, port uint16) bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	if _, ok := c.services[host]; !ok {
		return false
	}

	return c.services[host][port]
}

func (c *Connection) registerService(host string, port uint16) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if _, ok := c.services[host]; !ok {
		c.services[host] = make(map[uint16]bool)
	}
	if c.services[host][port] {
		return ErrServiceAlreadyRegistered
	}
	c.services[host][port] = true

	log.Debugf("registered service: user = %s, host = %s, port = %d", c.user, host, port)
	return nil
}

func (c *Connection) deregisterService(host string, port uint16) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if _, ok := c.services[host]; ok {
		delete(c.services[host], port)
	}

	log.Debugf("deregistered service: user = %s, host = %s, port = %d", c.user, host, port)
	return nil
}

func (c *Connection) handleRequests(requests <-chan *ssh.Request) {
	defer c.Close()

	for request := range requests {
		go c.handleRequest(request)
	}
}

func (c *Connection) handleChannels(channels <-chan ssh.NewChannel) {
	defer c.Close()

	for channel := range channels {
		go c.handleChannel(channel)
	}
}

func (c *Connection) handleRequest(request *ssh.Request) {
	log.Debugf("request received: type = %s, want_reply = %v, payload = %v", request.Type, request.WantReply, request.Payload)

	ok := false
	switch request.Type {
	case "tcpip-forward":
		request, err := unmarshalForwardRequest(request.Payload)
		if err != nil {
			log.Warningf("failed to decode request: %s", err)
			break
		}

		if request.Port == 0 {
			log.Warningf("requested forwarding port is not allowed: %d", request.Port)
			break
		}

		if err := c.registerService(request.Host, uint16(request.Port)); err != nil {
			log.Warningf("failed to register service in connection: %s", err)
			break
		}

		ok = true

	case "cancel-tcpip-forward":
		request, err := unmarshalForwardRequest(request.Payload)
		if err != nil {
			log.Warningf("failed to decode request: %s", err)
			break
		}

		if err := c.deregisterService(request.Host, uint16(request.Port)); err != nil {
			log.Warningf("failed to register service in connection: %s", err)
			break
		}

		ok = true

	}

	if request.WantReply {
		if err := request.Reply(ok, nil); err != nil {
			log.Warningf("failed to reply to request: %s", err)
		}
	}
}

func (c *Connection) handleChannel(newChannel ssh.NewChannel) {
	log.Debugf("new channel: type = %s, data = %v", newChannel.ChannelType(), newChannel.ExtraData())

	ok := false
	rejection := ssh.UnknownChannelType
	message := "unknown channel type"

	switch newChannel.ChannelType() {
	case "session":
		ok, rejection, message = c.handleSessionChannel(newChannel)
	case "direct-tcpip":
		ok, rejection, message = c.handleTunnelChannel(newChannel)
	}

	if ok {
		return
	}
	log.Debugf("channel rejected due to %d: %s", rejection, message)

	// reject the channel, by accepting it then immediately close
	// this is because Reject() leaks
	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Warningf("failed to reject channel: %s", err)
		return
	}
	go ssh.DiscardRequests(requests)

	if err := channel.Close(); err != nil {
		log.Warningf("failed to close rejected channel: %s", err)
		return
	}
}

func (c *Connection) handleSessionChannel(newChannel ssh.NewChannel) (bool, ssh.RejectionReason, string) {
	if len(newChannel.ExtraData()) > 0 {
		// do not accept extra data in connection channel request
		return false, ssh.Prohibited, "extra data not allowed"
	}

	// accept the channel
	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Warningf("failed to accept channel: %s", err)
		return true, 0, ""
	}

	// cannot return false from this point on
	// also need to accepted close the channel
	defer func() {
		if channel != nil {
			if err := channel.Close(); err != nil {
				log.Warningf("failed to close accepted channel: %s", err)
			}
		}
	}()

	session := newSession(c, channel, newChannel.ChannelType(), newChannel.ExtraData())
	c.addSession(session)

	// no failure
	go session.handleRequests(requests)

	// do not close channel on exit
	channel = nil
	return true, 0, ""
}

func (c *Connection) handleTunnelChannel(newChannel ssh.NewChannel) (bool, ssh.RejectionReason, string) {

	data, err := unmarshalTunnelData(newChannel.ExtraData())
	if err != nil {
		return false, ssh.UnknownChannelType, "failed to decode extra data"
	}

	// look up connection by name
	connection, host, port := c.gateway.lookupConnectionService(data.Host, uint16(data.Port))
	if connection == nil {
		return false, ssh.ConnectionFailed, "service not found or not online"
	}

	// see if this connection is allowed
	if !c.admin {
		log.Warningf("no permission to port forward: user = %s", c.user)
		return false, ssh.Prohibited, "permission denied"
	}

	// found the service, attempt to open a channel
	data2 := &tunnelData{
		Host:          host,
		Port:          uint32(port),
		OriginAddress: data.OriginAddress,
		OriginPort:    data.OriginPort,
	}

	tunnel2, err := connection.openTunnel("forwarded-tcpip", marshalTunnelData(data2), map[string]interface{}{
		"origin": data.OriginAddress,
		"from": map[string]interface{}{
			"address": c.remoteAddr.String(),
			"user":    c.user,
		},
		"service": map[string]interface{}{
			"host": data.Host,
			"port": data.Port,
		},
	})
	if err != nil {
		return false, ssh.ConnectionFailed, "failed to connect"
	}
	defer func() {
		if tunnel2 != nil {
			tunnel2.Close()
		}
	}()

	// accept the channel
	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Warningf("failed to accept channel: %s", err)
		return true, 0, ""
	}

	// cannot return false from this point on
	// also need to close the accepted channel
	defer func() {
		if channel != nil {
			if err := channel.Close(); err != nil {
				log.Warningf("failed to close accepted channel: %s", err)
			}
		}
	}()

	tunnel := newTunnel(c, channel, newChannel.ChannelType(), newChannel.ExtraData(), map[string]interface{}{
		"origin": data.OriginAddress,
		"to": map[string]interface{}{
			"user":    connection.user,
			"address": connection.remoteAddr.String(),
		},
		"service": map[string]interface{}{
			"host": data.Host,
			"port": data.Port,
		},
	})
	c.addTunnel(tunnel)

	// no failure
	go tunnel.handleRequests(requests)
	go tunnel.handleTunnel(tunnel2)

	// do not close channel on exit
	channel = nil
	tunnel2 = nil
	return true, 0, ""
}

// open a channel from the server to the client side
func (c *Connection) openTunnel(channelType string, extraData []byte, metadata map[string]interface{}) (*Tunnel, error) {
	log.Debugf("opening channel: type = %s, data = %v", channelType, extraData)

	channel, requests, err := c.conn.OpenChannel(channelType, extraData)
	if err != nil {
		return nil, err
	}
	defer func() {
		if channel != nil {
			if err := channel.Close(); err != nil {
				log.Warningf("failed to close opened channel: %s", err)
			}
		}
	}()

	tunnel := newTunnel(c, channel, channelType, extraData, metadata)
	c.addTunnel(tunnel)

	// no failure
	go tunnel.handleRequests(requests)

	// do not close channel on exit
	channel = nil
	return tunnel, nil
}

func (c *Connection) updateUser() {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.gateway.database.updateUser(&userModel{
		ID:       c.user,
		Address:  c.remoteAddr.String(),
		Location: c.location,
		Status:   c.status,
		Used:     c.usage.used.Unix(),
	}); err != nil {
		log.Errorf("failed to save user in database: %s", err)
	}
}
