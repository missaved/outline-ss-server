// Copyright 2018 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-internal-sdk/transport/shadowsocks"
	"github.com/Jigsaw-Code/outline-ss-server/ipinfo"
	onet "github.com/Jigsaw-Code/outline-ss-server/net"
	logging "github.com/op/go-logging"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

// UDPMetrics is used to report metrics on UDP connections.
type UDPMetrics interface {
	ipinfo.IPInfoMap

	// UDP metrics
	AddUDPPacketFromClient(clientInfo ipinfo.IPInfo, accessKey, status string, clientProxyBytes, proxyTargetBytes int)
	AddUDPPacketFromTarget(clientInfo ipinfo.IPInfo, accessKey, status string, targetProxyBytes, proxyClientBytes int)
	AddUDPNatEntry()
	RemoveUDPNatEntry()

	// Shadowsocks metrics
	AddUDPCipherSearch(accessKeyFound bool, timeToCipher time.Duration)
}

// Max UDP buffer size for the server code.
const serverUDPBufferSize = 64 * 1024

// Wrapper for logger.Debugf during UDP proxying.
func debugUDP(tag string, template string, val interface{}) {
	// This is an optimization to reduce unnecessary allocations due to an interaction
	// between Go's inlining/escape analysis and varargs functions like logger.Debugf.
	if logger.IsEnabledFor(logging.DEBUG) {
		logger.Debugf("UDP(%s): "+template, tag, val)
	}
}

func debugUDPAddr(addr net.Addr, template string, val interface{}) {
	if logger.IsEnabledFor(logging.DEBUG) {
		// Avoid calling addr.String() unless debugging is enabled.
		debugUDP(addr.String(), template, val)
	}
}

// Decrypts src into dst. It tries each cipher until it finds one that authenticates
// correctly. dst and src must not overlap.
func findAccessKeyUDP(clientIP net.IP, dst, src []byte, cipherList CipherList) ([]byte, string, *shadowsocks.EncryptionKey, error) {
	// Try each cipher until we find one that authenticates successfully. This assumes that all ciphers are AEAD.
	// We snapshot the list because it may be modified while we use it.
	snapshot := cipherList.SnapshotForClientIP(clientIP)
	for ci, entry := range snapshot {
		id, cryptoKey := entry.Value.(*CipherEntry).ID, entry.Value.(*CipherEntry).CryptoKey
		buf, err := shadowsocks.Unpack(dst, src, cryptoKey)
		if err != nil {
			debugUDP(id, "Failed to unpack: %v", err)
			continue
		}
		debugUDP(id, "Found cipher at index %d", ci)
		// Move the active cipher to the front, so that the search is quicker next time.
		cipherList.MarkUsedByClientIP(entry, clientIP)
		return buf, id, cryptoKey, nil
	}
	return nil, "", nil, errors.New("could not find valid cipher")
}

type packetHandler struct {
	natTimeout        time.Duration
	ciphers           CipherList
	m                 UDPMetrics
	targetIPValidator onet.TargetIPValidator
}

// NewPacketHandler creates a UDPService
func NewPacketHandler(natTimeout time.Duration, cipherList CipherList, m UDPMetrics) PacketHandler {
	return &packetHandler{natTimeout: natTimeout, ciphers: cipherList, m: m, targetIPValidator: onet.RequirePublicIP}
}

// PacketHandler is a running UDP shadowsocks proxy that can be stopped.
type PacketHandler interface {
	// SetTargetIPValidator sets the function to be used to validate the target IP addresses.
	SetTargetIPValidator(targetIPValidator onet.TargetIPValidator)
	// Handle returns after clientConn closes and all the sub goroutines return.
	Handle(clientConn net.PacketConn)
}

func (h *packetHandler) SetTargetIPValidator(targetIPValidator onet.TargetIPValidator) {
	h.targetIPValidator = targetIPValidator
}

// Listen on addr for encrypted packets and basically do UDP NAT.
// We take the ciphers as a pointer because it gets replaced on config updates.
func (h *packetHandler) Handle(clientConn net.PacketConn) {
	var running sync.WaitGroup

	nm := newNATmap(h.natTimeout, h.m, &running)
	defer nm.Close()
	cipherBuf := make([]byte, serverUDPBufferSize)
	textBuf := make([]byte, serverUDPBufferSize)

	for {
		clientProxyBytes, clientAddr, err := clientConn.ReadFrom(cipherBuf)
		if errors.Is(err, net.ErrClosed) {
			break
		}

		var clientInfo ipinfo.IPInfo
		keyID := ""
		var proxyTargetBytes int

		connError := func() (connError *onet.ConnectionError) {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("Panic in UDP loop: %v. Continuing to listen.", r)
					debug.PrintStack()
				}
			}()

			// Error from ReadFrom
			if err != nil {
				return onet.NewConnectionError("ERR_READ", "Failed to read from client", err)
			}
			if logger.IsEnabledFor(logging.DEBUG) {
				defer logger.Debugf("UDP(%v): done", clientAddr)
				logger.Debugf("UDP(%v): Outbound packet has %d bytes", clientAddr, clientProxyBytes)
			}

			cipherData := cipherBuf[:clientProxyBytes]
			var payload []byte
			var tgtUDPAddr *net.UDPAddr
			targetConn := nm.Get(clientAddr.String())
			if targetConn == nil {
				var locErr error
				clientInfo, locErr = ipinfo.GetIPInfoFromAddr(h.m, clientAddr)
				if locErr != nil {
					logger.Warningf("Failed client info lookup: %v", locErr)
				}
				debugUDPAddr(clientAddr, "Got info \"%#v\"", clientInfo)

				ip := clientAddr.(*net.UDPAddr).IP
				var textData []byte
				var cryptoKey *shadowsocks.EncryptionKey
				unpackStart := time.Now()
				textData, keyID, cryptoKey, err = findAccessKeyUDP(ip, textBuf, cipherData, h.ciphers)
				timeToCipher := time.Since(unpackStart)
				h.m.AddUDPCipherSearch(err == nil, timeToCipher)

				if err != nil {
					return onet.NewConnectionError("ERR_CIPHER", "Failed to unpack initial packet", err)
				}

				var onetErr *onet.ConnectionError
				if payload, tgtUDPAddr, onetErr = h.validatePacket(textData); onetErr != nil {
					return onetErr
				}

				udpConn, err := net.ListenPacket("udp", "")
				if err != nil {
					return onet.NewConnectionError("ERR_CREATE_SOCKET", "Failed to create UDP socket", err)
				}
				targetConn = nm.Add(clientAddr, clientConn, cryptoKey, udpConn, clientInfo, keyID)
			} else {
				clientInfo = targetConn.clientInfo

				unpackStart := time.Now()
				textData, err := shadowsocks.Unpack(nil, cipherData, targetConn.cryptoKey)
				timeToCipher := time.Since(unpackStart)
				h.m.AddUDPCipherSearch(err == nil, timeToCipher)

				if err != nil {
					return onet.NewConnectionError("ERR_CIPHER", "Failed to unpack data from client", err)
				}

				// The key ID is known with confidence once decryption succeeds.
				keyID = targetConn.keyID

				var onetErr *onet.ConnectionError
				if payload, tgtUDPAddr, onetErr = h.validatePacket(textData); onetErr != nil {
					return onetErr
				}
			}

			debugUDPAddr(clientAddr, "Proxy exit %v", targetConn.LocalAddr())
			proxyTargetBytes, err = targetConn.WriteTo(payload, tgtUDPAddr) // accept only UDPAddr despite the signature
			if err != nil {
				return onet.NewConnectionError("ERR_WRITE", "Failed to write to target", err)
			}
			return nil
		}()

		status := "OK"
		if connError != nil {
			logger.Debugf("UDP Error: %v: %v", connError.Message, connError.Cause)
			status = connError.Status
		}
		h.m.AddUDPPacketFromClient(clientInfo, keyID, status, clientProxyBytes, proxyTargetBytes)
	}
}

// Given the decrypted contents of a UDP packet, return
// the payload and the destination address, or an error if
// this packet cannot or should not be forwarded.
func (h *packetHandler) validatePacket(textData []byte) ([]byte, *net.UDPAddr, *onet.ConnectionError) {
	tgtAddr := socks.SplitAddr(textData)
	if tgtAddr == nil {
		return nil, nil, onet.NewConnectionError("ERR_READ_ADDRESS", "Failed to get target address", nil)
	}

	tgtUDPAddr, err := net.ResolveUDPAddr("udp", tgtAddr.String())
	if err != nil {
		return nil, nil, onet.NewConnectionError("ERR_RESOLVE_ADDRESS", fmt.Sprintf("Failed to resolve target address %v", tgtAddr), err)
	}
	if err := h.targetIPValidator(tgtUDPAddr.IP); err != nil {
		return nil, nil, err
	}

	payload := textData[len(tgtAddr):]
	return payload, tgtUDPAddr, nil
}

func isDNS(addr net.Addr) bool {
	_, port, _ := net.SplitHostPort(addr.String())
	return port == "53"
}

type natconn struct {
	net.PacketConn
	cryptoKey *shadowsocks.EncryptionKey
	keyID     string
	// We store the client information in the NAT map to avoid recomputing it
	// for every downstream packet in a UDP-based connection.
	clientInfo ipinfo.IPInfo
	// NAT timeout to apply for non-DNS packets.
	defaultTimeout time.Duration
	// Current read deadline of PacketConn.  Used to avoid decreasing the
	// deadline.  Initially zero.
	readDeadline time.Time
	// If the connection has only sent one DNS query, it will close
	// if it receives a DNS response.
	fastClose sync.Once
}

func (c *natconn) onWrite(addr net.Addr) {
	// Fast close is only allowed if there has been exactly one write,
	// and it was a DNS query.
	isDNS := isDNS(addr)
	isFirstWrite := c.readDeadline.IsZero()
	if !isDNS || !isFirstWrite {
		// Disable fast close.  (Idempotent.)
		c.fastClose.Do(func() {})
	}

	timeout := c.defaultTimeout
	if isDNS {
		// Shorten timeout as required by RFC 5452 Section 10.
		timeout = 17 * time.Second
	}

	newDeadline := time.Now().Add(timeout)
	if newDeadline.After(c.readDeadline) {
		c.readDeadline = newDeadline
		c.SetReadDeadline(newDeadline)
	}
}

func (c *natconn) onRead(addr net.Addr) {
	c.fastClose.Do(func() {
		if isDNS(addr) {
			// The next ReadFrom() should time out immediately.
			c.SetReadDeadline(time.Now())
		}
	})
}

func (c *natconn) WriteTo(buf []byte, dst net.Addr) (int, error) {
	c.onWrite(dst)
	return c.PacketConn.WriteTo(buf, dst)
}

func (c *natconn) ReadFrom(buf []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(buf)
	if err == nil {
		c.onRead(addr)
	}
	return n, addr, err
}

// Packet NAT table
type natmap struct {
	sync.RWMutex
	keyConn map[string]*natconn
	timeout time.Duration
	metrics UDPMetrics
	running *sync.WaitGroup
}

func newNATmap(timeout time.Duration, sm UDPMetrics, running *sync.WaitGroup) *natmap {
	m := &natmap{metrics: sm, running: running}
	m.keyConn = make(map[string]*natconn)
	m.timeout = timeout
	return m
}

func (m *natmap) Get(key string) *natconn {
	m.RLock()
	defer m.RUnlock()
	return m.keyConn[key]
}

func (m *natmap) set(key string, pc net.PacketConn, cryptoKey *shadowsocks.EncryptionKey, keyID string, clientInfo ipinfo.IPInfo) *natconn {
	entry := &natconn{
		PacketConn:     pc,
		cryptoKey:      cryptoKey,
		keyID:          keyID,
		clientInfo:     clientInfo,
		defaultTimeout: m.timeout,
	}

	m.Lock()
	defer m.Unlock()

	m.keyConn[key] = entry
	return entry
}

func (m *natmap) del(key string) net.PacketConn {
	m.Lock()
	defer m.Unlock()

	entry, ok := m.keyConn[key]
	if ok {
		delete(m.keyConn, key)
		return entry
	}
	return nil
}

func (m *natmap) Add(clientAddr net.Addr, clientConn net.PacketConn, cryptoKey *shadowsocks.EncryptionKey, targetConn net.PacketConn, clientInfo ipinfo.IPInfo, keyID string) *natconn {
	entry := m.set(clientAddr.String(), targetConn, cryptoKey, keyID, clientInfo)

	m.metrics.AddUDPNatEntry()
	m.running.Add(1)
	go func() {
		timedCopy(clientAddr, clientConn, entry, keyID, m.metrics)
		m.metrics.RemoveUDPNatEntry()
		if pc := m.del(clientAddr.String()); pc != nil {
			pc.Close()
		}
		m.running.Done()
	}()
	return entry
}

func (m *natmap) Close() error {
	m.Lock()
	defer m.Unlock()

	var err error
	now := time.Now()
	for _, pc := range m.keyConn {
		if e := pc.SetReadDeadline(now); e != nil {
			err = e
		}
	}
	return err
}

// Get the maximum length of the shadowsocks address header by parsing
// and serializing an IPv6 address from the example range.
var maxAddrLen int = len(socks.ParseAddr("[2001:db8::1]:12345"))

// copy from target to client until read timeout
func timedCopy(clientAddr net.Addr, clientConn net.PacketConn, targetConn *natconn,
	keyID string, sm UDPMetrics) {
	// pkt is used for in-place encryption of downstream UDP packets, with the layout
	// [padding?][salt][address][body][tag][extra]
	// Padding is only used if the address is IPv4.
	pkt := make([]byte, serverUDPBufferSize)

	saltSize := targetConn.cryptoKey.SaltSize()
	// Leave enough room at the beginning of the packet for a max-length header (i.e. IPv6).
	bodyStart := saltSize + maxAddrLen

	expired := false
	for {
		var bodyLen, proxyClientBytes int
		connError := func() (connError *onet.ConnectionError) {
			var (
				raddr net.Addr
				err   error
			)
			// `readBuf` receives the plaintext body in `pkt`:
			// [padding?][salt][address][body][tag][unused]
			// |--     bodyStart     --|[      readBuf    ]
			readBuf := pkt[bodyStart:]
			bodyLen, raddr, err = targetConn.ReadFrom(readBuf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok {
					if netErr.Timeout() {
						expired = true
						return nil
					}
				}
				return onet.NewConnectionError("ERR_READ", "Failed to read from target", err)
			}

			debugUDPAddr(clientAddr, "Got response from %v", raddr)
			srcAddr := socks.ParseAddr(raddr.String())
			addrStart := bodyStart - len(srcAddr)
			// `plainTextBuf` concatenates the SOCKS address and body:
			// [padding?][salt][address][body][tag][unused]
			// |-- addrStart -|[plaintextBuf ]
			plaintextBuf := pkt[addrStart : bodyStart+bodyLen]
			copy(plaintextBuf, srcAddr)

			// saltStart is 0 if raddr is IPv6.
			saltStart := addrStart - saltSize
			// `packBuf` adds space for the salt and tag.
			// `buf` shows the space that was used.
			// [padding?][salt][address][body][tag][unused]
			//           [            packBuf             ]
			//           [          buf           ]
			packBuf := pkt[saltStart:]
			buf, err := shadowsocks.Pack(packBuf, plaintextBuf, targetConn.cryptoKey) // Encrypt in-place
			if err != nil {
				return onet.NewConnectionError("ERR_PACK", "Failed to pack data to client", err)
			}
			proxyClientBytes, err = clientConn.WriteTo(buf, clientAddr)
			if err != nil {
				return onet.NewConnectionError("ERR_WRITE", "Failed to write to client", err)
			}
			return nil
		}()
		status := "OK"
		if connError != nil {
			logger.Debugf("UDP Error: %v: %v", connError.Message, connError.Cause)
			status = connError.Status
		}
		if expired {
			break
		}
		sm.AddUDPPacketFromTarget(targetConn.clientInfo, keyID, status, bodyLen, proxyClientBytes)
	}
}

// NoOpUDPMetrics is a [UDPMetrics] that doesn't do anything. Useful in tests
// or if you don't want to track metrics.
type NoOpUDPMetrics struct{}

var _ UDPMetrics = (*NoOpUDPMetrics)(nil)

func (m *NoOpUDPMetrics) GetIPInfo(net.IP) (ipinfo.IPInfo, error) {
	return ipinfo.IPInfo{}, nil
}
func (m *NoOpUDPMetrics) AddUDPPacketFromClient(clientInfo ipinfo.IPInfo, accessKey, status string, clientProxyBytes, proxyTargetBytes int) {
}
func (m *NoOpUDPMetrics) AddUDPPacketFromTarget(clientInfo ipinfo.IPInfo, accessKey, status string, targetProxyBytes, proxyClientBytes int) {
}
func (m *NoOpUDPMetrics) AddUDPNatEntry()                                                    {}
func (m *NoOpUDPMetrics) RemoveUDPNatEntry()                                                 {}
func (m *NoOpUDPMetrics) AddUDPCipherSearch(accessKeyFound bool, timeToCipher time.Duration) {}
