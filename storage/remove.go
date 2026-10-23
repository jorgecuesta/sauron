package storage

import "strings"

// NodeRef names an internal node in one network.
type NodeRef struct {
	Network string
	Node    string
}

// SplitNodeKey returns the network and node of a "network:node[:protocol]"
// key. The network is up to the first ':' and, when hasProtocol, the protocol
// is after the last one, so a node name containing ':' survives.
func SplitNodeKey(key string, hasProtocol bool) (network, node string, ok bool) {
	network, rest, ok := strings.Cut(key, ":")
	if !ok {
		return "", "", false
	}
	if hasProtocol {
		i := strings.LastIndex(rest, ":")
		if i < 0 {
			return "", "", false
		}
		rest = rest[:i]
	}
	return network, rest, true
}

// RemoveNodes deletes every entry of a node for which keep returns false (a
// node no longer in the config) and returns each removed node once.
func (s *HeightStore) RemoveNodes(keep func(network, node string) bool) []NodeRef {
	seen := map[NodeRef]bool{}
	var removed []NodeRef
	s.data.Range(func(key string, _ *NodeMetrics) bool {
		network, node, ok := SplitNodeKey(key, true)
		if ok && !keep(network, node) {
			s.data.Delete(key)
			ref := NodeRef{Network: network, Node: node}
			if !seen[ref] {
				seen[ref] = true
				removed = append(removed, ref)
			}
		}
		return true
	})
	return removed
}

// RemoveNodes deletes the health of every node for which keep returns false
// and returns how many entries it removed.
func (s *HealthStore) RemoveNodes(keep func(network, node string) bool) int {
	n := 0
	s.data.Range(func(key string, _ *EndpointHealth) bool {
		if network, node, ok := SplitNodeKey(key, true); ok && !keep(network, node) {
			s.data.Delete(key)
			n++
		}
		return true
	})
	return n
}

// RemoveNodes deletes the archival status of every node for which keep
// returns false and returns how many it removed.
func (s *ArchivalStore) RemoveNodes(keep func(network, node string) bool) int {
	n := 0
	s.data.Range(func(key string, _ *ArchivalStatus) bool {
		if network, node, ok := SplitNodeKey(key, false); ok && !keep(network, node) {
			s.data.Delete(key)
			n++
		}
		return true
	})
	return n
}

// URLs returns the URLs of every stored endpoint of endpointType, whatever
// its state.
func (s *ExternalEndpointStore) URLs(endpointType string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	urls := make(map[string]bool)
	for _, ep := range s.endpoints {
		if ep.Type == endpointType {
			urls[ep.URL] = true
		}
	}
	return urls
}

// RemoveExternals deletes the endpoints of every external ring for which keep
// returns false (an external, or one of its rings, gone from the config) and
// returns copies of them.
func (s *ExternalEndpointStore) RemoveExternals(keep func(externalName, ringURL string) bool) []*ExternalEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []*ExternalEndpoint
	for key, ep := range s.endpoints {
		if !keep(ep.ExternalName, ep.RingURL) {
			epCopy := *ep
			removed = append(removed, &epCopy)
			delete(s.endpoints, key)
		}
	}
	// Forget the transport of gRPC URLs no remaining endpoint has.
	for url := range s.grpcTransports {
		still := false
		for _, ep := range s.endpoints {
			if ep.Type == "grpc" && ep.URL == url {
				still = true
				break
			}
		}
		if !still {
			delete(s.grpcTransports, url)
		}
	}
	return removed
}
