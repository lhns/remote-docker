package session

import "github.com/lhns/remote-docker/client/internal/rewrite"

// ContainerList and ContainerInspect show the proxy's clients the ports this
// session opens, by the rule that opens them (ADR 0008). Like
// explainMountFailure they use the live connection if there is one and never
// open one: the request being answered holds it. Without one the daemon's
// numbers are relayed.
func (s *Session) ContainerList(body []byte) ([]byte, bool) {
	live, ok := s.gate.currentLive()
	if !ok {
		return nil, false
	}
	return rewrite.ListPorts(body, portRule(live))
}

func (s *Session) ContainerInspect(body []byte) ([]byte, bool) {
	live, ok := s.gate.currentLive()
	if !ok {
		return nil, false
	}
	return rewrite.InspectPorts(body, portRule(live))
}
