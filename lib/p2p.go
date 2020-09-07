package ptp

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	upnp "github.com/NebulousLabs/go-upnp"
)

// GlobalMTU value specified on daemon start
var GlobalMTU = DefaultMTU

var UsePMTU = false

// PeerToPeer - Main structure
type PeerToPeer struct {
	UDPSocket       *Network                             // Peer-to-peer interconnection socket
	LocalIPs        []net.IP                             // List of IPs available in the system
	Dht             *DHTClient                           // DHT Client
	Crypter         Crypto                               // Cryptography subsystem
	Shutdown        bool                                 // Set to true when instance in shutdown mode
	ForwardMode     bool                                 // Skip local peer discovery
	ReadyToStop     bool                                 // Set to true when instance is ready to stop
	MessageHandlers map[uint16]MessageHandler            // Callbacks for network packets
	PacketHandlers  map[PacketType]PacketHandlerCallback // Callbacks for packets received by TAP interface
	Hash            string                               // Infohash for this instance
	Interface       TAP                                  // TAP Interface
	Swarm           *Swarm                               // Known peers
	HolePunching    sync.Mutex                           // Mutex for hole punching sync
	ProxyManager    *ProxyManager                        // Proxy manager
	outboundIP      net.IP                               // Outbound IP
	UsePMTU         bool                                 // Whether PMTU capabilities are enabled or not
	StartedAt       time.Time                            // Timestamp of instance creation time
	ConfiguredAt    time.Time                            // Time when configuration of the instance was finished
}

// PeerHandshake holds handshake information received from peer
type PeerHandshake struct {
	ID           string
	IP           net.IP
	HardwareAddr net.HardwareAddr
	Endpoint     *net.UDPAddr
	AutoIP       bool // Whether or not peer have automatic IP
}

// ActiveInterfaces is a global (daemon-wise) list of reserved IP addresses
var ActiveInterfaces []net.IP

// AssignInterface - Creates TUN/TAP Interface and configures it with provided IP tool
func (p *PeerToPeer) AssignInterface(interfaceName string) error {
	var err error
	if p.Interface == nil {
		return fmt.Errorf("Failed to initialize TAP")
	}
	if p.Interface.IsConfigured() {
		return nil
	}
	err = p.Interface.Init(interfaceName)
	if err != nil {
		return fmt.Errorf("Failed to initialize TAP: %s", err)
	}

	if p.Interface.GetIP() == nil && !p.Interface.IsAuto() {
		return fmt.Errorf("No IP provided")
	}
	if p.Interface.GetHardwareAddress() == nil {
		return fmt.Errorf("No Hardware address provided")
	}

	// Extract necessary information from config file
	// err = p.Config.Read()
	// if err != nil {
	// 	Error( "Failed to extract information from config file: %v", err)
	// 	return err
	// }

	err = p.Interface.Open()
	if err != nil {
		Error("Failed to open TAP device %s: %v", p.Interface.GetName(), err)
		return err
	}
	Debug("%v TAP Device created", p.Interface.GetName())

	lazy := false
	if p.Interface.IsAuto() {
		lazy = true
	}

	err = p.Interface.Configure(lazy)
	if err != nil {
		return err
	}
	ActiveInterfaces = append(ActiveInterfaces, p.Interface.GetIP())
	if !p.Interface.IsAuto() {
		Debug("Interface has been configured")
		p.Interface.MarkConfigured()
	}
	return err
}

// ListenInterface - Listens TAP interface for incoming packets
// Read packets received by TAP interface and send them to a handlePacket goroutine
// This goroutine will execute a callback method based on packet type
func (p *PeerToPeer) ListenInterface() error {
	if p.Interface == nil {
		Error("Failed to start TAP listener: nil object")
		return fmt.Errorf("nil interface")
	}
	p.Interface.Run()
	for {
		if p.Shutdown {
			break
		}
		if p.Interface.GetIP() == nil || p.Interface.IsConfigured() == false {
			time.Sleep(time.Millisecond * 100)
			continue
		}
		packet, err := p.Interface.ReadPacket()
		if err != nil && err != errPacketTooBig {
			Error("Reading packet: %s", err)
			p.Close()
			break
		}
		if packet != nil {
			go p.handlePacket(packet.Packet, packet.Protocol)
		}
	}
	Debug("Shutting down interface listener")

	if p.Interface != nil {
		return p.Interface.Close()
	}
	return fmt.Errorf("Interface already closed")
}

// GenerateDeviceName method will generate device name if none were specified at startup
func (p *PeerToPeer) GenerateDeviceName(i int) string {
	tap, _ := newTAP("", "127.0.0.1", "00:00:00:00:00:00", "", 0, p.UsePMTU)
	var devName = tap.GetBasename() + fmt.Sprintf("%d", i)
	if isDeviceExists(devName) {
		return p.GenerateDeviceName(i + 1)
	}
	return devName
}

// IsIPv4 checks whether interface is IPv4 or IPv6
func (p *PeerToPeer) IsIPv4(ip string) bool {
	for i := 0; i < len(ip); i++ {
		switch ip[i] {
		case ':':
			return false
		case '.':
			return true
		}
	}
	return false
}

// New is an entry point of a P2P library.
// This function will return new PeerToPeer object which later
// should be configured and started using Run() method
func New(mac, hash, keyfile, key, ttl, target string, fwd bool, port int, outboundIP net.IP) *PeerToPeer {
	Debug("Starting new P2P Instance: %s", hash)
	Debug("Mac: %s", mac)
	p := new(PeerToPeer)
	p.outboundIP = outboundIP
	p.Init()
	var err error
	p.Interface, err = newTAP(GetConfigurationTool(), "127.0.0.1", "00:00:00:00:00:00", "", DefaultMTU, UsePMTU)
	if err != nil {
		Error("Failed to create TAP object: %s", err)
		return nil
	}
	p.Interface.SetHardwareAddress(p.validateMac(mac))
	p.FindNetworkAddresses()

	if fwd {
		p.ForwardMode = true
	}

	if keyfile != "" {
		p.Crypter.ReadKeysFromFile(keyfile)
	}
	if key != "" {
		// Override key from file
		if ttl == "" {
			ttl = "default"
		}
		var newKey CryptoKey
		newKey = p.Crypter.EnrichKeyValues(newKey, key, ttl)
		p.Crypter.Keys = append(p.Crypter.Keys, newKey)
		p.Crypter.ActiveKey = p.Crypter.Keys[0]
		p.Crypter.Active = true
	}

	if p.Crypter.Active {
		Debug("Traffic encryption is enabled. Key valid until %s", p.Crypter.ActiveKey.Until.String())
	} else {
		Debug("No AES key were provided. Traffic encryption is disabled")
	}

	p.Hash = hash

	p.setupHandlers()

	p.UDPSocket = new(Network)
	p.UDPSocket.Init("", port)
	go p.UDPSocket.Listen(p.HandleP2PMessage)
	go p.UDPSocket.KeepAlive(target)
	p.waitForRemotePort()

	// Create new DHT Client, configure it and initialize
	// During initialization procedure, DHT Client will send
	// a introduction packet along with a hash to a DHT bootstrap
	// nodes that was hardcoded into it's code

	Debug("Started UDP Listener at port %d", p.UDPSocket.GetPort())

	p.Dht = new(DHTClient)
	err = p.Dht.Init(p.Hash)
	if err != nil {
		Error("Failed to initialize DHT: %s", err)
		return nil
	}

	p.setupTCPCallbacks()
	p.ProxyManager = new(ProxyManager)
	p.ProxyManager.init()
	return p
}

// ReadDHT will read packets from bootstrap node
func (p *PeerToPeer) ReadDHT() error {
	if p.Dht == nil {
		return fmt.Errorf("ReadDHT: nil DHT")
	}
	for !p.Shutdown {
		packet, err := p.Dht.read()
		if err != nil {
			break
		}
		go func() {
			cb, e := p.Dht.TCPCallbacks[packet.Type]
			if !e {
				Error("Unsupported packet from DHT")
				return
			}
			err = cb(packet)
			if err != nil {
				Error("DHT: %s", err)
			}
		}()
	}
	return nil
}

// This method will block for seconds or unless we receive remote port
// from echo server
func (p *PeerToPeer) waitForRemotePort() error {
	if p.UDPSocket == nil {
		return fmt.Errorf("waitForRemotePort: nil udp socket")
	}
	started := time.Now()
	for p.UDPSocket.remotePort == 0 {
		time.Sleep(time.Millisecond * 100)
		if time.Since(started) > time.Duration(time.Second*3) {
			break
		}
	}
	if p.UDPSocket != nil && p.UDPSocket.remotePort == 0 {
		Warn("Didn't receive remote port")
		p.UDPSocket.remotePort = p.UDPSocket.GetPort()
		return fmt.Errorf("Didn't receive remote port")
	}
	Warn("Remote port received: %d", p.UDPSocket.remotePort)
	return nil
}

// PrepareInterfaces will assign IPs to interfaces
func (p *PeerToPeer) PrepareInterfaces(ip, interfaceName string) error {
	if p.Interface == nil {
		return fmt.Errorf("PrepareInterfaces: nil interface")
	}

	iface, err := p.validateInterfaceName(interfaceName)
	if err != nil {
		Error("Interface name validation failed: %s", err)
		return fmt.Errorf("Failed to validate interface name: %s", err)

	}
	if isDeviceExists(iface) {
		Error("Interface is already in use. Can't create duplicate")
		return fmt.Errorf("Interface is already in use")
	}

	if ip == "dhcp" || ip == "auto" {
		ip = "dhcp"
		ipn, maskn, err := p.RequestIP(p.Interface.GetHardwareAddress().String(), iface)
		if err != nil {
			return err
		}

		p.Interface.SetIP(ipn)
		p.Interface.SetMask(maskn)
		return nil
	} else if ip == "discover" {
		p.Interface.SetAuto(true)
		p.Interface.SetIP(nil)
		p.Interface.SetSubnet(nil)
		p.AssignInterface(iface)
		return nil
	}
	staticIP := net.ParseIP(ip)
	if staticIP == nil {
		return fmt.Errorf("Failed to parse specified IP: %s", ip)
	}
	p.Interface.SetIP(staticIP)
	ipn, maskn, err := p.ReportIP(ip, p.Interface.GetHardwareAddress().String(), iface)
	if err != nil {
		return err
	}
	p.Interface.SetIP(ipn)
	p.Interface.SetMask(maskn)
	return nil
}

func (p *PeerToPeer) attemptPortForward(port uint16, name string) error {
	Debug("Trying to forward port %d", port)
	d, err := upnp.Discover()
	if err != nil {
		return err
	}
	err = d.Forward(port, "subutai-"+name)
	if err != nil {
		return err
	}
	Debug("Port %d has been forwarded", port)
	return nil
}

// Init will initialize PeerToPeer
func (p *PeerToPeer) Init() error {
	p.Swarm = new(Swarm)
	p.Swarm.Init()
	return nil
}

func (p *PeerToPeer) validateMac(mac string) net.HardwareAddr {
	var hw net.HardwareAddr
	var err error
	if mac != "" {
		hw, err = net.ParseMAC(mac)
		if err != nil {
			Error("Invalid MAC address provided: %v", err)
			return nil
		}
		return hw
	}
	mac, hw = GenerateMAC()
	Debug("Generate MAC for TAP device: %s", mac)
	return hw
}

func (p *PeerToPeer) validateInterfaceName(name string) (string, error) {
	if name == "" {
		name = p.GenerateDeviceName(1)
	} else {
		if len(name) > MaximumInterfaceNameLength {
			Debug("Interface name length should be %d symbols max", MaximumInterfaceNameLength)
			return "", fmt.Errorf("Interface name is too big")
		}
	}
	return name, nil
}

func (p *PeerToPeer) setupHandlers() error {
	// Register network message handlers
	p.MessageHandlers = make(map[uint16]MessageHandler)
	p.MessageHandlers[MsgTypeNenc] = p.HandleNotEncryptedMessage
	p.MessageHandlers[MsgTypePing] = p.HandlePingMessage
	p.MessageHandlers[MsgTypeXpeerPing] = p.HandleXpeerPingMessage
	p.MessageHandlers[MsgTypeIntro] = p.HandleIntroMessage
	p.MessageHandlers[MsgTypeIntroReq] = p.HandleIntroRequestMessage
	p.MessageHandlers[MsgTypeProxy] = p.HandleProxyMessage
	p.MessageHandlers[MsgTypeLatency] = p.HandleLatency
	p.MessageHandlers[MsgTypeComm] = p.HandleComm

	// Register packet handlers
	p.PacketHandlers = make(map[PacketType]PacketHandlerCallback)
	p.PacketHandlers[PacketPARCUniversal] = p.handlePARCUniversalPacket
	p.PacketHandlers[PacketIPv4] = p.handlePacketIPv4
	p.PacketHandlers[PacketARP] = p.handlePacketARP
	p.PacketHandlers[PacketRARP] = p.handleRARPPacket
	p.PacketHandlers[Packet8021Q] = p.handle8021qPacket
	p.PacketHandlers[PacketIPv6] = p.handlePacketIPv6
	p.PacketHandlers[PacketPPPoEDiscovery] = p.handlePPPoEDiscoveryPacket
	p.PacketHandlers[PacketPPPoESession] = p.handlePPPoESessionPacket
	p.PacketHandlers[PacketLLDP] = p.handlePacketLLDP

	return nil
}

// RequestIP asks DHT to get IP from DHCP-like service
func (p *PeerToPeer) RequestIP(mac, device string) (net.IP, net.IPMask, error) {
	if p.Dht == nil {
		return nil, nil, fmt.Errorf("RequestIP: nil dht")
	}

	Debug("Requesting IP from Bootstrap node")
	requestedAt := time.Now()
	interval := time.Duration(2 * time.Second)
	attempt := 0
	p.Dht.sendDHCP(nil, nil)
	for p.Dht.IP == nil && p.Dht.Network == nil {
		if time.Since(requestedAt) > interval {
			if attempt >= 3 {
				return nil, nil, fmt.Errorf("No IP were received. Swarm is empty")
			}
			Info("IP wasn't received. Requesting again: attempt %d/3", (attempt + 1))
			attempt++
			p.Dht.sendDHCP(nil, nil)
			requestedAt = time.Now()
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.Interface.SetIP(p.Dht.IP)
	p.Interface.SetMask(p.Dht.Network.Mask)
	err := p.AssignInterface(device)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to configure interface: %s", err)
	}
	return p.Dht.IP, p.Dht.Network.Mask, nil
}

// ReportIP will send IP specified at service start to DHCP-like service
func (p *PeerToPeer) ReportIP(ipAddress, mac, device string) (net.IP, net.IPMask, error) {
	if p.Dht == nil {
		return nil, nil, fmt.Errorf("nil dht")
	}

	Debug("Reporting IP to bootstranp node: %s", ipAddress)
	ip, ipnet, err := net.ParseCIDR(ipAddress)
	if err != nil {
		nip := net.ParseIP(ipAddress)
		if nip == nil {
			return nil, nil, fmt.Errorf("Invalid address were provided for network interface. Use -ip \"dhcp\" or specify correct IP address")
		}
		ipAddress += `/24`
		Debug("IP was not in CIDR format. Assumming /24")
		ip, ipnet, err = net.ParseCIDR(ipAddress)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to configure interface with provided IP")
		}
	}
	if ipnet == nil {
		return nil, nil, fmt.Errorf("Can't report network information. Reason: Unknown")
	}
	p.Dht.IP = ip
	p.Dht.Network = ipnet

	p.Dht.sendDHCP(ip, ipnet)
	err = p.AssignInterface(device)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to configure interface: %s", err)
	}
	return ip, ipnet.Mask, nil
}

// Run is a main loop
func (p *PeerToPeer) Run() error {
	if p.Dht == nil {
		return fmt.Errorf("Run: nil dht")
	}
	if p.Interface == nil {
		return fmt.Errorf("Run: nil interface")
	}
	// Request proxies from DHT
	p.Dht.sendProxy()

	initialRequestSent := false
	started := time.Now()
	p.Dht.LastUpdate = time.Now()
	for {
		if p.Shutdown {
			// TODO: Do it more safely
			if p.ReadyToStop {
				break
			}
			time.Sleep(1 * time.Second)
			continue
		}
		p.removeStoppedPeers()
		p.checkLastDHTUpdate()
		p.checkProxies()
		p.checkPeers()
		time.Sleep(100 * time.Millisecond)
		if !initialRequestSent && time.Since(started) > time.Duration(time.Millisecond*5000) {
			initialRequestSent = true
			p.Dht.sendFind()
		}
		if p.Interface.IsBroken() {
			Info("TAP interface is broken. Shutting down instance %s", p.Hash)
			p.Close()
		}
	}
	Info("Shutting down instance %s completed", p.Dht.NetworkHash)
	return nil
}

func (p *PeerToPeer) checkLastDHTUpdate() error {
	if p.Dht == nil {
		return fmt.Errorf("checkLastDHTUpdate: nil dht")
	}
	if p.ProxyManager == nil {
		return fmt.Errorf("checkLastDHTUpdate: nil proxy manager")
	}
	passed := time.Since(p.Dht.LastUpdate)
	if passed > time.Duration(30*time.Second) {
		Debug("DHT Last Update timeout passed")
		// Request new proxies if we don't have any more
		if len(p.ProxyManager.get()) == 0 {
			p.Dht.sendProxy()
		}
		err := p.Dht.sendFind()
		if err != nil {
			Error("Failed to send update: %s", err)
			return fmt.Errorf("Failed to send DHT update: %s", err)
		}
	}
	return nil
}

// TODO: Check if this method is still actual
func (p *PeerToPeer) removeStoppedPeers() error {
	if p.Swarm == nil {
		return fmt.Errorf("removeStoppedPeers: nil peer list")
	}
	peers := p.Swarm.Get()
	for id, peer := range peers {
		if peer.State == PeerStateStop {
			Info("Removing peer %s", id)
			p.Swarm.Delete(id)
			Info("Peer %s has been removed", id)
			break
		}
	}
	return nil
}

func (p *PeerToPeer) checkProxies() error {
	if p.Dht == nil {
		return fmt.Errorf("checkProxies: nil dht")
	}
	if p.ProxyManager == nil {
		return fmt.Errorf("checkProxies: nil proxy manager")
	}
	if p.UDPSocket == nil {
		return fmt.Errorf("checkProxies: nil socket")
	}
	p.ProxyManager.check()
	// Unlink dead proxies
	proxies := p.ProxyManager.get()
	list := []*net.UDPAddr{}
	for _, proxy := range proxies {
		if proxy.Endpoint != nil && proxy.Status == proxyActive {
			list = append(list, proxy.Endpoint)
			proxy.Measure(p.UDPSocket)
		}
	}
	if p.ProxyManager.hasChanges && len(list) > 0 {
		p.ProxyManager.hasChanges = false
		p.Dht.sendReportProxy(list)
	}
	return nil
}

func (p *PeerToPeer) checkPeers() error {
	if p.Dht == nil {
		return fmt.Errorf("checkPeers: nil dht")
	}
	if p.Swarm == nil {
		return fmt.Errorf("checkPeers: nil peer list")
	}
	if p.UDPSocket == nil {
		return fmt.Errorf("checkPeers: nil udp socket")
	}
	if len(p.Dht.ID) != 36 {
		return fmt.Errorf("checkPeers ID is too small")
	}
	for _, peer := range p.Swarm.Get() {
		if peer.State == PeerStateConnected {
			if !p.Interface.IsConfigured() && p.Interface.IsAuto() && p.Interface.GetSubnet() == nil && p.Interface.GetIP() == nil {
				// Starting interface configuration process
				p.Interface.Configure(true)
				p.discoverIP()
			}
		}
		for _, e := range peer.EndpointsHeap {
			if e == nil {
				continue
			}
			e.Measure(p.UDPSocket, p.Dht.ID)
		}
	}
	return nil
}

// discoverSubnet will ask all known peers about subnet they use.
// The first one to response will be used in the further interface configuration process
func (p *PeerToPeer) discoverIP() error {
	if p.Swarm == nil {
		return fmt.Errorf("nil swarm")
	}
	if p.Interface == nil {
		return fmt.Errorf("nil interface")
	}
	if p.Dht == nil {
		return fmt.Errorf("nil dht")
	}

	Info("Discovering IP for this swarm")

	p.Interface.SetSubnet(nil)
	p.Interface.SetIP(nil)

	// Send subnet request
	payload := make([]byte, 38)
	binary.BigEndian.PutUint16(payload[0:2], CommIPSubnet)
	copy(payload[2:38], p.Dht.ID)
	msg, _ := p.CreateMessage(MsgTypeComm, payload, 0, true)
	for _, peer := range p.Swarm.Get() {
		if peer.State == PeerStateConnected && peer.Endpoint != nil {
			p.UDPSocket.SendMessage(msg, peer.Endpoint)
		}
	}

	// Waiting for subnet
	lastRequest := time.Now()
	for p.Interface.GetSubnet() == nil {
		if time.Since(lastRequest) > time.Duration(time.Millisecond*2000) {
			p.Interface.Deconfigure()
			return fmt.Errorf("Didn't received subnet information")
		}
		time.Sleep(time.Millisecond * 100)
	}

	sn := p.Interface.GetSubnet()
	Info("Received subnet for this swarm: %s", sn.String())

	// Discover free IP
	i := 255
	lastRequest = time.Unix(0, 0)
	for p.Interface.GetIP() == nil && i > 0 {
		if time.Since(lastRequest) > time.Duration(time.Millisecond*1500) {
			i--
			lastRequest = time.Now()
			payload := make([]byte, 42)

			binary.BigEndian.PutUint16(payload[0:2], CommIPInfo)
			copy(payload[2:38], p.Dht.ID)
			copy(payload[38:42], []byte{sn[0], sn[1], sn[2], byte(i)})
			msg, _ := p.CreateMessage(MsgTypeComm, payload, 0, true)
			for _, peer := range p.Swarm.Get() {
				if peer.Endpoint == nil || peer.State != PeerStateConnected {
					continue
				}

				p.UDPSocket.SendMessage(msg, peer.Endpoint)
			}
		}
		time.Sleep(time.Millisecond * 100)
	}

	if p.Interface.GetIP() == nil {
		Error("Couldn't find free IP for this swarm")
		return fmt.Errorf("Failed to get free IP for this swarm")
	}

	return nil
}

// notifyIP will notify all known peers about it's new IP
func (p *PeerToPeer) notifyIP() error {
	if p.Dht == nil {
		return fmt.Errorf("nil dht")
	}
	payload := make([]byte, 42)
	binary.BigEndian.PutUint16(payload[0:2], CommIPSet)
	copy(payload[2:38], p.Dht.ID)
	copy(payload[38:42], p.Interface.GetIP().To4())

	msg, _ := p.CreateMessage(MsgTypeComm, payload, 0, true)

	for _, peer := range p.Swarm.Get() {
		if peer.Endpoint != nil {
			p.UDPSocket.SendMessage(msg, peer.Endpoint)
		}
	}

	return nil
}

// PrepareIntroductionMessage collects client ID, mac and IP address
// and create a comma-separated line
// endpoint is an address that received this introduction message
func (p *PeerToPeer) PrepareIntroductionMessage(id, endpoint string) (*P2PMessage, error) {
	if p.Interface == nil {
		return nil, fmt.Errorf("PrepareIntroductionMessage: nil interface")
	}

	ip := "auto"
	if !p.Interface.IsAuto() {
		ip = p.Interface.GetIP().String()
	}

	var intro = id + "," + p.Interface.GetHardwareAddress().String() + "," + ip + "," + endpoint
	msg, err := p.CreateMessage(MsgTypeIntro, []byte(intro), 0, true)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// WriteToDevice writes data to created TAP interface
func (p *PeerToPeer) WriteToDevice(b []byte, proto uint16, truncated bool) error {
	if p.Interface == nil {
		Error("TAP Interface not initialized")
		return fmt.Errorf("WriteToDevice: interface is nil")
	}

	var packet Packet
	packet.Protocol = int(proto)
	packet.Packet = b
	err := p.Interface.WritePacket(&packet)
	if err != nil {
		Error("Failed to write to TAP Interface: %v", err)
		return fmt.Errorf("Failed to write to TAP Interface: %v", err)
	}
	return nil
}

// SendTo sends a p2p packet by MAC address
func (p *PeerToPeer) SendTo(dst net.HardwareAddr, msg *P2PMessage) (int, error) {
	if p.Swarm == nil {
		return -1, fmt.Errorf("SendTo: nil peer list")
	}
	if p.UDPSocket == nil {
		return -1, fmt.Errorf("SendTo: nil udp socket")
	}
	if msg == nil {
		return -1, fmt.Errorf("SendTo: nil msg")
	}
	if dst == nil {
		return -1, fmt.Errorf("SendTo: nil dst")
	}
	endpoint, err := p.Swarm.GetEndpoint(dst.String())
	if err == nil && endpoint != nil {
		size, err := p.UDPSocket.SendMessage(msg, endpoint)
		return size, err
	}
	return 0, nil
}

// Close stops current instance
func (p *PeerToPeer) Close() error {
	hash := "Unknown hash"
	if p.Dht != nil {
		hash = p.Dht.NetworkHash
	}
	Info("Stopping instance %s", hash)
	p.deactivateInterface()
	p.stopPeers()
	p.Shutdown = true
	p.stopDHT()
	p.stopSocket()
	p.stopInterface()
	p.ReadyToStop = true
	Info("Instance %s stopped", hash)
	return nil
}

func (p *PeerToPeer) deactivateInterface() error {
	if p.Interface == nil {
		return fmt.Errorf("nil interface")
	}
	for i, ip := range ActiveInterfaces {
		if ip.Equal(p.Interface.GetIP()) {
			ActiveInterfaces = append(ActiveInterfaces[:i], ActiveInterfaces[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("Interface %s wasn't listed as active", p.Interface.GetName())
}

func (p *PeerToPeer) stopInterface() error {
	if p.Interface == nil {
		return fmt.Errorf("nil interface")
	}
	err := p.Interface.Close()
	if err != nil {
		Error("Failed to close TAP interface: %s", err)
		return err
	}
	return nil
}

func (p *PeerToPeer) stopPeers() error {
	if p.Swarm == nil {
		return fmt.Errorf("nil peer list")
	}
	peers := p.Swarm.Get()
	for i, peer := range peers {
		peer.SetState(PeerStateDisconnect, p)
		p.Swarm.Update(i, peer)
	}
	stopStarted := time.Now()
	for p.Swarm.Length() > 0 {
		if time.Since(stopStarted) > time.Duration(time.Second*5) {
			Warn("Peer remove timeout passed")
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	Debug("All peers under this instance has been removed")
	return nil
}

func (p *PeerToPeer) stopDHT() error {
	if p.Dht == nil {
		return fmt.Errorf("nil dht")
	}
	err := p.Dht.Close()
	if err != nil {
		Error("Failed to stop DHT: %s", err)
		return err
	}
	return nil
}

func (p *PeerToPeer) stopSocket() error {
	if p.UDPSocket == nil {
		return fmt.Errorf("nil socket")
	}
	return p.UDPSocket.Close()
}
