package session

import "fmt"

// explainMountFailure names why the daemon could not mount a share volume of
// ours on a container start (see proxy.Proxy.ExplainMountFailure). It uses the
// live connection if there is one and never opens one: the start request being
// answered already holds it. Without one it declines, and the daemon's own
// text is relayed.
func (s *Session) explainMountFailure(volume, client string, port int) (msg, fix string, ok bool) {
	live, connected := s.gate.currentLive()
	if !connected {
		return "", "", false
	}
	return explainMount(live.clientID, live.info.NFSPort, volume, client, port)
}

// explainMount decides between the three causes a refused mount can have, from
// facts this client holds: the client id in the volume's name, the port in its
// options, and this session's own id and port. A refused connection proves
// nothing listens on that port; whether the owning client is connected
// somewhere else is not known here, so the message does not say.
func explainMount(ourClient string, ourPort int, volume, client string, port int) (msg, fix string, ok bool) {
	switch {
	case client != ourClient:
		return fmt.Sprintf("volume %s: its directory is shared by another client (%s), and nothing is serving it", volume, client),
			"start the container from that client, or remove it and run it again from this one", true
	case ourPort > 0 && port != ourPort:
		return fmt.Sprintf("volume %s: made for tunnel port %d, and this session's is %d", volume, port, ourPort),
			"remove the container and run it again; compose: up --force-recreate", true
	case ourPort > 0:
		return fmt.Sprintf("volume %s: this session's file server is not answering on port %d", volume, port),
			"run `" + OurCommand("restart") + "`", true
	}
	return "", "", false
}
