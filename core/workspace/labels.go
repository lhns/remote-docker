package workspace

// The labels the client stamps on what it creates, and the agent reads back.
//
// Here rather than beside the code that writes them because both ends have to
// AGREE on the strings. The client writes them; the agent answers questions
// about them, and a second spelling on either side does not fail to compile --
// it silently matches nothing, so a filter returns an empty set and whatever
// depended on it decides there is nothing there.

// ManagedLabel marks a volume as one this client created, and is what makes
// garbage collection safe: the rd- prefix alone proves nothing, since a user is
// entitled to name a volume "rd-backups".
const ManagedLabel = "com.github.lhns.remote-docker"

// ManagedShare is ManagedLabel's value on a volume backing a bind mount, as
// distinct from anything else that might one day be managed.
const ManagedShare = "share"

// ManagedSeed is ManagedLabel's value on the temporary container that exists
// only to have a volume mounted while the client fills it (ADR 0043). It is
// never started and is removed as soon as the tar is in, so the label is for
// the case where that did not happen: a container orphaned by a crash holds
// the volume, and this is what says whose it was.
const ManagedSeed = "seed"

// OwnerLabel marks every container this client creates.
//
// The workspace daemon may be shared between accounts (ADR 0012), so its event
// stream carries other people's containers. Without a mark of our own, port
// forwarding would open listeners on this machine because somebody else ran
// docker compose up.
const OwnerLabel = "com.github.lhns.remote-docker.owner"

// ClientLabel marks which of an account's MACHINES created something.
//
// The owner label is not enough once one account is used from two machines:
// both label their volumes and containers with the same account, so each
// machine's collector would delete the other's volumes and each machine would
// think the other's containers depended on its connection (ADR 0029).
//
// This is also how the agent tells whose volumes are whose when it needs to
// know which reverse-tunnel port a machine's volumes were built for. A volume
// created before this label existed carries no client and is attributed to
// nobody, which is the safe answer rather than a guess.
const ClientLabel = "com.github.lhns.remote-docker.client"

// PortsLabel records which port the user asked for, per container port:
//
//	80/tcp=8080,443/tcp=8443
//
// The daemon assigns the published port itself so nobody collides, and this
// says which local port the client opens in front of it (ADR 0008).
//
// A label rather than memory: forwards are rebuilt from the daemon's container
// list, so after a restart or a reconnect nothing else remembers what was
// typed. Keyed by container port, which is the half that does not change.
const PortsLabel = "com.github.lhns.remote-docker.ports"
