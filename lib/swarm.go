package ptp

import (
	"fmt"
	"net"
	"sync"
)

// ListOperation will specify which operation is performed on peer list
type ListOperation int

// List operations
const (
	OperateDelete ListOperation = 0 // Delete entry from map
	OperateUpdate ListOperation = 1 // Add/Update entry in map
)

// Swarm is for handling list of peers with all mappings
type Swarm struct {
	peers      map[string]*NetworkPeer // Map of peers in this swarm
	tableIPID  map[string]string       // Mapping for IP->ID
	tableMacID map[string]string       // Mapping for MAC->ID
	lock       sync.RWMutex            // Mutex for the tables
}

// Init will initialize Swarm's maps
func (l *Swarm) Init() {
	l.peers = make(map[string]*NetworkPeer)
	l.tableIPID = make(map[string]string)
	l.tableMacID = make(map[string]string)
}

func (l *Swarm) operate(action ListOperation, id string, peer *NetworkPeer) error {
	if l.peers == nil {
		return fmt.Errorf("peers is nil - not initialized")
	}
	if l.tableIPID == nil {
		return fmt.Errorf("IP-ID table is nil - not initialized")
	}
	if l.tableMacID == nil {
		return fmt.Errorf("Mac-ID table is nil - not initialized")
	}
	l.lock.Lock()
	defer l.lock.Unlock()
	if action == OperateUpdate {
		l.peers[id] = peer
		ip := ""
		mac := ""
		if peer.PeerLocalIP != nil {
			ip = peer.PeerLocalIP.String()
		}
		if peer.PeerHW != nil {
			mac = peer.PeerHW.String()
		}
		l.updateTables(id, ip, mac)
		return nil
	} else if action == OperateDelete {
		peer, exists := l.peers[id]
		if !exists {
			return fmt.Errorf("can't delete peer: entry doesn't exists")
		}
		l.deleteTables(peer.PeerLocalIP.String(), peer.PeerHW.String())
		delete(l.peers, id)
		return nil
	}
	return nil
}

func (l *Swarm) updateTables(id, ip, mac string) {
	if ip != "" {
		l.tableIPID[ip] = id
	}
	if mac != "" {
		l.tableMacID[mac] = id
	}
}

func (l *Swarm) deleteTables(ip, mac string) {
	if ip != "" {
		_, exists := l.tableIPID[ip]
		if exists {
			delete(l.tableIPID, ip)
		}
	}
	if mac != "" {
		_, exists := l.tableMacID[mac]
		if exists {
			delete(l.tableMacID, mac)
		}
	}
}

// Delete will remove entry with specified ID from peer list
func (l *Swarm) Delete(id string) {
	l.operate(OperateDelete, id, nil)
}

// Update will append/edit peer in list
func (l *Swarm) Update(id string, peer *NetworkPeer) {
	l.operate(OperateUpdate, id, peer)
}

// Get returns copy of map with all peers
func (l *Swarm) Get() map[string]*NetworkPeer {
	result := make(map[string]*NetworkPeer)
	l.lock.RLock()
	for id, peer := range l.peers {
		result[id] = peer
	}
	l.lock.RUnlock()
	return result
}

// GetPeer returns single peer by id
func (l *Swarm) GetPeer(id string) *NetworkPeer {
	l.lock.RLock()
	peer, exists := l.peers[id]
	l.lock.RUnlock()
	if exists {
		return peer
	}
	return nil
}

// GetEndpoint returns endpoint address and proxy id
func (l *Swarm) GetEndpoint(mac string) (*net.UDPAddr, error) {
	l.lock.RLock()
	defer l.lock.RUnlock()
	id, exists := l.tableMacID[mac]
	if exists {
		peer, exists := l.peers[id]
		if exists && peer.Endpoint != nil {
			return peer.Endpoint, nil
		}
	}
	return nil, fmt.Errorf("Specified hardware address was not found in table")
}

// GetID returns ID by specified IP
func (l *Swarm) GetID(ip string) (string, error) {
	l.lock.RLock()
	defer l.lock.RUnlock()
	id, exists := l.tableIPID[ip]
	if exists {
		return id, nil
	}
	return "", fmt.Errorf("Specified IP was not found in table")
}

// Length returns size of peer list map
func (l *Swarm) Length() int {
	return len(l.peers)
}

// RunPeer should be called once on each peer when added
// to list
func (l *Swarm) RunPeer(id string, p *PeerToPeer) {
	Info("Running peer %s", id)
	l.lock.RLock()
	defer l.lock.RUnlock()
	if !l.peers[id].IsRunning() {
		go l.peers[id].Run(p)
	} else {
		Info("Peer %s is already running", id)
	}
}
