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
