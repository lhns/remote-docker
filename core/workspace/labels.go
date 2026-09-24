package workspace

// The labels the client stamps and the agent reads back. Shared because a
// second spelling on either side does not fail to compile: it matches nothing,
// and whatever filtered on it decides there is nothing there.

// ManagedLabel marks a volume this client created. It, not the rd- prefix, is
// what makes garbage collection safe: a user may name a volume "rd-backups".
const ManagedLabel = "com.github.lhns.remote-docker"

// ManagedShare is ManagedLabel's value on a volume backing a bind mount.
const ManagedShare = "share"

// OwnerLabel marks every container this client creates. A shared daemon (ADR
// 0012) streams other accounts' containers too, and without it this machine
// would forward their ports.
const OwnerLabel = "com.github.lhns.remote-docker.owner"

// ClientLabel marks which of an account's machines created something, so one
// machine's collector leaves another's volumes alone and the agent can tell
// which reverse-tunnel port a machine's volumes were built for (ADR 0029). A
// volume without it is attributed to nobody.
const ClientLabel = "com.github.lhns.remote-docker.client"

// PortsLabel records the port the user asked for, per container port
// (`80/tcp=8080,443/tcp=8443`); the daemon picks the published one (ADR 0008).
// It is the only record: forwards are rebuilt from the container list after
// every reconnect.
const PortsLabel = "com.github.lhns.remote-docker.ports"
